package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

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
	params, err := manager.Start(namespaces.WithNamespace(ctx, *namespace), *id, shim.StartOpts{
		Address: *address, TTRPCAddress: os.Getenv("TTRPC_ADDRESS"), Debug: *debug,
	})
	if err != nil {
		return err
	}
	if err := persistentrootfs.WriteFileAtomic("address", []byte(params.Address), 0o600); err != nil {
		return err
	}
	_, err = io.WriteString(output, params.Address)
	return err
}
