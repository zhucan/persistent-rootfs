//go:build linux

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/manager"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/shim"
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
	status, err := m.Manager.Stop(ctx, id)
	if err != nil {
		return status, err
	}
	if cleanupErr := m.lifecycle.Delete(id, func(target string) error {
		return mount.UnmountRecursive(target, 0)
	}); cleanupErr != nil {
		log.G(ctx).WithError(cleanupErr).WithField("id", id).Error("cleanup persistent rootfs after shim stop")
		if err == nil {
			err = cleanupErr
		}
	}
	return status, err
}
