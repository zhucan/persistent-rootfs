# Architecture

## Goal

Persist writes made to a selected container's writable root filesystem while
keeping image distribution in containerd and storage lifecycle in Kubernetes
CSI. No Kubernetes, containerd, runc, or CSI driver source changes are needed.

## Data path

```text
containerd image snapshot ----> private lower mount
                                      |
CSI ReadWriteOncePod PVC ------> upper/work
                                      |
                                      v
                              OverlayFS at bundle/rootfs
                                      |
                                      v
                                     runc
```

The Pod contains a normal PVC volume. The webhook mounts it into the selected
container at `/run/.persistent-rootfs`. Before runc starts, the shim reads the
host source of that mount from the OCI spec, validates it against the kubelet
path allowlist, removes the mount from the container spec, and uses the source
as host-only OverlayFS storage.

The application never receives the PVC mount or `CAP_SYS_ADMIN`.

## CreateTask transaction

Create, init-task Delete and shim Stop hold the same node-local file lock
through the delegated runc call and persistent-layer processing. The lock is
selected by hashing the containerd namespace and task ID into 256 files.
Each task locks a hashed byte range using a Linux open-file-description lock.
A shared flock on the same file coordinates with the exclusive flock in older shims.
Lock contention observes request cancellation
and a 30-second deadline. This deadline applies to lock acquisition.

For a selected application container, the shim:

1. Reads the OCI annotations and selects the target container and its PVC-local
   layer (the default on new volumes, or the matching legacy directory).
2. Removes the hidden CSI mount from the OCI spec.
3. Locks the layer and writes a `claiming` journal with the planned lease and
   original OCI spec. It publishes the lease, then writes `preparing` before
   replacing the spec. Recovery of `claiming` only releases a matching lease.
   Mount cleanup starts with `preparing`. The `active` state records completed
   mount preparation and recovery checks runc for task existence.
4. Mounts containerd's `CreateTaskRequest.Rootfs` at
   `.persistent-rootfs-lower` in the OCI bundle.
5. Creates `upper` and `work` on the CSI volume.
6. Mounts OverlayFS at the normal `bundle/rootfs` path.
7. Clears `CreateTaskRequest.Rootfs` and delegates to containerd's official
   runc v2 TaskService. This prevents a second rootfs mount over the overlay.
8. Rolls back the mounts, lease, state, and OCI spec if delegated creation
   fails.

For the sandbox and non-selected sidecars, requests pass directly to the
official runc TaskService.

## DeleteTask transaction

The shim delegates deletion to the official TaskService first. Once runc has
deleted the init task, it unmounts the overlay and private lower mount, releases
the lease, and removes node-local recovery state. Persistent `upper` data is
never deleted by task cleanup.

## CSI integration

The project does not implement a CSI driver. Any CSI filesystem volume can be
used if its filesystem supports OverlayFS upperdir semantics. Kubernetes and
the CSI driver remain responsible for provisioning, attach/detach,
NodeStage/NodePublish, expansion, VolumeSnapshot, and restore.

State PVCs are labeled `persistent-rootfs.io/state-volume=true`. The supplied
PVC validating webhook requires exactly `ReadWriteOncePod`, which provides
cluster-level single-Pod fencing. The shim lease adds same-node/same-boot
protection and stale-owner detection.

## Persistent layout

```text
PVC root
└── .persistent-rootfs-runtime/
    ├── @layout.lock
    └── @default/  (legacy volumes use <rootfs-id>/)
        ├── active.json
        ├── lock
        ├── metadata.json
        ├── upper/
        └── work/
```

`metadata.json` identifies the writable layer by rootfs ID. Its `baseID`
records the original image for diagnostics; it does not pin subsequent mounts.

## Security boundaries

- Only host paths below `allowedVolumePrefixes` are accepted.
- Managed path components and mount targets reject symlinks and traversal.
- Rootfs IDs use a restricted character set.
- The application cannot access `upper` or `work` directly.
- The state PVC, not the rootfs ID, is the storage isolation boundary.
- RuntimeClass scheduling confines the shim to explicitly labeled nodes.

The shim is a privileged node component equivalent in trust to containerd and
runc. Its binary and configuration must be root-owned.

## Current limitations

- The containerd 1.6/1.7 build returns a plain socket address, saves it in
  the bundle for reconnection and serves `containerd.task.v2.Task`. The 2.1.x
  build uses JSON bootstrap and `containerd.task.v3.Task`. Both use the same
  runc task service and persistent layer lifecycle.
- Linux, runc, and filesystem PVCs only.
- One selected container and one writer per rootfs ID.
- No rootless/user-namespace support.
- No automatic base-image migration or CRIU process-state persistence.
- CSI snapshots are crash-consistent unless the application is quiesced.
- ext4 and XFS require distribution-specific OverlayFS/SELinux validation.
- NFS and other network filesystems are not assumed to support upperdir.
