package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/persistent-rootfs/persistent-rootfs/internal/persistentrootfs"
)

func TestShutdownDrainsActiveRequest(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		close(entered)
		<-release
		writer.WriteHeader(http.StatusNoContent)
	})}
	stoppedListening := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- serveUntilShutdown(ctx, server, func() error {
			defer close(stoppedListening)
			return server.Serve(listener)
		})
	}()
	client := &http.Client{Timeout: 5 * time.Second}
	responseDone := make(chan error, 1)
	go func() {
		response, err := client.Get("http://" + listener.Addr().String())
		if err == nil {
			response.Body.Close()
		}
		responseDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach handler")
	}
	cancel()
	select {
	case <-stoppedListening:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not stop")
	}
	select {
	case err := <-done:
		t.Fatalf("shutdown returned with active request: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	release <- struct{}{}
	if err := <-responseDone; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

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

func TestCacheReadinessTransition(t *testing.T) {
	synced := false
	kube := kubePVCClient{synced: func() bool { return synced }}
	calls := 0
	handler := kube.whenReady(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		writer.WriteHeader(http.StatusOK)
	}))
	for _, path := range []string{"/healthz", "/validate-pvc", "/validate", "/mutate"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != http.StatusServiceUnavailable || calls != 0 {
			t.Fatalf("unsynced %s: status=%d calls=%d", path, response.Code, calls)
		}
	}
	synced = true
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/validate", nil))
	if response.Code != http.StatusOK || calls != 1 {
		t.Fatalf("synced: status=%d calls=%d", response.Code, calls)
	}
}

func TestClaimAliasesShareOnlyTheLookup(t *testing.T) {
	pod := corev1.Pod{}
	for _, name := range []string{"rootfs-state", "data"} {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "shared"}}})
	}
	for _, protected := range []bool{false, true} {
		if protected {
			pod.Annotations = map[string]string{persistentrootfs.AnnotationEnabled: "true"}
		}
		raw, err := json.Marshal(pod)
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		err = checkClaimUses(context.Background(), &persistentrootfs.AdmissionRequest{Object: raw}, func(context.Context, string, string) (bool, error) {
			calls++
			return protected, nil
		})
		if calls != 1 || (err != nil) != protected {
			t.Fatalf("protected=%v: calls=%d err=%v", protected, calls, err)
		}
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
