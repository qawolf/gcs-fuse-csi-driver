/*
Copyright 2018 The Kubernetes Authors.
Copyright 2022 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	sidecarmounter "github.com/googlecloudplatform/gcs-fuse-csi-driver/pkg/sidecar_mounter"
	"github.com/googlecloudplatform/gcs-fuse-csi-driver/pkg/util"
	"github.com/googlecloudplatform/gcs-fuse-csi-driver/pkg/webhook"
	"k8s.io/klog/v2"
)

var (
	gcsfusePath    = flag.String("gcsfuse-path", "/gcsfuse", "gcsfuse path")
	volumeBasePath = flag.String("volume-base-path", "/gcsfuse-tmp/.volumes", "volume base path")
	gracePeriod    = flag.Int("grace-period", 30, "grace period for gcsfuse termination")
	// This is set at compile time.
	version = "unknown"
)

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	klog.Infof("Running Google Cloud Storage FUSE CSI driver sidecar mounter version %v", version)
	socketPathPattern := *volumeBasePath + "/*/socket"
	socketPaths, err := filepath.Glob(socketPathPattern)
	if err != nil {
		klog.Fatalf("failed to look up socket paths: %v", err)
	}
	mounter := sidecarmounter.New(*gcsfusePath)
	server := &http.Server{
		Addr:    ":8080",
		Handler: http.DefaultServeMux,
	}
	http.DefaultServeMux.Handle("/mount", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		type MountRequest struct {
			VolumeName   string `json:"volumeName"`
			ObjectPrefix string `json:"objectPrefix"`
		}
		mountRequest := &MountRequest{}
		if err := json.NewDecoder(r.Body).Decode(mountRequest); err != nil {
			klog.Errorf("failed to decode the request body: %v", err)
			http.Error(w, fmt.Sprintf("failed to decode the request body: %v", err), http.StatusBadRequest)
			return
		}
		for _, sp := range socketPaths {
			// If the request is not for the volume of this socketPath, skip it.
			if filepath.Base(filepath.Dir(sp)) != mountRequest.VolumeName {
				continue
			}
			errWriter := sidecarmounter.NewErrorWriter(filepath.Join(filepath.Dir(sp), "error"))
			mountConfig, err := prepareMountConfig(sp, mountRequest.ObjectPrefix)
			if err != nil {
				errMsg := fmt.Sprintf("failed prepare mount config: socket path %q: %v\n", sp, err)
				klog.Errorf(errMsg)
				if _, e := errWriter.Write([]byte(errMsg)); e != nil {
					klog.Errorf("failed to write the error message %q: %v", errMsg, e)
				}
				continue
			}
			mountConfig.ErrWriter = errWriter

			go func(mc *sidecarmounter.MountConfig) {
				if cmd, ok := mounter.GetCmds()[mountRequest.VolumeName]; ok {
					klog.V(4).Infof("killing existing gcsfuse process: %v", cmd)
					err := cmd.Process.Kill()
					if err != nil {
						klog.Errorf("failed to kill process %v with error: %v", cmd, err)
					}
				}
				cmd, err := mounter.Mount(mc)
				if err != nil {
					errMsg := fmt.Sprintf("failed to mount bucket %q for volume %q: %v\n", mc.BucketName, mc.VolumeName, err)
					klog.Errorf(errMsg)
					if _, e := errWriter.Write([]byte(errMsg)); e != nil {
						klog.Errorf("failed to write the error message %q: %v", errMsg, e)
					}

					return
				}

				if err = cmd.Start(); err != nil {
					errMsg := fmt.Sprintf("failed to start gcsfuse with error: %v\n", err)
					klog.Errorf(errMsg)
					if _, e := errWriter.Write([]byte(errMsg)); e != nil {
						klog.Errorf("failed to write the error message %q: %v", errMsg, e)
					}

					return
				}

				// Since the gcsfuse has taken over the file descriptor,
				// closing the file descriptor to avoid other process forking it.
				syscall.Close(mc.FileDescriptor)
				if err = cmd.Wait(); err != nil {
					errMsg := fmt.Sprintf("gcsfuse exited with error: %v\n", err)
					klog.Errorf(errMsg)
					if _, e := errWriter.Write([]byte(errMsg)); e != nil {
						klog.Errorf("failed to write the error message %q: %v", errMsg, e)
					}
				} else {
					klog.Infof("[%v] gcsfuse exited normally.", mc.VolumeName)
				}
			}(mountConfig)
		}
		return
	}))

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM)
	klog.Info("waiting for SIGTERM signal...")

	// Monitor the exit file.
	// If the exit file is detected, send a syscall.SIGTERM signal to the signal channel.
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		for {
			<-ticker.C
			if _, err := os.Stat(*volumeBasePath + "/exit"); err != nil {
				continue
			}
			klog.Infof("all the other containers terminated in the Pod, exiting the sidecar container. Sleep %v seconds before terminating gcsfuse processes.", *gracePeriod)
			time.Sleep(time.Duration(*gracePeriod) * time.Second)

			for _, cmd := range mounter.GetCmds() {
				klog.V(4).Infof("killing gcsfuse process: %v", cmd)
				err := cmd.Process.Kill()
				if err != nil {
					klog.Errorf("failed to kill process %v with error: %v", cmd, err)
				}
			}
			_ = server.Close()
			c <- syscall.SIGTERM

			return
		}
	}()
	if err := server.ListenAndServe(); err != nil {
		klog.Fatalf("failed to start the http server: %v", err)
	}
	<-c // blocking the process
	klog.Info("received SIGTERM signal, waiting for all the gcsfuse processes exit...")

	klog.Info("exiting sidecar mounter...")
}

// Fetch the following information from a given socket path:
// 1. Pod volume name
// 2. The file descriptor
// 3. GCS bucket name
// 4. Mount options passing to gcsfuse (passed by the csi mounter).
func prepareMountConfig(sp string, dir string) (*sidecarmounter.MountConfig, error) {
	// socket path pattern: /gcsfuse-tmp/.volumes/<volume-name>/socket
	volumeName := filepath.Base(filepath.Dir(sp))
	mc := sidecarmounter.MountConfig{
		VolumeName: volumeName,
		CacheDir:   filepath.Join(webhook.SidecarContainerCacheVolumeMountPath, ".volumes", volumeName),
		ConfigFile: filepath.Join(webhook.SidecarContainerTmpVolumeMountPath, ".volumes", volumeName, "config.yaml"),
	}

	klog.Infof("connecting to socket %q", sp)
	c, err := net.Dial("unix", sp)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to the socket %q: %w", sp, err)
	}

	fd, msg, err := util.RecvMsg(c)
	if err != nil {
		return nil, fmt.Errorf("failed to receive mount options from the socket %q: %w", sp, err)
	}
	// as we got all the information from the socket, closing the connection and deleting the socket
	c.Close()
	if err = syscall.Unlink(sp); err != nil {
		klog.Errorf("failed to close socket %q: %v", sp, err)
	}

	mc.FileDescriptor = fd

	if err := json.Unmarshal(msg, &mc); err != nil {
		return nil, fmt.Errorf("failed to unmarshal the mount config: %w", err)
	}

	if mc.BucketName == "" {
		return nil, fmt.Errorf("failed to fetch bucket name from CSI driver")
	}
	options := []string{}
	for _, opt := range mc.Options {
		if strings.Contains(opt, "only_dir=") {
			continue
		}
		options = append(options, opt)
	}
	mc.Options = append(options, "only_dir="+dir)
	return &mc, nil
}
