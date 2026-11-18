//go:build linux

package persistentrootfs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrepareRestoresSpecAndLeaseWhenLowerMountFails(t *testing.T) {
	root := t.TempDir()
	volume := filepath.Join(root, "volume")
	bundle := filepath.Join(root, "bundle")
	for _, path := range []string{volume, filepath.Join(bundle, "rootfs")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	specPath := filepath.Join(bundle, "config.json")
	original := []byte("{\n  \"root\": {\"path\": \"rootfs\"},\n  \"annotations\": {\n    \"persistent-rootfs.io/enabled\": \"true\",\n    \"persistent-rootfs.io/rootfs-id\": \"app\",\n    \"persistent-rootfs.io/container-name\": \"app\",\n    \"io.kubernetes.cri.container-type\": \"container\",\n    \"io.kubernetes.cri.container-name\": \"app\",\n    \"io.kubernetes.cri.image-name\": \"example/app@sha256:abc\"\n  },\n  \"mounts\": [{\"destination\": \"/run/.persistent-rootfs\", \"source\": \"" + volume + "\", \"type\": \"bind\"}]\n}\n")
	if err := os.WriteFile(specPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	config := Config{
		StateDir: filepath.Join(root, "runtime-state"),
		VolumeDestination: DefaultVolumeDestination,
		AllowedVolumePrefixes: []string{root},
		RequireBaseID: true,
	}
	lifecycle, err := NewShimLifecycle(config)
	if err != nil {
		t.Fatal(err)
	}
	_, err = lifecycle.Prepare("container-1", bundle, func(string) error {
		return errors.New("mount failed")
	}, func(string) error {
		return nil
	})
	if err == nil {
		t.Fatal("Prepare succeeded despite lower mount failure")
	}
	restored, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, original) {
		t.Fatalf("OCI spec was not restored:\n%s", restored)
	}
	leasePath := filepath.Join(volume, rootfsDataDirectory, "app", "active.json")
	if _, err := os.Stat(leasePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lease remains after rollback: %v", err)
	}
}

func TestCleanupRetainsLeaseAndStateWhenUnmountFails(t *testing.T) {
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	stateRoot := filepath.Join(root, "volume", rootfsDataDirectory, "app")
	stateDir := filepath.Join(root, "runtime-state")
	for _, path := range []string{bundle, filepath.Join(bundle, "rootfs"), filepath.Join(bundle, ".persistent-rootfs-lower"), stateRoot, stateDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	statePath := StateFile(stateDir, "container-1")
	leasePath := filepath.Join(stateRoot, "active.json")
	lease := ShimLease{Version: shimStateVersion, ContainerID: "container-1", MachineID: "machine", BootID: "boot", PID: 1, ProcessStartTime: "start"}
	if err := SaveJSON(statePath, RuntimeState{Version: shimStateVersion, ContainerID: "container-1", Status: "active", RootfsID: "app", Lease: lease}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveJSON(leasePath, lease, 0o600); err != nil {
		t.Fatal(err)
	}
	lifecycle := &ShimLifecycle{config: Config{AllowedVolumePrefixes: []string{root}}}
	session := &ShimSession{
		ContainerID: "container-1",
		Bundle: bundle,
		Lower: filepath.Join(bundle, ".persistent-rootfs-lower"),
		Merged: filepath.Join(bundle, "rootfs"),
		StateRoot: stateRoot,
		StatePath: statePath,
		RootfsID: "app",
		Lease: lease,
		lowerMount: true,
	}
	if err := lifecycle.cleanup(session, func(string) error { return errors.New("unmount failed") }); err == nil {
		t.Fatal("cleanup succeeded despite lower unmount failure")
	}
	for _, path := range []string{statePath, leasePath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("recovery file %q was removed: %v", path, err)
		}
	}
	if err := lifecycle.cleanup(session, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{statePath, leasePath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("recovery file %q still exists: %v", path, err)
		}
	}
}

func TestCleanupRejectsStateRootWithWrongRootfsID(t *testing.T) {
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	stateRoot := filepath.Join(root, "volume", rootfsDataDirectory, "other")
	for _, path := range []string{bundle, filepath.Join(bundle, "rootfs"), filepath.Join(bundle, ".persistent-rootfs-lower"), stateRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	lifecycle := &ShimLifecycle{config: Config{AllowedVolumePrefixes: []string{root}}}
	err := lifecycle.cleanup(&ShimSession{
		ContainerID: "container-1",
		Bundle: bundle,
		Lower: filepath.Join(bundle, ".persistent-rootfs-lower"),
		Merged: filepath.Join(bundle, "rootfs"),
		StateRoot: stateRoot,
		RootfsID: "app",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "does not match rootfs ID") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDeleteRejectsUnsupportedStateVersion(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := SaveJSON(StateFile(stateDir, "container-1"), RuntimeState{Version: shimStateVersion + 1}, 0o600); err != nil {
		t.Fatal(err)
	}
	lifecycle := &ShimLifecycle{config: Config{StateDir: stateDir, AllowedVolumePrefixes: []string{root}}}
	if err := lifecycle.Delete("container-1", nil); err == nil {
		t.Fatal("unsupported runtime state version was accepted")
	}
}

func TestValidateShimMetadataRejectsUnsupportedVersion(t *testing.T) {
	root := t.TempDir()
	metadata := RootfsMetadata{Version: shimStateVersion + 1, RootfsID: "app", BaseID: "image", CreatedAt: time.Now()}
	if err := SaveJSON(filepath.Join(root, "metadata.json"), metadata, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateShimMetadata(root, "app", "image"); err == nil {
		t.Fatal("unsupported metadata version was accepted")
	}
}

func TestProcessStartTimeMatchesCurrentProcess(t *testing.T) {
	startTime, err := processStartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if startTime == "" || !processMatches(os.Getpid(), startTime) {
		t.Fatal("current process identity did not match")
	}
	if processMatches(os.Getpid(), startTime+"0") {
		t.Fatal("incorrect process start time matched")
	}
}

func TestReleaseShimLeaseRejectsChangedOwner(t *testing.T) {
	root := t.TempDir()
	lease := ShimLease{
		Version: shimStateVersion, ContainerID: "container-1",
		MachineID: "machine-a", BootID: "boot-a", PID: 10, ProcessStartTime: "start-a",
	}
	if err := SaveJSON(filepath.Join(root, "active.json"), lease, 0o600); err != nil {
		t.Fatal(err)
	}
	err := releaseShimLease(root, "container-1", ShimLease{
		Version: shimStateVersion, ContainerID: "container-1",
		MachineID: "machine-b", BootID: "boot-a", PID: 10, ProcessStartTime: "start-a",
	})
	if err == nil || !strings.Contains(err.Error(), "owner changed") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "active.json")); err != nil {
		t.Fatalf("lease was removed after owner mismatch: %v", err)
	}
}
