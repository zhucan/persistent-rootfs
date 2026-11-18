package main

import (
	"context"
	"net"
	"testing"
	"time"

	legacy "github.com/containerd/containerd/api/runtime/task/v2"
	task "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/ttrpc"
)

type testTask struct{ task.TTRPCTaskService }

func (testTask) Create(_ context.Context, req *task.CreateTaskRequest) (*task.CreateTaskResponse, error) {
	if req.ID != "sandbox" || req.Bundle != "/bundle" {
		return &task.CreateTaskResponse{}, nil
	}
	return &task.CreateTaskResponse{Pid: 42}, nil
}

func TestLegacyClientCreate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server, err := ttrpc.NewServer()
	if err != nil {
		t.Fatal(err)
	}
	registerLegacyTask(server, testTask{})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go server.Serve(ctx, listener)
	defer server.Close()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client := ttrpc.NewClient(conn)
	defer client.Close()
	response, err := legacy.NewTaskClient(client).Create(ctx, &legacy.CreateTaskRequest{ID: "sandbox", Bundle: "/bundle"})
	if err != nil || response.GetPid() != 42 {
		t.Fatalf("response=%v err=%v", response, err)
	}
}
