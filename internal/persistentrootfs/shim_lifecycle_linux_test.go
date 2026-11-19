//go:build linux

package persistentrootfs

import (
	"bytes"
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func selectLayer(root, requested, base string, explicit bool) (string, string, error) {
	return selectLayerContext(context.Background(), root, requested, base, explicit)
}

func TestTaskLockDeadline(t *testing.T) {
	root := t.TempDir()
	base := &ShimLifecycle{config: Config{StateDir: root}}
	creator, err := base.ForTask("namespace", root, nil)
	if err != nil {
		t.Fatal(err)
	}
	deleter, err := base.ForTask("namespace", root, nil)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := creator.LockTask(context.Background(), "task")
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := deleter.LockTask(ctx, "task"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second operation: %v", err)
	}
}

func TestClaimIntentRecovery(t *testing.T) {
	for _, phase := range []string{"claiming", "releasing"} {
		for _, owner := range []string{"unpublished", "same", "other"} {
			t.Run(phase+"/"+owner, func(t *testing.T) {
				root := t.TempDir()
				layer := filepath.Join(root, rootfsDataDirectory, "app")
				if err := os.MkdirAll(layer, 0o700); err != nil {
					t.Fatal(err)
				}
				bundle := t.TempDir()
				lifecycle := &ShimLifecycle{config: Config{StateDir: t.TempDir(), AllowedVolumePrefixes: []string{root}}}
				lease := ShimLease{Version: shimStateVersion, ContainerID: "task", PID: 1234}
				state := RuntimeState{Version: shimStateVersion, ContainerID: "task", Status: phase, Bundle: bundle, Lower: filepath.Join(bundle, ".persistent-rootfs-lower"), Merged: filepath.Join(bundle, "rootfs"), StateRoot: layer, RootfsID: "app", Lease: lease}
				if err := SaveJSON(StateFile(lifecycle.config.StateDir, "task"), state, 0o600); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(layer, "active.json")
				if owner != "unpublished" {
					if owner == "other" {
						lease.PID++
					}
					if err := SaveJSON(path, lease, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				for attempt := 0; attempt < 2; attempt++ {
					if err := lifecycle.Delete("task", func(string) error { t.Fatal("unexpected unmount"); return nil }); err != nil {
						t.Fatal(err)
					}
				}
				_, err := os.Stat(path)
				if owner == "other" && err != nil || owner != "other" && !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("lease after recovery: %v", err)
				}
			})
		}
	}
}

func TestMetadataReadLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(path, bytes.Repeat([]byte(" "), 65<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadJSONNoFollow(path, &ShimLease{}); err == nil {
		t.Fatal("accepted oversized lease")
	}
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadJSONNoFollow(fifo, &RootfsMetadata{}); err == nil {
		t.Fatal("accepted FIFO")
	}
}

func TestStopWithoutJournal(t *testing.T) {
	bundle := t.TempDir()
	lifecycle := &ShimLifecycle{bundle: bundle}
	specPath := filepath.Join(bundle, "config.json")
	if err := os.WriteFile(specPath, []byte(`{"annotations":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if released, err := lifecycle.StopWithoutJournal("task"); err != nil || released {
		t.Fatalf("ordinary bundle: %v %v", released, err)
	}
	if err := os.WriteFile(specPath, []byte(`{"annotations":{"persistent-rootfs.io/enabled":"true","persistent-rootfs.io/container-name":"app","io.kubernetes.cri.container-name":"app"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.StopWithoutJournal("task"); err == nil {
		t.Fatal("accepted legacy managed bundle without journal")
	}
	if err := os.Remove(specPath); err != nil {
		t.Fatal(err)
	}
	if err := SaveJSON(filepath.Join(bundle, bundleMarker), taskMarker{ContainerID: "task"}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.StopWithoutJournal("task"); err == nil {
		t.Fatal("accepted active managed marker")
	}
}

func TestReleasedJournalAfterVolumeDisappears(t *testing.T) {
	bundle := t.TempDir()
	lifecycle := &ShimLifecycle{bundle: bundle, config: Config{StateDir: t.TempDir()}}
	path := StateFile(lifecycle.config.StateDir, "task")
	state := RuntimeState{Version: shimStateVersion, ContainerID: "task", Bundle: bundle, StateRoot: filepath.Join(t.TempDir(), "absent"), Status: "releasing"}
	if err := SaveJSON(path, state, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Delete("task", nil); err == nil {
		t.Fatal("discarded unconfirmed release")
	}
	state.Status = "released"
	if err := SaveJSON(path, state, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Delete("task", nil); err != nil {
		t.Fatal(err)
	}
	if released, err := lifecycle.StopWithoutJournal("task"); err != nil || !released {
		t.Fatalf("completed bundle: %v %v", released, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal still present: %v", err)
	}
}

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
		StateDir:              filepath.Join(root, "runtime-state"),
		VolumeDestination:     DefaultVolumeDestination,
		AllowedVolumePrefixes: []string{root},
		RequireBaseID:         true,
	}
	lifecycle, err := NewShimLifecycle(config)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle.SetRuntime("/custom/runc-root", "/custom/runc")
	_, err = lifecycle.Prepare("container-1", bundle, func(string) error {
		var journal RuntimeState
		if err := LoadJSON(StateFile(config.StateDir, "container-1"), &journal); err != nil || journal.Status != "preparing" || !bytes.Equal(journal.OriginalSpec, original) {
			t.Fatalf("missing preparation journal before lower mount: %v", err)
		}
		if journal.RuncRoot != "/custom/runc-root" || journal.RuncBinary != "/custom/runc" {
			t.Fatal("journal lost runtime configuration")
		}
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
	t.Run("preparing journal write fails", func(t *testing.T) {
		writeErr := errors.New("journal write failed")
		_, err := lifecycle.prepare("container-1", bundle, nil, nil, func(path string, value any, mode os.FileMode) error {
			if value.(RuntimeState).Status == "preparing" {
				return writeErr
			}
			return SaveJSON(path, value, mode)
		})
		if !errors.Is(err, writeErr) {
			t.Fatalf("prepare returned %v", err)
		}
		for _, path := range []string{leasePath, StateFile(config.StateDir, "container-1")} {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rollback left %s: %v", path, err)
			}
		}
		var marker taskMarker
		if err := LoadJSON(filepath.Join(bundle, bundleMarker), &marker); err != nil || marker != (taskMarker{ContainerID: "container-1", Released: true}) {
			t.Fatalf("rollback marker = %+v, err = %v", marker, err)
		}
	})
}

func TestDefaultLayerSurvivesContainerRename(t *testing.T) {
	root := t.TempDir()
	first, identity, err := selectLayer(root, "app", "image", false)
	if err != nil {
		t.Fatal(err)
	}
	second, reused, err := selectLayer(root, "server", "new-image", false)
	if err != nil || first != second || reused != identity || identity == "app" {
		t.Fatalf("default layer changed: %q %q %v", second, reused, err)
	}
	if _, _, err := selectLayer(root, "app", "image", true); err == nil {
		t.Fatal("explicit ID accepted on fixed layout")
	}
}

func TestInterruptedExplicitLayer(t *testing.T) {
	for _, marker := range []string{"", ".persistent-rootfs-123", "upper"} {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "workload"), 0o700); err != nil {
			t.Fatal(err)
		}
		if marker != "" {
			legacy := filepath.Join(root, "workload", "app")
			if err := os.Mkdir(legacy, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(legacy, marker), []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		_, _, err := selectLayer(root, "workload/app", "image", true)
		if (err == nil) != (marker != "upper") {
			t.Fatalf("marker %q: %v", marker, err)
		}
	}
}

func TestNewTaskRetainsUnreleasedLease(t *testing.T) {
	root := t.TempDir()
	lease, err := planShimLease(root, "old")
	if err != nil {
		t.Fatal(err)
	}
	lease.PID = -1
	for _, machine := range []string{lease.MachineID, "other-machine"} {
		lease.MachineID = machine
		path := filepath.Join(root, "active.json")
		if err := SaveJSON(path, lease, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := planShimLease(root, "new"); err == nil {
			t.Fatal("new task claimed unreleased layer")
		}
		var stored ShimLease
		if err := LoadJSON(path, &stored); err != nil || stored != lease {
			t.Fatalf("old lease changed: %v", err)
		}
	}
}

func TestInterruptedDefaultLayerInitialization(t *testing.T) {
	root := t.TempDir()
	defaultDir := filepath.Join(root, defaultLayerDirectory)
	if err := os.Mkdir(defaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defaultDir, ".persistent-rootfs-12345"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := selectLayer(root, "app", "image", false); err != nil {
		t.Fatal(err)
	}
}

func TestNamespaceScopedRecovery(t *testing.T) {
	root := t.TempDir()
	base := &ShimLifecycle{config: Config{StateDir: root}}
	firstBundle := filepath.Join(root, "first", "task")
	first, err := base.ForTask("first", firstBundle, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := base.ForTask("second", filepath.Join(root, "second", "task"), nil)
	if err != nil {
		t.Fatal(err)
	}
	firstPath, err := first.statePath("task")
	if err != nil {
		t.Fatal(err)
	}
	secondPath, err := second.statePath("task")
	if err != nil || firstPath == secondPath {
		t.Fatalf("same-ID state collision: %v", err)
	}
	legacy := StateFile(root, "task")
	if err := SaveJSON(legacy, RuntimeState{ContainerID: "task", Bundle: firstBundle}, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := first.statePath("task"); err != nil || got != legacy {
		t.Fatalf("legacy owner: %q %v", got, err)
	}
	if err := second.Delete("task", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatal(err)
	}
}

func TestRetryAfterPreparationBeforeDelegatedCreate(t *testing.T) {
	root := t.TempDir()
	bundle := t.TempDir()
	stateRoot := filepath.Join(root, rootfsDataDirectory, "app")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	base := &ShimLifecycle{config: Config{StateDir: t.TempDir(), AllowedVolumePrefixes: []string{root}}}
	busy := errors.New("runtime owns task")
	lifecycle, err := base.ForTask("namespace", bundle, func(RuntimeState) error { return busy })
	if err != nil {
		t.Fatal(err)
	}
	statePath, err := lifecycle.statePath("task")
	if err != nil {
		t.Fatal(err)
	}
	state := RuntimeState{Version: shimStateVersion, ContainerID: "task", Bundle: bundle, StateRoot: stateRoot, RootfsID: "app", Status: "active", Lower: filepath.Join(bundle, ".persistent-rootfs-lower"), Merged: filepath.Join(bundle, "rootfs"), OriginalSpec: []byte(`{"annotations":{}}`), SpecMode: 0o600}
	if err := SaveJSON(statePath, state, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.Prepare("task", bundle, nil, nil); !errors.Is(err, busy) {
		t.Fatalf("expected runtime refusal: %v", err)
	}
	busy = nil
	if _, err := lifecycle.Prepare("task", bundle, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale journal: %v", err)
	}
}

func TestLegacyLayerRequiresMatchingIdentity(t *testing.T) {
	root := t.TempDir()
	dir, err := ensureDirectoryTree(root, "workload/app", 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateShimMetadata(dir, "workload/app", "image"); err != nil {
		t.Fatal(err)
	}
	if path, _, err := selectLayer(root, "workload/app", "image", true); err != nil || path != dir {
		t.Fatalf("legacy selection: %q %v", path, err)
	}
	if _, _, err := selectLayer(root, "renamed", "image", false); err == nil {
		t.Fatal("initialized a new layer alongside existing state")
	}
}

func TestDeleteInterruptedPreparation(t *testing.T) {
	root := t.TempDir()
	bundle := t.TempDir()
	stateRoot := filepath.Join(root, rootfsDataDirectory, "app")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	lifecycle := &ShimLifecycle{config: Config{StateDir: t.TempDir(), AllowedVolumePrefixes: []string{root}}}
	state := RuntimeState{Version: shimStateVersion, ContainerID: "task", RootfsID: "app", Bundle: bundle, StateRoot: stateRoot, Lower: filepath.Join(bundle, ".persistent-rootfs-lower"), Merged: filepath.Join(bundle, "rootfs"), Status: "preparing", OriginalSpec: []byte("original"), SpecMode: 0o600}
	statePath := StateFile(lifecycle.config.StateDir, state.ContainerID)
	if err := SaveJSON(statePath, state, 0o600); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := lifecycle.Delete(state.ContainerID, func(string) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if restored, err := os.ReadFile(filepath.Join(bundle, "config.json")); err != nil || !bytes.Equal(restored, state.OriginalSpec) {
		t.Fatalf("restore interrupted preparation: %v", err)
	}
	t.Run("new boot owns lease", func(t *testing.T) {
		active, err := planShimLease(stateRoot, "new-task")
		if err != nil {
			t.Fatal(err)
		}
		state.Lease = active
		state.Lease.ContainerID = state.ContainerID
		state.Lease.BootID = "previous-boot"
		if err := SaveJSON(statePath, state, 0o600); err != nil {
			t.Fatal(err)
		}
		leasePath := filepath.Join(stateRoot, "active.json")
		if err := SaveJSON(leasePath, active, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := lifecycle.Delete(state.ContainerID, nil); err == nil {
			t.Fatal("accepted takeover without runtime check")
		}
		lifecycle.checkTaskAbsent = func(RuntimeState) error { return nil }
		if err := lifecycle.Delete(state.ContainerID, func(string) error { return errors.New("unexpected unmount") }); err != nil {
			t.Fatal(err)
		}
		var preserved ShimLease
		if err := LoadJSON(leasePath, &preserved); err != nil || preserved != active {
			t.Fatalf("new lease changed: %+v %v", preserved, err)
		}
		if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("previous journal remains: %v", err)
		}
	})
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
		Bundle:      bundle,
		Lower:       filepath.Join(bundle, ".persistent-rootfs-lower"),
		Merged:      filepath.Join(bundle, "rootfs"),
		StateRoot:   stateRoot,
		StatePath:   statePath,
		RootfsID:    "app",
		Lease:       lease,
		lowerMount:  true,
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
		Bundle:      bundle,
		Lower:       filepath.Join(bundle, ".persistent-rootfs-lower"),
		Merged:      filepath.Join(bundle, "rootfs"),
		StateRoot:   stateRoot,
		RootfsID:    "app",
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

func TestValidateShimMetadataAllowsNewImageButNotNewRootfsID(t *testing.T) {
	root := t.TempDir()
	if err := validateShimMetadata(root, "app", "busybox:old"); err != nil {
		t.Fatal(err)
	}
	if err := validateShimMetadata(root, "app", "busybox:new"); err != nil {
		t.Fatal(err)
	}
	if err := validateShimMetadata(root, "other", "busybox:old"); err == nil {
		t.Fatal("accepted a different rootfs ID")
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
