package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/shim"

	"github.com/persistent-rootfs/persistent-rootfs/internal/persistentrootfs"
)

var legacyBuild = "false"

func startLegacy(ctx context.Context, manager shim.Manager, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("start", flag.ContinueOnError)
	namespace := flags.String("namespace", "", "containerd namespace")
	id := flags.String("id", "", "task ID")
	address := flags.String("address", "", "containerd address")
	debug := flags.Bool("debug", false, "enable shim debug logs")
	flags.String("publish-binary", "", "legacy event publisher")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 || flags.Arg(0) != "start" || *namespace == "" || *id == "" || *address == "" {
		return fmt.Errorf("shim start requires namespace, id and address")
	}
	ctx = namespaces.WithNamespace(ctx, *namespace)
	spec, _, _, err := persistentrootfs.ReadSpec("config.json")
	if err != nil {
		return err
	}
	annotations, err := spec.Annotations()
	if err != nil {
		return err
	}
	group := *id
	for _, label := range []string{"io.containerd.runc.v2.group", "io.kubernetes.cri.sandbox-id"} {
		if value, ok := annotations[label]; ok {
			group = value
			break
		}
	}
	expected, err := shim.SocketAddress(ctx, *address, group, false)
	if err != nil {
		return err
	}
	if info, err := os.Lstat("address"); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("bundle address is not a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	stage, err := os.MkdirTemp(".", ".shim-start-")
	if err != nil {
		return err
	}
	defer os.Remove(stage)
	stagedAddress := filepath.Join(stage, "address")
	defer os.Remove(stagedAddress)
	if err := persistentrootfs.WriteFileAtomic(stagedAddress, []byte(expected), 0o600); err != nil {
		return err
	}
	params, err := manager.Start(ctx, *id, shim.StartOpts{
		Address: *address, TTRPCAddress: os.Getenv("TTRPC_ADDRESS"), Debug: *debug,
	})
	if err != nil {
		return err
	}
	if params.Address != expected {
		return fmt.Errorf("shim manager returned unexpected address %q, expected %q", params.Address, expected)
	}
	if err := os.Rename(stagedAddress, "address"); err != nil {
		return err
	}
	dir, err := os.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return err
	}
	_, err = io.WriteString(output, params.Address)
	return err
}
