package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/persistent-rootfs/persistent-rootfs/internal/persistentrootfs"
)

const maxAdmissionBodyBytes = 4 << 20

func main() {
	listen := flag.String("listen", ":8443", "HTTPS listen address")
	certFile := flag.String("tls-cert-file", "/tls/tls.crt", "TLS certificate")
	keyFile := flag.String("tls-private-key-file", "/tls/tls.key", "TLS private key")
	runtimeClass := flag.String("runtime-class", persistentrootfs.DefaultRuntimeClass, "RuntimeClass injected into selected Pods")
	mountPath := flag.String("mount-path", persistentrootfs.DefaultVolumeDestination, "hidden rootfs state mount path")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	kube, err := newKubePVCClient(ctx)
	if err != nil {
		log.Fatalf("initialize Kubernetes PVC client: %v", err)
	}

	handler := http.NewServeMux()
	handler.HandleFunc("/validate-pvc", func(writer http.ResponseWriter, request *http.Request) {
		handleAdmission(writer, request, admitPVC)
	})
	handler.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("ok\n"))
	})
	handler.HandleFunc("/mutate", func(writer http.ResponseWriter, request *http.Request) {
		handleAdmission(writer, request, func(review *persistentrootfs.AdmissionReview) persistentrootfs.AdmissionResponse {
			return persistentrootfs.MutatePodWithPVCValidator(review.Request, *runtimeClass, *mountPath, func(namespace, claimName string) error {
				return kube.validate(request.Context(), namespace, claimName)
			})
		})
	})
	handler.HandleFunc("/validate", func(writer http.ResponseWriter, request *http.Request) {
		handleAdmission(writer, request, func(review *persistentrootfs.AdmissionReview) persistentrootfs.AdmissionResponse {
			if review.Request != nil {
				if err := checkClaimUses(request.Context(), review.Request, func(ctx context.Context, namespace, name string) (bool, error) {
					claim, err := kube.client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
					if err != nil {
						return false, err
					}
					return claim.Labels[persistentrootfs.LabelStateVolume] == "true", nil
				}); err != nil {
					return persistentrootfs.AdmissionResponse{UID: review.Request.UID, Status: &persistentrootfs.Status{Code: http.StatusForbidden, Message: err.Error()}}
				}
			}
			return persistentrootfs.ValidatePodWithPVCValidator(review.Request, *runtimeClass, *mountPath, func(namespace, claimName string) error {
				return kube.validate(request.Context(), namespace, claimName)
			})
		})
	})

	server := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	log.Printf("persistent rootfs admission webhook listening on %s", *listen)
	if err := serveUntilShutdown(ctx, server, func() error { return server.ListenAndServeTLS(*certFile, *keyFile) }); err != nil {
		log.Fatal(err)
	}
}

func serveUntilShutdown(ctx context.Context, server *http.Server, serve func() error) error {
	serveDone := make(chan error, 1)
	go func() { serveDone <- serve() }()
	select {
	case err := <-serveDone:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown admission webhook: %w", err)
		}
		err := <-serveDone
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func handleAdmission(writer http.ResponseWriter, request *http.Request, admit func(*persistentrootfs.AdmissionReview) persistentrootfs.AdmissionResponse) {
	defer request.Body.Close()
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maxAdmissionBodyBytes)
	body, err := io.ReadAll(request.Body)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			http.Error(writer, "admission review is too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(writer, fmt.Sprintf("read admission review: %v", err), http.StatusBadRequest)
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	var review persistentrootfs.AdmissionReview
	if err := decoder.Decode(&review); err != nil {
		http.Error(writer, fmt.Sprintf("decode admission review: %v", err), http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		http.Error(writer, "decode admission review: multiple JSON values", http.StatusBadRequest)
		return
	}
	response := admit(&review)
	result := persistentrootfs.AdmissionReview{APIVersion: review.APIVersion, Kind: review.Kind, Response: &response}
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(result); err != nil {
		log.Printf("encode admission response: %v", err)
	}
}

type kubePVCClient struct {
	client kubernetes.Interface
	lister corelisters.PersistentVolumeClaimLister
	synced cache.InformerSynced
}

func newKubePVCClient(ctx context.Context) (*kubePVCClient, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	factory := informers.NewSharedInformerFactory(client, 0)
	pvcInformer := factory.Core().V1().PersistentVolumeClaims()
	informer := pvcInformer.Informer()
	kube := &kubePVCClient{
		client: client,
		lister: pvcInformer.Lister(),
		synced: informer.HasSynced,
	}
	factory.Start(ctx.Done())
	return kube, nil
}

func (k *kubePVCClient) validate(ctx context.Context, namespace, claimName string) error {
	if namespace == "" || claimName == "" {
		return fmt.Errorf("namespace and claim name are required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !k.synced() {
		pvc, err := k.client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, claimName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		return validateStatePVC(pvc)
	}
	pvc, err := k.lister.PersistentVolumeClaims(namespace).Get(claimName)
	if err != nil {
		return err
	}
	return validateStatePVC(pvc)
}

func validateStatePVC(pvc *corev1.PersistentVolumeClaim) error {
	if pvc.Labels[persistentrootfs.LabelStateVolume] != "true" {
		return fmt.Errorf("PVC must have label persistent-rootfs.io/state-volume=true")
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOncePod {
		return fmt.Errorf("PVC must use exactly ReadWriteOncePod")
	}
	if pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode != corev1.PersistentVolumeFilesystem {
		return fmt.Errorf("PVC must use Filesystem volume mode")
	}
	return nil
}

func admitPVC(review *persistentrootfs.AdmissionReview) persistentrootfs.AdmissionResponse {
	response := persistentrootfs.AdmissionResponse{}
	deny := func(message string) persistentrootfs.AdmissionResponse {
		response.Status = &persistentrootfs.Status{Message: message, Code: http.StatusForbidden}
		return response
	}
	request := review.Request
	if request == nil {
		return deny("missing PVC admission request")
	}
	response.UID = request.UID
	if request.Resource != (persistentrootfs.GroupResource{Version: "v1", Resource: "persistentvolumeclaims"}) || request.SubResource != "" || (request.Operation != "CREATE" && request.Operation != "UPDATE") {
		return deny("unexpected resource or operation for PVC validation")
	}
	var pvc *corev1.PersistentVolumeClaim
	if err := json.Unmarshal(request.Object, &pvc); err != nil || pvc == nil {
		return deny("invalid PVC in admission request")
	}
	selected := pvc.Labels[persistentrootfs.LabelStateVolume] == "true"
	if request.Operation == "UPDATE" {
		var oldPVC *corev1.PersistentVolumeClaim
		if err := json.Unmarshal(request.OldObject, &oldPVC); err != nil || oldPVC == nil {
			return deny("invalid old PVC in update request")
		}
		if oldPVC.Labels[persistentrootfs.LabelStateVolume] == "true" && !selected {
			return deny("persistent rootfs state PVC label cannot be removed")
		}
		if oldPVC.Labels[persistentrootfs.LabelStateVolume] != "true" && selected {
			return deny("persistent rootfs state PVCs must be labeled at creation")
		}
	}
	if selected {
		if err := validateStatePVC(pvc); err != nil {
			return deny(err.Error())
		}
	}
	response.Allowed = true
	return response
}
