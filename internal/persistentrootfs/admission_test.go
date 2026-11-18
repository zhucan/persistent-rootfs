package persistentrootfs

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestMutatePodRejectsInvalidStatePVC(t *testing.T) {
	digest := strings.Repeat("a", 64)
	pod := map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{
			AnnotationEnabled: "true", AnnotationRootfsID: "workspace-0", AnnotationContainerName: "app",
		}},
		"spec": map[string]any{
			"volumes": []map[string]any{{"name": "rootfs-state", "persistentVolumeClaim": map[string]string{"claimName": "rootfs-state"}}},
			"containers": []map[string]any{{"name": "app", "image": "registry.example/app@sha256:" + digest}},
		},
	}
	raw, _ := json.Marshal(pod)
	request := &AdmissionRequest{UID: "3", Namespace: "default", Operation: "CREATE", Resource: GroupResource{Resource: "pods"}, Object: raw}
	response := MutatePodWithPVCValidator(request, "", DefaultVolumeDestination, func(namespace, claimName string) error {
		if namespace != "default" || claimName != "rootfs-state" {
			t.Fatalf("unexpected PVC lookup %s/%s", namespace, claimName)
		}
		return errors.New("PVC must use ReadWriteOncePod")
	})
	if response.Allowed || response.Status == nil || !strings.Contains(response.Status.Message, "ReadWriteOncePod") {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestMutatePodRejectsStateVolumeMountedBySidecar(t *testing.T) {
	digest := strings.Repeat("a", 64)
	pod := map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{
			AnnotationEnabled: "true", AnnotationRootfsID: "workspace-0", AnnotationContainerName: "app",
		}},
		"spec": map[string]any{
			"volumes": []map[string]any{{"name": "rootfs-state", "persistentVolumeClaim": map[string]string{"claimName": "rootfs-state"}}},
			"containers": []map[string]any{
				{"name": "app", "image": "registry.example/app@sha256:" + digest},
				{"name": "sidecar", "image": "registry.example/sidecar@sha256:" + digest, "volumeMounts": []map[string]string{{"name": "rootfs-state", "mountPath": "/state"}}},
			},
		},
	}
	raw, _ := json.Marshal(pod)
	response := MutatePodWithPVCValidator(&AdmissionRequest{UID: "4", Namespace: "default", Operation: "CREATE", Resource: GroupResource{Resource: "pods"}, Object: raw}, "", DefaultVolumeDestination, func(string, string) error { return nil })
	if response.Allowed || response.Status == nil || !strings.Contains(response.Status.Message, "sidecar") {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestMutatePodRejectsModifiedStateVolumeMount(t *testing.T) {
	digest := strings.Repeat("a", 64)
	pod := map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{
			AnnotationEnabled: "true", AnnotationRootfsID: "workspace-0", AnnotationContainerName: "app",
		}},
		"spec": map[string]any{
			"volumes": []map[string]any{{"name": "rootfs-state", "persistentVolumeClaim": map[string]string{"claimName": "rootfs-state"}}},
			"containers": []map[string]any{{"name": "app", "image": "registry.example/app@sha256:" + digest, "volumeMounts": []map[string]any{{"name": "rootfs-state", "mountPath": DefaultVolumeDestination, "subPath": "unsafe"}}}},
		},
	}
	raw, _ := json.Marshal(pod)
	response := MutatePodWithPVCValidator(&AdmissionRequest{UID: "5", Namespace: "default", Operation: "CREATE", Resource: GroupResource{Resource: "pods"}, Object: raw}, "", DefaultVolumeDestination, func(string, string) error { return nil })
	if response.Allowed || response.Status == nil || !strings.Contains(response.Status.Message, "unmodified") {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestMutatePodRejectsEphemeralContainerStateMount(t *testing.T) {
	digest := strings.Repeat("a", 64)
	pod := map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{
			AnnotationEnabled: "true", AnnotationRootfsID: "workspace-0", AnnotationContainerName: "app",
		}},
		"spec": map[string]any{
			"volumes": []map[string]any{{"name": "rootfs-state", "persistentVolumeClaim": map[string]string{"claimName": "rootfs-state"}}},
			"containers": []map[string]any{{"name": "app", "image": "registry.example/app@sha256:" + digest}},
			"ephemeralContainers": []map[string]any{{"name": "debug", "image": "busybox", "volumeMounts": []map[string]string{{"name": "rootfs-state", "mountPath": "/state"}}}},
		},
	}
	raw, _ := json.Marshal(pod)
	request := &AdmissionRequest{UID: "6", Namespace: "default", Operation: "UPDATE", SubResource: "ephemeralcontainers", Resource: GroupResource{Resource: "pods"}, Object: raw}
	response := MutatePodWithPVCValidator(request, "", DefaultVolumeDestination, func(string, string) error { return nil })
	if response.Allowed || response.Status == nil || !strings.Contains(response.Status.Message, "ephemeral") {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestValidatePodRequiresFinalRuntimeAndMount(t *testing.T) {
	digest := strings.Repeat("a", 64)
	pod := map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{
			AnnotationEnabled: "true", AnnotationRootfsID: "workspace-0", AnnotationContainerName: "app",
		}},
		"spec": map[string]any{
			"volumes": []map[string]any{{"name": "rootfs-state", "persistentVolumeClaim": map[string]string{"claimName": "rootfs-state"}}},
			"containers": []map[string]any{{"name": "app", "image": "registry.example/app@sha256:" + digest}},
		},
	}
	raw, _ := json.Marshal(pod)
	request := &AdmissionRequest{UID: "7", Namespace: "default", Operation: "CREATE", Resource: GroupResource{Resource: "pods"}, Object: raw}
	response := ValidatePodWithPVCValidator(request, DefaultRuntimeClass, DefaultVolumeDestination, func(string, string) error { return nil })
	if response.Allowed || response.Status == nil || !strings.Contains(response.Status.Message, "runtimeClassName") {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestMutatePodInjectsRuntimeAndMount(t *testing.T) {
	digest := strings.Repeat("a", 64)
	pod := map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{
			AnnotationEnabled: "true", AnnotationRootfsID: "workspace-0", AnnotationContainerName: "app",
		}},
		"spec": map[string]any{
			"volumes": []map[string]any{{"name": "rootfs-state", "persistentVolumeClaim": map[string]string{"claimName": "rootfs-state"}}},
			"containers": []map[string]any{{"name": "app", "image": "registry.example/app@sha256:" + digest}},
		},
	}
	raw, _ := json.Marshal(pod)
	response := MutatePod(&AdmissionRequest{UID: "1", Operation: "CREATE", Resource: GroupResource{Resource: "pods"}, Object: raw}, "persistent-rootfs", DefaultVolumeDestination)
	if !response.Allowed || response.PatchType == nil {
		t.Fatalf("unexpected response: %#v", response)
	}
	patch := string(response.Patch)
	for _, expected := range []string{"/spec/runtimeClassName", "/spec/containers/0/volumeMounts"} {
		if !strings.Contains(patch, expected) {
			t.Fatalf("patch %s does not contain %s", patch, expected)
		}
	}
}

