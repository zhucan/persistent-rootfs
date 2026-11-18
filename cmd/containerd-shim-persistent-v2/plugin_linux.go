//go:build linux

package main

import (
	"context"
	"fmt"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/task"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"

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
	session, err := s.lifecycle.Prepare(request.ID, request.Bundle, mountLower, unmountLower)
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
		if cleanupErr := s.lifecycle.Abort(session, unmountLower); cleanupErr != nil {
			log.G(ctx).WithError(cleanupErr).Error("rollback persistent rootfs after CreateTask failure")
		}
		return nil, err
	}
	return response, nil
}

func (s *persistentTaskService) Delete(ctx context.Context, request *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	response, err := s.TTRPCTaskService.Delete(ctx, request)
	if request.ExecID != "" || (err != nil && !errdefs.IsNotFound(err)) {
		return response, err
	}
	if cleanupErr := s.lifecycle.Delete(request.ID, func(target string) error {
		return mount.UnmountRecursive(target, 0)
	}); cleanupErr != nil {
		return response, fmt.Errorf("task deleted but persistent rootfs cleanup failed: %w", cleanupErr)
	}
	return response, err
}
