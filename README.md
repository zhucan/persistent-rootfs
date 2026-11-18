# Persistent RootFS for Kubernetes

This repository implements persistent writable container root filesystems by
combining three existing control points without patching Kubernetes or
containerd:

- Kubernetes CSI publishes a normal PVC to the Pod.
- An admission webhook injects a dedicated RuntimeClass and hides the PVC at a
  reserved mount path.
- A containerd Runtime v2 shim uses the containerd snapshot as OverlayFS
  `lowerdir` and the CSI volume as `upperdir`/`workdir`.

Writes to ordinary paths such as `/opt`, `/usr/local`, or `/home` survive
container and Pod recreation. Kubernetes-managed mounts such as Secrets,
ConfigMaps, `/proc`, and other PVC mounts retain their normal semantics.

## Status

This is an engineering preview targeting containerd 1.6/1.7 (`make shim-1x`)
and 2.1.x (`make shim`) on Linux. It is
suitable for controlled test nodes and single-replica stateful workloads. Read
[the architecture](docs/architecture.md) and [the usage guide](docs/usage.md)
before deploying it.

## Build

```bash
make test
make shim
make webhook
make webhook-image IMAGE=registry.example/persistent-rootfs-webhook:v0.1.0
```

Outputs:

```text
build/containerd-shim-persistent-v2
build/persistent-rootfs-webhook
```

## Repository layout

```text
cmd/containerd-shim-persistent-v2  containerd Runtime v2 shim
cmd/persistent-rootfs-webhook      Pod admission webhook
internal/persistentrootfs          OCI, mount, lease, and admission logic
deploy                             node and Kubernetes deployment assets
docs                               architecture and operations guides
```
