# PVE Per-Storage Lockfile Behaviour

This document captures how Proxmox VE serialises storage operations behind a per-storage lockfile, what failure mode it produces under bursty concurrent CPI calls, and the retry strategy this CPI uses to absorb it.

## What PVE does

PVE protects every mutation against a single storage pool with an exclusive lockfile at:

```
/var/lock/pve-manager/pve-storage-<storage-name>
```

The lock is acquired by `pvesm` (the storage manager) and by the QEMU helpers (`qm`, `qemu-img`) whenever they need to allocate, free, resize, snapshot, or import a volume on that storage. Any other caller racing for the same lock waits up to the storage timeout (default ~30 s) and then fails the task.

There is one lockfile per storage. Operations against different storages do not contend with each other.

## Operations that take the lock

Empirically observed (PVE 9.x; same applies back to 7.x):

- `pvesm alloc` / `pvesm free` — volume create and delete.

- `qm importdisk` / `qm create … --scsiN <storage>:0,import-from=…` — stemcell import.

- `qm resize` — grows or shrinks a backing volume via `qemu-img resize`.

- `qm snapshot` / `qm delsnapshot` — snapshot create and delete on zfs, lvm, lvmthin, btrfs.

- `qm destroy --destroy-unreferenced-disks` — VM deletion that frees attached volumes.

- `pvesm upload` (multipart POST under the API, but the resulting storage task takes the lock too) — ISO and stemcell uploads.

The following do **not** take the lock:

- `qm set` editing config keys like `scsiN: <vol>,size=…` (pure config-file PUT, no storage call). This is why `attach_disk` and `detach_disk` do not contend.

- `nodes/{node}/qemu/{vmid}/config` reads.

## How the failure surfaces

A loser on the lock returns a task failure like:

```
task failed: can't lock file '/var/lock/pve-manager/pve-storage-data' - got timeout
```

The task is reported as failed (not running, not retried by PVE). The CPI sees this as either a synchronous error from the SDK call or a non-zero exit status when awaiting the task UPID.

The substring match `"can't lock file" && "got timeout"` is the canonical detector; `pve.IsStorageLockTimeout` implements it case-insensitively.

## When the CPI hits it

A BOSH director driving a Cloud Foundry deploy can launch a dozen or more concurrent `create_vm` calls within a second of each other. Each one runs as a separate OS process (one `bosh-pve-cpi` invocation per VM), so in-process serialisation does nothing. The director's worker pool is the only knob, and it defaults wide.

Observed contention points, in deploy-time order:

1. **Stemcell upload** (`create_stemcell` → `Storage().Upload` → task). One-shot per stemcell; a single deploy rarely triggers more than one, but a bosh-bootloader-style rebuild with multiple stemcells will.

2. **VM create + import** (`create_vm` → `QEMU().Create` with `import-from`). One per VM. The qcow2 stemcell is copied into the target VM storage under the lock; latecomers time out.

3. **Root-disk resize** (`create_vm` → `ResizeDisk(virtio0)`). One per VM that requests `disk` > stemcell base. Hits the same lock as the import that just finished, against the same storage.

4. **Persistent-disk create** (`create_disk` → `Storage().CreateVolume`). One per job instance with a persistent disk.

5. **ConfigDrive upload** (`agent/configdrive` → `Storage().Upload` → task). One per VM. Uploads to the ISO storage, which may be the same or different from the VM storage; if the same, this stacks on top of the import + resize burst.

6. **VM/disk delete** (`delete_vm` with `destroy-unreferenced-disks=true`, `delete_disk`). On scale-down or redeploy.

## The retry strategy

Per-storage lock timeouts are transient: the holder finishes in seconds-to-minutes and the lock becomes available. Cross-process distributed coordination is the larger fix; retry with backoff is the pragmatic one.

This CPI implements both pieces in `internal/pve/retry.go`:

- `pve.IsStorageLockTimeout(err)` — substring predicate on the SDK error message.

