# PVE Transient Transport Faults

This document captures the second class of transient PVE-side failure the CPI must absorb under burst load: pvedaemon worker recycling, pveproxy backend stalls, and the auth-ticket EOF that follows. Related: [PVE Storage Locking](pve-storage-locking.md) covers the per-storage lockfile class.

## What PVE does

`pvedaemon` runs a fixed pool of HTTP workers (default 3) behind `pveproxy`. Each worker has a built-in **per-worker request quota** plus a soft memory limit; when either is hit, the worker exits cleanly and the parent respawns a fresh one. Workers cycle silently — operators rarely notice — and **every in-flight TCP connection to the exiting worker is dropped without an HTTP response**.

The shape pveproxy returns to the client depends on where the worker died:

- **Mid-response** → empty body and a TCP RST. SDKs that expect JSON report it as `EOF` or `failed to parse response`.

- **Before accepting the request** → pveproxy retries the backend, fails to connect, and emits its non-standard **HTTP 596** ("backend gone") to the client.

- **During TLS handshake** → SDK reports `connection refused` or `tls: read on closed connection`.

## How the failure surfaces

Three distinct error strings show up in CPI logs:

```
API request failed: HTTP 596 (code: 596)
```

```
auto-login failed: authentication failed: failed to parse login response: EOF
```

```
operation get-config timed out after 30s
```

The first hits any API call. The second is specific to `POST /access/ticket` — the SDK's first call on a new connection that requires a fresh ticket. The third is the SDK's `TimeoutError` wrapper.

The substring detector `pve.IsTransientTransport` covers all three (plus generic `*ConnectionError` and any 5xx).

## When the CPI hits it

A bosh-director driving a Cloud Foundry deploy launches a dozen+ concurrent `create_vm` calls within a second. With 3 pvedaemon workers, the ratio of in-flight requests to workers is ~10:1, so a worker recycle during the burst window is statistically guaranteed — once every few hundred requests in the field.

Observed at Task 343 (cf deploy retry):

```
13:15:44 pve pvedaemon[5599]: worker 1072563 finished
13:15:44 pve pvedaemon[5599]: starting 1 worker(s)
13:15:44 pve pvedaemon[5599]: worker 1106002 started
```

Two in-flight `create_vm` POSTs riding worker 1072563 died at exactly that timestamp — one with HTTP 596 (POST never reached the new worker), one with auth-EOF (login response truncated).

## The retry strategy

The CPI absorbs the recycle window the same way it absorbs the storage-lock window: predicate + exponential backoff + bounded attempts. Worker restart is sub-second, so the backoff curve is **tighter than the storage-lock one**: `1s × 1.5^attempt`, ±30% jitter, capped at 15s, default 8 attempts.

Implementation lives in `internal/pve/retry.go` alongside the storage-lock helpers:

- `pve.IsTransientTransport(err)` — predicate over `sdkerrors.ErrServer`, `*ConnectionError`, `*TimeoutError`, `net.Error.Timeout()`, and the substrings `"failed to parse login response"`, `"auto-login failed"`, `"(code: 596)"`, `"http 596"`.

- `pve.TransientBackoff(attempt)` — the 1s × 1.5^n curve, cap 15s.

- `pve.RetryOnTransient(ctx, logger, label, maxAttempts, op)` — invokes `op`, retries on `IsTransientTransport`, surfaces other errors immediately. Default `maxAttempts` is `DefaultTransientMaxAttempts` (8).

- `pve.RetryOnTransientOrLock(ctx, logger, label, maxAttempts, op)` — combines both predicates with per-attempt backoff selection: a lock-timeout attempt uses `StorageLockBackoff`, a transient-transport attempt uses `TransientBackoff`. Default `maxAttempts` is `DefaultStorageLockMaxAttempts` (10) so the longer-running condition dictates the budget.

### Where the helpers are wired

