# Usage

## Prerequisites

- Kubernetes with containerd 2.1.x and runc on Linux nodes.
- Kubernetes support for `ReadWriteOncePod` and
  `ValidatingAdmissionPolicy`.
- A CSI StorageClass that provisions an OverlayFS-compatible filesystem.
- Go 1.23 or newer for builds.
- `openssl` and `kubectl` for the provided control-plane installer.

Start with dedicated test nodes. Do not deploy this runtime cluster-wide until
node reboot, drain, CSI detach/attach, snapshot restore, and storage exhaustion
have been tested with the target CSI driver.

## 1. Build

```bash
make test
make shim
make webhook
make webhook-image IMAGE=registry.example/persistent-rootfs-webhook:v0.1.0
docker push registry.example/persistent-rootfs-webhook:v0.1.0
```

## 2. Install the shim on each runtime node

Copy `build/containerd-shim-persistent-v2` and the `deploy` directory to the
node, then run:

```bash
sudo deploy/install-node.sh build/containerd-shim-persistent-v2
```

If kubelet does not use `/var/lib/kubelet/pods`, edit
`/etc/persistent-rootfs/config.json` and set the exact kubelet pod-directory
prefix. Never add `/` as an allowed prefix.

Merge `deploy/containerd-2.x.toml` into `/etc/containerd/config.toml`. Do not
replace the complete containerd configuration with the snippet. Validate and
restart containerd:

```bash
containerd config dump >/tmp/containerd-effective.toml
sudo systemctl restart containerd
sudo systemctl status containerd
```

Enable only prepared nodes:

```bash
kubectl label node NODE_NAME persistent-rootfs.io/enabled=true
```

## 3. Install admission and policy components

```bash
deploy/install-control-plane.sh \
  registry.example/persistent-rootfs-webhook:v0.1.0
```

The script creates a self-signed webhook certificate valid for 36500 days
(about 100 years). Set `CERT_VALID_DAYS` to override the validity period.

## 4. Configure a workload

Every selected Pod needs these annotations:

```yaml
metadata:
  annotations:
    persistent-rootfs.io/enabled: "true"
    persistent-rootfs.io/container-name: app
    persistent-rootfs.io/rootfs-id: stable-workload-identity
    persistent-rootfs.io/volume-name: rootfs-state
```

Requirements:

- `rootfs-id` remains stable across Pod recreation.
- The selected image is an immutable `name@sha256:<64 hex>` reference.
- `rootfs-state` is a PVC volume labeled
  `persistent-rootfs.io/state-volume=true`.
- The PVC access mode is `ReadWriteOncePod`.
- Do not manually add `runtimeClassName` or the hidden volume mount; the
  webhook injects both.

Edit the digest and storage settings in `deploy/example-statefulset.yaml`, then
apply it:

```bash
kubectl apply -f deploy/example-statefulset.yaml
kubectl -n persistent-rootfs-demo rollout status statefulset/shell
```

## 5. Verify persistence

```bash
kubectl -n persistent-rootfs-demo exec shell-0 -- \
  sh -c 'echo persisted >/opt/rootfs-test'
kubectl -n persistent-rootfs-demo delete pod shell-0
kubectl -n persistent-rootfs-demo wait \
  --for=condition=Ready pod/shell-0 --timeout=180s
kubectl -n persistent-rootfs-demo exec shell-0 -- cat /opt/rootfs-test
```

The final command must print `persisted`.

## Snapshot and restore

Use the CSI driver's normal `VolumeSnapshot` API. For application-consistent
snapshots, stop or quiesce the workload first. Restore to a new PVC, keep the
same image digest, and use a new rootfs ID while validating the restored copy.

## Image upgrades

An existing upper layer can hide files from a newer lower image, including
security fixes. The runtime therefore rejects a changed base identity.

Upgrade procedure:

1. Stop the workload.
2. Create and verify a CSI snapshot.
3. Restore/clone to a new PVC or allocate a new rootfs ID.
4. Run an application-specific offline migration if needed.
5. Change the image digest and start the migrated copy.

## Troubleshooting

```bash
journalctl -u containerd -n 300 --no-pager
ls -la /run/persistent-rootfs
kubectl get events -A --sort-by=.lastTimestamp
kubectl get pvc -A -l persistent-rootfs.io/state-volume=true
```

Common failures:

| Message | Meaning |
| --- | --- |
| `outside allowed prefixes` | Kubelet root path is not in the node allowlist. |
| `must use an immutable sha256 digest` | The image uses a tag or malformed digest. |
| `identity mismatch` | The PVC/rootfs ID belongs to another base image. |
| `already active` | A live shim owns the same rootfs ID. |
| `invalid argument` from overlay mount | The CSI filesystem does not meet OverlayFS upperdir requirements. |
