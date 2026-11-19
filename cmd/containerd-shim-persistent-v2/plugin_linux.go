//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types/runc/options"
	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/process"
	runcconfig "github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/runc"
	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/task"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/containerd/v2/plugins"
	runc "github.com/containerd/go-runc"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
	"github.com/containerd/typeurl/v2"

	"github.com/persistent-rootfs/persistent-rootfs/internal/persistentrootfs"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.TTRPCPlugin,
		ID:   "task",
		Requires: []plugin.Type{
			plugins.EventPlugin,
			plugins.InternalPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			publisher, err := ic.GetByID(plugins.EventPlugin, "publisher")
			if err != nil {
				return nil, err
			}
			shutdownService, err := ic.GetByID(plugins.InternalPlugin, "shutdown")
			if err != nil {
				return nil, err
			}
			delegate, err := task.NewTaskService(ic.Context, publisher.(shim.Publisher), shutdownService.(shutdown.Service))
			if err != nil {
				return nil, err
			}
			cfg, err := persistentrootfs.LoadConfig("")
			if err != nil {
				return nil, err
			}
			lifecycle, err := persistentrootfs.NewShimLifecycle(cfg)
			if err != nil {
				return nil, err
			}
			return &persistentTaskService{TTRPCTaskService: delegate, lifecycle: lifecycle}, nil
		},
	})
}

type persistentTaskService struct {
	taskAPI.TTRPCTaskService
	lifecycle *persistentrootfs.ShimLifecycle
}

func taskLifecycle(ctx context.Context, base *persistentrootfs.ShimLifecycle, id, bundle string) (*persistentrootfs.ShimLifecycle, error) {
	namespace, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}
	if bundle == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		bundle = filepath.Join(filepath.Dir(cwd), id)
	}
	return base.ForTask(namespace, bundle, func(state persistentrootfs.RuntimeState) error {
		runtime, err := recoveryRuntime(state, namespace)
		if err != nil {
			return err
		}
		containers, err := runtime.List(ctx)
		if err != nil {
			return fmt.Errorf("check runtime before cleanup: %w", err)
		}
		for _, container := range containers {
			if container.ID == id {
				return fmt.Errorf("runtime still owns container %q", id)
			}
		}
		return nil
	})
}

func recoveryRuntime(state persistentrootfs.RuntimeState, namespace string) (*runc.Runc, error) {
	root, binary := state.RuncRoot, state.RuncBinary
	if binary == "" {
		opts, err := runcconfig.ReadOptions(state.Bundle)
		if err != nil {
			return nil, err
		}
		if opts == nil {
			return nil, fmt.Errorf("cannot verify runtime for legacy journal without bundle options")
		}
		root, binary = opts.Root, opts.BinaryName
	}
	return process.NewRunc(root, state.Bundle, namespace, binary, false), nil
}

func (s *persistentTaskService) RegisterTTRPC(server *ttrpc.Server) error {
	if legacyBuild == "true" {
		registerLegacyTask(server, s)
		return nil
	}
	taskAPI.RegisterTTRPCTaskService(server, s)
	return nil
}

func (s *persistentTaskService) Create(ctx context.Context, request *taskAPI.CreateTaskRequest) (*taskAPI.CreateTaskResponse, error) {
	mountLower := func(target string) error {
		mounts := make([]mount.Mount, 0, len(request.Rootfs))
		for _, item := range request.Rootfs {
			mounts = append(mounts, mount.Mount{Type: item.Type, Source: item.Source, Target: item.Target, Options: item.Options})
		}
		if len(mounts) == 0 {
			return fmt.Errorf("containerd CreateTask request has no rootfs mounts")
		}
		return mount.All(mounts, target)
	}
	unmountLower := func(target string) error {
		return mount.UnmountRecursive(target, 0)
	}
	lifecycle, err := taskLifecycle(ctx, s.lifecycle, request.ID, request.Bundle)
	if err != nil {
		return nil, err
	}
	root, binary := process.RuncRoot, "runc"
	if request.Options.GetValue() != nil {
		decoded, err := typeurl.UnmarshalAny(request.Options)
		if err != nil {
			return nil, err
		}
		opts, ok := decoded.(*options.Options)
		if !ok {
			return nil, fmt.Errorf("unexpected runc options %T", decoded)
		}
		if opts.Root != "" {
			root = opts.Root
		}
		if opts.BinaryName != "" {
			binary = opts.BinaryName
		}
	}
	lifecycle.SetRuntime(root, binary)
	guard, err := lifecycle.LockTask(ctx, request.ID)
	if err != nil {
		return nil, err
	}
	defer guard.Close()
	session, err := lifecycle.Prepare(request.ID, request.Bundle, mountLower, unmountLower)
	if err != nil {
		return nil, err
	}
	if session == nil {
		return s.TTRPCTaskService.Create(ctx, request)
	}

	originalRootfs := request.Rootfs
	request.Rootfs = nil
	response, err := s.TTRPCTaskService.Create(ctx, request)
	if err != nil {
		request.Rootfs = originalRootfs
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		rollback, cleanupErr := taskLifecycle(rollbackCtx, s.lifecycle, request.ID, request.Bundle)
		if cleanupErr == nil {
			cleanupErr = rollback.AbortContext(rollbackCtx, session, unmountLower)
		}
		if cleanupErr != nil {
			return nil, errors.Join(err, fmt.Errorf("rollback persistent rootfs: %w", cleanupErr))
		}
		return nil, err
	}
	return response, nil
}

func (s *persistentTaskService) Delete(ctx context.Context, request *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	if request.ExecID != "" {
		return s.TTRPCTaskService.Delete(ctx, request)
	}
	lifecycle, scopeErr := taskLifecycle(ctx, s.lifecycle, request.ID, "")
	if scopeErr != nil {
		return nil, scopeErr
	}
	guard, err := lifecycle.LockTask(ctx, request.ID)
	if err != nil {
		return nil, err
	}
	defer guard.Close()
	state, err := lifecycle.RecoveryState(request.ID)
	if err != nil {
		return nil, err
	}
	if state == nil {
		_, err := lifecycle.StopWithoutJournal(request.ID)
		if errors.Is(err, persistentrootfs.ErrBundleAbsent) {
			if _, stateErr := s.TTRPCTaskService.State(ctx, &taskAPI.StateRequest{ID: request.ID}); stateErr != nil {
				return nil, stateErr
			}
			return nil, fmt.Errorf("task %q still exists: %w", request.ID, err)
		}
		if err != nil {
			return nil, err
		}
		return s.TTRPCTaskService.Delete(ctx, request)
	}
	response, err := s.TTRPCTaskService.Delete(ctx, request)
	if err != nil && !taskNotFound(err) {
		return response, err
	}
	if cleanupErr := lifecycle.Delete(request.ID, func(target string) error {
		return mount.UnmountRecursive(target, 0)
	}); cleanupErr != nil {
		return response, fmt.Errorf("task deleted but persistent rootfs cleanup failed: %w", cleanupErr)
	}
	return response, err
}