| Surface | Wrapper | Reason |
|---------|---------|--------|
| `vmid.listClusterVMIDs` | `RetryOnTransient` | `GET /cluster/resources` runs before every VMID allocation; an auth-EOF here aborts `create_vm` before any work. |
| `vmid.listStorageVMIDs` | `RetryOnTransient` | Same as above for the disk-VMID storage scan. |
| `create_vm.AllocateWithRetry` callback | `IsTransientTransport` added to `isRetryable` predicate; `cleanupVM` runs on both create-error and await-error transient branches. | A 596 mid-POST may leave the VMID partially registered; sweep before next attempt. |
| `create_vm` resize-virtio0 | `RetryOnTransientOrLock` | Resize can hit storage lock **or** worker cycle. |
| `create_disk` CreateVolume | `RetryOnTransientOrLock` | Same dual exposure. |
| `delete_vm`, `delete_disk`, `delete_stemcell`, `delete_snapshot` | `RetryOnTransientOrLock` | Storage mutations on the cleanup path. |
| `resize_disk`, `snapshot_disk` | `RetryOnTransientOrLock` | Storage mutations. |
| `create_stemcell` upload, `agent/configdrive` upload/delete | `RetryOnTransientOrLock` | Streaming uploads need file reopen inside the callback; pattern already in place. |
| `attach_disk`, `detach_disk` | `RetryOnTransient` (no lock) | Pure config PUT, no storage lock, but worker cycle still applies. |

## Diagnosing

In `/var/vcap/sys/log/cpi/cpi.log` on the director:

```
pve: transient transport fault, retrying op=create_vm attempt=1 max_attempts=8 backoff_ms=1185 error="..."
```

Or, from the combined helper:

```
pve: retrying after retryable fault op=create_disk reason=transient_transport attempt=2 max_attempts=10 backoff_ms=2031 error="..."
pve: retrying after retryable fault op=create_disk reason=storage_lock     attempt=3 max_attempts=10 backoff_ms=4218 error="..."
```

The `reason` field tells you which predicate fired. Watch the mix during a deploy:

- All `storage_lock` → storages saturated; split or throttle (see storage-locking doc).

- All `transient_transport` → pvedaemon worker pool too small for the burst rate.

- Both interleaved → both bottlenecks active; the storage-lock side dominates wall time so address it first.

On the PVE node:

```
journalctl -u pvedaemon --since '<deploy-start>' --until '<deploy-end>' | grep -E 'worker .* finished|starting 1 worker'
```

Worker-cycle events should be visible at exactly the failed timestamps in the CPI log. One cycle per failure is the smoking gun.

## Operator-side knobs

The CPI's retry absorbs typical recycle windows. If you see retries climbing past attempt 4 routinely, the worker pool is undersized for your deploy concurrency:

1. **Raise pvedaemon worker count.** Edit `/etc/default/pvedaemon`:

   ```
   MAX_WORKERS=8
   ```

   Then `systemctl restart pvedaemon`. Each worker is ~80 MB resident; eight workers is fine on any PVE host with 8+ GB RAM.

2. **Raise pveproxy worker count similarly.** `/etc/default/pveproxy` `MAX_WORKERS=8`, `systemctl restart pveproxy.service spiceproxy.service`.

3. **Throttle the director.** Lower `director.workers` or per-instance-group `max_in_flight`. Cheapest if you cannot edit the PVE node, and also helps with storage-lock contention.

See [PVE Host Tuning](pve-host-tuning.md) for full sizing guidance, verification, and the related storage-side knobs.

## References

- `internal/pve/retry.go` — helper implementations.

- `internal/pve/error_map.go` — `IsTransientTransport`, `IsStorageLockTimeout` predicates.

- `internal/cpi/handlers/create_vm.go` — `isRetryable` composing all three predicates (`IsVMIDConflict`, `IsStorageLockTimeout`, `IsTransientTransport`) and per-attempt rollback wiring.

- PVE source (for the curious): `/usr/share/perl5/PVE/Daemon.pm` `finish_workers()` — the worker-recycle path.
