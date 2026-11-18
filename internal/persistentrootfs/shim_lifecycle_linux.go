//go:build linux

package persistentrootfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const shimStateVersion = 3

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
	lowerMount   bool
	overlayMount bool
}

// ShimLifecycle prepares persistent rootfs mounts inside the Runtime v2 shim.
type ShimLifecycle struct {
	config Config
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
	bundle, err := filepath.Abs(bundle)
	if err != nil {
		return nil, err
	}
	specPath := filepath.Join(bundle, "config.json")
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
	stateRoot, err := ensureDirectoryTree(dataRoot, selection.RootfsID, 0o700)
	if err != nil {
		return nil, fmt.Errorf("create rootfs state directory: %w", err)
	}
	lock, err := lockFile(filepath.Join(stateRoot, "lock"))
	if err != nil {
		return nil, fmt.Errorf("lock persistent rootfs: %w", err)
	}
	defer lock.Close()

	if err := validateShimMetadata(stateRoot, selection.RootfsID, selection.BaseID); err != nil {
		return nil, err
	}
	lease, err := claimShimLease(stateRoot, containerID)
	if err != nil {
		return nil, err
	}

	s := &ShimSession{
		ContainerID: containerID,
		Bundle:      bundle,
		Lower:       filepath.Join(bundle, ".persistent-rootfs-lower"),
		Merged:      filepath.Join(bundle, "rootfs"),
		StateRoot:   stateRoot,
		StatePath:   StateFile(l.config.StateDir, containerID),
		RootfsID:    selection.RootfsID,
		Lease:       lease,
		original:    original,
		specMode:    mode,
	}
	specModified := false
	fail := func(cause error) (*ShimSession, error) {
		failures := []error{cause}
		if cleanupErr := l.cleanup(s, unmountLower); cleanupErr != nil {
			failures = append(failures, fmt.Errorf("rollback mounts: %w", cleanupErr))
		}
		if specModified {
			if restoreErr := WriteFileAtomic(specPath, original, mode); restoreErr != nil {
				failures = append(failures, fmt.Errorf("restore OCI spec: %w", restoreErr))
			}
		}
		return nil, errors.Join(failures...)
	}
	for _, path := range []string{s.Lower, s.Merged} {
		if err := prepareMergedDirectory(path); err != nil {
			return fail(err)
		}
	}
	for _, path := range []string{filepath.Join(stateRoot, "upper"), filepath.Join(stateRoot, "work")} {
		if err := ensurePlainDirectory(path, 0o700); err != nil {
			return fail(fmt.Errorf("prepare overlay directory %q: %w", path, err))
		}
	}
	modified, err := spec.Marshal()
	if err != nil {
		return fail(err)
	}
	if err := WriteFileAtomic(specPath, modified, mode); err != nil {
		return fail(fmt.Errorf("write modified OCI spec: %w", err))
	}
	specModified = true
	s.lowerMount = true
	if err := mountLower(s.Lower); err != nil {
		return fail(fmt.Errorf("mount containerd lower rootfs: %w", err))
	}
	if err := mountOverlay(s.Lower, filepath.Join(stateRoot, "upper"), filepath.Join(stateRoot, "work"), s.Merged); err != nil {
		return fail(err)
	}
	s.overlayMount = true
	if err := os.MkdirAll(l.config.StateDir, 0o700); err != nil {
		return fail(err)
	}
	state := RuntimeState{
		Version: shimStateVersion, ContainerID: containerID, Bundle: bundle,
		Lower: s.Lower, Merged: s.Merged, StateRoot: stateRoot,
		RootfsID: selection.RootfsID, BaseID: selection.BaseID,
		Status: "active", Lease: lease, CreatedAt: time.Now().UTC(),
	}
	if err := SaveJSON(s.StatePath, state, 0o600); err != nil {
		return fail(fmt.Errorf("save shim runtime state: %w", err))
	}
	return s, nil
}

// Abort rolls back a failed CreateTask transaction, including the OCI spec.
func (l *ShimLifecycle) Abort(s *ShimSession, unmountLower func(string) error) error {
	if s == nil {
		return nil
	}
	err := l.cleanup(s, unmountLower)
	if restoreErr := WriteFileAtomic(filepath.Join(s.Bundle, "config.json"), s.original, s.specMode); err == nil {
		err = restoreErr
	}
	return err
}

// Delete cleans mounts after the delegated runc TaskService deleted a task.
func (l *ShimLifecycle) Delete(containerID string, unmountLower func(string) error) error {
	var state RuntimeState
	statePath := StateFile(l.config.StateDir, containerID)
	if err := loadJSONNoFollow(statePath, &state); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if state.Version != shimStateVersion {
		return fmt.Errorf("unsupported runtime state version %d", state.Version)
	}
	if state.ContainerID != containerID || state.Status != "active" {
		return fmt.Errorf("runtime state does not belong to active container %q", containerID)
	}
	s := &ShimSession{ContainerID: containerID, Bundle: state.Bundle, Lower: state.Lower, Merged: state.Merged, StateRoot: state.StateRoot, StatePath: statePath, RootfsID: state.RootfsID, Lease: state.Lease, lowerMount: state.Lower != "", overlayMount: state.Merged != ""}
	return l.cleanup(s, unmountLower)
}

func (l *ShimLifecycle) cleanup(s *ShimSession, unmountLower func(string) error) error {
	if err := l.validateCleanupSession(s); err != nil {
		return err
	}
	var failures []string
	if s.overlayMount {
		if err := unmountOverlay(s.Merged); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if s.lowerMount && unmountLower != nil {
		if err := unmountLower(s.Lower); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	if err := releaseShimLease(s.StateRoot, s.ContainerID, s.Lease); err != nil {
		return fmt.Errorf("release persistent rootfs lease: %w", err)
	}
	if err := os.Remove(s.StatePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove runtime state: %w", err)
	}
	return nil
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
	if metadata.RootfsID != rootfsID || metadata.BaseID != baseID {
		return fmt.Errorf("persistent rootfs identity mismatch: stored rootfs=%q base=%q, requested rootfs=%q base=%q", metadata.RootfsID, metadata.BaseID, rootfsID, baseID)
	}
	return nil
}

func claimShimLease(stateRoot, containerID string) (ShimLease, error) {
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
		if active.MachineID == machineID && active.BootID == bootID && processMatches(active.PID, active.ProcessStartTime) {
			return ShimLease{}, fmt.Errorf("persistent rootfs is already active in shim pid %d for container %q", active.PID, active.ContainerID)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return ShimLease{}, err
	}
	lease := ShimLease{Version: shimStateVersion, ContainerID: containerID, MachineID: machineID, BootID: bootID, PID: os.Getpid(), ProcessStartTime: startTime, CreatedAt: time.Now().UTC()}
	if err := SaveJSON(path, lease, 0o600); err != nil {
		return ShimLease{}, err
	}
	return lease, nil
}

func releaseShimLease(stateRoot, containerID string, expected ShimLease) error {
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
	return os.Remove(path)
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
