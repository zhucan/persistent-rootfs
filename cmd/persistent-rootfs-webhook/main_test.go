package main

import (
	"context"
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

func TestPVCValidatorUsesInformerCache(t *testing.T) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name: "state",
			Labels: map[string]string{persistentrootfs.LabelStateVolume: "true"},
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
		Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}},
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