- `pve.StorageLockBackoff(attempt)` — exponential `2 s × 1.5^attempt` with ±30 % jitter, hard-capped at 30 s.

- `pve.RetryOnStorageLock(ctx, logger, label, maxAttempts, op)` — invokes `op` up to `maxAttempts` times (default 10), retrying only on `IsStorageLockTimeout`. Other errors propagate immediately. Context cancellation short-circuits the sleep.

At default settings, a worst-case all-retries run waits roughly `2 + 3 + 4.5 + 6.75 + 10.1 + 15.2 + 22.8 + 30 + 30 = 124 s` before giving up — well inside BOSH's task timeout but long enough for any reasonable lock holder to finish.

### Where the helper is wired

| Surface | Operation wrapped |
|---------|-------------------|
| `create_vm` | initial `Create+import` (via `AllocateWithRetry` + `WithBackoffFunc`); `ResizeDisk(virtio0)` + await |
| `create_disk` | `CreateVolume` inside the `AllocateDiskWithRetry` callback |
| `delete_disk` | `DeleteVolume` |
| `resize_disk` | `ResizeDisk` + await |
| `delete_vm` | `DeleteQemu` (with `DestroyUnreferencedDisks=true`) |
| `create_stemcell` | `Storage().Upload` + await (file handle reopened per attempt) |
| `delete_stemcell` | `DeleteVolumeIfExists` |
| `snapshot_disk` | `Snapshot` + await |
| `delete_snapshot` | `DeleteSnapshot` |
| `agent/configdrive` | `Upload` + await (file reopened per attempt), `DeleteVolume` |

`attach_disk` and `detach_disk` are not wrapped — they only mutate VM config and never touch the storage lock.

### Streaming upload note

For multipart uploads (`Storage().Upload`), the request body is a single-use `io.Reader`. A retry must reopen the source from its on-disk path so PVE sees a fresh stream each attempt. Both `uploadStemcellImage` and `agent/configdrive.uploadISO` do this — the `os.Open` lives inside the retry callback.

## Operator-side knobs

The CPI's retry absorbs short bursts. If your deploys consistently exhaust the retry budget, the contention is structural and you have three options:

1. **Throttle the director.** `director.workers` and per-instance-group `max_in_flight` cap how many `create_vm` calls run concurrently. Cutting this to half the number of vCPUs on the PVE node usually flattens the burst enough.

2. **Split storages.** Putting the stemcell content on one PVE storage and the VM root disks on another removes the contention between step 1 (import) and step 3 (resize) because they grab different lockfiles. The CPI already supports this via `pve_stemcell_storage` vs `pve_vm_storage` in `vars.yml`; pointing them at different backends is the lever.

3. **Tune retry attempts.** `VMIDAllocAttempts` in the CPI config also bounds the storage-lock retry count (default 10). Raising it lets you ride out longer lock holds at the cost of slower failure when something is genuinely stuck.

## Diagnosing

In the director's CPI log (`/var/vcap/sys/log/cpi/cpi.log` on the director VM):

```
pve: storage lock timeout, retrying op=create_disk attempt=1 max_attempts=10 backoff_ms=1837 error="..."
```

Nonzero retry log lines on a deploy that ultimately succeeds: working as intended. Watch the `attempt` value — if it routinely climbs above 5, the lock holder is taking long enough that you should look at option 1 or 2 above.

`attempt=10` followed by the operation failing means retries were exhausted. Look at the PVE node's `journalctl -u pvedaemon -u pveproxy` from the same window to see what was holding the lock.

## References

- `internal/pve/retry.go` — helper implementation and backoff curve.

- `internal/pve/error_map.go` — `IsStorageLockTimeout` predicate.

- `internal/cpi/handlers/create_vm.go` — `createVMRetryBackoff` (per-error backoff routing) and `AllocateWithRetry` wiring.

- PVE source (for the curious): `/usr/share/perl5/PVE/Storage/Plugin.pm` `lock_storage()` — the canonical lock acquisition site.
