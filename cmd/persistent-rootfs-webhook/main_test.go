package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/persistent-rootfs/persistent-rootfs/internal/persistentrootfs"
)

func TestPVCAdmission(t *testing.T) {
	const valid = `{"metadata":{"labels":{"persistent-rootfs.io/state-volume":"true"}},"spec":{"accessModes":["ReadWriteOncePod"]}}`
	const ordinary = `{"spec":{"accessModes":["ReadWriteOnce"]}}`
	for _, test := range []struct {
		name, operation, object, old string
		allowed                      bool
	}{
		{"default filesystem", "CREATE", valid, "", true},
		{"explicit filesystem", "CREATE", strings.Replace(valid, `"accessModes"`, `"volumeMode":"Filesystem","accessModes"`, 1), "", true},
		{"block", "CREATE", strings.Replace(valid, `"accessModes"`, `"volumeMode":"Block","accessModes"`, 1), "", false},
		{"wrong mode", "CREATE", strings.Replace(valid, "ReadWriteOncePod", "ReadWriteOnce", 1), "", false},
		{"extra mode", "CREATE", strings.Replace(valid, `"ReadWriteOncePod"`, `"ReadWriteOncePod","ReadWriteOnce"`, 1), "", false},
		{"label removed", "UPDATE", ordinary, valid, false},
		{"existing ordinary claim cannot become protected", "UPDATE", valid, ordinary, false},
		{"label disabled", "UPDATE", strings.Replace(valid, "true", "false", 1), valid, false},
		{"label added to invalid claim", "UPDATE", strings.Replace(valid, "ReadWriteOncePod", "ReadWriteOnce", 1), ordinary, false},
		{"unchanged state claim", "UPDATE", valid, valid, true},
		{"ordinary claim", "UPDATE", ordinary, ordinary, true},
		{"missing old claim", "UPDATE", valid, "", false},
		{"missing new claim", "CREATE", "null", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := &persistentrootfs.AdmissionRequest{UID: "review-uid", Resource: persistentrootfs.GroupResource{Version: "v1", Resource: "persistentvolumeclaims"}, Operation: test.operation, Object: json.RawMessage(test.object), OldObject: json.RawMessage(test.old)}
			response := admitPVC(&persistentrootfs.AdmissionReview{Request: request})
			if response.UID != request.UID || response.Allowed != test.allowed || (!response.Allowed && response.Status == nil) {
				t.Fatalf("unexpected admission result: %+v", response)
			}
		})
	}
}

func TestProtectedClaimUse(t *testing.T) {
	for _, test := range []struct {
		enabled, name, claim string
		allowed              bool
	}{
		{"false", "data", "protected", false},
		{"false", "data", "ordinary", true},
		{"true", "data", "protected", false},
		{"true", "rootfs-state", "protected", true},
		{"true", "rootfs-state", "ordinary", false},
	} {
		pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{persistentrootfs.AnnotationEnabled: test.enabled}}}
		pod.Spec.Volumes = []corev1.Volume{{Name: test.name, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: test.claim}}}}
		raw, err := json.Marshal(pod)
		if err != nil {
			t.Fatal(err)
		}
		err = checkClaimUses(context.Background(), &persistentrootfs.AdmissionRequest{Object: raw}, func(_ context.Context, _, name string) (bool, error) { return name == "protected", nil })
		if (err == nil) != test.allowed {
			t.Errorf("%+v: %v", test, err)
		}
	}
}

func TestPVCValidatorUsesInformerCache(t *testing.T) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "state",
			Labels:    map[string]string{persistentrootfs.LabelStateVolume: "true"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod},
		},
	}
	if err := indexer.Add(pvc); err != nil {
		t.Fatal(err)
	}
	validator := &kubePVCClient{lister: corelisters.NewPersistentVolumeClaimLister(indexer)}
	if err := validator.validate(context.Background(), "default", "state"); err != nil {
		t.Fatal(err)
	}
}

func TestPVCValidatorRejectsUnsafeCachedPVC(t *testing.T) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "state"},
		Spec:       corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}},
	}
	if err := indexer.Add(pvc); err != nil {
		t.Fatal(err)
	}
	validator := &kubePVCClient{lister: corelisters.NewPersistentVolumeClaimLister(indexer)}
	if err := validator.validate(context.Background(), "default", "state"); err == nil {
		t.Fatal("unsafe cached PVC was accepted")
	}
}

func TestHandleAdmissionRejectsNonPostRequest(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/validate", nil)
	response := httptest.NewRecorder()
	handleAdmission(response, request, func(*persistentrootfs.AdmissionReview) persistentrootfs.AdmissionResponse {
		t.Fatal("admission handler was called")
		return persistentrootfs.AdmissionResponse{}
	})
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d", response.Code)
	}
}

func TestHandleAdmissionRejectsOversizedRequest(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/validate", strings.NewReader(strings.Repeat("x", maxAdmissionBodyBytes+1)))
	response := httptest.NewRecorder()
	handleAdmission(response, request, func(*persistentrootfs.AdmissionReview) persistentrootfs.AdmissionResponse {
		t.Fatal("admission handler was called")
		return persistentrootfs.AdmissionResponse{}
	})
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d", response.Code)
	}
}
