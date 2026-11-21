//go:build linux

package persistentrootfs

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const shimStateVersion = 3

const bundleMarker = ".persistent-rootfs-task.json"

var ErrBundleAbsent = errors.New("task bundle is absent")

type taskMarker struct {
	ContainerID string `json:"containerID"`
	Released    bool   `json:"released"`
}

func (l *ShimLifecycle) StopWithoutJournal(id string) (bool, error) {
	if _, err := os.Lstat(l.bundle); errors.Is(err, os.ErrNotExist) {
		return false, ErrBundleAbsent
	} else if err != nil {
		return false, err
	}
	var marker taskMarker
	err := loadJSONNoFollow(filepath.Join(l.bundle, bundleMarker), &marker)
	if err == nil {
		if marker.ContainerID == id && marker.Released {
			return true, nil
		}
		return false, fmt.Errorf("managed bundle %q requires its original recovery journal", l.bundle)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	spec, _, _, err := ReadSpec(filepath.Join(l.bundle, "config.json"))
	if err != nil {
		return false, err
	}
	annotations, err := spec.Annotations()
	if err != nil {
		return false, err
	}
	selection, err := SelectContainer(annotations, false)
	if err != nil {
		return false, err
	}
	if selection.Selected {
		return false, fmt.Errorf("managed bundle %q has no recovery journal", l.bundle)
	}
	return false, nil
}

// ShimSession owns the mounts prepared for one containerd CreateTask call.
type ShimSession struct {
	ContainerID  string
	Bundle       string
	Lower        string
	Merged       string
	StateRoot    string
	StatePath    string
	RootfsID     string
	Lease        ShimLease
	original     []byte
	specMode     os.FileMode
	runtimeState RuntimeState
	lowerMount   bool
	overlayMount bool
}

// ShimLifecycle prepares persistent rootfs mounts inside the Runtime v2 shim.
type ShimLifecycle struct {
	ctx             context.Context
	config          Config
	namespace       string
	bundle          string
	checkTaskAbsent func(RuntimeState) error
	runcRoot        string
	runcBinary      string
}

func (l *ShimLifecycle) ForTask(namespace, bundle string, checkTaskAbsent func(RuntimeState) error) (*ShimLifecycle, error) {
	if namespace == "" || !filepath.IsAbs(bundle) {
		return nil, errors.New("task namespace and absolute bundle are required")
	}
	return &ShimLifecycle{ctx: context.Background(), config: l.config, namespace: namespace, bundle: filepath.Clean(bundle), checkTaskAbsent: checkTaskAbsent}, nil
}

func (l *ShimLifecycle) LockTask(ctx context.Context, id string) (*os.File, error) {
	l.ctx = ctx
	if err := os.MkdirAll(l.config.StateDir, 0o700); err != nil {
		return nil, err
	}
	key := filepath.Base(StateFile(l.config.StateDir, l.namespace+"\x00"+id))
	digest, err := hex.DecodeString(key[2:18])
	if err != nil {
		return nil, err
	}
	offset := int64(binary.BigEndian.Uint64(digest) >> 1)
	return acquireFileLock(ctx, filepath.Join(l.config.StateDir, "task-"+key[:2]+".lock"), func(fd int) error {
		if err := unix.Flock(fd, unix.LOCK_SH|unix.LOCK_NB); err != nil {
			return err
		}
		return unix.FcntlFlock(uintptr(fd), unix.F_OFD_SETLK, &unix.Flock_t{Type: unix.F_WRLCK, Start: offset, Len: 1})
	})
}

func (l *ShimLifecycle) lockContext() context.Context {
	if l.ctx == nil {
		return context.Background()
	}
	return l.ctx
}

func (l *ShimLifecycle) SetRuntime(root, binary string) {
	l.runcRoot, l.runcBinary = root, binary
}

func (l *ShimLifecycle) statePath(id string) (string, error) {
	legacy := StateFile(l.config.StateDir, id)
	if l.namespace == "" {
		return legacy, nil
	}
	scoped := StateFile(l.config.StateDir, l.namespace+"\x00"+id)
	if _, err := os.Lstat(scoped); err == nil || !errors.Is(err, os.ErrNotExist) {
		return scoped, err
	}
	var old RuntimeState
	if err := loadJSONNoFollow(legacy, &old); err == nil {
		if old.Bundle == l.bundle && old.ContainerID == id && filepath.Base(filepath.Dir(old.Bundle)) == l.namespace {
			return legacy, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return scoped, nil
}

func NewShimLifecycle(config Config) (*ShimLifecycle, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &ShimLifecycle{config: config}, nil
}

// Prepare mounts the containerd snapshot at a private lower path and mounts the
// CSI-backed overlay at bundle/rootfs. mountLower must mount the CreateTask
// request's rootfs mounts at the supplied path.
func (l *ShimLifecycle) Prepare(containerID, bundle string, mountLower, unmountLower func(string) error) (*ShimSession, error) {
	return l.prepare(containerID, bundle, mountLower, unmountLower, SaveJSON)
}

func (l *ShimLifecycle) prepare(containerID, bundle string, mountLower, unmountLower func(string) error, saveJournal func(string, any, os.FileMode) error) (*ShimSession, error) {
	bundle, err := filepath.Abs(bundle)
	if err != nil {
		return nil, err
	}
	specPath := filepath.Join(bundle, "config.json")
	var pending RuntimeState
	statePath, err := l.statePath(containerID)
	if err != nil {
		return nil, err
	}
	if err := loadJSONNoFollow(statePath, &pending); err == nil {
		if pending.Bundle != bundle || pending.ContainerID != containerID {
			return nil, errors.New("preparation journal belongs to another bundle or container")
		}
		bootID, err := readRequiredTrimmed("/proc/sys/kernel/random/boot_id")
		if err != nil {
			return nil, err
		}
		if pending.Status != "claiming" && pending.Status != "releasing" && pending.Status != "released" && pending.Lease.PID != os.Getpid() && pending.Lease.BootID == bootID && processMatches(pending.Lease.PID, pending.Lease.ProcessStartTime) {
			return nil, errors.New("container preparation still owned by a live shim")
		}
		if err := l.deleteState(containerID, unmountLower, &pending.Lease); err != nil {
			return nil, err
		}
		statePath, err = l.statePath(containerID)
		if err != nil {
			return nil, err
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	spec, original, mode, err := ReadSpec(specPath)
	if err != nil {
		return nil, err
	}
	annotations, err := spec.Annotations()
	if err != nil {
		return nil, err
	}
	selection, err := SelectContainer(annotations, l.config.RequireBaseID)
	if err != nil {
		return nil, err
	}
	if !selection.Selected {
		return nil, nil
	}

	volumeSource, err := spec.RemoveMount(l.config.VolumeDestination)
	if err != nil {
		return nil, err
	}
	volumeSource, err = resolveAllowedVolume(volumeSource, l.config.AllowedVolumePrefixes)
	if err != nil {
		return nil, err
	}
	dataRoot := filepath.Join(volumeSource, rootfsDataDirectory)
	if err := ensurePlainDirectory(dataRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create rootfs data directory: %w", err)
	}
	resolvedDataRoot, err := filepath.EvalSymlinks(dataRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve rootfs data directory: %w", err)
	}
	if !pathWithin(volumeSource, resolvedDataRoot) || filepath.Clean(resolvedDataRoot) != filepath.Clean(dataRoot) {
		return nil, fmt.Errorf("rootfs data directory escapes state volume: %q", resolvedDataRoot)
	}
	_, explicit := annotations[AnnotationRootfsID]
	stateRoot, rootfsID, err := selectLayerContext(l.lockContext(), dataRoot, selection.RootfsID, selection.BaseID, explicit)
	if err != nil {
		return nil, fmt.Errorf("create rootfs state directory: %w", err)
	}
	lock, err := lockFile(l.lockContext(), filepath.Join(stateRoot, "lock"))
	if err != nil {
		return nil, fmt.Errorf("lock persistent rootfs: %w", err)
	}
	defer lock.Close()

	if _, err := os.Lstat(statePath); err == nil {
		return nil, fmt.Errorf("container %q already has runtime state pending cleanup", containerID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	lease, err := planShimLease(stateRoot, containerID)
	if err != nil {
		return nil, err
	}

	s := &ShimSession{
		ContainerID: containerID,
		Bundle:      bundle,
		Lower:       filepath.Join(bundle, ".persistent-rootfs-lower"),
		Merged:      filepath.Join(bundle, "rootfs"),
		StateRoot:   stateRoot,
		StatePath:   statePath,
		RootfsID:    rootfsID,
		Lease:       lease,
		specMode:    mode,
	}
	fail := func(cause error) (*ShimSession, error) {
		failures := []error{cause}
		if cleanupErr := l.cleanup(s, unmountLower); cleanupErr != nil {
			failures = append(failures, fmt.Errorf("rollback mounts: %w", cleanupErr))
		}
		return nil, errors.Join(failures...)
	}
	for _, path := range []string{s.Lower, s.Merged} {
		if err := prepareMergedDirectory(path); err != nil {
			return nil, err
		}
	}
	for _, path := range []string{filepath.Join(stateRoot, "upper"), filepath.Join(stateRoot, "work")} {
		if err := ensurePlainDirectory(path, 0o700); err != nil {
			return nil, fmt.Errorf("prepare overlay directory %q: %w", path, err)
		}
	}
	modified, err := spec.Marshal()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(l.config.StateDir, 0o700); err != nil {
		return nil, err
	}
	state := RuntimeState{
		RuncRoot: l.runcRoot, RuncBinary: l.runcBinary,
		Version: shimStateVersion, ContainerID: containerID, Bundle: bundle,
		Lower: s.Lower, Merged: s.Merged, StateRoot: stateRoot,
		RootfsID: rootfsID, BaseID: selection.BaseID, OriginalSpec: original, SpecMode: mode,
		Status: "claiming", Lease: lease, CreatedAt: time.Now().UTC(),
	}
	if err := saveJournal(s.StatePath, state, 0o600); err != nil {
		return nil, fmt.Errorf("save lease intent: %w", err)
	}
	if err := SaveJSON(filepath.Join(bundle, bundleMarker), taskMarker{ContainerID: containerID}, 0o600); err != nil {
		return nil, err
	}
	if err := SaveJSON(filepath.Join(stateRoot, "active.json"), lease, 0o600); err != nil {
		return nil, fmt.Errorf("publish journaled lease: %w", err)
	}
	s.runtimeState = state
	state.Status = "preparing"
	if err := saveJournal(s.StatePath, state, 0o600); err != nil {
		return fail(fmt.Errorf("save preparation journal: %w", err))
	}
	s.original = original
	s.runtimeState = state
	if err := WriteFileAtomic(specPath, modified, mode); err != nil {
		return fail(fmt.Errorf("write modified OCI spec: %w", err))
	}
	s.lowerMount = true
	if err := mountLower(s.Lower); err != nil {
		return fail(fmt.Errorf("mount containerd lower rootfs: %w", err))
	}
	if err := mountOverlay(s.Lower, filepath.Join(stateRoot, "upper"), filepath.Join(stateRoot, "work"), s.Merged); err != nil {
		return fail(err)
	}
	s.overlayMount = true
	state.Status = "active"
	if err := saveJournal(s.StatePath, state, 0o600); err != nil {
		return fail(fmt.Errorf("save shim runtime state: %w", err))
	}
	return s, nil
}

// Abort rolls back a failed CreateTask transaction, including the OCI spec.
func (l *ShimLifecycle) AbortContext(ctx context.Context, s *ShimSession, unmountLower func(string) error) error {
	if s == nil {
		return nil
	}
	lock, err := lockFile(ctx, filepath.Join(s.StateRoot, "lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	if l.checkTaskAbsent != nil {
		if err := l.checkTaskAbsent(s.runtimeState); err != nil {
			return err
		}
	}
	return l.cleanup(s, unmountLower)
}

// Delete cleans mounts after the delegated runc TaskService deleted a task.
func (l *ShimLifecycle) Delete(containerID string, unmountLower func(string) error) error {
	return l.deleteState(containerID, unmountLower, nil)
}

func (l *ShimLifecycle) deleteState(containerID string, unmountLower func(string) error, expected *ShimLease) error {
	state, err := l.RecoveryState(containerID)
	if err != nil || state == nil {
		return err
	}
	statePath, err := l.statePath(containerID)
	if err != nil {
		return err
	}
	return l.cleanupJournal(statePath, *state, unmountLower, expected)
}

func (l *ShimLifecycle) RecoveryState(containerID string) (*RuntimeState, error) {
	var state RuntimeState
	statePath, err := l.statePath(containerID)
	if err != nil {
		return nil, err
	}
	if err := loadJSONNoFollow(statePath, &state); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if state.Version != shimStateVersion {
		return nil, fmt.Errorf("unsupported runtime state version %d", state.Version)
	}
	if state.ContainerID != containerID || (l.bundle != "" && state.Bundle != l.bundle) || (state.Status != "active" && state.Status != "preparing" && state.Status != "claiming" && state.Status != "releasing" && state.Status != "released") {
		return nil, fmt.Errorf("invalid runtime state for container %q", containerID)
	}
	return &state, nil
}

func (l *ShimLifecycle) cleanupJournal(statePath string, state RuntimeState, unmountLower func(string) error, expected *ShimLease) error {
	if expected != nil && *expected != state.Lease {
		return errors.New("recovery journal owner changed")
	}
	if state.Status == "released" {
		return finishReleased(statePath, state)
	}
	containerID := state.ContainerID
	s := &ShimSession{ContainerID: containerID, Bundle: state.Bundle, Lower: state.Lower, Merged: state.Merged, StateRoot: state.StateRoot, StatePath: statePath, RootfsID: state.RootfsID, Lease: state.Lease, lowerMount: state.Lower != "", overlayMount: state.Merged != ""}
	if err := l.validateCleanupSession(s); err != nil {
		return err
	}
	lock, err := lockFile(l.lockContext(), filepath.Join(s.StateRoot, "lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	var current RuntimeState
	if err := loadJSONNoFollow(statePath, &current); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if current.Lease != state.Lease || current.Status != state.Status || (expected != nil && *expected != current.Lease) {
		return errors.New("runtime state changed during cleanup")
	}
	if state.Status == "claiming" || state.Status == "releasing" {
		path := filepath.Join(s.StateRoot, "active.json")
		var active ShimLease
		err := loadJSONNoFollow(path, &active)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && active == state.Lease {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
		return recordReleased(statePath, state)
	}
	if l.checkTaskAbsent != nil {
		if err := l.checkTaskAbsent(state); err != nil {
			return err
		}
	} else if expected != nil {
		return errors.New("task recovery requires a runtime existence check")
	}
	if state.Status == "preparing" && len(state.OriginalSpec) == 0 {
		return errors.New("preparation journal lacks original OCI spec")
	}
	s.original, s.specMode = state.OriginalSpec, state.SpecMode
	s.runtimeState = state
	var active ShimLease
	err = loadJSONNoFollow(filepath.Join(s.StateRoot, "active.json"), &active)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && active != state.Lease && active.BootID != state.Lease.BootID {
		machine, err := readRequiredTrimmed("/etc/machine-id")
		if err != nil {
			return err
		}
		boot, err := readRequiredTrimmed("/proc/sys/kernel/random/boot_id")
		if err != nil {
			return err
		}
		if l.checkTaskAbsent == nil || active.Version != shimStateVersion || active.MachineID != machine || state.Lease.MachineID != machine || state.Lease.BootID == "" || active.BootID != boot || active.ContainerID == "" {
			return errors.New("cannot verify reboot lease takeover")
		}
		for _, path := range []string{s.Merged, s.Lower} {
			mounted, err := hasMount(path, true)
			if err != nil {
				return err
			}
			if mounted {
				return fmt.Errorf("mount remains under previous task path %q", path)
			}
		}
		if err := restoreSessionSpec(s); err != nil {
			return err
		}
		return recordReleased(statePath, state)
	}
	return l.cleanup(s, unmountLower)
}

func (l *ShimLifecycle) cleanup(s *ShimSession, unmountLower func(string) error) error {
	if err := l.validateCleanupSession(s); err != nil {
		return err
	}
	if err := checkShimLease(s.StateRoot, s.ContainerID, s.Lease); err != nil {
		return err
	}
	if s.overlayMount {
		if err := unmountOverlay(s.Merged); err != nil {
			return err
		}
	}
	if s.lowerMount && unmountLower != nil {
		if err := unmountLower(s.Lower); err != nil {
			return err
		}
	}
	if err := restoreSessionSpec(s); err != nil {
		return err
	}
	if s.runtimeState.ContainerID != "" {
		state := s.runtimeState
		state.Status = "releasing"
		state.OriginalSpec = nil
		if err := SaveJSON(s.StatePath, state, 0o600); err != nil {
			return err
		}
	}
	if err := releaseShimLease(s.StateRoot, s.ContainerID, s.Lease); err != nil {
		return fmt.Errorf("release persistent rootfs lease: %w", err)
	}
	if s.runtimeState.ContainerID != "" {
		return recordReleased(s.StatePath, s.runtimeState)
	}
	if err := os.Remove(s.StatePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove runtime state: %w", err)
	}
	return nil
}

func restoreSessionSpec(s *ShimSession) error {
	if len(s.original) == 0 {
		return nil
	}
	if _, err := os.Stat(s.Bundle); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := WriteFileAtomic(filepath.Join(s.Bundle, "config.json"), s.original, s.specMode); err != nil {
		return fmt.Errorf("restore original OCI spec: %w", err)
	}
	return nil
}

func recordReleased(statePath string, state RuntimeState) error {
	dir, err := os.Open(state.StateRoot)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return err
	}
	state.Status = "released"
	state.OriginalSpec = nil
	if err := SaveJSON(statePath, state, 0o600); err != nil {
		return err
	}
	return finishReleased(statePath, state)
}

func finishReleased(statePath string, state RuntimeState) error {
	if _, err := os.Stat(state.Bundle); err == nil {
		if err := SaveJSON(filepath.Join(state.Bundle, bundleMarker), taskMarker{ContainerID: state.ContainerID, Released: true}, 0o600); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Remove(statePath)
}

func (l *ShimLifecycle) validateCleanupSession(s *ShimSession) error {
	if s == nil || !filepath.IsAbs(s.Bundle) || !filepath.IsAbs(s.Merged) || !filepath.IsAbs(s.Lower) {
		return errors.New("refuse cleanup with incomplete runtime state")
	}
	if filepath.Clean(s.Merged) != filepath.Join(filepath.Clean(s.Bundle), "rootfs") {
		return fmt.Errorf("refuse unsafe merged path %q", s.Merged)
	}
	if filepath.Clean(s.Lower) != filepath.Join(filepath.Clean(s.Bundle), ".persistent-rootfs-lower") {
		return fmt.Errorf("refuse unsafe lower path %q", s.Lower)
	}
	if !filepath.IsAbs(s.StateRoot) || !pathWithinAnyRoot(s.StateRoot, l.config.AllowedVolumePrefixes) {
		return fmt.Errorf("refuse unsafe state root %q", s.StateRoot)
	}
	if err := ValidateRootfsID(s.RootfsID); err != nil {
		return fmt.Errorf("refuse invalid rootfs ID: %w", err)
	}
	expectedSuffix := filepath.Join(rootfsDataDirectory, filepath.FromSlash(s.RootfsID))
	if filepath.Base(s.StateRoot) == defaultLayerDirectory {
		expectedSuffix = filepath.Join(rootfsDataDirectory, defaultLayerDirectory)
		var metadata RootfsMetadata
		if err := loadJSONNoFollow(filepath.Join(s.StateRoot, "metadata.json"), &metadata); err != nil {
			return err
		}
		if metadata.RootfsID != s.RootfsID || metadata.Version != shimStateVersion {
			return errors.New("default layer metadata does not match runtime state")
		}
	}
	if !strings.HasSuffix(filepath.Clean(s.StateRoot), string(filepath.Separator)+expectedSuffix) {
		return fmt.Errorf("refuse state root %q: does not match rootfs ID %q", s.StateRoot, s.RootfsID)
	}
	return nil
}

func validateShimMetadata(stateRoot, rootfsID, baseID string) error {
	path := filepath.Join(stateRoot, "metadata.json")
	var metadata RootfsMetadata
	err := loadJSONNoFollow(path, &metadata)
	if errors.Is(err, os.ErrNotExist) {
		return SaveJSON(path, RootfsMetadata{Version: shimStateVersion, RootfsID: rootfsID, BaseID: baseID, CreatedAt: time.Now().UTC()}, 0o600)
	}
	if err != nil {
		return err
	}
	if metadata.Version != shimStateVersion {
		return fmt.Errorf("unsupported rootfs metadata version %d", metadata.Version)
	}
	if metadata.RootfsID != rootfsID {
		return fmt.Errorf("persistent rootfs identity mismatch: stored rootfs=%q, requested rootfs=%q", metadata.RootfsID, rootfsID)
	}
	return nil
}

func planShimLease(stateRoot, containerID string) (ShimLease, error) {
	path := filepath.Join(stateRoot, "active.json")
	machineID, err := readRequiredTrimmed("/etc/machine-id")
	if err != nil {
		return ShimLease{}, err
	}
	bootID, err := readRequiredTrimmed("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ShimLease{}, err
	}
	startTime, err := processStartTime(os.Getpid())
	if err != nil {
		return ShimLease{}, fmt.Errorf("read shim process identity: %w", err)
	}
	var active ShimLease
	if err := loadJSONNoFollow(path, &active); err == nil {
		if active.Version != shimStateVersion {
			return ShimLease{}, fmt.Errorf("unsupported shim lease version %d", active.Version)
		}
		if active.MachineID != machineID || active.BootID == "" || active.BootID == bootID {
			return ShimLease{}, fmt.Errorf("persistent rootfs lease for container %q on machine %q requires owner cleanup or verified storage fencing", active.ContainerID, active.MachineID)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return ShimLease{}, err
	}
	lease := ShimLease{Version: shimStateVersion, ContainerID: containerID, MachineID: machineID, BootID: bootID, PID: os.Getpid(), ProcessStartTime: startTime, CreatedAt: time.Now().UTC()}
	return lease, nil
}

func releaseShimLease(stateRoot, containerID string, expected ShimLease) error {
	if err := checkShimLease(stateRoot, containerID, expected); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(stateRoot, "active.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func checkShimLease(stateRoot, containerID string, expected ShimLease) error {
	path := filepath.Join(stateRoot, "active.json")
	var active ShimLease
	if err := loadJSONNoFollow(path, &active); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if active.Version != shimStateVersion {
		return fmt.Errorf("unsupported shim lease version %d", active.Version)
	}
	if active.ContainerID != containerID {
		return fmt.Errorf("persistent rootfs lease belongs to container %q, not %q", active.ContainerID, containerID)
	}
	if active.MachineID != expected.MachineID || active.BootID != expected.BootID || active.PID != expected.PID || active.ProcessStartTime != expected.ProcessStartTime {
		return errors.New("persistent rootfs lease owner changed before cleanup")
	}
	return nil
}

func readRequiredTrimmed(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %q: %w", path, err)
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", fmt.Errorf("read %q: value is empty", path)
	}
	return value, nil
}

func processStartTime(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid pid %d", pid)
	}
	raw, err := os.ReadFile(filepath.Join("/proc", fmt.Sprint(pid), "stat"))
	if err != nil {
		return "", err
	}
	end := strings.LastIndex(string(raw), ") ")
	if end < 0 {
		return "", errors.New("invalid proc stat format")
	}
	fields := strings.Fields(string(raw)[end+2:])
	if len(fields) <= 19 {
		return "", errors.New("proc stat is missing process start time")
	}
	return fields[19], nil
}

func processMatches(pid int, expectedStartTime string) bool {
	actualStartTime, err := processStartTime(pid)
	if err != nil {
		return false
	}
	return expectedStartTime == "" || actualStartTime == expectedStartTime
}
