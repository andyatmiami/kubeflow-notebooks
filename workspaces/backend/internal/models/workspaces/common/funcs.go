/*
Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package common

import (
	kubefloworgv1beta1 "github.com/kubeflow/notebooks/workspaces/controller/api/v1beta1"
	"k8s.io/utils/ptr"
)

const (
	UnknownHomeMountPath = "__UNKNOWN_HOME_MOUNT_PATH__"
)

func WskExists(wsk *kubefloworgv1beta1.WorkspaceKind) bool {
	return wsk != nil && wsk.UID != ""
}

func BuildPodMetadata(ws *kubefloworgv1beta1.Workspace) PodMetadata {
	podLabels := make(map[string]string)
	podAnnotations := make(map[string]string)
	if ws.Spec.PodTemplate.PodMetadata != nil {
		for k, v := range ws.Spec.PodTemplate.PodMetadata.Labels {
			podLabels[k] = v
		}
		for k, v := range ws.Spec.PodTemplate.PodMetadata.Annotations {
			podAnnotations[k] = v
		}
	}
	return PodMetadata{
		Labels:      podLabels,
		Annotations: podAnnotations,
	}
}

func BuildHomeVolume(ws *kubefloworgv1beta1.Workspace, wsk *kubefloworgv1beta1.WorkspaceKind) *PodVolumeInfo {
	if ws.Spec.PodTemplate.Volumes.Home == nil {
		return nil
	}

	homeMountPath := UnknownHomeMountPath
	if WskExists(wsk) {
		homeMountPath = wsk.Spec.PodTemplate.VolumeMounts.Home
	}

	return &PodVolumeInfo{
		PVCName:   *ws.Spec.PodTemplate.Volumes.Home,
		MountPath: homeMountPath,
		ReadOnly:  false,
	}
}

func BuildDataVolumes(ws *kubefloworgv1beta1.Workspace) []PodVolumeInfo {
	if len(ws.Spec.PodTemplate.Volumes.Data) == 0 {
		return nil
	}

	dataVolumes := make([]PodVolumeInfo, len(ws.Spec.PodTemplate.Volumes.Data))
	for i, volume := range ws.Spec.PodTemplate.Volumes.Data {
		dataVolumes[i] = PodVolumeInfo{
			PVCName:   volume.PVCName,
			MountPath: volume.MountPath,
			ReadOnly:  ptr.Deref(volume.ReadOnly, false),
		}
	}
	return dataVolumes
}
