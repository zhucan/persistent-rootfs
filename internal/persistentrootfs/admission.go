package persistentrootfs

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const DefaultRuntimeClass = "persistent-rootfs"

var digestImage = regexp.MustCompile(`@sha256:[0-9a-fA-F]{64}$`)

type AdmissionReview struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Request    *AdmissionRequest  `json:"request,omitempty"`
	Response   *AdmissionResponse `json:"response,omitempty"`
}

type AdmissionRequest struct {
	UID         string          `json:"uid"`
	Namespace   string          `json:"namespace"`
	Operation   string          `json:"operation"`
	SubResource string          `json:"subResource"`
	Resource    GroupResource   `json:"resource"`
	Object      json.RawMessage `json:"object"`
	OldObject   json.RawMessage `json:"oldObject,omitempty"`
}

type GroupResource struct {
	Group    string `json:"group"`
	Version  string `json:"version"`
	Resource string `json:"resource"`
}

type AdmissionResponse struct {
	UID       string  `json:"uid"`
	Allowed   bool    `json:"allowed"`
	Status    *Status `json:"status,omitempty"`
	Patch     []byte  `json:"patch,omitempty"`
	PatchType *string `json:"patchType,omitempty"`
}

type Status struct {
	Message string `json:"message"`
	Code    int    `json:"code,omitempty"`
}

type podAdmission struct {
	Metadata struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		RuntimeClassName string `json:"runtimeClassName"`
		Volumes          []struct {
			Name                  string `json:"name"`
			PersistentVolumeClaim *struct {
				ClaimName string `json:"claimName"`
				ReadOnly  bool   `json:"readOnly"`
			} `json:"persistentVolumeClaim"`
		} `json:"volumes"`
		Containers          []podContainer `json:"containers"`
		InitContainers      []podContainer `json:"initContainers"`
		EphemeralContainers []podContainer `json:"ephemeralContainers"`
	} `json:"spec"`
}

type podContainer struct {
	Name          string            `json:"name"`
	Image         string            `json:"image"`
	VolumeMounts  []podVolumeMount  `json:"volumeMounts"`
	VolumeDevices []podVolumeDevice `json:"volumeDevices"`
}

type podVolumeDevice struct {
	Name       string `json:"name"`
	DevicePath string `json:"devicePath"`
}

type podVolumeMount struct {
	Name              string  `json:"name"`
	MountPath         string  `json:"mountPath"`
	ReadOnly          bool    `json:"readOnly"`
	SubPath           string  `json:"subPath"`
	SubPathExpr       string  `json:"subPathExpr"`
	MountPropagation  *string `json:"mountPropagation"`
	RecursiveReadOnly string  `json:"recursiveReadOnly"`
}

type PVCValidator func(namespace, claimName string) error

type jsonPatch struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// MutatePod validates the persistent-rootfs contract and injects the runtime
// class and hidden CSI volume mount. Non-enabled Pods pass through unchanged.
func MutatePod(request *AdmissionRequest, runtimeClass, mountPath string) AdmissionResponse {
	return MutatePodWithPVCValidator(request, runtimeClass, mountPath, nil)
}

// MutatePodWithPVCValidator additionally verifies the referenced state PVC.
func MutatePodWithPVCValidator(request *AdmissionRequest, runtimeClass, mountPath string, validatePVC PVCValidator) AdmissionResponse {
	return admitPod(request, runtimeClass, mountPath, validatePVC, true)
}

// ValidatePodWithPVCValidator verifies the final Pod after all mutation.
func ValidatePodWithPVCValidator(request *AdmissionRequest, runtimeClass, mountPath string, validatePVC PVCValidator) AdmissionResponse {
	return admitPod(request, runtimeClass, mountPath, validatePVC, false)
}

