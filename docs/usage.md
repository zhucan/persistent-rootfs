# Usage

## Prerequisites

- Kubernetes with containerd 1.6/1.7 or 2.1.x and runc on Linux nodes.
- Kubernetes 1.29 with admission webhooks and webhook match conditions enabled,
  and a CSI driver supporting `ReadWriteOncePod`. The installer does not require
  `ValidatingAdmissionPolicy` or its beta API.
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
node for containerd 2.1.x. For containerd 1.6/1.7, build the legacy shim with
`make shim-1x` and copy `build/1.x/containerd-shim-persistent-v2` instead.
Install the selected binary on the node, passing its actual path:

```bash
sudo sh deploy/install-node.sh build/containerd-shim-persistent-v2
```

If kubelet does not use `/var/lib/kubelet/pods`, edit
`/etc/persistent-rootfs/config.json` and set the exact kubelet pod-directory
prefix. Never add `/` as an allowed prefix.

Merge `deploy/containerd-1.x.toml` (config version 2) for containerd 1.6/1.7
or `deploy/containerd-2.x.toml` (config version 3) for 2.1.x into
`/etc/containerd/config.toml`. Match `SystemdCgroup` to the node. Do not
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

Build and push a fresh webhook image tag when upgrading from the policy-based
installer: older images do not serve `/validate-pvc`. The installer validates
state PVC creation and updates through this webhook. Do not apply the optional
`deploy/pvc-policy.yaml` on Kubernetes 1.29.

```bash
sh deploy/install-control-plane.sh \
  registry.example/persistent-rootfs-webhook:v0.1.0
```

On first install the script creates a self-signed webhook certificate valid for
36500 days (about 100 years). Set `CERT_VALID_DAYS` before first install to
override the validity period. Re-running the installer reuses the existing TLS
Secret; it does not renew certificates. It checks the manifests with server-side
dry-run before installing them (the namespace must exist for namespaced checks).

## 4. Configure a workload

Every selected Pod needs these annotations:

```yaml
metadata:
  annotations:
    persistent-rootfs.io/enabled: "true"
    persistent-rootfs.io/container-name: app
    persistent-rootfs.io/volume-name: rootfs-state
```

Requirements:

- Use a dedicated state PVC per workload replica. The writable layer defaults
  to the selected container name inside that PVC. Identical names on separate
  PVCs do not share state. Reusing the same PVC and name reuses the same layer;
  `ReadWriteOncePod` prevents concurrent access, not later reuse by another Pod.
- Existing workloads with an explicit `persistent-rootfs.io/rootfs-id` should
  keep it: removing it selects a different directory if it differs from the
  container name. Tags and digests are both supported.
- `rootfs-state` is a PVC volume labeled
  `persistent-rootfs.io/state-volume=true`.
- The PVC access mode is `ReadWriteOncePod`.
- Do not manually add `runtimeClassName` or the hidden volume mount; the
  webhook injects both.

Review the image and storage settings in `deploy/example-statefulset.yaml`, then
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
snapshots, stop or quiesce the workload first. Restore to a new PVC and keep the
same container name (or explicit legacy ID) to test the restored writable layer.

## Image upgrades

Update the workload image and recreate the Pod with the same state PVC and
container name (or legacy ID). Saved files override the new image, including
updated system files, and previously deleted paths remain hidden. Use a new
PVC to start clean. Before upgrading a layer created by an older shim, take a
snapshot and test a restored copy: older mounts used node-specific OverlayFS
defaults and may contain metadata tied to the original lower layer.

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
| `identity mismatch` | The stored rootfs ID does not match the requested ID. |
| `already active` | A live shim owns the same rootfs ID. |
| `invalid argument` from overlay mount | The CSI filesystem does not meet OverlayFS upperdir requirements. |
