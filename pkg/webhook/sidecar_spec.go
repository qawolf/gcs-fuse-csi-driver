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

package webhook

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/util/parsers"
	"k8s.io/utils/ptr"
)

const (
	SidecarContainerName                 = "gke-gcsfuse-sidecar"
	SidecarContainerTmpVolumeName        = "gke-gcsfuse-tmp"
	SidecarContainerTmpVolumeMountPath   = "/gcsfuse-tmp"
	SidecarContainerCacheVolumeName      = "gke-gcsfuse-cache"
	SidecarContainerCacheVolumeMountPath = "/gcsfuse-cache"

	// See the nonroot user discussion: https://github.com/GoogleContainerTools/distroless/issues/443
	NobodyUID = 65534
	NobodyGID = 65534
)

func GetSidecarContainerSpec(c *Config) v1.Container {
	resourceList := v1.ResourceList{}

	if c.CPULimit != resource.MustParse("0") {
		resourceList[v1.ResourceCPU] = c.CPULimit
	}

	if c.MemoryLimit != resource.MustParse("0") {
		resourceList[v1.ResourceMemory] = c.MemoryLimit
	}

	if c.EphemeralStorageLimit != resource.MustParse("0") {
		resourceList[v1.ResourceEphemeralStorage] = c.EphemeralStorageLimit
	}

	// The sidecar container follows Restricted Pod Security Standard,
	// see https://kubernetes.io/docs/concepts/security/pod-security-standards/#restricted
	container := v1.Container{
		Name:            SidecarContainerName,
		Image:           c.ContainerImage,
		ImagePullPolicy: v1.PullPolicy(c.ImagePullPolicy),
		SecurityContext: &v1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities: &v1.Capabilities{
				Drop: []v1.Capability{
					v1.Capability("ALL"),
				},
			},
			SeccompProfile: &v1.SeccompProfile{Type: v1.SeccompProfileTypeRuntimeDefault},
			RunAsNonRoot:   ptr.To(true),
			RunAsUser:      ptr.To(int64(NobodyUID)),
			RunAsGroup:     ptr.To(int64(NobodyGID)),
		},
		Args: []string{
			"--v=5",
			fmt.Sprintf("--grace-period=%v", c.TerminationGracePeriodSeconds),
		},
		Resources: v1.ResourceRequirements{
			Limits:   resourceList,
			Requests: resourceList,
		},
		VolumeMounts: []v1.VolumeMount{
			{
				Name:      SidecarContainerTmpVolumeName,
				MountPath: SidecarContainerTmpVolumeMountPath,
			},
			{
				Name:      SidecarContainerCacheVolumeName,
				MountPath: SidecarContainerCacheVolumeMountPath,
			},
		},
	}

	return container
}

func GetSidecarContainerVolumeSpec() []v1.Volume {
	return []v1.Volume{
		{
			Name: SidecarContainerTmpVolumeName,
			VolumeSource: v1.VolumeSource{
				EmptyDir: &v1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: SidecarContainerCacheVolumeName,
			VolumeSource: v1.VolumeSource{
				EmptyDir: &v1.EmptyDirVolumeSource{},
			},
		},
	}
}

// ValidatePodHasSidecarContainerInjected validates the following:
// 1. One of the container name matches the sidecar container name.
// 2. The image repo matches.
// 3. The container uses the temp volume.
// 4. The temp volume have correct volume mount paths.
// 5. The Pod has the temp volume. The temp volume has to be an emptyDir.
func ValidatePodHasSidecarContainerInjected(image string, pod *v1.Pod) bool {
	containerInjected := false
	tempVolumeInjected := false

	expectedImageRepo, _, _, err := parsers.ParseImageName(image)
	if err != nil {
		klog.Errorf("Could not parse expected image : name %q, error: %v", image, err)

		return false
	}

	for _, c := range pod.Spec.Containers {
		if c.Name == SidecarContainerName {
			inputImageRepo, _, _, err := parsers.ParseImageName(c.Image)
			if err != nil {
				klog.Errorf("Could not parse input image : name %q, error: %v", image, err)

				return false
			}

			if inputImageRepo == expectedImageRepo &&
				c.SecurityContext != nil &&
				*c.SecurityContext.RunAsUser == NobodyUID &&
				*c.SecurityContext.RunAsGroup == NobodyGID {
				containerInjected = true
			}

			for _, v := range c.VolumeMounts {
				if v.Name == SidecarContainerTmpVolumeName && v.MountPath == SidecarContainerTmpVolumeMountPath {
					tempVolumeInjected = true
				}
			}

			break
		}
	}

	if !containerInjected || !tempVolumeInjected {
		return false
	}

	tempVolumeInjected = false

	for _, v := range pod.Spec.Volumes {
		if v.Name == SidecarContainerTmpVolumeName && v.VolumeSource.EmptyDir != nil {
			tempVolumeInjected = true
		}
	}

	return containerInjected && tempVolumeInjected
}
