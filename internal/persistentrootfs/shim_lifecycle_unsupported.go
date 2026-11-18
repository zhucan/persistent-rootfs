//go:build !linux

package persistentrootfs

import "errors"

type ShimLifecycle struct{}
type ShimSession struct{}

func NewShimLifecycle(Config) (*ShimLifecycle, error) {
	return nil, errors.New("persistent rootfs shim requires Linux")
}