func admitPod(request *AdmissionRequest, runtimeClass, mountPath string, validatePVC PVCValidator, mutate bool) AdmissionResponse {
	response := AdmissionResponse{Allowed: true}
	if request == nil {
		return deny("admission request is missing")
	}
	response.UID = request.UID
	isCreate := request.Resource.Resource == "pods" && request.Operation == "CREATE" && request.SubResource == ""
	isUpdate := request.Resource.Resource == "pods" && request.Operation == "UPDATE" && request.SubResource == ""
	isEphemeralUpdate := request.Resource.Resource == "pods" && request.Operation == "UPDATE" && request.SubResource == "ephemeralcontainers"
	if !isCreate && !isUpdate && !isEphemeralUpdate {
		return response
	}
	if isUpdate {
		mutate = false
	}
	var pod podAdmission
	if err := json.Unmarshal(request.Object, &pod); err != nil {
		return denyUID(request.UID, fmt.Sprintf("decode Pod: %v", err))
	}
	annotations := pod.Metadata.Annotations
	if isUpdate {
		var oldPod podAdmission
		if len(request.OldObject) == 0 {
			return denyUID(request.UID, "old Pod is missing from update request")
		}
		if err := json.Unmarshal(request.OldObject, &oldPod); err != nil {
			return denyUID(request.UID, fmt.Sprintf("decode old Pod: %v", err))
		}
		oldEnabled := oldPod.Metadata.Annotations[AnnotationEnabled] == "true"
		newEnabled := annotations[AnnotationEnabled] == "true"
		if !oldEnabled && !newEnabled {
			return response
		}
		for _, name := range []string{
			AnnotationEnabled,
			AnnotationRootfsID,
			AnnotationBaseID,
			AnnotationContainerName,
			AnnotationVolumeName,
		} {
			if oldPod.Metadata.Annotations[name] != annotations[name] {
				return denyUID(request.UID, fmt.Sprintf("annotation %q is immutable after Pod creation", name))
			}
		}
	}
	if annotations[AnnotationEnabled] != "true" {
		return response
	}
	rootfsID := annotations[AnnotationRootfsID]
	if err := ValidateRootfsID(rootfsID); err != nil {
		return denyUID(request.UID, err.Error())
	}
	target := annotations[AnnotationContainerName]
	if target == "" {
		return denyUID(request.UID, AnnotationContainerName+" is required")
	}
	volumeName := annotations[AnnotationVolumeName]
	if volumeName == "" {
		volumeName = "rootfs-state"
	}
	volumeFound := false
	claimName := ""
	stateVolumeNames := map[string]struct{}{}
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == volumeName && (volume.PersistentVolumeClaim == nil || volume.PersistentVolumeClaim.ClaimName == "") {
			return denyUID(request.UID, fmt.Sprintf("volume %q must be a PersistentVolumeClaim", volumeName))
		}
		if volume.Name == volumeName {
			claimName = volume.PersistentVolumeClaim.ClaimName
			if volume.PersistentVolumeClaim.ReadOnly {
				return denyUID(request.UID, fmt.Sprintf("state PVC volume %q must not be read-only", volumeName))
			}
			volumeFound = true
		}
	}
	if !volumeFound {
		return denyUID(request.UID, fmt.Sprintf("CSI/PVC volume %q is required", volumeName))
	}
	if validatePVC != nil {
		if err := validatePVC(request.Namespace, claimName); err != nil {
			return denyUID(request.UID, fmt.Sprintf("validate state PVC %q: %v", claimName, err))
		}
	}
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == claimName {
			stateVolumeNames[volume.Name] = struct{}{}
			if volume.Name != volumeName {
				return denyUID(request.UID, fmt.Sprintf("state PVC %q must not be referenced by alias volume %q", claimName, volume.Name))
			}
		}
	}
	if containerName, mountName, found := stateMountInContainers(pod.Spec.InitContainers, stateVolumeNames, ""); found {
		return denyUID(request.UID, fmt.Sprintf("init container %q must not mount state volume %q", containerName, mountName))
	}
	if containerName, mountName, found := stateMountInContainers(pod.Spec.EphemeralContainers, stateVolumeNames, ""); found {
		return denyUID(request.UID, fmt.Sprintf("ephemeral container %q must not mount state volume %q", containerName, mountName))
	}
	if containerName, mountName, found := stateMountInContainers(pod.Spec.Containers, stateVolumeNames, target); found {
		return denyUID(request.UID, fmt.Sprintf("container %q must not mount state volume %q", containerName, mountName))
	}

	containerIndex := -1
	mountExists := false
	for index, container := range pod.Spec.Containers {
		if container.Name != target {
			continue
		}
		containerIndex = index
		if !digestImage.MatchString(container.Image) {
			return denyUID(request.UID, fmt.Sprintf("container %q image must use an immutable sha256 digest", target))
		}
		for _, device := range container.VolumeDevices {
			if _, ok := stateVolumeNames[device.Name]; ok {
				return denyUID(request.UID, fmt.Sprintf("target container %q must mount state volume %q as a filesystem", target, device.Name))
			}
		}
		for _, volumeMount := range container.VolumeMounts {
			if volumeMount.MountPath == mountPath && volumeMount.Name != volumeName {
				return denyUID(request.UID, fmt.Sprintf("mount path %q is already used by volume %q", mountPath, volumeMount.Name))
			}
			if volumeMount.Name == volumeName {
				if volumeMount.MountPath != mountPath {
					return denyUID(request.UID, fmt.Sprintf("volume %q must mount at %q", volumeName, mountPath))
				}
				if volumeMount.ReadOnly || volumeMount.SubPath != "" || volumeMount.SubPathExpr != "" || volumeMount.MountPropagation != nil || volumeMount.RecursiveReadOnly != "" {
					return denyUID(request.UID, fmt.Sprintf("state volume %q must use an unmodified read-write volume mount", volumeName))
				}
				mountExists = true
			}
		}
		break
	}
	if containerIndex < 0 {
		return denyUID(request.UID, fmt.Sprintf("target container %q not found", target))
	}
	if runtimeClass == "" {
		runtimeClass = DefaultRuntimeClass
	}
	if isEphemeralUpdate {
		return response
	}
	var patches []jsonPatch
	if pod.Spec.RuntimeClassName == "" {
		if !mutate {
			return denyUID(request.UID, fmt.Sprintf("runtimeClassName must be %q", runtimeClass))
		}
		patches = append(patches, jsonPatch{Op: "add", Path: "/spec/runtimeClassName", Value: runtimeClass})
	} else if pod.Spec.RuntimeClassName != runtimeClass {
		return denyUID(request.UID, fmt.Sprintf("runtimeClassName must be %q", runtimeClass))
	}
	if !mountExists {
		if !mutate {
			return denyUID(request.UID, fmt.Sprintf("volume %q must mount at %q", volumeName, mountPath))
		}
		path := fmt.Sprintf("/spec/containers/%d/volumeMounts", containerIndex)
		if pod.Spec.Containers[containerIndex].VolumeMounts == nil {
			patches = append(patches, jsonPatch{Op: "add", Path: path, Value: []map[string]string{{"name": volumeName, "mountPath": mountPath}}})
		} else {
			patches = append(patches, jsonPatch{Op: "add", Path: path + "/-", Value: map[string]string{"name": volumeName, "mountPath": mountPath}})
		}
	}
	if len(patches) == 0 {
		return response
	}
	patch, err := json.Marshal(patches)
	if err != nil {
		return denyUID(request.UID, err.Error())
	}
	patchType := "JSONPatch"
	response.Patch = patch
	response.PatchType = &patchType
	return response
}

func stateMountInContainers(containers []podContainer, stateVolumeNames map[string]struct{}, allowedContainer string) (string, string, bool) {
	for _, container := range containers {
		if container.Name == allowedContainer {
			continue
		}
		for _, mount := range container.VolumeMounts {
			if _, ok := stateVolumeNames[mount.Name]; ok {
				return container.Name, mount.Name, true
			}
		}
		for _, device := range container.VolumeDevices {
			if _, ok := stateVolumeNames[device.Name]; ok {
				return container.Name, device.Name, true
			}
		}
	}
	return "", "", false
}

func deny(message string) AdmissionResponse {
	return AdmissionResponse{Allowed: false, Status: &Status{Message: strings.TrimSpace(message), Code: 400}}
}

func denyUID(uid, message string) AdmissionResponse {
	response := deny(message)
	response.UID = uid
	return response
}
