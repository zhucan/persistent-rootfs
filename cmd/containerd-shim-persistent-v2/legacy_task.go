package main

import (
	"context"

	task "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/ttrpc"
)

func registerLegacyTask(server *ttrpc.Server, service task.TTRPCTaskService) {
	server.RegisterService("containerd.task.v2.Task", &ttrpc.ServiceDesc{Methods: map[string]ttrpc.Method{
		"State": legacyMethod(service.State), "Create": legacyMethod(service.Create),
		"Start": legacyMethod(service.Start), "Delete": legacyMethod(service.Delete),
		"Pids": legacyMethod(service.Pids), "Pause": legacyMethod(service.Pause),
		"Resume": legacyMethod(service.Resume), "Checkpoint": legacyMethod(service.Checkpoint),
		"Kill": legacyMethod(service.Kill), "Exec": legacyMethod(service.Exec),
		"ResizePty": legacyMethod(service.ResizePty), "CloseIO": legacyMethod(service.CloseIO),
		"Update": legacyMethod(service.Update), "Wait": legacyMethod(service.Wait),
		"Stats": legacyMethod(service.Stats), "Connect": legacyMethod(service.Connect),
		"Shutdown": legacyMethod(service.Shutdown),
	}})
}

func legacyMethod[Request, Response any](call func(context.Context, *Request) (*Response, error)) ttrpc.Method {
	return func(ctx context.Context, unmarshal func(any) error) (any, error) {
		var request Request
		if err := unmarshal(&request); err != nil {
			return nil, err
		}
		return call(ctx, &request)
	}
}
