package main

import (
	"github.com/containerd/errdefs"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func taskNotFound(err error) bool {
	return errdefs.IsNotFound(err) || status.Code(err) == codes.NotFound
}
