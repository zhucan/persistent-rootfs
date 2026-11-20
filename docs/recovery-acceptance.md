# Persistent rootfs recovery acceptance

Run `go test -count=1 ./...` on Linux and install the build under test on isolated nodes using the target containerd version and CSI StorageClass. Record the commit, kernel, containerd patch version, runc version and CSI driver version for each run. Repeat with the 1.x shim build for each deployed containerd 1.x version.

Deploy the example StatefulSet with a disposable PVC. Choose a fresh token for each scenario. Run `bash deploy/check-rootfs-recovery.sh CONTEXT NAMESPACE seed TOKEN` and save the output. For each Pod replacement, record the Pod UID immediately before deletion, wait for the replacement to become Ready, then run `bash deploy/check-rootfs-recovery.sh CONTEXT NAMESPACE verify-replacement TOKEN PREVIOUS_POD_UID`. Keep the output and use the newly observed UID before the next deletion. For faults where the Pod can retain its UID, use `verify TOKEN` instead; that mode proves data continuity, not replacement. In both cases collect the separate fault and cleanup evidence below.

| Scenario | Action in the isolated environment | Evidence required |
| --- | --- | --- |
| Normal replacement | Delete `shell-0` normally and allow its controller to recreate it. | The new Pod reads the seeded file. Repeat the replacement and check again. |
| Shim exit | Terminate the shim for the test sandbox on the isolated node. Observe runtime cleanup and recreate the Pod. | Data verification passes. The former task ID has no journal, private mounts or active lease after cleanup. |
| Node reboot | Reboot the node hosting the test task, recover the workload and repeat the replacement. | Record the boot IDs and task IDs before and after. Data verification passes after each recovery. |
| PVC takeover | Start a second managed Pod using the same PVC and same rootfs identity while the first Pod is running. Then terminate the first normally and allow the second to acquire the volume. | While the first writes, the second cannot start its application. After takeover only the second can write. Verify the seeded file from its app container and inspect the new lease before and after retrying old-task cleanup. Its owner must remain the new task. |

Use node mountinfo, runtime task inventory and the configured journal directory to verify cleanup. Inspect the volume lease from the test node. Save Pod events, shim logs and lease owner IDs with the run. A pending second Pod demonstrates exclusion at the storage or scheduling layer. The Linux takeover regression separately exercises the old-journal cleanup branch. Mark a scenario pending if its fault or its postconditions were not observed.
