package main

import (
	"testing"

	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
)

func TestTaskNotFound(t *testing.T) {
	if !taskNotFound(errgrpc.ToGRPCf(errdefs.ErrNotFound, "container not created")) || !taskNotFound(errdefs.ErrNotFound) {
		t.Fatal("missed task absence")
	}
	if taskNotFound(errgrpc.ToGRPC(errdefs.ErrUnavailable)) || taskNotFound(nil) {
		t.Fatal("misclassified task status")
	}
}