func TestMutatePodRejectsMutableImage(t *testing.T) {
	raw := []byte(`{"metadata":{"annotations":{"persistent-rootfs.io/enabled":"true","persistent-rootfs.io/rootfs-id":"app","persistent-rootfs.io/container-name":"app"}},"spec":{"volumes":[{"name":"rootfs-state","persistentVolumeClaim":{"claimName":"rootfs-state"}}],"containers":[{"name":"app","image":"busybox:latest"}]}}`)
	response := MutatePod(&AdmissionRequest{UID: "2", Operation: "CREATE", Resource: GroupResource{Resource: "pods"}, Object: raw}, "", DefaultVolumeDestination)
	if response.Allowed || response.Status == nil || !strings.Contains(response.Status.Message, "digest") {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestValidatePodUpdateRejectsDisablingPersistence(t *testing.T) {
	oldPod := validPersistentPod(t)
	newPod := validPersistentPod(t)
	newPod["metadata"].(map[string]any)["annotations"].(map[string]string)[AnnotationEnabled] = "false"
	oldRaw, _ := json.Marshal(oldPod)
	newRaw, _ := json.Marshal(newPod)
	response := ValidatePodWithPVCValidator(&AdmissionRequest{
		UID: "8", Namespace: "default", Operation: "UPDATE",
		Resource: GroupResource{Resource: "pods"}, Object: newRaw, OldObject: oldRaw,
	}, DefaultRuntimeClass, DefaultVolumeDestination, func(string, string) error { return nil })
	if response.Allowed || response.Status == nil || !strings.Contains(response.Status.Message, "immutable") {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestValidatePodUpdateAllowsUnrelatedAnnotation(t *testing.T) {
	oldPod := validPersistentPod(t)
	newPod := validPersistentPod(t)
	newPod["metadata"].(map[string]any)["annotations"].(map[string]string)["example.com/revision"] = "2"
	oldRaw, _ := json.Marshal(oldPod)
	newRaw, _ := json.Marshal(newPod)
	response := ValidatePodWithPVCValidator(&AdmissionRequest{
		UID: "9", Namespace: "default", Operation: "UPDATE",
		Resource: GroupResource{Resource: "pods"}, Object: newRaw, OldObject: oldRaw,
	}, DefaultRuntimeClass, DefaultVolumeDestination, func(string, string) error { return nil })
	if !response.Allowed {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func validPersistentPod(t *testing.T) map[string]any {
	t.Helper()
	digest := strings.Repeat("a", 64)
	return map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{
			AnnotationEnabled: "true", AnnotationRootfsID: "workspace-0", AnnotationContainerName: "app",
		}},
		"spec": map[string]any{
			"runtimeClassName": DefaultRuntimeClass,
			"volumes": []map[string]any{{"name": "rootfs-state", "persistentVolumeClaim": map[string]string{"claimName": "rootfs-state"}}},
			"containers": []map[string]any{{
				"name": "app", "image": "registry.example/app@sha256:" + digest,
				"volumeMounts": []map[string]string{{"name": "rootfs-state", "mountPath": DefaultVolumeDestination}},
			}},
		},
	}
}
