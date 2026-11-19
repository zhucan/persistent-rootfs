//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/manager"
	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/process"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/shim"
	runc "github.com/containerd/go-runc"
	"github.com/containerd/log"

	"github.com/persistent-rootfs/persistent-rootfs/internal/persistentrootfs"
)

func main() {
	cfg, err := persistentrootfs.LoadConfig("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "load persistent rootfs config: %v\n", err)
		os.Exit(1)
	}
	lifecycle, err := persistentrootfs.NewShimLifecycle(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "initialize persistent rootfs lifecycle: %v\n", err)
		os.Exit(1)
	}
	runtimeManager := &persistentManager{
		Manager:   manager.NewShimManager("io.containerd.persistent.v2"),
		lifecycle: lifecycle,
	}
	if legacyBuild == "true" && os.Args[len(os.Args)-1] == "start" {
		if err := startLegacy(context.Background(), runtimeManager, os.Args[1:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	shim.Run(context.Background(), runtimeManager)
}

type persistentManager struct {
	shim.Manager
	lifecycle *persistentrootfs.ShimLifecycle
}

func (m *persistentManager) Stop(ctx context.Context, id string) (shim.StopStatus, error) {
	lifecycle, err := taskLifecycle(ctx, m.lifecycle, id, "")
	if err != nil {
		return shim.StopStatus{}, err
	}
	guard, err := lifecycle.LockTask(ctx, id)
	if err != nil {
		return shim.StopStatus{}, err
	}
	defer guard.Close()
	state, err := lifecycle.RecoveryState(id)
	if err != nil {
		return shim.StopStatus{}, err
	}
	if state == nil {
		released, err := lifecycle.StopWithoutJournal(id)
		if err != nil {
			return shim.StopStatus{}, err
		}
		if released {
			return shim.StopStatus{ExitedAt: time.Now(), ExitStatus: 128 + int(syscall.SIGKILL)}, nil
		}
		return m.Manager.Stop(ctx, id)
	}
	if state.Status == "active" {
		namespace, err := namespaces.NamespaceRequired(ctx)
		if err != nil {
			return shim.StopStatus{}, err
		}
		runtime, err := recoveryRuntime(*state, namespace)
		if err != nil {
			return shim.StopStatus{}, err
		}
		containers, err := runtime.List(ctx)
		if err != nil {
			return shim.StopStatus{}, err
		}
		for _, container := range containers {
			if container.ID != id {
				continue
			}
			if filepath.Clean(container.Bundle) != state.Bundle {
				return shim.StopStatus{}, fmt.Errorf("runtime task %q belongs to bundle %q", id, container.Bundle)
			}
			if err := runtime.Delete(ctx, id, &runc.DeleteOpts{Force: true}); err != nil {
				return shim.StopStatus{}, err
			}
			break
		}
	}
	pid, _ := runc.ReadPidFile(filepath.Join(state.Bundle, process.InitPidFile))
	status := shim.StopStatus{Pid: pid, ExitedAt: time.Now(), ExitStatus: 128 + int(syscall.SIGKILL)}
	if cleanupErr := lifecycle.Delete(id, func(target string) error {
		return mount.UnmountRecursive(target, 0)
	}); cleanupErr != nil {
		log.G(ctx).WithError(cleanupErr).WithField("id", id).Error("cleanup persistent rootfs after shim stop")
		if err == nil {
			err = cleanupErr
		}
	}
	return status, err
}
