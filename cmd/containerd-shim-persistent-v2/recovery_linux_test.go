package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/persistent-rootfs/persistent-rootfs/internal/persistentrootfs"
)

type deleteProbe struct {
	taskAPI.TTRPCTaskService
	calls     int
	stateErr  error
	deleteErr error
}

func (probe *deleteProbe) State(context.Context, *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {
	return nil, probe.stateErr
}

func (probe *deleteProbe) Delete(context.Context, *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	probe.calls++
	if probe.deleteErr != nil {
		return nil, probe.deleteErr
	}
	return &taskAPI.DeleteResponse{}, nil
}

func TestDeleteChecksBundleBeforeDelegate(t *testing.T) {
	bundle := t.TempDir()
	t.Chdir(bundle)
	cfg := persistentrootfs.DefaultConfig()
	cfg.StateDir = t.TempDir()
	lifecycle, err := persistentrootfs.NewShimLifecycle(cfg)
	if err != nil {
		t.Fatal(err)
	}
	probe := &deleteProbe{}
	service := &persistentTaskService{TTRPCTaskService: probe, lifecycle: lifecycle}
	ctx := namespaces.WithNamespace(context.Background(), "test")
	request := &taskAPI.DeleteRequest{ID: filepath.Base(bundle)}
	for _, tc := range []struct {
		spec    string
		allowed bool
	}{
		{`{"annotations":{"persistent-rootfs.io/enabled":"true","persistent-rootfs.io/container-name":"app","io.kubernetes.cri.container-name":"app"}}`, false},
		{`{"annotations":{}}`, true},
	} {
		if err := os.WriteFile("config.json", []byte(tc.spec), 0o600); err != nil {
			t.Fatal(err)
		}
		probe.calls = 0
		_, err := service.Delete(ctx, request)
		if (err == nil) != tc.allowed || (probe.calls == 1) != tc.allowed {
			t.Fatalf("allowed=%v calls=%d err=%v", tc.allowed, probe.calls, err)
		}
	}
	request.ID = "absent-task"
	probe.calls = 0
	probe.stateErr = errdefs.ErrNotFound
	if _, err := service.Delete(ctx, request); !errors.Is(err, errdefs.ErrNotFound) || probe.calls != 0 {
		t.Fatalf("missing task: calls=%d err=%v", probe.calls, err)
	}
	probe.stateErr = nil
	if _, err := service.Delete(ctx, request); !errors.Is(err, persistentrootfs.ErrBundleAbsent) || probe.calls != 0 {
		t.Fatalf("existing task without bundle: calls=%d err=%v", probe.calls, err)
	}
}

func TestDeleteRetryAfterCleanupFailure(t *testing.T) {
	bundle := t.TempDir()
	t.Chdir(bundle)
	id := filepath.Base(bundle)
	cfg := persistentrootfs.DefaultConfig()
	cfg.StateDir = t.TempDir()
	volume := t.TempDir()
	cfg.AllowedVolumePrefixes = []string{volume}
	lifecycle, err := persistentrootfs.NewShimLifecycle(cfg)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(volume, ".persistent-rootfs-runtime", "app")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	lease := persistentrootfs.ShimLease{Version: 3, ContainerID: id}
	journal := persistentrootfs.StateFile(cfg.StateDir, "test\x00"+id)
	state := persistentrootfs.RuntimeState{Version: 3, ContainerID: id, Bundle: bundle, Lower: filepath.Join(bundle, ".persistent-rootfs-lower"), Merged: filepath.Join(bundle, "rootfs"), StateRoot: root, RootfsID: "app", Status: "releasing", Lease: lease}
	if err := persistentrootfs.SaveJSON(journal, state, 0o600); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(root, "active.json")
	if err := os.WriteFile(active, []byte("incomplete"), 0o600); err != nil {
		t.Fatal(err)
	}
	probe := &deleteProbe{}
	service := &persistentTaskService{TTRPCTaskService: probe, lifecycle: lifecycle}
	ctx := namespaces.WithNamespace(context.Background(), "test")
	request := &taskAPI.DeleteRequest{ID: id}
	if _, err := service.Delete(ctx, request); err == nil || probe.calls != 1 {
		t.Fatalf("first delete: calls=%d err=%v", probe.calls, err)
	}
	if err := persistentrootfs.SaveJSON(active, lease, 0o600); err != nil {
		t.Fatal(err)
	}
	probe.deleteErr = errgrpc.ToGRPCf(errdefs.ErrNotFound, "container not created")
	if _, err := service.Delete(ctx, request); err != probe.deleteErr || probe.calls != 2 {
		t.Fatalf("retry: calls=%d err=%v", probe.calls, err)
	}
	for _, path := range []string{active, journal} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("retry did not remove %s: %v", path, err)
		}
	}
}

type legacyStartProbe struct {
	shim.Manager
	started  bool
	address  string
	startErr error
}

func (probe *legacyStartProbe) Start(ctx context.Context, id string, opts shim.StartOpts) (shim.BootstrapParams, error) {
	probe.started = true
	if probe.startErr != nil {
		return shim.BootstrapParams{}, probe.startErr
	}
	address, err := shim.SocketAddress(ctx, opts.Address, "sandbox", false)
	probe.address = address
	return shim.BootstrapParams{Address: address}, err
}

func TestLegacyStartPublishesGroupAddress(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(cwd); err != nil {
			t.Error(err)
		}
	})
	if err := os.WriteFile("config.json", []byte(`{"annotations":{"io.kubernetes.cri.sandbox-id":"sandbox"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-namespace", "k8s.io", "-id", "sidecar", "-address", "/run/containerd/containerd.sock", "start"}
	probe := &legacyStartProbe{}
	var output bytes.Buffer
	if err := os.Mkdir("address", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := startLegacy(context.Background(), probe, args, &output); err == nil || probe.started {
		t.Fatalf("started after publication failure: %v", err)
	}
	if err := os.Remove("address"); err != nil {
		t.Fatal(err)
	}
	probe.startErr = errors.New("failed to start manager")
	if err := startLegacy(context.Background(), probe, args, &output); !errors.Is(err, probe.startErr) {
		t.Fatal(err)
	}
	if _, err := os.Stat("address"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("address published after failed start: %v", err)
	}
	if err := os.WriteFile("address", []byte("old-address"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := startLegacy(context.Background(), probe, args, &output); !errors.Is(err, probe.startErr) {
		t.Fatal(err)
	}
	if previous, err := os.ReadFile("address"); err != nil || string(previous) != "old-address" {
		t.Fatalf("previous address changed: %q %v", previous, err)
	}
	probe.startErr = nil
	if err := startLegacy(context.Background(), probe, args, &output); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile("address")
	if err != nil || string(stored) != probe.address || output.String() != probe.address {
		t.Fatalf("address publication: %q %v", stored, err)
	}
}

func TestRecoveryRuntimeWithMissingOptions(t *testing.T) {
	state := persistentrootfs.RuntimeState{Bundle: t.TempDir(), RuncRoot: "/alternate", RuncBinary: "custom-runc"}
	runtime, err := recoveryRuntime(state, "k8s.io")
	if err != nil || runtime.Root != filepath.Join(state.RuncRoot, "k8s.io") || runtime.Command != state.RuncBinary {
		t.Fatalf("runtime: %#v %v", runtime, err)
	}
	state.RuncBinary = ""
	if _, err := recoveryRuntime(state, "k8s.io"); err == nil {
		t.Fatal("accepted ambiguous legacy runtime")
	}
}
