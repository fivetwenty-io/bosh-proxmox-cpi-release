# Troubleshooting

This is a symptom-first triage runbook. Start from the failure you see, follow the diagnosis steps, apply the fix. For log access commands, ISO verification, orphan cleanup procedures, and preflight checks, see the [Operations Runbook](operations.md).

## Reading CPI errors

The CPI surfaces three error shapes in BOSH task output:

- **`Bosh::Clouds::CloudError`** (`ok_to_retry: false`) — terminal failure; the Director will not retry. Operator action required.

- **`Bosh::Clouds::VMCreationFailed`** (`ok_to_retry: true`): transient `create_vm` failure. The Director retries the create with identical arguments, up to its `max_vm_create_tries` budget (default 5). If those retries also exhaust, the error escalates to the task log as a permanent failure.

- **`Bosh::Clouds::CloudError`** (`ok_to_retry: true`): transient fault from a method the Director has no retry loop for. The CPI already exhausted its internal retry budget before surfacing it; retrying the deploy usually succeeds once the underlying fault clears.

Errors appear in the BOSH task debug log. To view them:

```bash
bosh task <id> --debug
bosh task <id> --debug 2>&1 | grep -E '"method":"|"error":|pve:'
```

For instructions on accessing the CPI log file on the Director VM, see the [Operations Runbook](operations.md).

## Authentication and permission failures

### 401 Unauthorized or token rejected

**Symptom**

A failed call wrapped by the CPI. The text after the `PVE API error:` prefix is the message PVE returned and varies by PVE version:

```text
PVE API error: <PVE 401 authentication message>
```

A transient worker recycle during login instead surfaces as `auto-login failed` or `failed to parse login response`; those are retried automatically and covered in [PVE Transient Transport Faults](pve-transient-transport.md).

**Diagnosis**

The token format must be `PVEAPIToken=<user>!<tokenid>=<secret>`, where `<user>` includes the realm (e.g., `bosh@pve` or `root@pam`). Verify `pve.api_token` in the rendered config on the Director VM:

```bash
sudo cat /var/vcap/jobs/pve_cpi/config/cpi.json | jq '.api_token'
```

Check that privilege separation is disabled on the token:

```bash
curl -sk -H "Authorization: $PVE_TOKEN" \
  https://<pve>:8006/api2/json/access/users/<user>/token/<tokenid> | jq '.data.privsep'
# must return 0
```

**Fix**

Set `privsep=0` on the token in the PVE web UI or via `pveum token modify`. Ensure `pve.user` carries the realm suffix. See [PVE API Permissions](pve-api-permissions.md) and [PVE Settings](pve-settings.md).

### 403 Permission denied on specific operations

**Symptom**

A failed call wrapped by the CPI. The text after the `PVE API error:` prefix is the permission message PVE returned for the denied path:

```text
PVE API error: <PVE 403 permission message>
```

**Diagnosis**

The token lacks the ACL grant required for the operation. Different CPI methods require different privileges: `VM.Allocate` for create/delete, `Datastore.AllocateSpace` for disk operations, `SDN.Allocate` for SDN management.

**Fix**

Grant the missing privilege in the PVE web UI under Datacenter → Permissions. See the per-method privilege table in [PVE API Permissions](pve-api-permissions.md).

## Configuration and startup errors

### Missing required field

**Symptom**

```text
config validation failed: host is required; user is required; vm_storage is required; disk_storage is required; network_bridge is required
```

or

```text
config validation failed: one of password or api_token is required
```

**Diagnosis**

The rendered `cpi.json` on the Director is missing one or more required properties. View it:

```bash
sudo cat /var/vcap/jobs/pve_cpi/config/cpi.json
```

**Fix**

Add the missing values to your deployment manifest under `properties.pve.*` and redeploy. See [Configuration](configuration.md).

### Invalid agent_mode value

**Symptom**

```text
config validation failed: agent_mode must be one of cloudinit|noagent|auto, got "X"
```

**Fix**

Set `pve.agent_mode` to `cloudinit`, `noagent`, or `auto`. The default is `cloudinit`. The `auto` value selects configdrive bootstrap for all stemcells.

### agent_mode: registry or registry.* keys rejected

**Symptom**

```text
config validation failed: agent_mode "registry" is no longer supported (the BOSH registry was deprecated upstream); set agent_mode to "cloudinit"
```

or, for a leftover `registry.*` property (rendered as a `registry_*` config key):

```text
config validation failed: config key "registry_endpoint" is no longer supported (the BOSH registry was removed)
```

**Fix**

The BOSH registry agent mode has been removed in line with the upstream BOSH deprecation. Remove `pve.agent_mode: registry` and all `registry.*` properties from the CPI config. Set `pve.agent_mode: cloudinit` (or omit it — `cloudinit` is the default). See [Configuration](configuration.md#removed-bosh-registry).

### VMID range too low

**Symptom**

```text
config validation failed: vmid_range_start must be ≥100
```

**Fix**

Set `pve.vmid_range_start` to 100 or higher in your manifest.

### Unknown field in config

**Symptom**

```text
config: decode failed: <json error>
```

**Diagnosis**

The CPI uses `DisallowUnknownFields` when parsing `cpi.json`. A typo in a property name causes an immediate decode failure.

**Fix**

Compare the failing `cpi.json` against the property list in [Configuration](configuration.md). Correct the misspelled field name and redeploy.

### Stemcell path outside staging directory

**Symptom**

```text
create_stemcell: open <path>: path escapes staging root
```

or a similar `os.Root` path-escape error referencing the configured `stemcell_staging_dir`.

**Diagnosis**

`pve.stemcell_staging_dir` is set and the BOSH director supplied a stemcell image path that resolves outside the declared root. This is a configuration mismatch: either the staging directory is set too narrowly, or the director is supplying an unexpected path.

**Fix**

Set `pve.stemcell_staging_dir` to a directory that is a parent of all paths the director supplies (typically the BOSH blob store temp directory), or remove the property to revert to unrestricted path access. See [Configuration](configuration.md).

### PVE CA certificate parse failure

**Symptom**

```text
config validation failed: pve.ca_cert: no valid PEM certificates found
```

**Diagnosis**

`pve.ca_cert` is set but the value is not valid PEM-encoded certificate data. The CPI validates the PEM at startup using `crypto/x509`.

**Fix**

Verify the PEM block with `openssl x509 -text -noout -in <cert.pem>`. Ensure the property contains the full PEM block including `-----BEGIN CERTIFICATE-----` and `-----END CERTIFICATE-----` markers. Remove the property to fall back to the system trust pool. See [Configuration](configuration.md).

## VM creation failures

When `create_vm` fails with `create_vm refused:` and a count of audit conflicts or issues, see [An operation fails with an allocation audit refusal](#an-operation-fails-with-an-allocation-audit-refusal).

### Target node not set

**Symptom**

```text
create_vm: target node not set in cloud_properties.target_node or config.node
```

**Fix**

Set `pve.node` in the CPI config (the common case) or set `target_node` in `cloud_properties` for per-VM overrides. See [Configuration](configuration.md).

### VMID range exhausted

**Symptom**

```text
no free VMID in range [100, 8999]: all 8900 IDs exhausted
```

or

```text
AllocateWithRetry: failed to allocate VMID after 10 attempts (last attempted VMID <N>)
```

**Diagnosis**

List existing VMIDs to see how many are in use:

```bash
pvesh get /cluster/resources --type vm | jq '.[].vmid' | sort -n
```

**Fix**

Delete unused VMs to free VMIDs, or widen the range by setting `pve.vmid_range_end` to a higher value (maximum 8999; the 9000–29999 band is reserved for persistent-disk VMIDs).

### Too many persistent disks at create time

**Symptom**

```text
create_vm: too many persistent disks at create time (N); CPI reserves scsi29 (headroom) and scsi30 (cloud-init drive)
```

**Fix**

Reduce the number of persistent disks passed at create time to 28 or fewer. If your workload genuinely requires more, disks can be attached after VM creation via `attach_disk`.

### NIC or bridge configuration failed

**Symptom**

```text
create_vm: configure NICs vmid=N: <error>
```

**Diagnosis**

Verify the bridge exists on the PVE node:

```bash
ip link show <bridge>
pvesh get /nodes/<node>/network | jq '.[] | select(.iface == "<bridge>")'
```

**Fix**

Set `pve.network_bridge` to an existing Linux bridge name (e.g., `vmbr0`). See [Configuration](configuration.md).

### VM will not start

**Symptom**

```text
create_vm: start vmid=N: <error>
```

**Diagnosis**

Check resource limits and quota on the PVE node:

```bash
pvesh get /nodes/<node>/status
qm config <vmid>
```

**Fix**

Free memory or CPU capacity on the node, or adjust `cores` and `memory` in `cloud_properties`. If the VM is orphaned after a failed create, see the [Operations Runbook](operations.md) for manual cleanup.

### Leaked VMID after post-create fault

**Symptom**

The CPI reported an error after creating the VM but before the director recorded the CID. A VMID is occupied on PVE with no corresponding BOSH record.

**Diagnosis**

Look for VMs in the VMID range that carry no BOSH tags:

```bash
pvesh get /cluster/resources --type vm | jq '.[] | select(.tags == null) | {vmid:.vmid, name:.name}'
```

**Fix**

See the [Operations Runbook](operations.md) for orphan cleanup procedures. The CPI runs `cleanupVM` (stop + delete) automatically before retrying, but cleanup can itself fail and leave a leaked VMID requiring `qm destroy`.

### VM is locked

**Symptom**

`delete_vm` (or a `create_vm` rollback) fails repeatedly against the same VMID with a message naming a lock type and a recovery command:

```text
PVE VM 106 on node "pve01" is locked (clone); recover with `qm unlock 106` on node "pve01", then retry: ...
```

Because this error is retriable, the BOSH Director keeps re-driving the same failing call — `bosh task <id> --debug` shows the identical error repeating across retries without ever succeeding.

**Diagnosis**

A worker process (`pvedaemon` or a `qm` child) was killed, or the PVE node rebooted, while a `clone`, `create`, `backup`, `migrate`, `snapshot`, or `rollback` task was in flight against the VM. PVE writes the operation's name into the guest config's `lock:` attribute before starting the task and only clears it on that task's normal completion; a task that dies mid-flight leaves the lock behind permanently. Every subsequent stop/destroy call against that VMID is rejected by PVE's own guest-config lock check until the lock is cleared — PVE's HTTP API has no unlock endpoint, so nothing the CPI does over the API can clear it unilaterally.

Confirm the lock directly:

```bash
qm config <vmid> | grep ^lock:
```

**Fix**

The CPI attempts an automatic recovery first: if it is authenticated as the `root@pam` superuser via **password** — `pve.user: root`, `pve.realm: pam` (or `pve.user: root@pam`) and a password — it retries the failing stop/destroy call once with PVE's `skiplock` parameter, which bypasses the guest-config lock check. PVE honors `skiplock` **only** for that exact identity; it rejects the parameter for every other identity, including an API token owned by `root@pam` (`pve.api_token: root@pam!<token-id>=<secret>`) and a least-privilege token issued to any other user, regardless of the ACL roles or privileges granted to that user — a token's authenticated identity always carries a `!<token-id>` suffix, which never equals the bare `root@pam` PVE's check requires. When the CPI is *not* authenticated as `root@pam` via password, or the `skiplock` retry itself fails, the error above is the final, actionable outcome and manual recovery is required:

```bash
qm unlock <vmid>
```

Run this on the PVE node that hosts the VM (the node named in the error message). Once unlocked, the BOSH Director's next retry of `delete_vm` (or the original `create_vm`, if the lock was hit during a rollback) succeeds normally — no CPI restart or manifest change is needed.

**`pve.fast_path_delete` and this same `skiplock` dependency:** `fast_path_delete`'s destroy calls also rely on `skiplock=true` to reclaim a locked or still-running VM without a separate unlock/stop step. If `fast_path_delete: true` is set under an identity `skiplock` is not honored for, the CPI logs a startup Warn naming the configured identity (see [Configuration Reference](configuration.md) for `pve.fast_path_delete`); check the CPI process log for that Warn before assuming a manual `qm unlock` is the only path forward.

**Interaction with `debug.keep_failed_vms` and the `bosh-create-failed` tag**

When a `create_vm` rollback (`cleanupVM`) hits this condition and cannot clear the lock (not `root@pam`, or the `skiplock` retry also failed), the VM is left running and orphaned — it was never meant to be preserved, but ended up stuck regardless of the `pve.debug.keep_failed_vms` setting (see [CPI Methods — `create_vm`](cpi_methods.md#create_vm) for that flag's normal, opt-in preserve-for-inspection behavior). The CPI tags it `bosh-create-failed` on a best-effort basis (when the failing rollback has a BOSH deploy identity to tag with) so an operator can find it the same way:

```bash
pvesh get /cluster/resources --type vm | jq '.[] | select(.tags != null and (.tags | contains("bosh-create-failed"))) | {vmid:.vmid, node:.node, tags:.tags}'
```

A VM found this way needs the same `qm unlock <vmid>` fix before it can be destroyed or adopted.

### A VM's metadata lock is held by a process that died

`set_vm_metadata`, `set_disk_metadata`, and stemcell reference counting each take a lock on the VM before they rewrite its tags and description. The lock lives in a PVE resource pool named `bosh-lock-vm-<vmid>`, and the pool's comment holds the claim of whoever has it. When another process holds the lock for the whole 10-second wait, the call fails with a retriable error that quotes that claim.

```text
withVMIDLock: lock "bosh-lock-vm-4242" is held by owner=set_vm_metadata/4242@director-0/1234-9f2c1a7e-7 exp=1790778395, which lapses at 2026-09-30T14:26:35Z: AcquireClusterLock: timed out after 10s waiting for lock "bosh-lock-vm-4242": ...
```

We read the owner token from left to right. `set_vm_metadata/4242` is the operation that took the lock and the VMID it locked, `director-0` is the host the CPI ran on, and `1234` is the CPI's process ID on that host. `9f2c1a7e` is a random number that process drew when it started, and the trailing `7` counts the locks that process has taken. `exp` is the claim's expiry in Unix seconds, and the error repeats it as a UTC time.

A claim clears by itself at its recorded expiry. A VM lock's claim lasts 25 minutes from the moment it was taken, so a lock that a dead process left behind is free again within 25 minutes, and the first call after that takes it over. When we have lengthened a backoff curve or the transient attempt budget under `pve.retry`, the claim lasts longer, and `exp` still says exactly when it lapses.

When we can't wait that long, we can clear the lock at once, but only after we have confirmed that the process is gone. We log in to the host the token names and look for the process ID.

```bash
ps -p 1234 -o pid,cmd
```

If `ps` prints no process, or prints one that isn't the CPI, the holder is gone. We read the claim once more to make sure it is still the same one, and then we delete the pool.

```bash
pvesh get /pools/bosh-lock-vm-4242 --output-format json
pvesh delete /pools --poolid bosh-lock-vm-4242
```

Deleting the pool while the process that holds it is still alive lets two writers into the VM's metadata at the same time, which is exactly what the lock exists to prevent. Each writer reads the tags and rewrites them, and whichever writes last silently drops the other's changes. We never delete the pool on a guess.

A parker's protection lock uses the same pool name. Its claim names a parker operation such as `transfer_out/90000`, and it lasts just under four minutes on the shipped retry settings, longer when `pve.retry` lengthens the curves, so we let it lapse rather than delete it.

### Every create_vm times out reaching the PVE API

**Symptom**

A deploy reaches "Creating missing vms" and every instance fails with the same error, while `create-env` for the Director on the *same* network succeeded:

```
create_vm: allocate+create VM: vmid: list cluster resources: cluster.ListResources:
... Get "https://<pve_host>:8006/api2/json/cluster/resources?type=vm":
dial tcp <pve_host>:8006: connect: connection timed out
```

**Diagnosis**

`create-env`'s CPI runs on the workstation, which can reach the PVE API; the **in-VM CPI** runs on the Director, so this only appears once a deploy drives the Director's CPI. It means the Director cannot reach `https://<pve_host>:8006`. The usual cause is the **host firewall**: with `pve-firewall` enabled, `8006` is allowed only from a management source set, and a Director on an isolated SDN subnet is not in it. A `connect: connection timed out` (no RST) is the signature of a DROP, not a routing black hole — the packet reaches the host and is dropped.

Confirm from inside the Director (jumpbox user, key in `creds.yml:/jumpbox_ssh/private_key`):

```bash
ssh -i <key> -o IdentitiesOnly=yes jumpbox@<internal_ip> \
  'curl -sk -o /dev/null -w "%{http_code}\n" --max-time 10 https://<pve_host>:8006/'
```

A timeout confirms the block; `200` means the API is reachable and the fault is elsewhere. Inspect the live rules on the host: `iptables -S | grep 8006`.

**Fix**

Permit the isolated subnet to reach the API. `BOSH_PVE_ENV=<env> ./scripts/bosh net-up` does this automatically (idempotent rules in `/etc/pve/nodes/<node>/host.fw` for the configured subnet → `8006` + ICMP, then a firewall reload); `net-status` shows them and `net-down` removes them. See [Networks — Host firewall: API access from the isolated subnet](networks.md#isolated-test-network-sdn).

## Stemcell upload and import failures

### Stemcell storage is local-only on a multi-node cluster

**Symptom**

```text
create_stemcell: stemcell storage "X" is local-only but the cluster has N nodes; stemcell_storage must be a shared storage pool (NFS, Ceph, CIFS, etc.) accessible from all cluster nodes
```

**Fix**

Set `pve.stemcell_storage` to a storage pool that is shared across nodes (`shared=1` in `storage.cfg`). Acceptable types: `nfs`, `cifs`, `glusterfs`, `cephfs`, `dir` with shared=1. See [Configuration](configuration.md#stemcell-storage).

### Storage not available on the configured node

**Symptom**

```text
PVE API error: storage 'X' is not available on node 'Y'
```

**Diagnosis**

The storage carries a `nodes` restriction in `storage.cfg` that excludes the node the call was addressed to. For stemcell uploads this should not occur on current releases: when the stemcell storage is node-restricted and `pve.node` is not an owner, we address the storage-scoped calls (upload, server-side download, and content listings) to the restriction set's first owning node and log the retarget at info. Seeing this error means some other storage (for example `pve.vm_storage` during clone) excludes the node the VM landed on.

**Fix**

Either widen the storage's `nodes` restriction in `storage.cfg` to include the node, or pin placement to an owning node (`pve.node`, an AZ `target_node`, or `cloud_properties.node`).

### Block storage rejects qcow2 upload

**Symptom**

A rejection from PVE, surfaced through the CPI behind the `PVE API error:` prefix, naming the offending storage type (`lvmthin` shown here as an example):

```text
PVE API error: can't upload to storage type 'lvmthin'
```

**Diagnosis**

Block-based storage types (`lvm`, `lvmthin`, `zfspool`, `rbd`) cannot accept file uploads.

**Fix**

Set `pve.stemcell_storage` to a file-based storage type. See [Configuration](configuration.md#stemcell-storage).

### Import content type not enabled

**Symptom**

The upload succeeds but the file is not accessible, or PVE returns a 400 error on the import path.

**Diagnosis**

Verify the storage has `import` content enabled:

```bash
grep "^dir:\|^nfs:\|^cifs:" /etc/pve/storage.cfg -A5 | grep content
pvesh get /storage/<stemcell_storage> | jq '.data.content'
```

**Fix**

Enable `import` content on the storage in the PVE web UI under Datacenter → Storage. See [PVE Settings](pve-settings.md#2-enable-import-content-on-stemcell-storage).

### Legacy integer stemcell CID

**Symptom**

```text
ParseStemcellPathCID: CID "5042" missing leading ':' — expected ":light:<storage>:import/<file>" or ":heavy:<storage>:import/<file>"
```

**Diagnosis**

The BOSH state database holds a legacy integer (template-VMID) CID from a previous CPI version. The current CPI uses a path-identity string CID: `:light:<storage>:import/<file>` for an operator-managed qcow2, `:heavy:<storage>:import/<file>` for one the CPI uploaded.

**Fix**

Run `bosh delete-stemcell <os>/<version>` and then `bosh upload-stemcell` to regenerate the CID in the correct format. If the stemcell record is orphaned in the DB, use `bosh cck` to reconcile.

### Missing cloud_properties.name

**Symptom**

```text
create_stemcell: cloud_properties.name is required for direct-qcow stemcell upload
```

**Fix**

Ensure the stemcell manifest's `cloud_properties` block contains a `name` field. Official BOSH stemcells include this; custom stemcells may omit it. Add it to the stemcell metadata before upload.

## Disk operation failures

### Snapshot guard blocks disk attach

**Symptom**

```text
attach_disk: VM <N> (node X) has <k> snapshot(s) [<names>]: attaching a persistent disk while snapshots exist makes the disk invisible in all prior snapshot rollbacks. Delete all snapshots before attaching persistent disks, or set pve.allow_disk_ops_with_snapshots=true in CPI config to bypass this guard.
```

**Diagnosis**

List snapshots on the VM:

```bash
qm listsnapshot <vmid>
```

**Fix**

Delete all snapshots before attaching persistent disks. If you understand the data-loss risk (snapshot rollback will not see the disk), you can bypass the guard by setting `pve.allow_disk_ops_with_snapshots: true` in the manifest and redeploying. See [Snapshot guard on disk operations](cpi_methods.md#snapshot-guard-on-disk-operations).

### Local-backend disk on different node from VM

**Symptom**

```text
attach_disk: local-backend disk <cid> lives on node X but VM <vmid> runs on node Y, and cross-node disk migration is disabled by configuration (pve.disk_migration: "off"). ...
```

or, for a disk created before stable disk identities:

```text
attach_disk: local-backend disk <cid> lives on node X but VM <vmid> runs on node Y, and this is a legacy disk whose CID is its volume name ...
```

**Fix**

By default (`pve.disk_migration: on_attach`) we migrate a stranded stable-ID disk to the VM's node automatically, so the first message only appears when the knob was set to `off`: remove the override (or set `on_attach`) and retry, or move the disk by hand. The second message is a real limitation: the migration renames the volume, and a legacy disk's CID is the volume name, so migrating it would orphan the CID. Recreate the VM pinned to the disk's node, move the volume manually, or use a shared storage backend (`nfs`, `cephfs`, `cifs`, etc.) for `pve.disk_storage`. See [Persistent Disks](persistent-disks.md).

### Resize with unsupported unit

**Symptom**

```text
unsupported size unit in "X" (only GiB supported)
```

**Fix**

Specify disk sizes in GiB in the BOSH deployment manifest. Other units (`M`, `T`) are not accepted.

### Resize shrink attempted

**Symptom**

The resize operation returns a PVE error indicating the new size is smaller than the current size.

**Fix**

PVE does not support disk shrink. You can only increase disk size. See [Persistent Disks — Known Limitations](persistent-disks.md#known-limitations).

### Parker anchor missing (parked disk with no holder)

**Symptom**

```text
attach_disk: disk <cid> was created under the parked strategy and its CID promises a parker anchor, but no VM in the cluster references the volume; the parker holding it was likely deleted out-of-band. Verify the volume is intact on storage, then set pve.parked_anchor_strict: false to proceed against the free-floating volume and retry
```

or, when a parker vanishes between the cluster listing and its config read:

```text
disk holder vmid <N> (node X) sits inside the parker band [90000,90999] but its config vanished mid-scan; a parker VM holding <volid> was likely deleted out-of-band. ...
```

or, when the volume the parker held is provably gone from storage as well:

```text
attach_disk: disk <cid> was created under the parked strategy and its CID promises a parker anchor, but no VM in the cluster references the volume and the volume <volid> is not on storage; the parker VM and the disk it held were both removed out-of-band, so the data is gone. Remove the disk from the Director's records with `bosh -d <deployment> cck` (or drop it from the create-env state file) and redeploy to have a fresh disk created
```

`create_vm` with `disk_cids` reaches the attach path, so it returns the same message under its own method name.

**Diagnosis**

A disk created under the parked strategy is held by a parker VM whenever it is detached; the CPI never deletes parkers, so a promised disk with no holder means someone removed the parker out-of-band (`qm destroy` after clearing protection, a cluster restore, a cleanup script). The volume itself may still be intact on storage. Check:

```bash
pvesm list <disk_storage> | grep <volid>
./scripts/disk-audit --config cpi.json
```

**Fix**

Verify the volume exists and its data is intact. Then set `pve.parked_anchor_strict: false` in the CPI manifest (or cpi-config entry) and retry: the CPI treats the disk as free-floating, and the next detach re-parks it onto a fresh parker, restoring the anchor. Re-enable strict afterwards (remove the property). Labs that intentionally delete parkers can leave the property false. See [Persistent Disk Strategy](persistent-disk-strategy.md).

That recovery applies to a volume that is still intact, because it hands the CPI a disk it can go on using. The third message above is a different case, and no setting recovers it. The volume itself is gone, so the disk has to leave the Director's records before the deployment can move again. Run `bosh -d <deployment> cck` and choose to delete the disk reference, or drop the disk from the create-env state file, and then redeploy so that a fresh disk is created.

`delete_disk` reaches this refusal only when the volume is still on storage, or when the CPI cannot prove that the volume is gone. The CPI proves an absence by reading the storage's content listing and finding no entry for the volid. It cannot settle the question from the error instead, because on `dir`, NFS, and CIFS storage a missing file comes back as an HTTP 500 naming `volume_size_info` rather than as a 404, and that one error also covers a denied read and an export that went away. A `delete_disk` whose volume is provably gone returns success instead, because nothing is left to delete. A token without `Sys.Audit` at `/access` cannot read a listing that PVE left unfiltered, so it cannot prove an absence at all. The refusal then stands, and the CPI writes a warning to its log naming why the proof did not land.

### Absence unproven on a dir storage with no is_mountpoint

**Symptom**

A parker-anchor refusal stands even though the volume is gone, and the CPI log carries a warning whose error reads:

```text
storage <name> is a dir storage with no is_mountpoint and its content listing came back empty, which a dropped mount produces as readily as a genuinely empty storage; set is_mountpoint on that storage so PVE reports it offline instead of listing nothing
```

**Diagnosis**

The CPI proves a volume absent by reading the storage's content listing. On a `dir` or `btrfs` storage that carries no `is_mountpoint` flag, an empty listing proves nothing. PVE still lists the bare mount point when the backing filesystem has gone away, and it even recreates `images/` underneath it, so a dropped mount and a genuinely empty storage look identical. The CPI therefore accepts an empty listing as proof only from a storage PVE refuses to activate when its backing is unreachable. That means NFS, CIFS, the block and Ceph plugins, and any `dir` or `btrfs` storage carrying `is_mountpoint`. On a plain `dir` storage, other volumes in the listing are what prove the tree is really mounted, so the refusal stands only when the listing is empty.

**Fix**

Tell PVE that the storage is a mount point, which makes it fail loudly instead of listing nothing:

```bash
pvesm set <storage> --is_mountpoint yes
```

The flag takes effect on the next call, because the CPI reads it live rather than from its storage cache. A storage whose only volume was the one just deleted still lists empty for an honest reason, and that case reads as unproven until another volume lands there.

The local-backend cluster scan, which walks the nodes looking for a node-pinned volume, classifies the storage the same way. It reads the storage index live the first time it needs a classification, so the flag we set a moment ago is the flag the scan uses, and no cache has to expire first. When that read cannot be made, the scan falls back to the classification the backend was built with, and a fallback that carries no storage type counts as no classification at all, which leaves the absence unproven instead of letting the scan guess.

### Absence unproven because an empty listing was contradicted

**Symptom**

A parker-anchor refusal stands, or an orphan sweep is skipped, and the CPI log carries a warning whose error reads:

```text
storage <name> listed no content but its absence is contradicted by <source>: <detail>; the export may be mounted from the wrong tree, so the volume is not proven gone
```

or, when the check that would have corroborated the empty listing failed instead of answering:

```text
storage <name> listed no content and the <source> check that would corroborate it did not land, so the volume is not proven gone: <error>
```

`<source>` names where the evidence came from, and it is one of `cluster configs`, `allocation journal`, or `storage status`.

**Diagnosis**

An NFS or CIFS export that mounts but serves the wrong tree lists nothing at all. The mount itself succeeded, so PVE activates the storage without complaint, and both types sit on the CPI's allow-list of storages whose listing fails outright when the backing is gone. Left alone, the proof would read that empty listing as evidence that every volume on the storage is gone, and `delete_disk` would report success over a disk whose data is intact. The realistic shape is a filer that lost its dataset and now exports the empty parent directory, or an export that somebody repointed.

So when a listing comes back empty on a storage we would otherwise trust, the CPI asks three sources whether anything contradicts it. It stops at the first source that answers, and the order is the cost order:

- **The cluster's VM configs** come first. A config that still references a volume on the storage contradicts an empty listing, and the check costs no extra call on `delete_disk`, `attach_disk`, and a `create_vm` that carries `disk_cids`, because those calls already read every VM config in the cluster to find the disk's holder. On node-pinned storage the reference has to come from a VM on the same node we listed, because a storage name such as `local-lvm` is a different tree on every node, while on shared storage a reference from any node counts. No other caller has that reading in hand. `has_disk`, `delete_vm`, the orphan sweeps that run after a failed create, and the local-backend cluster scan go straight to the next source.

- **The allocation journal** comes next. It contradicts an empty listing when the CPI recorded allocating some other volume on the storage and never recorded deleting it, and it answers on every caller without touching the cluster. The disk we are asking about never counts against itself. The journal still holds an open allocation record for that disk at the moment we ask, and it holds exactly that record whether the volume is gone or not. The journal reads per node on node-pinned storage for the same reason the configs do, so a record made against `local-lvm` on another node contradicts nothing here, while on shared storage any node's record counts. The journal also misses a disk that was born before the journal existed, and it misses everything once the journal directory has been wiped.

- **The storage's own status on the node** comes last, because it is the only source that spends an API call. It contradicts an empty listing when the storage is not active on that node, and that is the only contradiction it draws. A used figure cannot settle the question, because PVE reports the used bytes of the whole mounted filesystem while a content listing covers only the content types the storage is configured for. One export that carries a backup storage's `dump` directory beside an images storage would therefore report bytes in use against every honest empty listing on the images storage. `has_disk` and the orphan sweeps carry no setting an operator could turn off to get past a refusal like that. When the status does show at least 1 GiB in use with nothing listed, the CPI writes a warning naming the storage, the node, and the byte counts, and the proof goes on to succeed.

A source that fails rather than answers also leaves the absence unproven, which is the second message above. A check that did not land is not a check that agreed.

One shape still gets through. A wrong-tree export that no VM config references, and that the allocation journal holds no other record for, proves absent from its empty listing whatever its used figure says. A storage holding only the disk we are asking about is the everyday version of that shape. The journal cannot rescue that one either, because the only disk it could name there is the one under proof, and that disk never counts against itself. At the PVE API level such a storage looks exactly like one whose last volume was genuinely deleted. The warning about bytes in use with nothing listed is the trail to the cases where PVE did see something. An operator chasing a disk that went missing this way should search the CPI log for that storage name.

**Fix**

Go to the node and look at what the storage is actually serving:

```bash
pvesm status --storage <storage>
pvesm list <storage>
findmnt /mnt/pve/<storage>
ls -la /mnt/pve/<storage>/images
```

Then compare the storage's backing against the filer. The definition in `/etc/pve/storage.cfg` carries a `path` for a dir-style storage, a `server` and an `export` for NFS, and a `server` and a `share` for CIFS. Confirm on the filer that the export our storage names still points at the dataset that holds our disks. An export somebody repointed, or a dataset a filer rebuild left empty, is the fault this refusal exists to catch, and remounting the right tree resolves it with the data intact.

If the storage turns out to be genuinely empty, `pve.parked_anchor_strict: false` lets the calls that refuse on the parker-anchor guard proceed. Those are `delete_disk`, `attach_disk`, and a `create_vm` carrying `disk_cids`, for a disk whose CID promises a parker anchor. With the property set, the guard logs and returns without running the proof at all, and `delete_disk` folds a missing volume into success. That setting turns off the protection this section describes, so set it only after inspecting the export on the node and confirming that the data is not there. Remove the property again once the deployment is moving, so that the anchor guard gets its proof back.

The setting reaches no further than that guard. `has_disk`, the stale-slot check in `delete_vm`, the orphan sweeps that run after a failed create, and the local-backend cluster scan that `delete_disk`, `attach_disk`, and `detach_disk` run to locate a node-pinned volume all keep asking for the proof, whatever the property says. A contradiction raised during that scan comes back as a retriable error, and the Director re-drives the call until it clears. Those paths therefore move again only once the contradiction is gone from the storage itself, which means remounting the right tree, or clearing whatever the named source still points at.

### Parked-disk records fill a parker's description

**Symptom**

```text
transfer in: write intent record on parker vmid <N> (fail-closed: the record is the crash-window identity carrier): ... description: value may only be 8192 characters long
```

The same ceiling had a quieter shape on an ordinary detach, where the parker
still had a free slot and the record was advisory:

```text
WARN parker provenance: provenance not updated  parker_vmid=<N> volid=<volid> error=... parker provenance store is full
```

**Diagnosis**

Releases before this fix never collected parked-disk records, so a long-lived
parker accumulated entries for disks that had left by a route nothing recorded,
until the next write crossed PVE's description cap. `detach_disk` and
`delete_vm` then failed closed rather than park a disk whose identity they could
not record. Failing closed is the right answer to that state; reaching the state
at all is the defect. An ordinary detach failed differently: the parker still had
free slots, so it took the disk and lost only the record, which is how a parker
ends up holding a volume it carries no entry for.

Read the store to confirm what is in it:

```bash
pvesh get /nodes/<node>/qemu/<parker-vmid>/config --output-format json | jq -r .description | grep -o '<!--BOSH:.*-->'
./scripts/disk-audit --config cpi.json
```

**Fix**

We fix this by upgrading. Every provenance write now collects records whose volume nothing on that parker references and whose `parked_at` is over an hour old, and a parker whose live records genuinely fill the store hands the disk to another parker instead of failing. An ordinary park confirms room for the record before it moves the disk, so a full store no longer costs a record silently.

To clear an affected parker before upgrading, we confirm from `disk-audit` which records name volumes the parker no longer holds, and we edit those records out of its description by hand. We make the edit while no Director task touches that parker, because a deploy, a detach, or an orphan cleanup can write the same description. `qm set --description` replaces the whole description, so we pin the edit to the config we read. `qm config` doesn't print the config's digest, so we read the description and the digest together with `pvesh get ... --output-format json`. That output quotes the description as a JSON string, so we unquote it with `jq -r` before we edit it, because a copy of the quoted string would write escaped newlines and quotes into the description. We run both of the following blocks as root on the PVE node that hosts the parker, and `<node>` is that node's name.

```bash
pvesh get /nodes/<node>/qemu/<parker-vmid>/config --output-format json > parker-config.json
jq -r .digest parker-config.json
jq -r .description parker-config.json > parker-description.txt
```

We remove only the stale records from `parker-description.txt`, keep the rest of the JSON exactly as it was, and write it back with the digest that the first `jq` printed.

```bash
qm set <parker-vmid> --digest <digest> --description "$(cat parker-description.txt)"
```

When PVE refuses the write because the digest no longer matches, something changed the description after we read it, so we read it again and start the edit over. See [Persistent Disk Strategy](persistent-disk-strategy.md).

### delete_vm refuses to destroy VM with attached unused disks

**Symptom**

```text
delete_vm: refusing to destroy VM <N> -- persistent volumes still attached as unused slots: [unusedN=<volid>] (call detach_disk first or verify pve.disk_storage configuration; if detach_disk succeeds and the slot stays, do not remove the slot or destroy the VM by hand, because PVE then deletes a volume named for the VM; see "delete_vm refuses to destroy VM with attached unused disks" in docs/troubleshooting.md of bosh-proxmox-cpi-release)
```

**Diagnosis**

The VM's config still has an `unusedN` entry that names a live volume on the disk storage. PVE leaves a volume there whenever a disk slot is deleted and nothing removes the entry afterwards. Most of the time the Director's view of the disk has drifted from PVE, and a detach puts it right.

Since this release, `delete_vm` checks the parkers before it refuses. When a parker's record names the volume as a transfer from this VM that hasn't landed, `delete_vm` finishes the move to that parker through the same resume `detach_disk` runs, and then it destroys the VM. So this refusal now means that no record could finish the move. Either no parker holds a record that names the volume, or a record does and `delete_vm` left it alone, because its parker is on another node, it belongs to a journal-managed disk, more than one record names the volume, the record's disk CID doesn't decode or carries another stable ID, or the disk the record names resolves to some other transfer. In each of the cases with a record, the CPI log carries a `delete_vm:` warning that names the unused entry and the reason. Three other outcomes replace this refusal with a different error. When the resume itself can't prove the volume is the disk, `delete_vm` fails with the resume's own refusal, and that refusal names its own section of this guide. When resolving the disk fails, or a parker can't be read, `delete_vm` returns that error, retriable unless PVE gave a verdict, and nothing is destroyed or moved. When PVE refuses the move because a snapshot of the VM still references the volume, `delete_vm` fails permanently and names the snapshots to delete. Delete them, and the next `delete_vm` finishes the move.

One case needs more care. A persistent disk with a stable ID moves to a parker in three steps, both when `detach_disk` parks it and when `delete_vm` preserves a disk that is still attached. The CPI writes a record of the transfer on the parker, deletes the disk's slot on the VM, and then moves the volume. If the move fails, the volume sits on the VM's unused entry, and an unused entry carries no serial, so the parker's record is the only link from the disk's CID to the volume. PVE keeps that unused entry only for a volume the VM owns, which is one whose name carries the VM's VMID. A volume named for any other VMID loses its last reference when the slot is deleted, so it never reaches the VM's unused entry, and since this release the CPI attaches it to the parker by config edit in place of the move. From 0.5.1 through 0.8.0 the next write to that parker removed the record once it was an hour old. After that, `detach_disk` can report success while the volume stays on the VM. Since this release the record stays for as long as the VM still names the volume, and a retried `detach_disk` finishes the move.

To tell the cases apart, read the VM's config:

```bash
qm config <N>
pve-cid decode <cid>
```

The VM's description carries a `bosh_attached_disks` entry for each disk the CPI attached. When an entry is keyed by a `bpd-` serial that no active slot of the VM carries, the unused entry is that disk's volume. `pve-cid decode` on the entry's CID prints the serial and the name the volume was created with. If the unused entry still has that name, it is the same disk.

**Fix**

Never remove the unused entry, unlink it, or destroy the VM by hand while it is there. When the volume's name carries the VM's VMID, PVE deletes the volume in each of those cases.

On this release, we start by retrying the failed task or rerunning the deploy. The parker keeps its record of the transfer while the VM still names the volume, so the retried `detach_disk` or `delete_vm` usually finishes the move on its own and the unused entry goes away. That includes a Director upgrade through `create-env`, which can't issue a `detach_disk` but does retry the `delete_vm`, so rerunning `create-env` finishes the move. When the unused entry still carries the name the volume was created with, the retried `detach_disk` also finds it without the record and moves it. We go further only when the retry leaves the unused entry there, which happens when the record is gone, most often because an earlier release removed it or the parker was deleted.

Before we touch anything, all four of these checks have to pass. If any one of them fails, or if more than one disk could be the candidate, we stop and leave the VM, its unused entry, and every parker exactly as they are.

1. The retried `detach_disk` succeeded, or the retried `delete_vm` refused again with this message, and the unused entry is still on the VM.

2. Nothing else in the cluster holds the disk's serial. We search every guest config on any node, because `/etc/pve` carries the whole cluster:

   ```bash
   grep -n '<bpd-serial>' /etc/pve/nodes/*/qemu-server/*.conf
   ```

   Every line it prints must come from `<N>.conf`, the VM we are recovering, and the text after the file name and line number must start with `#`, which is how PVE stores the description. A line from any other VM means another guest or a parker still names the disk, and a line that doesn't start with `#` means a drive of that VM still carries the serial. Either one fails the check.

3. The VM has exactly one candidate. Its description's `bosh_attached_disks` names exactly one `bpd-` serial that none of its active slots carries, and its config has exactly one unused entry on the disk storage. When the volume still has its birth name, the unused entry must name exactly the `volid` that `pve-cid decode <cid>` prints for that serial's CID. PVE leaves a birth-named volume on an unused entry only when the birth name carries this VM's own VMID. That happens only when the VM band was moved over VMIDs that still name disks, so the case is rare.

4. When the volume was renamed for the VM, which is when its name carries the VM's VMID, its size must match the disk the Director knows. `pvesm list <storage> --vmid <N>` prints the volume's size in bytes. The Director's size comes from the instance group's `persistent_disk` in `bosh -d <deployment> manifest`, or from its `persistent_disk_type` and that type's `disk_size` in `bosh cloud-config`. The CPI rounds that size up to whole GiB when it creates the disk, and a later resize changes it, so we compare against the size the disk has now.

When all four checks pass, we put the volume back on a free bus slot with its serial, then let BOSH detach it again:

```bash
qm set <N> --scsi<free-slot> <volid>,serial=<bpd-serial>
```

PVE drops the unused entry when it sees the same volume attached again, and it frees nothing. If the VM is running and disk hotplug is off, the change waits as pending until the VM restarts. Then we retry the failed task or rerun the deploy. `detach_disk` finds the disk by its serial and parks it the usual way, and `delete_vm` can go ahead after that. When the task is a `delete_vm`, as it is during a `create-env` upgrade, the retried `delete_vm` finds the disk on its slot and moves it to a parker itself before it destroys the VM.

A journal-managed disk whose volume name carries the VM's own VMID also leaves its record in `reconciliation_required` after the failed move, at a planned `lifecycle_detach_disk_Nodes_CreateQemuMoveDisk` step, and every call on the disk refuses on that step until the CPI settles it. A managed volume's usual name carries a VMID from the disk band, and no guest runs under that VMID, so its transfer leaves no unused entry. Since this release, such a volume parks by config edit instead. `storage-journal audit --summary` shows the allocation, its `planned step:` line, and the volume it still holds on the VM.

The CPI settles that step as a move that never started, in whichever call touches the record next. That can be a rerun `detach_disk` or `attach_disk`, a `delete_disk`, `resize_disk`, or `snapshot_disk`, the disk attach in `create_vm`, the disk preservation in `delete_vm`, or `storage-journal adopt`, `cleanup`, or `finalize-cleanup`. It first waits 10 minutes from the record's last save, which gives a move task that PVE did fork the time to show up in the node's task list or to finish. A call inside that wait refuses with `is planned; its move could not be settled as never started because its quiet period ends at <time>`, where the time is in UTC, and a rerun after that time goes ahead. When a lock step settles during the wait, its save restarts the wait, but only once. After the wait, the CPI lists the active move tasks on the step's node and on the node the VM runs on now, and only then does it read the VM and the volume. The step settles only when no move task for the VM is still active, the VM still names the volume under its old name with no pending delete or replacement, the volume there is still this disk, the volume still exists on the node, no other guest carries the disk's serial, and no parker names the volume. Otherwise the call refuses, names the check that failed after `because`, and changes nothing on PVE or in the journal.

The old name alone doesn't show that the volume is still this disk, because PVE gives a freed name to the next volume it renames onto the same VM. So when a bus slot names the volume, the CPI requires the disk's serial on that slot and refuses when the slot carries another disk's serial or none. An unused entry carries no serial, so for a volume there the CPI reads the parker instead. A parker has to keep its record of the disk's transfer from the VM under the old name, or the call refuses with `no parker keeps the record of the disk's transfer from VM <vmid>`. When we see that refusal, nothing left on PVE shows which disk the volume holds. So we first run the landing check that follows this paragraph, and only when it passes do we follow the recovery below, which checks the volume and puts it back on a bus slot of the VM with the disk's serial, and then we rerun. Only one parker may keep a record of the disk, so that the record proving the transfer and the landing the CPI checks come from the same parker. When two parkers keep one, the call refuses with `parkers <a> and <b> each keep a record of the disk's transfer`, or, when no slot carries the disk's serial, with the refusal that [A disk's serial or transfer record is on more than one guest](#a-disks-serial-or-transfer-record-is-on-more-than-one-guest) describes. The parker that keeps the record must also hold no volume named for it with no serial on any of its disk keys, because that's what a move that landed after its answer was lost leaves behind, on the recorded slot or on a fallback slot. When one does, the call refuses and names the key. Another disk's landing on that parker refuses the same way, since nothing on the parker tells the two apart. When the volume is another disk's landing, that disk's next call claims it, and our call goes ahead once no such volume remains there. We can tell the volume is this disk's own when its size matches the disk and the parker's description still names the disk's serial. In that case the move did happen after all, and a rerun won't clear it, so the step needs the investigation that [Delete an allocation through its retained authority](storage-journal-operations.md#delete-an-allocation-through-its-retained-authority) describes. PVE shows another user's tasks only to a token that has `Sys.Audit` on the node, so a token without it refuses with `listing move tasks on node <node> needs Sys.Audit on /nodes/<node>`, and we grant that privilege before we rerun.

The landing check comes before any of checks 2 through 4 and before the `qm set` reattach. A move that landed after its answer was lost leaves the disk's data on a parker, under a name for that parker and with no serial, and checks 2 through 4 can't see it. Check 2 looks for the disk's serial, which the landing doesn't carry, check 3 counts candidates only on the VM, and check 4 passes for any volume of the same size. So we list every disk key on every parker that names a volume for that parker and has no `serial=bpd-` option:

```bash
for conf in $(grep -l '^tags:.*bosh-parker' /etc/pve/nodes/*/qemu-server/*.conf); do
  parker=$(basename "$conf" .conf)
  grep -HnE "^(scsi|virtio|sata|ide|unused)[0-9]+: [^,]*vm-${parker}-disk-[0-9]+" "$conf" | grep -v 'serial=bpd-'
done
```

The check passes only when the loop prints nothing. When it prints a line, we stop and leave the VM, its unused entry, and every parker exactly as they are, because the volume on the unused entry may not be this disk. It may be another volume that took the disk's old name after the move landed, and if we reattached it under this disk's serial, the settlement would treat that volume as ours. When the volume the loop names is this disk's own, the step needs the investigation described above. When it belongs to another disk, we wait for that disk's next call to claim it, and then we run the check again.

The settlement only reads, so it can't rule out one case. A move task whose entry in the task list was never written, and whose rename then hung for more than 10 minutes, would pass every check. That takes two separate PVE failures, and it would leave the volume stranded under another VM's name rather than lost.

So on this release, we rerun the deploy once the quiet period has passed. When the parker still has its record of the transfer, which this release keeps while the VM names the volume, the rerun's `detach_disk` settles the step and finishes the move. When an earlier release already removed that record, `detach_disk` can't find the disk under any name and refuses with `terminal managed disk cannot be mutated` before the settlement runs. In that case we run the landing check above first. When it passes, we put the volume back using checks 2 through 4 and the `qm set` command that follows them, and then we rerun. The reattached slot on the VM is the one holder of the serial that the settlement accepts, so the rerun's `detach_disk` settles the step and parks the disk. A `delete_disk` that arrives before the reattach refuses on the step with `no parker keeps the record of the disk's transfer from VM <vmid>`, because nothing else shows that the volume on the unused entry is still this disk, and it deletes nothing.

For ordinary drift, run `bosh -d <deployment> cloud-check` to reconcile state. The Director offers to detach the disks and clean up the record. If the deployment can't be recovered, detach the disks with `bosh -d <deployment> detach-disk` before deleting the VM. See the [Operations Runbook](operations.md) for recovery procedures.

<a id="a-disk-call-refuses-while-a-move-to-a-parker-is-unsettled"></a>

### A disk call refuses while a move of the disk is unsettled

**Symptom**

```text
managed disk ownership provenance references a missing volume; audit required, because move step attempt-<n>-step-<m> of the disk's record isn't settled; see "A disk call refuses while a move of the disk is unsettled" in docs/troubleshooting.md of bosh-proxmox-cpi-release
```

The 0.9.0 release printed this entry's earlier title, "A disk call refuses while a move to a parker is unsettled", so a refusal from that release points here under that title.

Every call that resolves the journal-managed disk refuses this way, and the refusal is permanent, so the Director doesn't retry it. The call writes nothing to PVE or to the journal before it refuses. `has_disk` is the exception while something still carries the disk. When no other call holds the disk's record and the parker's transfer record still carries the disk, `has_disk` answers true, because the disk's data still exists, and a false answer could lead `bosh cloud-check` to forget the disk. It logs a warning that names the unsettled step. The other disk calls keep refusing until we settle that step.

`has_disk` refuses this way too when nothing carries the disk. That happens when no other call holds the disk's record, no VM or parker carries the disk, the name the record last gave the disk is gone from storage, and a move that set out from that name isn't settled. The record doesn't say where that move landed, so `has_disk` can't show the disk is missing, and a `false` answer could lead `bosh cloud-check` to forget a disk whose data still exists. In this case no parker keeps a record of the transfer, so we don't look for one. We start at step 1 of the fix below, because the audit's `planned step:` line still names the move. We then look for the landed volume on the guest the move was headed for. For a move onto a parker, that's the landing check in step 2. For an attach, it's the instance's VM, where the landed volume is named for that VM and its drive line carries no serial. We settle the step through the same investigation as the rest of this entry, and we don't add the serial or the disk's notes by hand.

This refusal can also come from a call that overlapped another call's move of the same disk while that move was still in progress. In that case nothing is wrong with the disk. Once the other call finishes, its move step is settled, and rerunning the refused call clears the refusal with no change from us. `has_disk` doesn't need that rerun. When its first look finds another call partway through the disk, it waits for that call to let go of the disk's record and then looks again before it answers. It does the same before it reports a journal-managed disk missing, because a disk on its way between two guests can escape every read of the first look. The wait stops 10 seconds before the request's deadline, which leaves that time for the second look, or after 120 seconds when the request has none. If the other call still holds the record by then, or the request ends first, `has_disk` fails with a retriable error that names the other call's operation, and we rerun the cloud check once that operation completes. When `has_disk` can't read the journal, it fails with a retriable error rather than report the disk missing. So before we look at anything else, we check whether another Director task on this disk was running when the refusal came, and if so, we let it finish and rerun. We go on to the diagnosis below only when the refusal comes back on a rerun with nothing else running on the disk.

**Diagnosis**

A move of the disk onto a parker sent its request, and the CPI never got PVE's answer, so the journal keeps that move step planned and the record sits in `reconciliation_required`. The parker still keeps its record of the transfer under the volume's old name, and that name is gone. The usual cause is that the move landed after its answer was lost, which leaves the disk's data on the parker under a name for that parker and with no serial. No step of the record names that landed volume, so the CPI can't finish the transfer, and it refuses before it writes anything. The settlement described in [delete_vm refuses to destroy VM with attached unused disks](#delete_vm-refuses-to-destroy-vm-with-attached-unused-disks) settles only a move that never started, so a rerun doesn't clear this state.

**Fix**

We start with checks that only read, and we change nothing on PVE or in the journal while we work out where the volume is.

1. We run `sudo -u vcap /var/vcap/packages/pve_cpi/bin/cpi storage-journal audit --summary --config /var/vcap/jobs/pve_cpi/config/cpi.json` on the Director and find the disk's allocation. Its `planned step:` line names the move step from the refusal.

2. We run the landing check from [delete_vm refuses to destroy VM with attached unused disks](#delete_vm-refuses-to-destroy-vm-with-attached-unused-disks) on a PVE node. It lists every disk key on a parker that names a volume for that parker and carries no serial.

3. For each line it prints, we read the parker's config with `pvesh get /nodes/<node>/qemu/<parker>/config` and the volume with `pvesh get /nodes/<node>/storage/<storage>/content/<volid>`. The volume is this disk's own when its size matches the disk and the parker's description still names the disk's serial `bpd-<id>`.

When the volume is this disk's own, the move did happen, and the step needs the investigation that [Delete an allocation through its retained authority](storage-journal-operations.md#delete-an-allocation-through-its-retained-authority) describes. We don't add the serial to the parker's slot by hand. The CPI would then find the disk by its serial, but because no step names the landed volume, every call would refuse it with `managed disk physical backing or volume conflicts with journal; audit required` instead.

When the check prints nothing, or the volume it names belongs to another disk, we leave every parker as it is and follow the same investigation, because the journal can't say where this disk's data went.

**An attach that stopped the same way**

The same state can come from `attach_disk`. When the CPI recorded the task of the move that brings the disk from its parker onto the instance's VM, and then stopped before it settled that move step, the VM carries the disk's serial on a volume that no step of the record names. Every disk call then refuses with this error.

```text
managed disk physical backing or volume conflicts with journal; audit required
```

`has_disk` answers true for this disk as well and logs a warning that names the step, but only when no other call holds the disk's record, the unsettled move's task names that VM as the move's target, and the move is on the same storage backing as the VM's volume. A move whose task is missing from the record, or names another VM, explains nothing, so `has_disk` keeps the refusal too, because the volume may be another disk's. We settle the step through the same investigation in [Delete an allocation through its retained authority](storage-journal-operations.md#delete-an-allocation-through-its-retained-authority), and we don't edit the VM's config or description by hand while we do.

### delete_disk refuses a disk stranded on an unused entry

**Symptom**

```text
delete_disk: refusing to delete disk <cid>, because VM <N> on node <node> still names its volume <volid> as unusedN, and no other configuration does. Removing that entry by hand makes PVE free the volume. Nothing was deleted; see "delete_disk refuses a disk stranded on an unused entry" in docs/troubleshooting.md of bosh-proxmox-cpi-release
```

Other calls refuse the same disk with the same pointer. `detach_disk` from a VM other than `<N>`, `attach_disk` on a journal-managed disk, `resize_disk`, and `snapshot_disk` each name the unused entry the same way. When more than one unused entry names the volume, or the only one sits on a VM in the parker band, every call refuses, because those entries can't be tied to one guest.

These refusals need VM `<N>`'s description to still hold the disk's attached-disk note under its stable ID, and `attach_disk` is what writes that note. When the note is gone, nothing proves that the unused entry holds this disk rather than another volume that took its old name. Every call then refuses with the error in [Another entry names a disk's birth volume](#another-entry-names-a-disks-birth-volume), and that section replaces this one for the disk, so the Fix below doesn't apply to it. That includes `delete_disk` on a journal-managed disk whose record already says it was deleted. That call used to refuse with `terminal managed disk still has ownership provenance`, and it now checks for the note before it reads the record.

**Diagnosis**

The disk's move to a parker stopped after the CPI deleted its slot on VM `<N>`, and the parker's record of the transfer was lost afterwards, which releases up to 0.8.0 did once the record was an hour old. The volume's name carries VM `<N>`'s own VMID. That happens when the VM band was moved over VMIDs that still name disks, and PVE then kept the volume on an unused entry instead of freeing it. No slot carries the disk's serial, and the Director treats the disk as orphaned, so it sends `delete_disk` and has no detach to send. A park that a snapshot defers leaves the disk in the same state, and when `detach_disk` reports such a park as a success, it still removes the disk's attached-disk note from VM `<N>`. Since 0.8.1, the parker keeps that record while VM `<N>` still holds the volume, so the disk resumes from it. A park like that can't move the volume while any snapshot of VM `<N>` names it, because PVE refuses the move for every such snapshot. `qm delsnapshot` can't be undone, and it removes VM `<N>`'s rollback point for all of its disks and its saved RAM state, so deleting a snapshot is the deployment owner's decision. A disk that lost both its record and its note gets the error in the next section instead of this one, and a release up to 0.8.0 could leave a disk in that state.

**Fix**

As in the section above, never destroy VM `<N>` by hand while the entry is there. There are two ways out, and the one we take depends on whether we want to keep the disk.

To keep the disk, we attach it to an instance with `bosh -d <deployment> attach-disk <instance-group>/<id> <cid>`. The attach moves the volume off VM `<N>`'s unused entry onto a parker first and then attaches it to the instance, so it ends up with one reference. That works for a disk without an allocation journal. A journal-managed disk refuses the attach for now, so we leave it exactly as it is and run `storage-journal audit --summary` to see its allocation.

To delete the disk, we run checks 2 through 4 from the section above against VM `<N>`, with the CID from the refusal. This is the one case where removing the unused entry is right, because removing it frees the volume, and freeing the volume is the delete the Director asked for. If any of those checks fails, we stop and leave the VM and its entry as they are. Only when all of them pass do we remove the entry:

```bash
qm set <N> --delete unusedN
```

Then we delete that one orphan with `bosh -d <deployment> delete-disk <cid>`. We don't use `bosh clean-up --all` here, because it deletes every orphaned disk on the Director. The `delete_disk` that `delete-disk` sends finds that the volume is gone and finishes.

### Another entry names a disk's birth volume

**Symptom**

```text
<op>: resolve disk identity for <cid>: ResolveDiskIdentity: refusing to resolve the disk by its birth volume: no slot carries the disk's serial bpd-<id>, but slot scsiN of VM <N> on node <node> with no serial names its birth volume <volid>, so we can't tell whether that volume is still the disk. Nothing acts on the volume until an operator confirms whose it is; see "Another entry names a disk's birth volume" in docs/troubleshooting.md of bosh-proxmox-cpi-release
```

The entry can also read `with serial bpd-<other>`, or `unused entry unusedN of VM <N> on node <node>, whose description holds no note for the disk`, and one refusal can list more than one entry. `attach_disk`, `detach_disk`, `resize_disk`, `update_disk`, `snapshot_disk`, `set_disk_metadata`, `delete_disk`, and the attach inside `create_vm` all refuse this way, and the refusal is permanent, so the Director doesn't retry it. `has_disk` answers false for the same disk.

**Diagnosis**

A disk with a stable ID keeps the name its volume had at `create_disk` in its CID, but a move to a parker or another VM renames the volume, so that birth name can later go to another volume. A legacy `create_disk` that reuses a freed VMID is the likeliest way that happens. The CPI finds such a disk by the `serial=bpd-<id>` on its drive entry, or by its parker's transfer record. While a parker keeps the disk's transfer record, the CPI resumes that transfer instead of refusing, even when the move landed the volume under its birth name before the serial was written. In this state, no slot carries the serial, and no parker keeps the disk's transfer record. The entry the refusal names holds the birth name under another serial or none, so we can't tell whether that volume is our disk or another one.

Earlier releases took the entry as our disk, so they could grow, snapshot, move, or hand out another disk's volume. An `unusedN` entry carries no serial, so the CPI takes it as our disk only when that VM's description still holds the disk's attached-disk note under its stable ID, and `attach_disk` is what writes that note. An operator who removed a disk's serial by hand, or cleared a VM's description, leaves the same state. This refusal comes before the refusals in the section titled [delete_disk refuses a disk stranded on an unused entry](#delete_disk-refuses-a-disk-stranded-on-an-unused-entry), so a disk stranded on a VM whose note is gone gets this error, and so does a journal-managed disk whose journal record already says it was deleted.

Earlier releases also left disks in this state themselves. When a snapshot blocks a park, `detach_disk` still removes the disk's note from the VM and reports success, and releases up to 0.8.0 then dropped the parker's record once it was an hour old, so the volume sat on an `unusedN` entry with neither. That's the likeliest way to reach this error from an `unusedN` entry. Since 0.8.1, the parker keeps its transfer record while the VM still holds the volume, so the disk resumes from that record. A resume like that can't move the volume while any snapshot of the VM names it, but a disk in this state has no record to resume from, so nothing in this section needs a snapshot deleted. Deleting one is the deployment owner's decision, because `qm delsnapshot` can't be undone and removes the VM's rollback point for all of its disks and its saved RAM state.

**Fix**

We worked this procedure out from the code and haven't run it against a lab, so we read each command's output before we take the next step.

We never delete the volume, remove its entry by hand, or destroy VM `<N>` while we work through this, because the volume may belong to another disk. We also never attach the volume to a slot, snapshot it, or run `qm rescan`, because each of those changes the state we're trying to keep. Nothing that PVE or the CPI records can prove which disk the volume is. PVE hands a freed name to the next volume it creates or moves onto the same VM. A volume can therefore carry our disk's old name, sit where our disk sat, and match its size and format, and still be another disk. So this recovery restores no note and puts no serial back on any slot. The only change it makes to any VM's configuration or to any volume's name is to move the entry, with its data untouched, onto an unused entry of a quarantine VM, and it makes that move only after every check below passes. Activating a volume for the copy changes only its activation state. After that, we release the disk's record from the Director, and the instance gets a new, empty disk.

**Keep the Director away from the disk**

Before anyone runs cck's "Delete disk reference" on this disk, we ignore the instance that owns it, because that resolution fails on this disk and leaves its record inactive, as the release step below explains. While its record is active, `bosh -d <deployment> instances --details` lists `<cid>` under that instance. Once the record is inactive, that list leaves it out, but `bosh -d <deployment> cloud-check --report` still names the instance on the disk's line. That holds only until we ignore the instance, because the report skips ignored instances.

```bash
bosh -d <deployment> ignore <instance-group>/<id>
```

While the flag is set, every deploy skips the instance and warns that it did, and cck and the resurrector leave its VM and its disks alone. The Director also refuses `bosh delete-deployment` for the deployment and any manifest that removes the instance group. It refuses `bosh start`, `stop`, `restart`, and `recreate` aimed at the instance, and it refuses `bosh attach-disk` to it. When an earlier cck run already left the record inactive, the flag also stops deploys from failing on it, because the Director no longer updates the instance.

**Gather what we can and copy the data out**

We gather what we can, using reads alone. None of it shows that a volume is our disk, because a name match proves nothing about which disk a volume is. We keep what we find for the deployment owner, who decides what happens to the volume later.

1. We search the CPI's output, which the Director keeps with each task, for the disk's CID `<cid>` from the refusal. Disk calls record the CID as `disk_cid`, and a line that records a move gives the name the volume moved to as `volid_after`. From a workstation that is logged in to the Director, we print each recent task's CPI output and search it for the CID. The loop makes one Director request for each of up to 1,000 tasks, and it changes nothing:

   ```bash
   for id in $(bosh tasks --recent=1000 --all --json | jq -r '.Tables[0].Rows[].id'); do bosh task "$id" --cpi 2>/dev/null | grep -F '<cid>' | sed "s/^/task $id: /"; done
   ```

   The search can miss lines. The Director prunes old tasks, a `pve.log_level` of `warn` or `error` drops the lines that record moves, and a CPI that `bosh create-env` or another Director launches writes its logs somewhere else. A name the search finds is only a candidate, because another volume can have taken that name since.

2. We read the entry on node `<node>` with `qm config <N>`. A serial on it that names another stable ID means another disk last held that slot. An entry without a serial tells us nothing about whose volume it is.

3. On an LVM storage, we read whether the volume is active with `lvs --noheadings -o lv_active <path>`, where `<path>` is what `pvesm path <volid>` prints for it. The command prints `active` for an active volume, and we note the answer, because the copy below can change it.

Then we copy the volume's data out, and we only read the volume. The copy writes a file to other storage, and activating an LVM volume changes its activation state but no data. On the PVE node `<node>` that the refusal names, we read the volume's format from its storage entry and its path from `pvesm`. Here `<storage>` is the part of `<volid>` before the colon. Then we convert the volume into a file outside the storage that holds it:

```bash
pvesh get /nodes/<node>/storage/<storage>/content --output-format json | jq -r '.[] | select(.volid == "<volid>") | .format'
pvesm path <volid>
qemu-img convert -U -f <format> -O qcow2 <path> <destination>
```

On LVM, the path may not exist until we activate the volume with `lvchange -ay <path>`, which changes no data. On Ceph RBD, we export the image with `rbd export <pool>/<image> <destination>` instead, where `<pool>` is the pool that storage `<storage>` uses in `/etc/pve/storage.cfg`, and `<image>` is the volume's name without its `<storage>:` prefix. When the entry is a slot of a running VM, that VM can write while we read, so the copy may not be consistent. When the search in the gather list named a candidate that still exists, we copy it out the same way, because it may hold our data instead. After the copy, when the volume was inactive in the third gather item, we deactivate it again with `lvchange -an <path>`. When it was already active, we leave it as it was.

**Prove that no other CID names the volume**

Next we prove that no disk CID other than `<cid>` names the volume `<volid>`, and that no CID names a volume of the quarantine VM we're about to create. The move gives the volume a new name under the quarantine VM's VMID, so a CID that already names that new name would own our volume after the move. This check is manual, and it's only as complete as the list of Directors and state files we feed it. A CID it misses can still destroy the volume later, because `delete_disk` deletes whatever volume carries the name in its CID, and PVE's content delete doesn't check whether a volume is in use.

We pick the quarantine VM's VMID `<Q>` first. It has to sit outside every VMID band. The bands are configurable, so we read them from every CPI configuration that uses this cluster, on each Director and in each `bosh create-env` CPI configuration, and we don't trust the defaults. That configuration holds the API token, so we read only the band property names below and never print or paste the whole file. On a Director, the rendered file is `/var/vcap/jobs/pve_cpi/config/cpi.json`:

```bash
sudo jq '{vmid_range_start, vmid_range_end, disk_vmid_range_start, disk_vmid_range_end, stemcell_template_vmid_range_start, stemcell_template_vmid_range_end, parked_disk_vmid_range_start, parked_disk_vmid_range_end}' /var/vcap/jobs/pve_cpi/config/cpi.json
```

A property that prints `null` isn't set, and its default applies. The defaults are 100-8999 for VMs, 9000-29999 for disks, 30000-30999 for stemcell templates, and 90000-90999 for parkers. `<Q>` is a free VMID outside all four bands of every one of those configurations. On node `<node>`, `pvesm list <storage> --vmid <Q>` has to print no volumes.

From a workstation with `jq`, we define a function that decodes a disk CID into the volume it names and the stable ID it carries:

```bash
decode_cid() {
  p=$(printf '%s' "${1#pv?-}" | tr '_-' '/+')
  while [ $(( ${#p} % 4 )) -ne 0 ]; do p="$p="; done
  case "$1" in
    pvz-*) printf '%s' "$p" | base64 -d | gunzip ;;
    pvd-*) printf '%s' "$p" | base64 -d ;;
  esac | jq -r '"\(.v) \(.m.id // "-")"'
}
```

Then we log in to each Director that uses this PVE cluster in turn and collect every disk CID it knows. `instances --details` lists only a deployment's active disks, so we add `cloud-check --report`, which also names the inactive and missing disk records of instances that aren't ignored. The report runs a scan task that takes each deployment's lock and records what it finds, and it changes nothing on PVE. We read every error the loop prints, because a Director or deployment we can't read adds nothing to the list and makes the search look clean.

```bash
{
  for d in $(bosh deployments --json | jq -r '.Tables[0].Rows[].name'); do
    bosh -d "$d" instances --details --json
    bosh -d "$d" cloud-check --report --json
  done
  bosh disks --orphaned --json
  bosh disks --dynamic --json
} | grep -oE 'pv[dz]-[A-Za-z0-9_-]+' >> cids.txt
```

We add the CIDs from every `bosh create-env` state file that uses this cluster, and then we decode them all and search the result:

```bash
grep -oE 'pv[dz]-[A-Za-z0-9_-]+' <state-file> >> cids.txt
sort -u cids.txt | while read -r c; do echo "$c $(decode_cid "$c")"; done > decoded.txt
awk 'NF != 3' decoded.txt
grep -F ' <volid> ' decoded.txt
grep -F 'vm-<Q>-' decoded.txt
```

The `awk` check has to print nothing before we trust either search. A CID that fails to decode, for example because it's truncated or `gunzip` is missing, leaves a line that holds only the CID, and neither search can match that line. When `awk` prints anything, we fix the cause and rebuild `decoded.txt` before we go on.

The first search must print no CID other than `<cid>`. When it prints any other CID, the volume may be that disk's live data, and we follow the part below titled When the volume belongs to another disk instead of moving it. The same goes for an entry whose serial, as we read it in the gather list, names another stable ID. When the second search prints anything, we stop, because a CID already names a volume of `<Q>`. We pick another `<Q>` and run both searches again. The second search matches `vm-<Q>-` anywhere in a line, so it also catches the new name on file storage, where `<Q>/` comes first.

The search covers only this CPI's CID envelopes, so a Director that runs another CPI against the cluster is a gap, and the legacy Perl CPI is one example, because it keeps bare volids that the search can't match.

The Director can't show us an ignored instance's inactive disk records. The `instances --details` listing leaves out inactive disks, and `cloud-check --report` skips ignored instances. Our own instance doesn't matter here, because the refusal gives us its CID. On every Director we searched, we list the ignored instances of every deployment, which `instances --details` marks with an `ignore` flag:

```bash
for d in $(bosh deployments --json | jq -r '.Tables[0].Rows[].name'); do
  bosh -d "$d" instances --details --json | jq -r --arg d "$d" '.Tables[0].Rows[] | select(.ignore == "true") | "\($d) \(.instance)"'
done
```

When this prints any instance other than ours, the search has a gap we can't close, so we don't move the entry, and we follow the part titled When the volume belongs to another disk. The one exception is an instance whose owner agrees to unignore it until a fresh `cloud-check --report` has run. After the scan, the owner sets the flag again, and we add that report's CIDs to `cids.txt` and run the searches again.

**Move the entry onto a quarantine VM**

We move the entry only when all of the following hold on node `<node>`:

- The refusal lists exactly one entry. The move renames the volume, so any other entry that names it would then point at nothing.

- The entry is an `unusedN` entry. We never move a slot.

- VM `<N>` is a VM CID that a Director we searched knows, `<N>` sits outside the parker band we read above, and VM `<N>` has no `bosh-parker` tag. We run the first command below on each Director we searched, in turn, and it has to print `<N>` on at least one of them. The second command has to print nothing:

  ```bash
  for d in $(bosh deployments --json | jq -r '.Tables[0].Rows[].name'); do bosh -d "$d" vms --json | jq -r '.Tables[0].Rows[].vm_cid'; done | grep -x '<N>'
  qm config <N> | grep -E '^tags:.*bosh-parker'
  ```

- No snapshot of VM `<N>` names the volume, and no other entry of VM `<N>` does. This command prints every line of VM `<N>`'s configuration that names the volume, together with the section it sits in, and it must print exactly one line, marked `[current]`:

  ```bash
  awk -v v='<volid>' '/^\[/ { s = $0 } { n = split($0, f, /[ ,]/); for (i = 1; i <= n; i++) if (f[i] == v) print (s == "" ? "[current]" : s) " " $0 }' /etc/pve/nodes/<node>/qemu-server/<N>.conf
  ```

A snapshot of VM `<N>` that names the volume stops PVE from moving it, and we never delete a snapshot to make room for the move. `qm delsnapshot` can't be undone, and it removes VM `<N>`'s rollback point for all of its disks and its saved RAM state, so deleting a snapshot is the deployment owner's decision.

We move only an unused entry on a VM that a Director we searched knows, because a slot with no serial proves nothing, and our CID searches can't see a boot disk, a non-BOSH VM's disk, or a stable-ID disk whose serial someone removed. A slot of any kind, and an unused entry on a VM that no Director we searched knows, go to the part titled When the volume belongs to another disk, and we don't move them.

Then we create the quarantine VM on node `<node>`, with no disks and no tags, and move the entry onto it. We never start the quarantine VM, because it exists only to hold the entry. Here `<slot>` is the entry's key from the refusal, such as `unused0`:

```bash
qm create <Q> --name quarantine-<Q> --description 'Holds a stranded volume. Never start or delete this VM.'
qm disk move <N> <slot> --target-vmid <Q> --target-disk unused0
qm config <N>
qm config <Q>
```

After the move, VM `<N>` no longer names the volume, and VM `<Q>` lists it as `unused0` under a new name. On block storage such as LVM or Ceph RBD, the name after the colon starts with `vm-<Q>-`, and on file storage such as directory, NFS, or CIFS, it reads `<Q>/vm-<Q>-disk-<n>.<format>`, such as `.qcow2`. We record that new name with the copy for the deployment owner.

A refusal before the move changes nothing, and we stop. Any other failure can leave the volume renamed while VM `<N>` still names its old name, or while neither VM names it, because PVE renames the volume before it updates either configuration. In that case we read and keep the output of these commands, and then we stop. We don't retry the move, and we don't release the record:

```bash
qm config <N>
qm config <Q>
pvesm list <storage> --vmid <Q>
```

When any check in this section fails, we don't move the entry. We leave the volume and its entry exactly as they are and keep the instance ignored, and removing them later is a separate decision for the deployment owner. When the volume sits on an unused entry of VM `<N>`, `delete_vm` on that VM refuses permanently for as long as the volume stays there. So every recreate and stemcell upgrade of the instance on VM `<N>` fails until the volume leaves VM `<N>`. Once the check that failed passes, for example after the deployment owner deletes a snapshot, we come back to this section. Instead of waiting, we can release the record without the move, as the part titled When the volume belongs to another disk describes, and accept the failing cleanup that part explains.

**Release the record and deploy**

We release the record with `bosh orphan-disk` and never with cck. The cck resolution "Delete disk reference" calls `detach_disk` for the disk, and while the entry names the birth volume, `detach_disk` refuses with the error above. By then the failed run has already marked the disk's record inactive, and it doesn't undo that. The next deploy of the instance creates and attaches a new, empty disk, and then it fails when it calls `detach_disk` on the old disk, and every deploy after that fails on the same call. Even after the move, cck is the wrong tool. For a disk without an allocation journal, its `detach_disk` would park whatever volume carries the birth name by then under our disk's serial, and for a journal-managed disk, its `detach_disk` fails, because the CPI reads the allocation as gone. `bosh orphan-disk` makes no call to the CPI at all. It moves the record to the Director's orphaned disks in one database step, whether the record is active or inactive.

Before we release anything, we check whether the Director holds snapshots of this disk, because releasing the record deletes them:

```bash
bosh -d <deployment> snapshots <instance-group>/<id>
```

When it lists any snapshot, we stop and hand the decision to the deployment owner. The Director moves a disk's snapshots to its orphaned snapshots along with the record, and when it deletes the orphan, it calls the CPI's `delete_snapshot` for each of them before it calls `delete_disk`. This CPI's snapshot CID names a whole-VM PVE snapshot, so the delete removes that VM's rollback point for all of its disks and its saved RAM state, and it can't be undone. A snapshot delete that fails also keeps the orphan and skips `delete_disk`.

When it lists no snapshot, we take these four steps in this order:

1. We release the record. `bosh orphan-disk` takes no lock on the deployment, so it would race any task that's working there. We run it only when `bosh -d <deployment> tasks` shows no task running or queued for the deployment, and we start no task there until it finishes:

   ```bash
   bosh -d <deployment> tasks
   bosh -d <deployment> orphan-disk <cid>
   ```

   When an earlier run already orphaned the record, `orphan-disk` warns that the disk doesn't exist and changes nothing.

2. We delete the orphan right away, and we don't wait for the scheduled cleanup. Before we do, we confirm that `pvesm list <storage>`, run on every node that sees the storage, shows no `<volid>`. When one does, we stop there. The move freed the birth name. PVE's `find_free_diskname` hands the lowest free index to the next volume that's created or moved onto that VMID, so the deploy's new disk can take the birth name. Our orphan's `delete_disk` would then meet that disk under the name, and it would refuse or, when the disk is free-floating, delete it in our disk's place.

   ```bash
   pvesm list <storage>
   bosh -d <deployment> delete-disk <cid>
   ```

3. We confirm that `bosh disks --orphaned` no longer lists `<cid>`. When it still does, we stop, read the error of the delete task, and go no further.

   ```bash
   bosh disks --orphaned
   ```

4. Only then do we let the Director manage the instance again and deploy it:

   ```bash
   bosh -d <deployment> unignore <instance-group>/<id>
   bosh -d <deployment> deploy <manifest>
   ```

The deploy finds the instance without a persistent disk, so it creates a new, empty disk and attaches it, and there's nothing to migrate. Restoring data onto that disk from the copy is the deployment owner's decision.

**What the orphan cleanup does afterwards**

The Director's scheduled orphan cleanup runs every 30 minutes, and by default it calls `delete_disk` for each orphan that's more than five days old. `bosh -d <deployment> delete-disk <cid>` in step 2 sends the same call for this one disk straight away. For a disk without an allocation journal, `delete_disk` finds no volume under the birth name, treats the disk as already deleted, and deletes nothing, and with no snapshots to delete first, the Director drops the orphan. For a journal-managed disk, `delete_disk` settles the journal record and deletes nothing. The exception is a journal that still holds evidence of a change to the disk that never settled, and then `delete_disk` refuses and the orphan stays. Neither answer touches the quarantined volume, because no CID names it.

**When the volume belongs to another disk**

The volume may be live data that isn't our disk's, and we don't move it, when any of these holds. The entry carries another disk's serial, the search finds another CID that names `<volid>`, the entry is a slot, the entry is an unused entry on a VM that no Director we searched knows, or a gap we can't close remains. A move would rename it, and whatever refers to it by its old name would then lose it. We still release our record. We run the snapshot check from the release step first, then `bosh -d <deployment> orphan-disk <cid>` while no task runs on the deployment, and then we unignore the instance and deploy it as step 4 describes, so the instance gets a new disk. We skip steps 2 and 3, because the volume still carries the birth name, and the delete in step 2 would refuse.

Our orphan's `delete_disk` then refuses with the error above every time the cleanup reaches it, so the scheduled cleanup task fails every 30 minutes. It still deletes the other orphans it picks up, because it reports the failure only at the end of its run. The Director at v283.1.11 removes an orphaned disk's row only when that disk's `delete_disk` succeeds or answers that the disk isn't found. `bosh delete-disk` and `bosh clean-up --all` run the same delete and fail the same way. `bosh attach-disk` takes the row out of the orphans only by putting the record back on an instance, which brings the refusal back, and nothing else in the Director removes the row. So the row and the failing task stay until the other disk's volume no longer carries the birth name, for example after that disk is deleted or its own detach parks it under a new name. The next `delete_disk` then finds no volume under the birth name, and the Director drops the row. We tell the other disk's owner about our orphan, because anything that leaves that volume free-floating under the birth name would let our orphan's next `delete_disk` delete it.

**What stays at risk**

The CID checks are manual. A Director or state file we didn't search can still hold a CID that names the quarantined volume, and a `delete_disk` through that CID would destroy it, because PVE's content delete doesn't check whether a volume is in use.

Until our orphan's `delete_disk` has run, a volume that nothing references could take over the birth name. That `delete_disk` would then delete it, because the CPI resolves a free-floating volume under the birth name as the disk's own. This applies only to a disk without a strict parked anchor, which means a disk whose CID promises no parker anchor, a deployment that runs the free strategy, or a deployment that sets `pve.parked_anchor_strict` to false. For any other disk, `delete_disk` refuses when no VM references the volume.

Any `delete_vm` on a VM that holds a legacy slot with no serial, naming a volume of another VMID, detaches that slot and leaves its volume free-floating. When that volume carries a stranded record's birth name, the record's next `delete_disk` deletes it, and if it held another disk's data, that data is lost.

We run `bosh orphan-disk` only while no task works on the deployment, as the release step says.

We checked PVE's own rule in qemu-server 9.2.10. It refuses to move a slot off a running VM and allows an unused entry. This procedure is stricter, because it moves only an unused entry.

For a journal-managed disk, `delete_disk` refuses while the journal holds a change that never settled, so the orphan cleanup can still fail for that reason.

`bosh orphan-disk` sends nothing to the instance's agent, while cck asks the agent to unmount the disk when the agent still lists it, and only then detaches it. We haven't read the agent's side, so we can't say how an agent treats a disk that its settings may still list.

We worked this procedure out from the CPI's code and the BOSH Director's code at v283.1.11, and we have not run it against a lab.

### A disk's serial or transfer record is on more than one guest

**Symptom**

```text
<op>: resolve disk identity for <cid>: ResolveDiskIdentity: refusing to resolve a disk whose serial more than one guest carries: the disk's serial bpd-<id> is on slot scsiN of VM <A> on node <node> with volume <volid-a> and slot scsiM of VM <B> on node <node> with volume <volid-b>, so we can't tell which volume is the disk. A clone or a backup restore copies a drive line with its serial. PVE files a qmrestore task under the copy it created, but it files a qmclone task under the VM it copied, so the VMID a task is filed under doesn't say which guest is the copy. The original is the guest that runs the VM CID `bosh instances --details` lists with the disk, or the guest that holds the volume the CPI's latest log line for the disk gives as volid_after. Nothing acts on the disk until an operator removes serial=bpd-<id> from the copy's drive line; see "A disk's serial or transfer record is on more than one guest" in docs/troubleshooting.md of bosh-proxmox-cpi-release
```

A guest in the parker band reads `parker VM <N>`. When no slot carries the serial, the refusal names parkers instead:

```text
<op>: resolve disk identity for <cid>: ResolveDiskIdentity: refusing to resolve a disk that more than one parker records: no slot carries the disk's serial bpd-<id>, but parker VM <P> on node <node> (recorded volume <volid>, source VM "<vm>") and parker VM <Q> on node <node> (recorded volume <volid>, source VM "<vm>") each keep a record of its transfer, so we can't tell which parker received the disk. A clone or a backup restore of a parker copies its records. PVE files a qmrestore task under the copy it created, but it files a qmclone task under the VM it copied, so the VMID a task is filed under doesn't say which guest is the copy. The original is the guest that runs the VM CID `bosh instances --details` lists with the disk, or the guest that holds the volume the CPI's latest log line for the disk gives as volid_after. Nothing acts on the disk until an operator removes the record from every parker but the one that received the disk; see "A disk's serial or transfer record is on more than one guest" in docs/troubleshooting.md of bosh-proxmox-cpi-release
```

When one slot carries the serial and a parker keeps a record of the disk's transfer that names another volume, and that volume is still there, the refusal names both:

```text
<op>: resolve disk identity for <cid>: ResolveDiskIdentity: refusing to resolve a disk whose slot and transfer record name different volumes: the disk's serial bpd-<id> is on slot scsiN of VM <A> on node <node> with volume <volid-a>, but parker VM <P> on node <node> keeps a record of the disk's transfer that names volume <volid-p>, and <where that volume still is>, so we can't tell which volume is the disk. A clone or a backup restore taken while the disk was moving to a parker copies its drive line with the serial, so the slot can be the copy's while the disk waits for the parker. PVE files a qmrestore task under the copy it created, but it files a qmclone task under the VM it copied, so the VMID a task is filed under doesn't say which guest is the copy. The original is the guest that runs the VM CID `bosh instances --details` lists with the disk, or the guest that holds the volume the CPI's latest log line for the disk gives as volid_after. Nothing acts on the disk until an operator removes serial=bpd-<id> from the slot's drive line when the slot is the copy's, or removes the record from the parker when the slot holds the disk; see "A disk's serial or transfer record is on more than one guest" in docs/troubleshooting.md of bosh-proxmox-cpi-release
```

The part that says where the volume still is takes one of four forms:

- `the parker holds volume <volid> on scsiM with no serial, the slot the record names, which is what a move that landed before its serial write leaves` when the record's own slot on its parker holds such a volume.

- `unusedN of VM <B> on node <node> still names that volume` when a disk key names it.

- `snapshot "<name>" of source VM <vmid> on node <node> names that volume on scsiM` when a snapshot of the VM the transfer came from names it on a drive line with this disk's serial or with none. PVE reuses a freed VMID, so a snapshot line under another disk's serial belongs to a new guest's disk and doesn't count.

- `storage <storage> on node <node> still holds that volume and no guest names it` when only storage shows it. On node-local storage the CPI checks every online cluster member where the storage is enabled, so the node it names can differ from the parker's node. Another disk's key or unfinished record sets aside only the volume on its own guest's node there, because a volume of the same name on another node is a different volume.

The storage form fits two different states. The volume can be an orphan that took the freed name after the disk moved on, or it can be the disk's own volume, which a deferred park released and whose snapshot is gone since. To tell them apart, we compare the volume's size and creation time with those of the volume on the slot that carries the serial, which `pvesh get /nodes/<node>/storage/<storage>/content` lists as `size` and `ctime`, and we look in the CPI's log for the disk's latest `volid_after`. We remove the parker's record only when that log line names the slot's volume, because otherwise the volume storage still holds may be our disk.

When the CPI can't tell whether the volume is still there, the call fails with a retriable error instead. The usual retriable errors begin with one of these lines:

- `ResolveDiskIdentity: can't tell whether volume <volid>, which parker vmid <P>'s record of the disk's transfer names, is still on storage <storage> on node(s) <nodes>` when a storage read failed and no other node shows the volume. When the snapshot read failed too, the error says so.

- `ResolveDiskIdentity: can't tell whether volume <volid>, which parker vmid <P>'s record of the disk's transfer names, is still on node-local storage <storage>, because node(s) <nodes> are offline` when the storage is local to each node and a node where it's enabled is offline, because that node could still hold the volume.

- `ResolveDiskIdentity: storage <storage> on node <node> still holds volume <volid>, which parker vmid <P>'s record of the disk's transfer names, and no guest we could read names it, but node(s) <nodes> are offline` when only storage shows the volume while the cluster reports a node offline.

- `ResolveDiskIdentity: can't tell whether volume <volid>, which parker vmid <P>'s record of the disk's transfer names, is still the disk's on storage <storage>, because another disk's key or unfinished record names it on node(s) <nodes>` when that other disk's claim would set the volume aside but the read of the source VM's snapshots failed, because a snapshot would outrank the claim.

`attach_disk`, `detach_disk`, `resize_disk`, `update_disk`, `snapshot_disk`, `set_disk_metadata`, `delete_disk`, and the attach inside `create_vm` all refuse this way. Each of the three refusals is permanent, so the Director doesn't retry it, and none of these calls changes anything on PVE. A record whose volume is shown by a landing, a key, or a snapshot refuses permanently even while a node is offline, because what the CPI read already shows that the volume is there. The retriable errors above are the usual ones the Director retries. Most of them clear once the reads answer or the offline node is back. A node where the storage can't answer at all keeps the error in place, and the error names that node and the storage. A node with no backing for a storage that's defined for the whole cluster is one example. To get past it, we restrict the storage's `nodes` list to the nodes that have the storage, or we make the comparison described above for the storage form and then remove the parker's record. A node that has left the cluster is never asked, even while the storage's `nodes` list still names it. A storage whose `nodes` list names only nodes that have left the cluster keeps the error in place too, because no node is left to ask, and the error says that the list names no current cluster member, so we correct the list to name a current member and rerun. A snapshot read that keeps failing keeps the error in place as well, and the error names the source VM, so we repair or remove the snapshot on that VM that PVE can't read, and then we rerun. A failed read of the guests that could name the volume is retriable too, and it begins `ResolveDiskIdentity: read the guests that name recorded volume <volid>`. `has_disk` answers true for the same disk, because one of the guests does hold it.

A record whose volume names no storage, such as `vm-<N>-disk-0` with no `<storage>:` in front of it, refuses permanently with a line that begins `ResolveDiskIdentity: refusing to resolve disk bpd-<id>, because parker VM <P> on node <node> keeps a record of its transfer that names volume "<volid>", which names no storage`. A retry can't change that record, so the refusal names the parker and the `bpd-<id>` entry of `bosh_parked_disks` to remove. Once we know the slot that carries the serial holds our disk, we take that entry out of the parker's description, as step 4 below shows, and we rerun.

**Diagnosis**

The CPI finds a disk with a stable ID by the `serial=bpd-<id>` on its drive line, and only one guest should ever carry that serial. A `qm clone` of a VM, or a `qmrestore` of its backup to a new VMID, copies every drive line with its serial onto volumes that belong to the copy. A copy of a parker also copies its description, so the copy keeps the same transfer records as the parker it came from. Up to 0.9.0, the CPI stopped at the first guest it read in the order PVE lists them, so when the copy came first, `attach_disk` moved the copy's volume onto the instance, and `delete_disk` deleted the copy's volume while our disk stayed where it was. The CPI now reads every guest and refuses when two of them carry the serial, when no slot carries it and two parkers each keep a record of its transfer, or when one slot carries it and a parker's record names another volume that is still there.

A guest counts once however it carries the serial. A slot whose change is still pending on a running VM shows the serial in both of PVE's views, and that's still one guest. A move of the disk between two guests never shows the serial on both, because PVE writes the source's config before the target's. The CPI reads the guests one after another without a lock, though, so a move that lands between two of those reads can make it see the disk on both ends. So when the first read finds the disk on two guests, the CPI reads the cluster again, and it refuses only when the second read finds the same. While one slot carries the serial, the CPI also reads the parkers' transfer records, because a clone taken while the disk was moving to a parker leaves the copy's slot as the only one with the serial, and only the record shows that the disk is somewhere else. A record that the slot's own guest keeps doesn't count, because a parker's record still gives the volume's old name until the transfer finishes. A record that names the volume the slot holds doesn't count either, because that's a transfer that hasn't deleted the source slot yet. A record counts only while the volume it names is still there and is still this disk's, and the CPI checks that in a fixed order. First, the record's parker can hold a volume named for it with no serial on the slot the record names, which is how a move leaves the disk before its serial write, as long as no other disk's unfinished record names that slot. Next, a disk key on some guest, other than the slot that carries the serial, can name the volume with this disk's serial or none. Then a snapshot of the VM the transfer came from can name the volume, which is all that a deferred park of a volume that VM doesn't own leaves behind. A snapshot names a volume by the name it had when the snapshot was taken, so nothing another disk holds now overrides it. After those three checks, the CPI sets the volume aside as another disk's when that disk's key names it with that disk's serial, when that disk's landing sits on a parker slot its record names, or when that disk's unfinished transfer record names it, because PVE gives a freed name to the next volume it renames onto the same guest. Another disk's record counts as unfinished only while no guest carries that disk's serial, since a record whose disk already sits on a slot is left over from a finished move and says nothing about who holds the name now. While a node is offline, a guest there could carry that serial, so no record sets the volume aside until the node is back. Last, storage can still hold the volume, and the CPI proves that the same way it proves a volume gone before it resumes a transfer. Shared storage shows every node the same content, so one proof on the parker's node settles it. Node-local storage holds a volume only on the node it was written on, and a parker can be migrated after its transfer, so the CPI proves the volume gone on every online node where that storage is enabled. A record whose volume is gone was left by a finished move that renamed the volume, as an attach leaves one when it can't remove the parker's record, or as a mover leaves one on the parker it took the disk from, so it doesn't block the disk either.

A node the CPI can't reach is left out of the scan, and the guests on it go unseen until it comes back. When a copy sits on that node, the CPI finds only the original, which is the right answer. When our disk's guest sits on that node instead, the CPI finds only the copy, and it attaches, grows, or deletes the copy's volume as though it were our disk, because a disk found on a reachable node settles the answer. The CPI keeps that rule, because refusing every disk call while a node is down would stop every deploy. The check of a parker's record uses the parker configs from the same scan, and its reads of other guests leave out the same offline nodes, so a parker or a guest on such a node doesn't fail a disk found on a reachable node. So when we clone or restore a guest whose drive lines carry a `bpd-` serial, we remove the serial from the copy at once, as step 3 below shows, and we don't wait for a refusal, which can't come while the original's node is offline.

**Fix**

We worked this procedure out from the code and haven't run it against a lab, so we read each command's output before we take the next step.

We don't delete either volume, and we don't destroy either guest, until we know which one is the copy. The copy's volume holds whatever the disk held when the copy was made, so it can look exactly like our disk.

1. We find the task that made the copy. PVE files a clone under the VMID it copied from, and a restore under the VMID it created. For each guest the refusal names, and on that guest's node, we list both kinds of task:

   ```bash
   pvesh get /nodes/<node>/tasks --vmid <N> --typefilter qmclone --limit 50
   pvesh get /nodes/<node>/tasks --vmid <N> --typefilter qmrestore --limit 50
   ```

   A `qmclone` task under `<A>` shows that someone cloned `<A>`, so the other guest is likely the copy, and the next step confirms it. A `qmrestore` task under `<B>` shows that `<B>` came from a backup, so `<B>` is the copy. The VMID a clone is filed under is the original's, so we never take a task's VMID alone as the copy's. PVE prunes old tasks, and a clone runs on the source's node, so we try every node when the first one shows nothing.

2. We check the answer against the Director. `bosh -d <deployment> instances --details` lists the instance that owns `<cid>` with its VM CID, and the guest that runs that VM is the one the Director attached the disk to. We also search the CPI's output for `<cid>`, as the gather list under [Another entry names a disk's birth volume](#another-entry-names-a-disks-birth-volume) shows. A line that records a move gives the name the disk's volume moved to as `volid_after`, and the latest one should match the volume of the guest we take to be the original.

3. When a slot carries the copy, we remove the serial from the copy's drive line and change nothing else. We read the line with `qm config <copy>`, and we set it again without the `serial=bpd-<id>` option:

   ```bash
   qm set <copy> --scsiM '<the same line without serial=bpd-<id>>'
   ```

   On a running guest, PVE keeps that change pending until the guest restarts. The CPI reads the pending value of a slot that keeps its volume, so the refusal clears as soon as `qm set` returns, and we don't need to restart the copy.

4. When two parkers keep a record, we remove the record for `bpd-<id>` from the copy's description. We read the description with `qm config <copy>`, and we set it again with only the `"bpd-<id>": {...}` entry taken out of `bosh_parked_disks`, leaving every other entry as it was:

   ```bash
   qm set <copy> --description '<the same description without the bpd-<id> entry>'
   ```

   When the refusal names one slot and one parker's record, and the slot is the copy's, step 3 is the fix. When the slot holds our disk, the parker's record is the one out of place, so we take the same entry out of the description of the parker the refusal names. When the refusal ends with the storage form, we first make the size, creation time, and `volid_after` comparison that the paragraph under that form describes, and we take the entry out only when the CPI's log names the slot's volume.

5. We rerun the deploy, or the call that failed. Once only one guest carries the serial, only one parker keeps the record, and no record names a volume other than the slot's that is still there, the call goes ahead. The copy's volume stays on the copy, and it's the deployment owner's decision whether to keep it, because the copy may still be in use for whatever it was made for.

### A detach left a disk's notes on the VM it left

**Symptom**

```text
WARN <operation>: couldn't read the VM the disk left to see which of the disk's names another disk holds, so any entries under keys_left_in_place stay in that VM's description, in bosh_attached_disks and bosh_disk_opt_overlays; see "A detach left a disk's notes on the VM it left" in docs/troubleshooting.md of bosh-proxmox-cpi-release  vmid=<N> node=<node> disk_cid=<cid> keys_left_in_place=<volid>,<volid> error=...
```

The operation is `detach_disk`. It is `attach_disk` or `create_vm` when the call first moved a disk off an unused entry of another VM.

**Diagnosis**

The call that moved the disk succeeded, and the disk is off VM `<N>`'s bus. After the move, the CPI reads VM `<N>` to see which of the disk's old names another disk now uses, so it can remove the disk's attached-disk note and drive-option overrides without touching that other disk's. When the read kept failing, the CPI removed the entries under the disk's stable ID, unless another warning says that removal failed. It left any entries under the names in `keys_left_in_place`, because another disk could be using one of those names. Those names are volume names. A later disk that lands on VM `<N>` under one of those names could pick up the drive options stored under that name, such as a cache mode or an I/O limit. Nothing removes these entries on a schedule, so they stay until we remove them, until a later detach or attach rewrites them, or until VM `<N>` is deleted.

**Fix**

We first make sure the entries aren't another disk's. We make these edits only while no BOSH operation is running against VM `<N>`, because the CPI rewrites the same description.

We check the VM's disk keys with `qm pending`, and we don't use `qm config` for this. `qm config` shows pending changes as already applied, so it hides a slot whose delete is still pending, even though the running guest still has that disk plugged in:

```bash
qm pending <N>
```

We look at every `scsiN`, `virtioN`, `sataN`, `ideN`, `efidiskN`, `tpmstateN`, and `unusedN` key. Any of those keys that `qm pending` lists, whether as a current value, a pending value, or a pending delete, belongs to a disk the VM still holds. When one of them names a volume in `keys_left_in_place`, the entries under that name belong to that disk, so we leave them. We remove the entries only under the names that no key mentions.

We then read the description, where the CPI's records sit in the `<!--BOSH:{...}-->` block at the end of the text. `pvesh` gives us the description as plain text:

```bash
pvesh get /nodes/<node>/qemu/<N>/config --output-format json | jq -r .description
```

In the JSON inside that block, we delete each unclaimed name's key from the `bosh_attached_disks` object and from the `bosh_disk_opt_overlays` object, keep everything else as it is, and write the whole description back, including any text above the block:

```bash
qm set <N> --description '<the text above the block>
<!--BOSH:{...the edited JSON...}-->'
```

We then read the description again the same way, to check that the JSON parses and that only those keys are gone.

### A disk detach stays pending on a running VM

**Symptom**

One of these comes from `detach_disk`, or from `attach_disk` when it moves a disk off a legacy `scsi0` slot:

```text
detach_disk: VM <N> on node <node> can't hot-unplug a disk while it runs, because its hotplug setting "<setting>" doesn't include disk. PVE could only record the delete of slot <slot> as pending, so we reverted it, and the disk is still attached. Add disk to the VM's hotplug setting or stop the VM, and then retry
```

```text
detach_disk: the guest on VM <N> (node <node>) still holds the disk on slot <slot>, so PVE could only record its delete as pending. We reverted that pending delete, and the disk is still attached. A retry can succeed once the guest lets go of the disk
```

`delete_vm` reports the same condition as retriable, because the VM it's destroying is on its way to stopped, and the next `delete_vm` finishes the detach. When a retried `detach_disk` or `attach_disk` finds a delete that an earlier attempt or an operator left pending, what it does depends on the VM's state. On a running VM it leaves the delete alone and fails retriably, with a message saying that PVE applies the delete at the VM's next clean stop and the transfer then finishes. On a stopped VM nothing would apply the delete until the VM starts, so the CPI applies it itself and finishes the transfer. Like a start, the CPI's write applies every other change pending on the VM as well.

A slot whose pending value names a different volume from its current drive gets its own retriable message, which says the slot carries a pending change from one volume to another. The CPI changes nothing on such a slot. PVE applies the change at the VM's next clean stop, or at its next start when the VM is already stopped, and the next attempt then goes ahead.

**Diagnosis**

On a running VM, PVE removes a disk slot by hot-unplugging it. When the VM's hotplug setting doesn't include disk, PVE can't unplug it, so it records the delete as pending and applies it when the VM stops. When the guest still holds the device, the unplug fails, and the delete is left pending in the same way. In both cases the running guest keeps the disk, while PVE's ordinary config read no longer shows the slot. Releases before 0.9.0 read only that config, so they could take the disk as detached and attach it to a parker while the guest still had it. The CPI now reads PVE's pending view as well, and when one of its own deletes stays pending it reverts that delete and reports it, so the disk stays exactly where the Director thinks it is.

**Fix**

For the hotplug message, we add `disk` to the hotplug setting the VM is created with, which is `pve.hotplug` or the VM type's `cloud_properties.hotplug`, and recreate the VM. Alternatively, we stop the VM and retry the operation, because PVE applies the delete at once on a stopped VM. For the busy message, the guest is still using the disk, usually because a filesystem on it is mounted. The Director retries on its own, and the detach finishes once the guest lets go.

### delete_disk refuses a disk whose slot delete is pending

**Symptom**

One of these:

```text
delete_disk: refusing to delete disk <cid>, because VM <N> on node <node> still names it on slot <slot>, and PVE has only recorded that slot's delete as pending. Stop the VM if it's running, or start it if it's already stopped, which applies the change before the guest boots, and then rerun the clean-up. Nothing was deleted; see "delete_disk refuses a disk whose slot delete is pending" in docs/troubleshooting.md of bosh-proxmox-cpi-release
```

```text
delete_disk: refusing to delete disk <cid>, because VM <N> on node <node> still names it on slot <slot>, and the slot carries a pending change to another volume. Stop the VM if it's running, or start it if it's already stopped, which applies the change before the guest boots, and then rerun the clean-up. Nothing was deleted; see "delete_disk refuses a disk whose slot delete is pending" in docs/troubleshooting.md of bosh-proxmox-cpi-release
```

**Diagnosis**

VM `<N>`'s current config still names the disk on slot `<slot>`, while a change pending on that slot hides it from PVE's ordinary config read. In the first message the change is a pending delete of the slot. In the second it's a pending value that puts another volume on the slot. A release before 0.9.0 could leave that behind by attaching a disk onto a slot whose delete was pending. The section above explains how a delete stays pending. A release before 0.9.0 could also leave a pending delete behind, because it took one as a finished detach, and so can a crash or an operator's `qm set --delete` on a running VM. When a crash or a kill stops the VM, the change stays pending, because only a clean stop or a start applies it. The Director believes the disk is detached, treats it as orphaned, and sends `delete_disk`. PVE deletes a volume without checking whether a guest uses it, so the CPI refuses rather than pull the volume out from under the guest. The refusal isn't retriable, because nothing `delete_disk` does applies the change.

**Fix**

We let the pending change apply. When VM `<N>` is running, we stop it at a convenient time, for example with `bosh -d <deployment> stop <instance-group>/<id>`. PVE applies the change when the VM stops, and then we start the instance again. When the VM is already stopped, we start it, for example with `bosh -d <deployment> start <instance-group>/<id>`. PVE applies the change before the guest boots, and there's no need to stop the VM again afterwards unless we want it stopped. Either way the disk leaves the VM. Then we rerun the delete, for example with `bosh -d <deployment> delete-disk <cid>`, and the next `delete_disk` deletes the disk.

We don't revert the pending delete, even though `detach_disk` reverts its own pending deletes. A revert puts the disk back on the VM, while the Director still wants it gone. The next `delete_disk` would then find an ordinary holder, which only the optional lock guard protects, and it would delete the volume from under the running VM. `detach_disk` reverts for the opposite reason, because there the Director still believes the disk is attached, and the revert keeps the disk where the Director expects it.

### A transfer resume can't prove which volume is the disk

**Symptom**

```text
update_disk: resume interrupted transfer for disk <cid>: transfer resume: disk <stable id> has an intent record on parker vmid <P> (node <node>, slot "<slot>", recorded volid "<volid>", source "<N>"), but <finding>, so the resume can't prove which volume is this disk. It moved nothing and wrote no serial; see "A transfer resume can't prove which volume is the disk" in docs/troubleshooting.md of bosh-proxmox-cpi-release
```

`detach_disk`, `attach_disk`, `resize_disk`, `snapshot_disk`, `set_disk_metadata`, and `delete_disk` give the same refusal with their own name in front, and so does `delete_vm` when it keeps a legacy ephemeral disk. For a journal-managed disk, the identity check that every disk call runs first, `has_disk` included, tries to finish the move by claiming its landing, and it gives the same refusal with the call's name in front. The error isn't retriable, because a retry reads the same state and reaches the same answer.

`detach_disk` and `attach_disk` apply a pending delete they find on a stopped VM `<N>` before they move the disk. When the check refuses after that, the error says `It applied the pending delete of <slot> on source vm <N>, but it moved nothing and wrote no serial` in place of `It moved nothing and wrote no serial`. PVE would have applied that delete at the VM's next start anyway.

When a read the check needs fails, the error is retriable, and it names the read that failed:

```text
update_disk: resume interrupted transfer for disk <cid>: transfer resume: read source vm <M> of disk <other id>'s transfer: <error>
```

The Director retries it, and the next attempt reads again. A read that fails in a way a retry can't change, such as a configuration that PVE returns malformed, keeps its permanent class. Every other refusal in this section is permanent, and when one names another disk's record, it doesn't go away by waiting. We rerun the other disk's operation, or we prove its record stale as the Fix below describes, and then we rerun this disk's operation.

**Diagnosis**

The disk's move to parker `<P>` stopped partway, and the CPI tried to finish it before it acted on the disk. A move that lands on the parker renames the volume for the parker, and the serial that ties the volume to the disk's stable ID is written in a later step. When the CPI stops between those two steps, the parker holds a volume named for the parker with no serial, and the parker's record still names the old volume `<volid>`. That old name is then free on VM `<N>`, and another disk can take it there and be left on one of `<N>`'s unused entries by its own unfinished detach. Neither the unused entry nor the landed volume carries a serial, so the CPI can no longer tell the two disks apart from VM `<N>` alone.

Before it moves a volume or writes the disk's serial onto one, the CPI checks the parker and the rest of the cluster, and `<finding>` names the check that failed:

- `parker vmid <P> also keeps an unfinished transfer record of disk <other id> naming slot "<slot>"`

  Another disk's transfer to the same parker names the slot this record names, and neither transfer has written its serial yet. A volume that landed on that slot may be either disk's. The CPI never gives two unfinished transfers the same slot, so this state comes from transfers that started on an older release.

- `<key> of parker vmid <P> holds <volume>, a volume named for the parker with no serial, while the record names slot "<slot>"`

  A volume landed on a slot other than the one this record names, and no other disk whose move is proved accounts for it. It may be this disk's landing or another disk's. When a resume finds this record's slot taken and falls back to another one, it writes the new slot into the record before it moves the volume, so this state comes from an older release's fallback or from a hand change.

- `<volume> on <key> is named for the parker, while this disk's recorded volume <volid> isn't named for its source vm <N>, so this disk's park attaches that volume under its own name and <volume> isn't this disk's (disk cid <cid>)`

  This disk's recorded volume isn't named for VM `<N>`, so VM `<N>` never owned it, and the disk's park attaches it to the parker under its recorded name by editing the parker's configuration. A volume that PVE renamed for the parker therefore can't be this disk's.

- `parker vmid <P> holds more than one volume named for it with no serial`

  Two landings are waiting on the parker, and the CPI can't tell which one is this disk.

- `<key> of parker vmid <P> has a pending change to volume <volume>`, or `<key> of source vm <N> has a pending change to volume <volid>`

  A configuration change on that key hasn't applied yet, so the key may not name the volume once it does.

- `disk keys <keys> of source vm <N> all name volume <volid>`

  More than one key of VM `<N>` names the recorded volume.

- `<key> of source vm <N> names volume <volid> with another disk's serial <serial>`

  The volume under the recorded name already belongs to another disk.

- `parker vmid <other> keeps a record of the disk's transfer as well as parker vmid <P>`

  Two parkers each keep a record of this disk, and the CPI can't tell which one received it.

- `vm <M> holds the disk's serial on <key>`, or `parker vmid <M> names volume <volume> on <key>`

  Another guest already carries this disk's serial, or another parker names the volume the CPI was about to take.

- `vm <M> names volume <volid> on <key>`

  VM `<N>` has let go of the recorded volume, but guest `<M>` still names it. Attaching the volume to the parker would give it two owners, and destroying `<M>` would then free it.

- `source vm <N> has no configuration on node <node> and the cluster finds it on node <other node>`

  VM `<N>` moved to another node, and the volume went with it. The CPI can only move a disk between guests on one node, so it doesn't treat the VM as gone.

**Whose landing it is**

A landing carries no serial, so the CPI works out whose landing it is from the parker's records, their source VMs, and storage. Each record names its disk's old volume, the VM the disk came from, and the slot its move aims at. The CPI counts a record as moved only when two things hold together. First, the source VM exists and doesn't name the old volume anywhere, whether in its current configuration, its pending changes, its unused entries, or its snapshots. Second, the old volume is either gone from storage or named by another guest, which shows that its name was reused. The CPI proves a volume gone by listing everything on the storage from the source's node, and it doesn't take an empty listing or a failed lookup of the one volume as proof. A record reads as unmoved when its source names the old volume on an unused entry, or on a slot with no serial or with that disk's own serial. Every other answer leaves the record's move unknown, and that includes a source VM that's gone from the cluster, a record that names no source VM, and an old volume that still exists while no guest names it. A record whose old volume isn't named for its source VM, or that the parker already names, never owns a volume renamed for the parker, because its park attaches the old volume under its own name.

The CPI reads each source VM once per attempt, on the node where the cluster finds it, so a source that migrated to another node is an ordinary read. A landing on the slot of a record whose move is proved belongs to that record's disk, and the CPI leaves it alone. In every other state where a landing might belong to another disk, the CPI refuses permanently with a message that ends in `(audit required)` and names both disks' CIDs and both slots, because nothing in its metadata says whose landing it is. Five states get that refusal:

- `<volume> on <key> sits on the slot disk <other id>'s unfinished record names (disk cid <other cid>), but <its status>, so it can't count as that disk's, and this disk's record (disk cid <cid>) names slot "<slot>" (audit required)`

  A volume landed on another record's slot, but that record's move isn't proved. The landing may be that disk's, or it may be this disk's, landed there by an older release's fallback.

- `disk <other id>'s unfinished record (disk cid <other cid>, slot "<other slot>") reads as moved, because <its status>, and no landing on its slot accounts for it, so <volume> on <key> may be that disk's and not this disk's (disk cid <cid>, slot "<slot>") (audit required)`

  Another disk's move is proved, but its slot holds no landing. An older release could land a disk on another free slot when its recorded slot was taken, so any landing on the parker may be that disk's. A stale record whose old volume is gone gives the same picture.

- `<volume> on <key> counts as disk <other id>'s (disk cid <other cid>, slot "<other slot>") only because its record reads as moved, since <its status>, and no serial says so, so <landing> on <slot> may be that disk's and not this disk's (disk cid <cid>, slot "<slot>") (audit required)`

  Two disks whose moves are proved each left a landing with no serial. If each landing sits on its own disk's slot, the slots give the right answer. But two fallbacks from an older release that took each other's slots leave exactly the same picture, and the CPI can't tell the two cases apart. So the CPI won't hand out the landings by slot alone, and a double crash, where two transfers to one parker both landed and both stopped before their serial writes, needs an operator as well.

- `<volume> on <slot>, the slot this record names, may be this disk's (disk cid <cid>) or disk <other id>'s (disk cid <other cid>), because <this disk's status>, while disk <other id>'s unfinished record names slot "<other slot>", which holds no landing, and <its status> (audit required)`

  A volume landed on this record's slot, but this disk's own move isn't proved, and another disk's record reads as unmoved or unknown while its slot stays empty. The landing may be this disk's, with a new disk holding this disk's freed name on VM `<N>`. It may also be the other disk's, landed here by an older release, with a new disk holding that disk's freed name on VM `<M>`. When this disk's source names its old volume only under another disk's serial, the name was reused, so the CPI counts this disk's move as proved and claims the landing.

- `disk <other id>'s unfinished record (disk cid <other cid>, slot "<other slot>") names the same source vm <N> and recorded volume <volid> as this disk's record (disk cid <cid>, slot "<slot>"), so one of the two names a volume that took the other's old name (audit required)`

  Two unfinished records name the same old volume on the same VM. That happens only when one disk's move freed the name, a new disk took it, and the new disk's own transfer started. The CPI gives this refusal whether or not a landing sits on the parker.

The `<its status>` and `<this disk's status>` parts say what the CPI read for that record:

| Status | What it means |
|--------|---------------|
| `its source vm <M> names its recorded volume <volid> nowhere, and storage <storage> no longer holds it` | Moved |
| `its source vm <M> names its recorded volume <volid> nowhere, and vm <V> names it on <key> now` | Moved |
| `its source vm <M> still names its recorded volume <volid> on <keys>` | Unmoved |
| `its source vm <M> names its recorded volume <volid> on <keys> under another disk's serial <serial>` | Unknown |
| `snapshot "<name>" of its source vm <M> names its recorded volume <volid> on <key>` | Unknown |
| `its source vm <M> is gone from the cluster` | Unknown |
| `its record names no source vm and volume` | Unknown |
| `its recorded volume <volid> names no storage` | Unknown |
| `its recorded volume <volid> still exists on storage <storage> and no guest names it` | Unknown, because the transfer may be released and not yet attached, or the record may be stale |
| `vm <V> carries its disk's serial on <key>, which shows its record is stale` | Unknown, because that disk's transfer finished on VM `<V>`, and its record outlived it |
| `its recorded volume <volid> isn't named for its source vm <M>, so its park attaches it under that name` | Never the owner of a volume renamed for the parker |
| `parker vmid <P> names its recorded volume <volid>` | Never the owner of a volume renamed for the parker |

When VM `<N>` or the parker changes between the check and the move, the move doesn't go out. The operation then fails with a retriable error that names the digest or the volume that changed, and the next attempt checks again.

**What the check doesn't cover**

Two states still pass the check, and both follow a hand change while the parker holds no landing and no guest carries the disk's serial. The record's old name is then all the CPI has to go on. In the first, someone removed the disk's unused entry from VM `<N>` by hand, and PVE freed the disk's volume. In the second, someone destroyed VM `<N>` by hand, and PVE later gave its VM ID to a new VM. Either way, another disk that later takes the same name on a VM with that ID passes the check, and the CPI moves that disk onto the parker and writes this disk's serial onto it. Closing these gaps takes a size or content check that the record would have to carry, and the record doesn't carry one today.

A stale record no longer slips through the check. Suppose an unpark wrote a disk's serial onto a guest and then failed before it removed that disk's record. The record's move reads as proved, so the check could once set aside a landing on the record's slot as belonging to that disk. Now the CPI reads such a record as unknown while any guest carries its disk's serial, and a landing on the record's slot gets the refusal described above, which says that the landing sits on a slot named by disk `<other id>`'s unfinished record. When the guest that carries the serial is the parker itself, another call finished that disk's transfer after the CPI read the parker, so the call gets a retriable error and the next one settles it.

The CPI doesn't settle any of the five `(audit required)` states listed above on its own, so each of them needs an operator.

So while a parker keeps a transfer record for a disk, we don't remove that disk's unused entry from its source VM by hand, and we don't destroy that VM by hand. The record sits in the parker's description under `bosh_parked_disks`, keyed by the stable ID, and the transfer is unfinished while no slot on any guest carries that stable ID as its serial. Removing the entry is right only for the delete described in "delete_disk refuses a disk stranded on an unused entry" above, where no parker keeps a record any more.

**Fix**

We leave the parker, VM `<N>`, and every unused entry as they are, and we don't destroy either VM. Then we read the state the refusal names:

```bash
qm config <P>
qm pending <P>
qm config <N>
qm pending <N>
pvesm list <storage> --vmid <P>
pvesm list <storage> --vmid <N>
```

When the finding is a pending change, we let the change apply and then rerun the operation. PVE applies it at the VM's next clean stop, or at its next start when the VM is already stopped, as "delete_disk refuses a disk whose slot delete is pending" above describes. The check runs again on the applied state.

Every other finding about a landing means we have to work out whose landing each volume is. The records and their sources tell us which disks a landing could belong to, and the volume's contents decide. We never decide from the records alone, and never from sizes, because the disks of one instance group usually share a size. Every record in parker `<P>`'s description sits under `bosh_parked_disks`, keyed by a stable ID. A record names its disk's old volume as `volid`, the VM the disk came from as `source_vm_cid`, and the slot its move aims at as `slot`:

```bash
pvesh get /nodes/<node>/qemu/<P>/config --output-format json | jq -r .description
```

A record's transfer is unfinished while no key of `<P>` carries its stable ID as `serial=`. For each unfinished record, we find the node its source VM is on, and then we read the VM and its snapshots there:

```bash
pvesh get /cluster/resources --type vm --output-format json | jq -r '.[] | select(.vmid == <source_vm_cid>) | .node'
qm config <source_vm_cid>
qm pending <source_vm_cid>
qm listsnapshot <source_vm_cid>
qm config <source_vm_cid> --snapshot <snapshot name>
```

We run the `qm` commands on the node the first command prints, and we read every snapshot `qm listsnapshot` shows. Then we check whether the record's `volid` is still on its storage. We list the whole storage on that node, because a lookup of the one volume can fail on file storage even while the volume is missing. We also check that the listing isn't empty, because a storage that didn't answer can list nothing at all:

```bash
pvesm list <storage> > content.txt
wc -l content.txt
awk -v volid='<volid>' '$1 == volid' content.txt
```

We sort each record by the rule the CPI uses. It has moved when its source exists and names the `volid` nowhere in its configuration, its pending changes, its unused entries, or its snapshots, and the `volid` is either missing from the full listing or named by another guest. It hasn't moved when its source names the `volid` on an unused entry, or on a slot with no serial or with the record's own stable ID. When the `volid` still exists and no guest names it, the transfer was released and hasn't attached yet, or the record is stale, and the paragraphs below tell those apart. Anything else leaves the record open, and its disk stays a candidate for every landing.

That sorting narrows the candidates, but it never settles a landing on its own. Even when exactly one record has moved and the parker holds exactly one landing, a stale or released record can look like the moved one, and a serial written from the records alone would give one disk's identity to another disk's data. So we find which instance each disk CID belongs to with `bosh -d <deployment> instances --details`, and then we inspect each landing and each volume the sources name, read-only on its node, without attaching any of them to a guest. On file or block storage we map a volume like this:

```bash
pvesm path <volume>
losetup --find --show --read-only --partscan <path>
mount -o ro,noload /dev/<loop device>p1 /mnt/inspect
```

On Ceph RBD storage, `pvesm path` doesn't give a path `losetup` can use, so we map the image read-only with `rbd map --read-only <pool>/<image>` and mount the partition of the device it prints with `-o ro,noload`. On any other network storage we use that storage's own read-only mapping, and we never map a volume read-write while we inspect it.

The persistent disk's data sits under the instance's job directories, which tell us which instance group the volume belongs to. We unmount the volume and remove the mapping, with `losetup --detach /dev/<loop device>` or `rbd unmap /dev/<rbd device>`, before we look at the next one. Then we settle one landing at a time. When the contents tie a landing to one record's disk, we write that disk's serial onto it. If the landing sits on a bus slot, such as `scsi4`, we write the serial where it sits. If it sits on one of the parker's unused entries, we attach it with the serial to the slot the record names when that slot is free, or to another free bus slot when it isn't. Attaching it this way also drops the unused entry:

```bash
qm set <P> --<slot> <volume>,serial=<that record's stable id>
```

We never put a serial on an unused entry, because PVE keeps no options there. Then we rerun that disk's operation, and the CPI finds the serial on the parker and finishes the transfer from it. We read the parker and every source again before we settle the next landing, and each record we haven't settled resumes from its own source on its next operation. A volume whose contents tie it to no record's disk, such as a new disk's, stays where it is. Until the contents tie a landing to one disk, every operation it blocks stays refused, and waiting loses nothing.

A record can be stale. The CPI removes a record when it unparks a disk, but a failed read or write there leaves the record behind, on this parker or on another one. A stale record looks like an unfinished transfer, and it can name the slot this disk's record names. The record of disk `<other id>` is stale when its stable ID is the serial on a slot of a guest other than the parker, because that disk's transfer finished there. We search every guest in the cluster for it:

```bash
pvesh get /cluster/resources --type vm --output-format json | jq -r '.[] | select(.type == "qemu") | "\(.node) \(.vmid)"' | while read -r node vmid; do pvesh get /nodes/$node/qemu/$vmid/config --output-format json | grep -q 'serial=<other id>' && echo "$node $vmid"; done
```

The record on parker `<P>` is also stale when four things hold together. First, no key of `<P>` carries `serial=<other id>`. Second, the record's source is gone or names its `volid` nowhere. Third, the full listing of its storage doesn't show the `volid`. Fourth, the contents tie every landing on the parker to another disk. When the `volid` still exists and no guest names it, the record isn't stale. Its transfer was released and hasn't attached yet, and removing the record would leave the volume with no disk CID that resolves to it. We leave that record in place, settle the landings on the parker first, and then rerun that disk's operation, which attaches the volume under its recorded name. If none of these proofs holds, the record isn't stale, and we leave it in place.

When the finding names a second parker's record, that record may be stale for the same reason. Before we touch it, we read parker `<other>`:

```bash
qm config <other>
qm pending <other>
```

The record is stale only when no key of `<other>` names a `vm-<other>-disk-*` volume without a `serial=bpd-` option, and no key carries `serial=<stable id>`. If either shows up, the record isn't stale, and we leave it in place.

No command-line tool edits parker records, so we remove a stale record by editing the description of the parker `<Q>` that keeps it. We make the edit while no deploy touches that parker, for example while `bosh tasks` shows no running task, because the CPI rewrites the same description when it parks, unparks, or transfers a disk there, and the later write undoes the earlier one. We save the description first:

```bash
pvesh get /nodes/<Q node>/qemu/<Q>/config --output-format json | jq -r .description > description.txt
```

In `description.txt` we delete the `"<stale id>": {...}` entry from the `bosh_parked_disks` object inside the `<!--BOSH:...-->` block, together with the comma that joins it to a neighboring entry. We keep every other entry and all the text outside the block exactly as it was, and we check that the block still holds valid JSON. Then we write it back, read it back, and rerun the operation:

```bash
qm set <Q> --description "$(cat description.txt)"
pvesh get /nodes/<Q node>/qemu/<Q>/config --output-format json | jq -r .description
```

A landing on one of the parker's unused entries whose contents tie it to no transfer's disk may be what's left of a `delete_disk` that stopped after it took a parked disk off its slot and before PVE freed the volume. That delete leaves the disk's record in place, and the record's slot on `<P>` is empty. We list the disks the Director is deleting with `bosh disks --orphaned` and compare their disk CIDs with the `disk_cid` of each record whose slot on `<P>` is empty. When one matches, we finish that delete first by rerunning the clean-up, for example with `bosh clean-up --all`, and then we rerun the operation.

When the finding names another guest that holds the serial or names the volume, or another parker that names the volume, the disk may already be somewhere else. We find it with `bosh -d <deployment> instances --details` and the guest's own configuration before we change anything, and we leave every record in place.

When the finding says VM `<N>` has no configuration on the parker's node and the cluster finds it on another node, we migrate nothing yet, because PVE reuses free VM IDs, and the guest it found may have nothing to do with this disk. We read that guest on the node the finding names:

```bash
pvesh get /nodes/<other node>/qemu/<N>/config --output-format json
```

When its name and description show that it's the VM this disk came from, and one of its keys still names `<volid>`, the VM moved with the disk. We then migrate it back to the parker's node and rerun the operation. When it's another VM, or none of its keys names `<volid>`, the disk's source is gone. The disk's old volume may then have landed on the parker, gone with the destroyed VM, or survived on its storage because VM `<N>` never owned it. We list the storage as above. When the `volid` is still there and no guest names it, we leave the record in place, settle any landing on the parker, and rerun the operation, which attaches the volume under its recorded name. Otherwise we work out whose landing each volume is from its contents, as above. When no landing on the parker is this disk's and no guest carries its serial, the record is stale, and we remove it as above.

## Network, bridge, and SDN failures

### Bridge not found

**Symptom**

```text
create_vm: configure NICs vmid=N: <error>
```

where the PVE error body references an unknown or missing bridge.

**Diagnosis**

```bash
ip link show vmbr0
pvesh get /nodes/<node>/network | jq '.[] | select(.type == "bridge")'
```

**Fix**

`pve.network_bridge` must match an existing Linux bridge on the PVE node. The bridge must be UP. Create it in the PVE web UI under Node → Network if absent. See [Configuration](configuration.md).

### vnet bridge missing on some nodes / SDN state stuck pending

**Symptom**

`create_vm` fails with a retriable error naming a bridge and a node:

```text
create_vm: resolve bridge "boshvnet" on node "pve03": SDN bridge "boshvnet" is not yet present
on node "pve03" within the retry/timeout budget
```

or `create_network` fails with:

```text
create_network: SDN vnet "boshvnet" has not converged into running config within the retry/timeout budget
```

The Director retries and the error persists past the retry/timeout budget (default 30 retries at ~1 s each) rather than clearing after a few seconds.

**Diagnosis**

SDN state propagates from the node that ran `UpdateSdn` to every other cluster node over inter-node SSH (root-to-root, keyed by the pmxcfs cluster trust). If that SSH path is broken to one node, the commit succeeds locally but never reaches that node — `create_network` reports success while the vnet stays permanently unrealized there, and every `create_vm` that lands a NIC on it fails this gate. This is node-trust breakage, not a CPI or PVE API fault; the CPI's own polling cannot fix it, only surface it.

On the node named in the error:

```bash
ip -d link show boshvnet
```

Absent or down means the interface was never realized on this node. Compare against a node where it works.

```bash
pvesh get /cluster/sdn | jq '.[] | select(.pending == true)'
```

A non-empty result naming the same vnet/zone confirms the SDN configuration is stuck in the pending (not-yet-applied) state cluster-wide or on specific nodes.

Verify root SSH trust between the node that committed the change and the node where the bridge is missing:

```bash
ssh root@<other-node> hostname
```

An interactive password prompt, a host-key mismatch, or a connection refusal — rather than an immediate, unattended success — is the signature of broken cluster SSH trust.

**Fix**

Repair root SSH trust between the affected nodes (regenerate/re-distribute `/etc/pve/priv/authorized_keys` via the PVE cluster join process, or manually reinstate the missing key), then re-apply the pending SDN configuration:

```bash
pvesh set /cluster/sdn
```

Once the vnet is confirmed present (`ip -d link show <vnet>` on every node, `pending` clears in `pvesh get /cluster/sdn`), retry the failed `create_vm`/`create_network` call — the Director re-drives automatically since the error is retriable, or trigger a fresh deploy attempt.

If this happens repeatedly across a cluster, treat it as a standing infrastructure issue (node join/rejoin, certificate rotation, or a firewall change breaking inter-node SSH) rather than a one-off — the CPI's `pve.network_resolve_retries` gate (see [Configuration — SDN network management](configuration.md#sdn-network-management)) only bounds how long a *transient* propagation delay is tolerated before failing loudly; it cannot resolve a persistently broken trust relationship.

### SDN zone or vnet not found

**Symptom**

PVE returns 404 or "Zone not found" when the CPI attempts to create a network, or the CPI's own error says the zone does not exist.

**Diagnosis**

```bash
pvesh get /cluster/sdn/zones
pvesh get /cluster/sdn/vnets
```

**Fix**

With defaults the CPI creates zones itself (the turnkey vxlan zone `bosh` when no zone is named), so this error means one of three things:

1. Zone auto-management was disabled (`pve.sdn_auto_manage_zone: false`) and the zone named by `pve.sdn_zone` or `cloud_properties.zone` was never created. Create it in the PVE web UI, or re-enable auto-management.

2. `sdn_zone_type: evpn` is in use and the EVPN zone is missing. The CPI never creates EVPN zones — the operator creates the zone and its controller (BGP peering, route reflectors) in PVE first; the CPI then manages only vnets and subnets inside it.

3. SDN itself is unavailable: `libpve-network-perl` is not installed on every cluster node, or the token lacks `SDN.Allocate` on `/sdn` — required by default now that SDN is the default network mode.

See [Configuration — SDN network management](configuration.md#sdn-network-management) and [create_network](cpi_methods.md#create_network).

### Small packets pass, large packets hang (SDN MTU)

**Symptom**

SSH connects and pings succeed, but bulk transfers stall: `bosh ssh` works while `bosh scp` hangs, agents connect to NATS but blobstore downloads time out, HTTP requests hang after the headers. Cross-node only; same-node VM pairs are unaffected.

This is the PMTUD-blackhole signature of an MTU mismatch on a VXLAN overlay: small frames fit inside the encapsulated path, large frames are dropped silently instead of returning "fragmentation needed".

**Diagnosis**

From a guest, probe the path MTU with the don't-fragment bit. On a healthy 1500-byte underlay the overlay MTU is 1450, so the largest passing ICMP payload is 1422 (1450 − 28):

```bash
ping -M do -s 1422 <peer-vm-ip>   # must pass
ping -M do -s 1472 <peer-vm-ip>   # must fail cleanly with "message too long"
```

A hang (not a clean failure) on the second probe, or a failure on the first, confirms the blackhole. Then compare bridge and guest MTUs on both nodes:

```bash
ip -d link show <vnet>            # on each PVE node — expect 1450
ip link show eth0                 # in each guest — must match the vnet MTU
```

**Fix**

VXLAN encapsulation spends roughly 50 bytes per frame, so the overlay MTU must be the smallest underlay MTU minus that tax — everywhere. The usual causes:

1. Mixed underlay MTUs across nodes: one node's physical path runs 1500 while another runs 9000, or a switch in between clamps lower. The overlay must fit the smallest underlay on every node-to-node path.

2. A manual MTU override: `pve.sdn_zone_mtu` (or a hand-edited zone) set higher than the underlay affords. Unset it and let PVE derive the value, or set it to smallest-underlay − 50.

3. A guest that does not inherit the bridge MTU: the CPI sets `mtu=1` on virtio NICs so guests inherit automatically, but non-virtio NIC models and hand-configured guests keep 1500. Use virtio NICs or set the guest interface MTU to match the vnet. `create_vm` logs a Warn at create time naming the NIC, model, and vnet whenever `cloud_properties.network_model` (or a `network_defaults`/`disk_type`/`vm_type` profile) overrides the virtio default on an SDN vnet — check the create_vm log for `non-virtio NIC model on an SDN vnet` if this is a newly deployed VM rather than a pre-existing one.

4. A middlebox dropping fragments or ICMP "fragmentation needed" between nodes, which turns a recoverable mismatch into a silent blackhole.

See [Networks — VXLAN overlay defaults](networks.md#vxlan-overlay-defaults-peers-vnis-and-mtu) for the MTU model and [Operations — SDN VXLAN operations](operations.md#sdn-vxlan-operations) for the verification commands.

### Cross-node VM traffic dead, same-node fine (VXLAN)

**Symptom**

Two VMs on the same PVE node communicate normally over a CPI-created vnet; the same pair split across nodes cannot pass any traffic at all — not even ARP or small pings (which distinguishes this from the MTU entry above, where small packets pass).

**Diagnosis**

VXLAN tunnels run node-to-node over UDP 4789. Verify the port is open between all cluster nodes (or all addresses listed in `pve.sdn_vxlan_peers`):

```bash
# On a PVE node — is anything arriving from the peer?
tcpdump -ni <underlay-iface> udp port 4789

# Are the peers configured on the zone?
pvesh get /cluster/sdn/zones --pending 1
```

**Fix**

Open UDP 4789 between all cluster nodes in every firewall on the path (PVE host firewall, switch ACLs, external firewalls). If the zone's peer list is stale — for example a node was offline when the CPI created the zone — set `pve.sdn_vxlan_peers` explicitly and recreate the network, or edit the zone's peer list in PVE and re-apply. See [Operations — SDN VXLAN operations](operations.md#sdn-vxlan-operations).

### VM on a VLAN vnet has no connectivity

**Symptom**

A VM attached to a VLAN vnet (`bridge: vlan59`-style, zone type `vlan`) cannot reach its gateway or peers on other nodes. Same-node VM-to-VM traffic on the vnet may still work — the tag never has to leave the node in that case.

**Diagnosis**

Work outward from the vnet, in this order — the CPI configures only the first item; the rest is fabric:

```bash
# 1. The vnet exists, carries the expected VLAN ID, and is applied (not pending).
pvesh get /cluster/sdn/vnets --pending 1

# 2. The zone's underlay bridge is VLAN-aware on the VM's node.
grep -A3 "iface vmbr0" /etc/network/interfaces
# expect: bridge-vlan-aware yes, and bridge-vids covering the VLAN ID

# 3. Tagged frames reach the physical uplink (run while pinging from the VM).
tcpdump -ni <underlay-uplink> -e vlan <id>

# 4. The switch port trunks the VLAN (check the switch config, not the node).
```

Also confirm the VLAN ID matches the fabric's plan — a vnet tagged with an auto-allocated ID (band 2000–2999) only works if the fabric trunks that range; explicit `cloud_properties.vnet_tag` values from the network team's VLAN plan are the recommended path.

**Fix**

Fix the first failing layer: apply pending SDN changes (`pvesh set /cluster/sdn`), set `bridge-vlan-aware yes` plus a covering `bridge-vids` on the underlay bridge of every node and reload networking, or trunk the VLAN on the switch ports. If the tag itself is wrong, recreate the network with the correct `vnet_tag`. See [Operations — SDN VLAN operations](operations.md#sdn-vlan-operations) for the pre-flight checklist.

## Memory pressure and ballooning

### VM memory shrinks below manifest size or jobs OOM unexpectedly

**Symptom**

`free` inside a BOSH VM reports less memory than the manifest's `memory` value, or jobs sized to the manifest OOM under host memory pressure while `qm config` shows the expected memory.

**Diagnosis**

Check whether ballooning is active on the VM:

```bash
qm config <vmid> | grep '^balloon'
# balloon: 0        → device disabled (the CPI default); ballooning is not the cause
# balloon: <MiB>    → auto-ballooning enabled with that floor
# (no balloon line) → PVE default: device enabled, target = memory
```

The CPI writes `balloon: 0` by default, so a non-zero or absent value means some layer set `pve.balloon`, `cloud_properties.balloon`, or the `pve-default` sentinel.

**Fix**

Remove the enabling value (or set `pve.balloon: "0"` explicitly) and recreate the affected VMs — the setting applies on VM creation, not in place. If ballooning is intentional, size job memory expectations to the balloon floor rather than the manifest `memory` value. See [Configuration](configuration.md) for the full `pve.balloon` reference.

## Live migration failures

### Migration fails or guest crashes after migration (cpu_type host)

**Symptom**

Live migration of a BOSH-created VM aborts with a CPU-feature error, or the guest kernel panics or applications SIGILL shortly after landing on the target node.

**Diagnosis**

The CPI defaults `pve.cpu_type` to `host`, which exposes the source node's exact physical CPU feature set to the guest. Migrating such a guest to a node with a different CPU generation hands it a CPU whose features no longer match what the kernel probed at boot. Confirm the VM's model and compare the two nodes:

```bash
qm config <vmid> | grep '^cpu'
# On each node:
lscpu | head -20
```

`cpu: host` combined with differing CPU models on source and target confirms the cause.

**Fix**

On clusters that mix CPU generations and rely on live migration, set a portable named model and recreate the affected VMs:

```yaml
properties:
  pve:
    cpu_type: x86-64-v2-AES
```

Per instance group, `cloud_properties.cpu_type` overrides the global value. The change applies on VM recreation (`bosh deploy --recreate`), not in place. For hardware older than the `x86-64-v2-AES` baseline, use the cluster's lowest-common-denominator named model. See [Configuration](configuration.md) for the full `pve.cpu_type` reference.

## Agent never comes up

**Symptom**

`bosh vms` shows `unresponsive agent` or the deploy hangs on `Waiting for the agent on VM '<vmid>'`.

**Diagnosis — ConfigDrive ISO missing**

The agent gets its BOSH settings from the ConfigDrive ISO on `scsi30`. If it is absent, the agent starts but has no config and never reports.

```bash
qm config <vmid> | grep scsi30
pvesm list local --content iso | grep vm-<vmid>-config.iso
```

If the ISO is missing, see the [Operations Runbook](operations.md) for ISO verification and recovery steps.

**Diagnosis — pve_host not reachable from Director VM**

`pve.host` is written into the rendered `cpi.json` on the Director. The Director's own CPI calls dial that address from inside the Director VM, not from your workstation.

Use the PVE node's LAN IP (e.g., `192.168.1.180`), not a Tailscale or VPN hostname. See [Deploying a Director](bosh-create-env.md#pve_host-must-be-reachable-from-the-director-vm).

**Diagnosis — NATS / mbus unreachable from new VM**

The new VM must be able to connect back to the Director's mbus address over port 6868. Verify the Director address in the rendered config:

```bash
sudo cat /var/vcap/jobs/pve_cpi/config/cpi.json | jq '.agent_mbus'
```

Confirm the bridge, gateway, and DNS in `vars.yml` place the VM on a subnet that can reach the Director IP. See [Deploying a Director](bosh-create-env.md) and [ConfigDrive](configdrive.md).

**Diagnosis — TCP keepalive idle drops**

If the agent was reachable but disconnects after an idle period, TCP keepalive settings on the PVE node may be too aggressive. Apply these sysctl values:

```bash
sysctl -w net.ipv4.tcp_keepalive_time=60
sysctl -w net.ipv4.tcp_keepalive_intvl=15
sysctl -w net.ipv4.tcp_keepalive_probes=4
```

See [PVE Host Tuning](pve-host-tuning.md) for persistent configuration.

**Diagnosis — duplicate IP on a shared LAN (ARP ambiguity)**

This is the root cause of the most confusing variant: an agent that connects, runs fine for ~15 seconds, then drops with `connection reset by peer`, reconnects, and repeats — while every resource, NATS, firewall, and the agent process itself measure healthy. It surfaces during large deploys (cf-deployment) as random instances failing `Timed out sending 'get_state'` even though nothing is wrong inside the VM.

The trigger is a second device on the same L2 segment answering ARP for an address BOSH assigned to a VM. When the deployment subnet overlaps a physical office/lab LAN (e.g. CF VMs placed directly on `192.168.1.0/24`), an address handed to a VM can collide with a printer, laptop, or appliance already using it. Two MACs then answer `who-has`, the Director's ARP cache flaps between them, and mbus packets are periodically delivered to the wrong host, which RSTs them. An idle connection (mbus between RPCs) drifts onto the wrong MAC on the next ARP refresh — hence the ~15 s reachable-then-break cadence that mimics a keepalive problem.

Confirm it from the PVE node by watching who replies to ARP while sweeping the range:

```bash
# Terminal 1: capture ARP replies on the deployment bridge.
tcpdump -i vmbr1 -nn -l arp

# Terminal 2: provoke a reply from every address in the band.
for o in $(seq 20 60); do ping -c1 -W1 192.168.1.$o >/dev/null 2>&1 & done; wait
```

In the capture, any IP that shows two distinct `is-at <mac>` answers is a duplicate. The genuine VM is the virtio NIC (its ARP frames are 28 bytes); a physical device's frame is padded to length 46. Each conflicting IP maps to exactly the instance that keeps flapping; non-conflicting IPs stay stable. Cross-check the VM MACs with `qm config <vmid> | grep -i net`.

**Fix**

Stop sharing an L2 segment with unmanaged devices. Put the Director and the deployment on a dedicated, isolated network the CPI controls, so BOSH owns the entire address range and no foreign device can claim an address. This repo ships a turnkey isolated network on a private `172.x` range backed by a PVE SDN vnet (`cpitest0` by default), created by `./scripts/bosh net-up` and selected with `BOSH_PVE_ENV=cpitest`. The vnet name, zone, CIDR, gateway, reserved bands, and static IPs are all operator-configurable. See [Isolated test network (SDN)](networks.md#isolated-test-network-sdn) for the full procedure.

If an isolated network is not an option, reserve every colliding address in the deployment's cloud-config `reserved:` list so BOSH never assigns it — but re-scan whenever the physical LAN changes, since any new device can introduce a fresh collision.

## Storage lock contention and transient transport faults

CPI retry logic handles these two failure classes and usually absorbs them without operator action.

### Storage lock timeout

**Symptom**

```text
can't lock file '/var/lock/pve-manager/pve-storage-<name>' - got timeout
```

or

```text
command '/sbin/lvs ...' failed: got timeout
```

**Behavior**

The CPI retries up to 10 times with exponential backoff starting at 2 seconds, capped at 30 seconds, with ±30% jitter. If all 10 attempts fail, the operation surfaces as a retriable error: from `create_vm` it arrives as `Bosh::Clouds::VMCreationFailed` and the director retries the create itself; from other methods it arrives as `Bosh::Clouds::CloudError` with `ok_to_retry: true`, marking the fault transient for the operator.

**Operator action needed only if**

The CPI log shows `attempt=N` with N approaching 10 routinely. This indicates persistent lock contention rather than a transient burst.

**Fix**

Lower BOSH director concurrency (`director.workers` and `max_in_flight`), split `stemcell_storage` and `vm_storage` onto separate PVE storage pools, or increase `pvedaemon` `MAX_WORKERS`. See [PVE Storage Locking](pve-storage-locking.md), [PVE Host Tuning](pve-host-tuning.md).

### Transient transport fault (HTTP 596 / auth-ticket EOF)

**Symptom**

```text
API request failed: HTTP 596
```

or

```text
auto-login failed: failed to parse login response: EOF
```

**Behavior**

Both errors indicate a `pvedaemon` worker was recycled mid-request. The CPI retries up to 8 times with exponential backoff starting at 1 second, capped at 15 seconds, with ±30% jitter. Burst deploys with the default `MAX_WORKERS=3` commonly trigger this.

**Fix**

If these appear frequently, raise `MAX_WORKERS` in `/etc/default/pvedaemon` and `/etc/default/pveproxy`. See [PVE Transient Transport](pve-transient-transport.md) and [PVE Host Tuning](pve-host-tuning.md).

## Cluster not quorate

**Symptom**

Every GET-style read (`qm status`, `qm config`, `pvesh get ...`) keeps working normally, but every mutating call — `create_vm`, `create_disk`, `attach_disk`, `resize_disk`, `delete_vm`, and any other operation that writes to a VM or storage config — fails with a 5xx error containing one of two phrases:

```text
error writing config, cfs-lock failed - not quorate
```

or

```text
no quorum
```

**Cause**

PVE's cluster filesystem (`/etc/pve`, backed by pmxcfs) requires a quorate corosync cluster — a strict majority of configured nodes reachable and agreeing on cluster state — before it will accept writes. When quorum is lost, `/etc/pve` becomes read-only cluster-wide. Two causes account for nearly all occurrences:

- **Node loss below majority** — enough nodes are powered off, rebooting, or network-partitioned that the surviving set no longer has more than half the configured votes. A 3-node cluster loses quorum the moment 2 nodes are unreachable; a 5-node cluster tolerates 2 node losses but not 3.

- **Corosync network trouble** — the corosync ring network (a separate link from the PVE management network on well-configured clusters, but sometimes shared) is dropping packets, partitioned, or saturated, so nodes that are otherwise up cannot agree on membership.

**Diagnosis**

Run on any reachable node — quorum status is cluster-wide, so any node's view is representative:

```bash
pvecm status
```

Look at the `Quorate` line (`Yes`/`No`) and the `Votequorum information` block: `Expected votes`, `Highest expected`, and `Total votes` show how many nodes the cluster currently sees versus how many it needs. `pvecm nodes` lists each node's corosync membership state individually, which helps distinguish "one node is down" from "the ring network is flapping and nodes are dropping in and out."

**Behavior**

The CPI classifies this condition as retriable and injects an operator hint into the error message (`` cluster has lost quorum; mutations are blocked until quorum returns — check `pvecm status` ``) so the raw 5xx is not left anonymous in task output. Because quorum loss is a minutes-scale condition — waiting for a node to reboot or a network partition to heal takes far longer than a worker-pool hiccup — the CPI retries it on the storage-lock backoff curve (2 seconds → 30 seconds, 10 attempts, ±30% jitter) rather than the shorter transient-transport curve (1 second → 15 seconds, 8 attempts) used for ordinary 5xx errors. If quorum returns within that window, the retry succeeds transparently and the BOSH task shows no visible interruption beyond the added latency; if quorum does not return in time, the error escalates to the task log as a retriable failure (`Bosh::Clouds::VMCreationFailed` from `create_vm`, `Bosh::Clouds::CloudError` with `ok_to_retry: true` elsewhere) and recovery happens on the next deploy or task retry.

**Fix**

Restore quorum: bring the missing node(s) back online, or resolve the corosync network partition. No CPI-side action is needed or possible — the CPI cannot restore cluster membership; it can only wait for `/etc/pve` to become writable again. Once `pvecm status` reports `Quorate: Yes`, mutating operations resume automatically on the next CPI retry.

## Task timeouts

**Symptom**

```text
AwaitTask <upid>: context cancelled
```

or a PVE task that does not complete within the CPI's deadline.

**Behavior**

The inner per-call PVE task poll uses fixed deadlines: 300 seconds for standard operations and 600 seconds for stemcell and VM disk import. These inner deadlines are not operator-configurable. The outer per-method deadline envelope is configurable via `pve.operation_timeout.*`. When `pve.operation_timeout.enabled` is `true`, each CPI method runs under a context deadline sized by its class (`create_sec`, `delete_sec`, `query_sec`, or `default_sec`). See [Operation Timeouts](configuration.md#operation-timeouts) for the full property reference.

**Diagnosis**

Inspect running PVE tasks to see whether the underlying operation is still in progress:

```bash
pvesh get /nodes/<node>/tasks | jq '.[] | select(.status == "running")'
pvesh get /nodes/<node>/tasks/<upid>/log
```

For slow storage, check I/O utilization:

```bash
iostat -xz 2 5
```

**Fix**

If the storage is consistently slow, move `stemcell_storage` to faster media (local SSD). For chronically undersized storage I/O, see [PVE Host Tuning](pve-host-tuning.md). For detailed task inspection commands, see the [Operations Runbook](operations.md).

## Configuration knobs affecting failure behavior

These properties control how the CPI behaves when it encounters the failure classes above. Storage-lock retries (10 attempts) and transient-transport retries (8 attempts) are built into the SDK and are not operator-configurable. The SDK client timeout (30 minutes) is also fixed.

| Property | Default | Effect |
|---|---|---|
| `pve.reboot_timeout` | `60s` | How long the CPI waits for a soft-reboot to complete |
| `pve.reboot_mode` | `soft` | `soft` uses ACPI shutdown; `hard` forces power off |
| `pve.allow_disk_ops_with_snapshots` | `false` | When `true`, bypasses the snapshot guard on attach, detach, and resize |
| `pve.require_snapshot_check_pass` | `false` | When `false`, the CPI proceeds (with a warning) if the snapshot-check API call fails; when `true`, any API failure on the snapshot check is a hard error |
| `pve.vmid_range_start` | `100` | Lower bound of the VMID range the CPI allocates from |
| `pve.vmid_range_end` | `8999` | Upper bound of the VMID range the CPI allocates from |
| `pve.log_level` | `info` | Log verbosity: `debug`, `info`, `warn`, or `error` |

## Interpreting retry log lines

Use these patterns to distinguish normal retry noise from actionable failures.

| Log pattern | Meaning | Action |
|---|---|---|
| `pve: storage lock timeout, retrying op=<op> attempt=N max_attempts=10` | Storage lock contention, CPI is retrying | Watch trend; if `attempt` > 5 routinely, split storages or throttle deploys |
| `pve: transient transport fault, retrying` | `pvedaemon` worker recycled mid-request | If frequent, raise `MAX_WORKERS`; see [PVE Host Tuning](pve-host-tuning.md) |
| `"type":"Bosh::Clouds::VMCreationFailed","ok_to_retry":true` | Transient `create_vm` failure, director retries the create | No action unless retries routinely exhaust |
| `"type":"Bosh::Clouds::CloudError","ok_to_retry":true` | Transient fault from a method the director does not retry | Retry the deploy; investigate if frequent |
| `"type":"Bosh::Clouds::CloudError","ok_to_retry":false` | Terminal failure, operator action required | Read the `message` field and consult the relevant section above |

## Multi-storage allocation requires reconciliation

Use the allocation UUID in the error to inspect the retained journal and the actual PVE resources. A missing task response does not establish that the mutation failed, so preserve the record, the VM marker, the disk provenance, and the historical backing while we reconcile the outcome.

A changed caller request conflicts with the active VM generation. A changed global strategy on its own does not create a replacement generation, though a more restrictive boundary can block the mutations that remain. Operating on an existing disk CID should never require restoring an unrelated set that is unavailable.

If an audit reports incomplete visibility, check the propagated `VM.Audit` and `Datastore.Audit` privileges, the image-access grants, and the ACL inventory access described in [PVE API permissions](pve-api-permissions.md#multi-storage-audit-visibility). When PVE has filtered a listing by permission, that listing cannot prove that historical resources are absent.

### Choose the command for the record

We choose the command by what the record still owns, and `storage-journal audit --summary` shows us that. It prints a `record:` line for each record that needs an operator, and under it one `evidence:` line for each volume, VM, or parker that still carries the allocation.

```text
record: id=65a2e32a-0ec7-4dd8-bfc3-8ba70f2dfcf3 kind=disk state=reconciliation_required charging=true cid=pvz-H4sIAAAAAAAC_zTMXW6DMBAE4LvMs7cF8xd8m_WutyAFnNo0fYi4e0WrPs030mheeCJgt0rrxh-pBt-3s39_bvQLirkuZKqTyjAP3ciR-5H4fs9C48A-dZ6pSTJRr3qjaNLRLfLUmFcT694-JX97OGwIL-THUa_UtQoXRUDe4bDmYymJr97CodY_nQ68y5ILwlG-koMh4P9vvTbxoRQTjyzz1HPTWvKM8_wZAIdmclDVAAAA reason="outcome requires reconciliation at lifecycle attach_disk Pool.CreatePool"
evidence: allocation=65a2e32a-0ec7-4dd8-bfc3-8ba70f2dfcf3 kind=disk volume=nfs-images:24192/vm-24192-bosh-fdd7dc59536aba46-alloc-65a2e32a-0ec7-4dd8-bfc3-8ba70f2dfcf3.qcow2 node=lab-pmx-0 holder_vmid=90372
```

The record line gives the `state` and the full `cid`, which is `none` when the allocation never returned one. The evidence lines tell us whether the resources are still where the record says. A record in `ready_to_return`, `adopted`, `cleaned`, or `deleted` that is not charging needs nothing from us, so the summary leaves it out.

| The record | What PVE still holds for it | What we run |
|---|---|---|
| A disk in `reconciliation_required` that still has its CID | Its volume, where the record says it is | `adopt` with the CID from the audit, and a rerun of the deploy that failed. A rerun keeps using the disk only when the Director already had it active on the instance before that deploy, as [Records left behind by the old lock bug](#records-left-behind-by-the-old-lock-bug) explains |
| An allocation that never returned a CID | Resources we intend to remove | `cleanup` |
| Any allocation | Nothing, and the audit shows no evidence for it | `finalize-cleanup` |

`adopt` is the safe first step whenever the Director still uses the disk, because it changes nothing on PVE. It accepts a record in `ready_to_return` or a disk in `reconciliation_required`, and for any other state, or for a CID that differs from the record's, it refuses with `adoption requires a ready_to_return record, or a disk in reconciliation_required, with the exact CID`. When a step is still unsettled, it refuses and names that step, for example `allocation has unsettled mutation evidence; step attempt-0-step-7 (lifecycle_attach_disk_Pool_CreatePool) is planned; ...`. [Record adoption or completed cleanup](storage-journal-operations.md#record-adoption-or-completed-cleanup) says which steps count as settled. A planned step that is not a lock step needs the investigation that [Delete an allocation through its retained authority](storage-journal-operations.md#delete-an-allocation-through-its-retained-authority) describes, and `cleanup` deletes what the allocation owns, so we never use it on a disk the Director still holds.

The disk operations and `delete_vm` refuse an unsettled record in two more ways, and the word at the front of the refusal tells us which. When every lock step and parker protection write is settled and some other step is still unsettled, each of `attach_disk`, `detach_disk`, and `delete_disk` puts its own name at the front of the refusal, as in `attach_disk has unresolved mutation evidence; step attempt-0-step-4 (lifecycle_attach_disk_Nodes_UpdateQemuConfig) is planned`. `delete_vm` puts `cleanup` there, as in `cleanup has unresolved mutation evidence; step attempt-0-step-7 (lifecycle_delete_vm_retain_ephemeral_Nodes_CreateQemuMoveDisk) is planned`, and `storage-journal cleanup` refuses the same step inside its decision line, as in `allocation decision refused (cleanup_pending_mutation_settlement: cleanup refuses unresolved allocation or asynchronous mutation; step attempt-0-step-7 (lifecycle_delete_vm_retain_ephemeral_Nodes_CreateQemuMoveDisk) is planned); inspect settled outcome, identity and audit evidence`. No readback can settle a step like that, so it needs the same investigation as the `adopt` refusal above. A planned move step on a persistent disk's record is the one exception. The CPI settles it once it proves the move never started, and until then the refusal adds `its move could not be settled as never started because` and the check that failed, as [delete_vm refuses to destroy VM with attached unused disks](#delete_vm-refuses-to-destroy-vm-with-attached-unused-disks) describes.

When a lock step or a parker protection write is still planned because the CPI could not read it back, a disk operation's refusal opens with `lifecycle` instead. It names the first unsettled step of the record, which can be a step that no readback settles, and when that first step is the lock step, it says which read failed and why, as in `lifecycle has unresolved mutation evidence; step attempt-0-step-7 (lifecycle_attach_disk_Pool_CreatePool) is planned; its lock sentinel could not be settled because PVE did not answer exactly for sentinel bosh-lock-vm-90372 (<description>)`. The sentinel it names is the first one in the record whose read failed, which is not always the lock step's own target. `delete_vm` keeps `cleanup` at the front in that case and adds the same reason, as in `cleanup has unresolved mutation evidence; step attempt-0-step-2 (vm.Pool.CreatePool) is planned; its lock sentinel could not be settled because PVE did not answer exactly for sentinel bosh-lock-vm-777 (<description>)`, and `storage-journal cleanup` gives the same step and reason inside its decision line. The description says what PVE sent back, such as an HTTP status with PVE's own message. A lock step is harmless, so once PVE answers that read cleanly again, the next call settles it and carries on, and if a step that no readback can settle is still there, that call refuses with its own name as above. For `attach_disk` or `detach_disk`, that next call comes from a rerun of the deploy, and for `delete_disk` it comes from the Director's next pass of orphan cleanup. For `delete_vm` it comes from a rerun of the deploy, or, for a VM that a deploy orphaned, from the Director's orphaned-VM cleanup, which retries the delete on its own schedule.

### Records left behind by the old lock bug

A CPI from 0.6.0 through 0.8.0 also left records here whenever two requests contended for one parker. In a deploy, the call that failed was an `attach_disk` or a `detach_disk`, and the deploy failed with `requires reconciliation at lifecycle attach_disk Pool.CreatePool` or with the same text for `detach_disk`. A `delete_disk` could fail the same way, but only during orphan cleanup, because a deploy never deletes a persistent disk and orphans it instead. Every step of such a record is observed except the last one, a planned lock step named `lifecycle_<operation>_Pool_CreatePool`. Its `bosh-lock-` pool is usually gone by the time we look, but another request may hold it again, and the step settles in either case. A fixed CPI settles that step with a fresh read of the lock pool for each VM that the steps of the record's active attempt target, which for a parked disk includes its parker. It does that in whichever call touches the record next, whether that is a rerun `attach_disk` or `detach_disk`, a `delete_disk`, `adopt`, or `cleanup`.

The Director retries only `create_vm` within a task, so a failed `attach_disk` or `detach_disk` stays failed until we rerun the deploy, which reissues the call. A failed `delete_disk` needs nothing from us, and that is good news for the orphans the old bug left behind. The Director keeps an orphan on its list when `delete_disk` fails, and its scheduled orphan cleanup calls `delete_disk` again every 30 minutes for each orphan older than `director.disks.max_orphaned_age_in_days`. Once a fixed CPI is in place, the next of those calls settles the lock step and deletes the orphan with no action from us. A `bosh -d <deployment> delete-disk <cid>` that failed the same way can be run again to remove the orphan sooner.

What a rerun deploy does with our disk depends on whether the Director already had that disk active on the instance before the deploy that failed.

- The disk was already active on the instance

  This covers a disk the deploy was moving to a recreated VM and a disk it was detaching. The rerun issues the same call for the same disk, and it completes against that disk whether or not we adopted it first. Anything the earlier attempt had already written, such as drive-option overrides on the receiving VM, is written again in place. When the deploy was detaching the old disk after a migration, the rerun finishes that detach and then orphans the disk as the Director intended.

- The disk was new in the deploy that failed

  This covers the most common case, which is the first attach of a disk that the same deploy created for a new instance, a scale-out, or a persistent disk added to an instance group. It also covers a change to a disk's size or cloud properties while `director.enable_cpi_resize_disk` and `director.enable_cpi_update_disk` are off, as they are by default, because the Director then migrates the data to a new disk. In every one of these cases, the Director created a new disk, and the attach that failed was that disk's first. The Director's model still holds that disk as inactive, so the rerun does not reuse it. The rerun creates another new disk, attaches that one, calls `detach_disk` on ours, and orphans it. Nothing is lost, because the failed attempt never wrote data to our disk. That `detach_disk` settles the lock step and returns the record to `ready_to_return`, so the record stops charging capacity before the disk is even orphaned. Adopting the disk helps only between the failure and the rerun, when it stops the record charging capacity early. After the rerun, `bosh disks --orphaned` lists our disk, and the Director's scheduled orphan cleanup deletes it after five days by default, while `bosh -d <deployment> delete-disk <cid>` removes it sooner.

With `director.enable_cpi_resize_disk` on, the Director resizes the disk in place instead, so the disk was already active on the instance, and the rerun resizes and attaches that same disk again.

A `create_disk` that failed the same way reports `requires reconciliation at persistent parker completion` and never returned a CID, so its record takes `cleanup`, which settles the lock step before it judges the evidence. Until that `cleanup` runs, the record keeps charging its planned size against capacity, and `storage-journal audit --summary` prints it with `charging=true` on its `record:` line, so when a create is refused for capacity we did not expect, this record is the first place we look. For a `create_disk` record, the record names no parker VMID for its lock step, so that read goes to `bosh-lock-vm-<vmid>`, where `<vmid>` is the number in the new volume's name, `vm-<vmid>-...`. That number comes from the persistent-disk VMID band, which never overlaps the parker band. When the park had to create its parker first, the record also holds that parker's create step, so the CPI reads the parker's lock pool as well, and any exact answer from either read settles the step.

### A parker lock wait runs out

A persistent disk on a parker can also end up here after a lock wait runs out. When several instances move disks into or out of the same parker at once, each request waits its turn for that parker's lock, and the error names it as `timed out after 3m55s waiting for lock "bosh-lock-vm-<parker>"`. The 235-second wait is one budget for the whole queue rather than a fresh allowance for each holder, so a request at the back of a long queue can still run out. That figure holds on the shipped retry settings. When `pve.retry` lengthens the retry curves, the parker's lock and the wait for it grow by the same amount, so the error names a longer wait. A request that confirms its claim on the lock so late that too little of the claim is left for its work gives the claim up without touching the disk. When it could delete that claim, it takes a fresh claim at once, but only if that claim can still be created and confirmed before the wait runs out and would then leave the protection window the time it needs before the call's time limit, less the 15 seconds the CPI keeps for finishing the call. A call with no time limit never has that time inside a parker lock's wait, so a CPI call running with `pve.operation_timeout` off gives up instead, while a `storage-journal cleanup` or a call under `pve.operation_timeout` takes the fresh claim when its time limit leaves that much. In every other case the request fails with a retriable `confirmed lock "bosh-lock-vm-<parker>" too late to use it` error, which we handle the same way as a wait that ran out, and a `storage-journal cleanup` reports it as a parker lock that was not taken before the disk moved. An `attach_disk`, `detach_disk`, or `delete_disk` that had changed nothing before its wait ran out puts the allocation back as it was before it fails. A `storage-journal cleanup` of a disk does the same, so we rerun it once the other request's window closes, and the CLI gives `cleanup` long enough for a full wait and for its own window after that. The CLI applies `pve.retry` from the config it loads, so its wait and that time limit follow the CPI's. The drive-option override note that the CPI writes onto the receiving VM before the wait does not count as a disk mutation, because the note plays no part in deciding where the disk is. A cross-node attach that had to create a mover VM or park the disk before its wait still ends in `reconciliation_required`, and we recover it with the ordered steps below. The Director retries only `create_vm` within a task, so a timed-out `attach_disk` or `detach_disk` fails the deploy task, and rerunning the deploy reissues the call against the restored record. A `delete_disk` comes from orphan cleanup rather than from a deploy, and the Director's scheduled orphan cleanup calls it again every 30 minutes once the orphan is older than `director.disks.max_orphaned_age_in_days`, so that one needs nothing from us.

A `create_vm` that attaches a disk from `disk_cids` restores its disk the same way when the attach changed nothing before its wait ran out, but it does not keep the VM it built. It disposes of that VM first, preserving any disk it had already attached, and closes the VM's allocation, so the Director's retry in the same task starts a fresh allocation and builds a fresh VM. With `pve.debug.keep_failed_vms` on, `create_vm` keeps the VM instead, and the next try resumes it. With fallback attempts configured, `create_vm` first disposes of the VM it built, preserving any disk it had already attached, and places a new one. That new placement isn't tied to the parker's node. When it lands on another node, the attach moves the disk through a mover VM before it waits for the parker's lock, so if that wait runs out too, the attach has already changed something, and it takes the handoff route below instead of failing cleanly. If that disposal fails partway, for example because preserving an attached disk runs out the same wait, the VM's allocation goes to `reconciliation_required` and the Director does not retry, because a half-disposed VM is not safe to resume. That preservation happens only when another disk from `disk_cids` had already attached before the wait ran out. With a single disk in `disk_cids`, which is the common case, nothing is attached when the wait runs out, so the disposal takes no parker lock at all and cannot fail that way. A later `create_vm` for the same agent refuses to resume a half-disposed allocation, so we remove it with `cleanup`, as [A crash-abandoned allocation keeps charging capacity](#a-crash-abandoned-allocation-keeps-charging-capacity) describes. If a disposal is refused before it begins, for example because the allocation audit is incomplete, the VM stays as it was and the next try resumes it. The Director makes five tries by default, which `director.max_vm_create_tries` sets. When every try runs out of lock wait, the task fails, and because each try disposed of its own VM, no guest or charging record is left behind. A `delete_vm` whose wait runs out leaves its allocation with every step observed, and the next `delete_vm` for that VM resumes it. When nothing calls `delete_vm` for that VM again, as happens when `bosh delete-deployment --force` ignores the error, the record needs `cleanup` in the same way. Everything above about `create_vm` assumes its attach changed nothing before its wait ran out. When the attach had already changed the disk, `create_vm` fails without a retry, and the way out is the attested cleanup of the VM's handoff step that the steps below describe.

A `create_vm` disk attach whose wait runs out after it has changed something does not fail cleanly. The disk's record goes to `reconciliation_required`, the VM's handoff step for that disk stays planned, and `create_vm` fails with `allocation <id> requires reconciliation at VM post-create; no alternate allocation was attempted`. That error is not retriable, so the Director doesn't retry the call, and the task fails on its first try. A later `create_vm` for the same agent that tries to resume the allocation is refused with `allocation <id> requires reconciliation at unsettled recorded mutation; step attempt-0-step-1 (vm.persistent.<sha256>) is planned; no alternate allocation was attempted`, where the step kind carries a 64-character hash. Before the deploy runs again, we recover in this order.

1. We run `storage-journal audit --summary`, which lists the disk's record and the VM's record together.

2. We resolve the disk first, from its own record. We settle it with `adopt` when the Director still holds its CID, or with `cleanup` when the allocation never returned one, as [Choose the command for the record](#choose-the-command-for-the-record) describes.

3. We run `cleanup` on the VM's record with `--authority-id`, `--previous-writer-fenced`, and `--remote-tasks-settled`. Those flags attest that we fenced the previous writer and confirmed that its remote tasks settled, as [Delete an allocation through its retained authority](storage-journal-operations.md#delete-an-allocation-through-its-retained-authority) describes. The disk is already settled in its own record, so this cleanup only closes the handoff and removes the VM. When the persistent volume is still attached to the VM, the cleanup parks it again under its own allocation before it destroys the VM, so the disk never goes with the VM. Plain `cleanup` without those flags is still refused, and so is `delete_vm` for that VM.

4. We rerun the deploy, and its `create_vm` creates a fresh VM and attaches the disk to it.

A cross-node attach that created a mover VM or parked the disk before the wait can leave more open steps on the VM record than the handoff, and cleanup still refuses it until those are settled. The audit names them.

A disk record that a lock wait leaves in `reconciliation_required`, with every step observed and its CID still in place, needs nothing from us but the rerun. The next call for that disk readmits the record after a fresh check that the disk is where the record says, so rerunning the deploy clears it. Until then, the record keeps charging its planned size against capacity, and we run `adopt` only when we want that charge gone before the rerun. That is why step 2 above runs `adopt` before the VM cleanup and the rerun, so that the disk stops charging while we work, even though the rerun would readmit the disk without an `adopt`.

A `create_disk` whose park runs out has already created its volume, and the next deploy's `create_disk` creates a new one under a new allocation. That record never returned a CID, so it stays `reconciliation_required` until we run `cleanup` on it.

### A parker's protection restore timed out

When the CPI moves a parked disk onto a VM, or deletes a disk that is named for its parker, it clears the parker's protection for the change and puts it back afterwards. A cross-node attach does the same on the migration mover it moves the disk through, so everything below about a parker holds for a mover too. The write that puts protection back has a deadline of its own, which fits inside the time the parker's lock sets aside for it. That deadline is 35 seconds on the shipped retry settings. When `pve.retry` lengthens the retry curves, the deadline grows to cover the longest the restore's retries can take, and the parker's lock grows with it, so the times in the messages below are longer. The restore still runs when the request itself ends inside the window, whether `pve.operation_timeout` fired or the CPI was told to stop, because the restore runs on its own deadline and not on the request's. If PVE doesn't answer before the deadline, or the connection fails on every attempt, the CPI can't tell whether protection went back on. When the disk transfer itself completed, the CPI still records the disk on its new VM, with its provenance and CID, and removes the parker's record of it, so the disk is where the Director expects it. When a deletion completed, the CPI removes the parker's record of the deleted disk in the same way. The call then fails with a retriable error that names the parker and says whether the disk change completed.

```text
attach_disk: parker protection restore cut off after the disk reached VM 777 as data:vm-777-disk-2 (disk <disk CID>): transfer out: protection restore on parker vmid 90000 did not finish within 35s and its outcome is unknown; the disk transfer to vm 777 slot scsi1 completed; check the parker with qm config 90000 and run qm set 90000 --protection 1 if protection is off: context deadline exceeded
```

When every try fails in transport instead of hanging, the text says `ended without an answer from PVE and its outcome is unknown` where the sample says `did not finish within 35s`, and the cause at the end is the transport error.

We check the parker's protection first, on the node that hosts the parker. When the `protection` line is missing or reads `protection: 0`, protection is off, and we put it back.

```bash
qm config 90000 | grep ^protection
qm set 90000 --protection 1
```

For a disk outside the allocation journal, we then rerun the deploy. When the transfer completed, the rerun's `attach_disk` finds the disk already on its VM and finishes, and when the deletion completed, the disk is already gone. When the parker was a migration mover, the attach kept the empty mover, and we remove it once the rerun's attach has succeeded, as [A cross-node attach left its migration mover behind](#a-cross-node-attach-left-its-migration-mover-behind) describes.

For a journal-managed disk, the allocation is left in `reconciliation_required`, and the only step it has not settled is the restore, a planned `lifecycle_<operation>_Nodes_UpdateQemuConfig`. The next call that touches the record settles that step by reading the parker back before it judges the record. That call can be a rerun deploy's `attach_disk` or `delete_disk`, `storage-journal adopt`, or the `cleanup` precheck. When the parker reads protected, the step is marked observed and the call carries on, and nothing on PVE changes. When the parker reads unprotected, the call is refused and names the command that puts protection back. When the parker is a migration mover, that read is why the attach kept it. We put the mover's protection back first if it reads unprotected, and we remove the mover only after that call has succeeded. If the mover is gone by then, the call still settles the step once the disk is on its VM, as described below.

```text
step attempt-0-step-18 (lifecycle_attach_disk_Nodes_UpdateQemuConfig) is planned; its parker protection write could not be settled because protection is off on parker 90000; run qm set 90000 --protection 1 on node pve01, then retry
```

We run that command and retry the call. The attach keeps a mover whose restore was cut off instead of destroying it, so that the retry can read the mover back, and we remove such a mover only after the retry has gone through.

The CPI also settles the step when the parker isn't where the record left it. When the parker isn't on its recorded node, the CPI looks its VMID up across the cluster and reads the config on whichever node holds it now. A VM there that carries the `bosh-parker` tag and reads protected settles the step, and one whose protection is off makes the call refuse with the `qm set` command for that node. The refusal names the VM and its node rather than saying that the parker moved, because PVE may have given the VMID to a parker that the CPI created later.

```text
VM 90000 on node pve02 carries the bosh-parker tag and its protection is off, and the parker's recorded node is pve01; run qm set 90000 --protection 1 on node pve02, then retry
```

When the cluster proves the VMID gone, the CPI settles the step only when the disk is accounted for without the parker. That means the disk is on another VM, or its volume reads absent and the record observed the `delete_disk` write that reached it. A rerun reaches that state when we destroyed a migration mover after the disk landed on its VM. When the disk is on no VM and the record observed no delete of it, the call refuses with `the parker may have taken the disk with it`, and we check that the volume still exists before anything else, as [Parker anchor missing](#parker-anchor-missing-parked-disk-with-no-holder) describes. When a node doesn't answer the search, the call refuses with `the cluster could not be searched for it` and asks us to retry once every node answers. When the cluster lists the VMID on a node whose config can't be read, the call refuses and names that node, and we bring the node back and retry. When that node has been removed from the cluster, no CPI call settles the step, as the paragraph on a removed recorded node below describes.

A recorded node that is down, or that fails the read for any other reason, doesn't tell the CPI whether the parker is there. The CPI searches the cluster in that case too. When the cluster places the VMID on another node, for example because we migrated the parker off before its node went down, the CPI reads the VM there and settles or refuses as it does for any parker on another node. Otherwise the call refuses, names the recorded node, and asks us to retry once that node answers.

```text
the config of parker 90000 could not be read on its recorded node pve01 (<description>), and the cluster places it on no other node; retry once node pve01 answers
```

We bring the node back and retry. The CPI never treats the parker as gone while its recorded node is down, because the parker and the disk may still be on that node, and the cluster keeps listing a down node's guests there. When the search itself fails, the refusal says `and the cluster could not be searched for it either` instead, and we retry once every node answers.

When we have removed the recorded node from the cluster, the node never answers again, so the refusal stays, just as it does for a parker that the cluster still lists on a removed node. No CPI call settles the restore step then, and resolving the record is up to us. The parker may have left with its node and taken the disk with it, so we first check whether the disk's volume still exists, as [Parker anchor missing](#parker-anchor-missing-parked-disk-with-no-holder) describes.

A VM at the parker's VMID that doesn't carry the `bosh-parker` tag settles the step when it reads protected, because then nothing is left unprotected, whoever the VM is. When its protection is off, PVE can't tell us whether the VM is the parker with its tag stripped or a new VM that PVE gave the same VMID, so the CPI writes nothing to that VM and refuses.

```text
VM 90000 on node pve01 no longer carries the bosh-parker tag and its protection is off; the CPI writes nothing to it, so check with qm config 90000 whether it is still the parker, put protection back with qm set 90000 --protection 1 on node pve01, and retry
```

We first run `qm config 90000` on that node. The parker's name is `<prefix>-parker-<vmid>`, and its description carries the parker's record of the disks it holds. When the VM is the parker, we put protection back with `qm set 90000 --protection 1`, and the retry settles the step. Because we have confirmed that the VM is the parker, we put its `bosh-parker` tag back too, so the CPI sees its other parked disks again. When the VM is not the parker, we can turn its protection on for the retry and turn it off again once the retry has gone through, because the step is settled by then and the CPI never reads that VM for it again. Protection blocks destroying that VM and removing its disks in the meantime, so we tell its owner first. We never add the `bosh-parker` tag to a VM that we haven't confirmed is the parker, because the tag would make the CPI treat a VM it doesn't own as a parker.

Each step that the CPI settles without a tagged, protected parker on its recorded node leaves a warning in the CPI log, which `bosh task <id> --cpi` shows. The warning names the step, the parker's VMID, its recorded node, and what the CPI read instead. The CPI writes nothing to PVE to settle the step.

A `create_vm` that attaches a disk from `disk_cids` handles a cut-off restore without any cleanup from us. The disk has already reached the new VM, so `create_vm` keeps the VM and records the handoff, and it returns the retriable error without rolling the attempt back or placing another VM. The Director retries `create_vm` in the same task, and the retry resumes that VM. Once we have put the parker's protection back, the retry settles the restore step and completes with the disk already in its slot. The retry also settles the step without us when the parker reads protected on another node, or when the parker is gone from the cluster and the disk is on the new VM. When the disk came through a migration mover, the mover stays until a retry has completed, and we remove it afterwards as [A cross-node attach left its migration mover behind](#a-cross-node-attach-left-its-migration-mover-behind) describes. While protection is still off, each retry is refused with the same `qm set` command and keeps the VM for the next try. Only a `create_vm` attach that changed the disk without landing it on the VM still takes the handoff cleanup route in [A parker lock wait runs out](#a-parker-lock-wait-runs-out). A disk in `disk_cids` that is outside the allocation journal still marks the VM's record uncertain on a cut-off restore, and it takes the reconciliation route in [Choose the command for the record](#choose-the-command-for-the-record). If the Director runs out of tries while protection is still off, the task fails with the VM kept. The Director never received that VM's CID, so no `delete_vm` comes for it, and the next deploy runs under a new agent ID and finds the disk held by the kept VM. We recover in order. We put the parker's protection back first. We then run a plain `storage-journal cleanup` on the VM's record, which parks the disk again under its own allocation and removes the VM. A `cleanup` run while protection is still off is refused with the same `qm set` command and leaves the VM and the disk where they are, although it marks the VM's record as needing reconciliation, which the next `cleanup` closes. Last, we rerun the deploy, and its `create_vm` attaches the disk to a fresh VM. If the disk came through a migration mover, the cleanup has settled the mover's restore step by then, and we remove the empty mover the same way.

When PVE answers the restore with a failure, the write did not apply, so the outcome is known. The call succeeds, the journal records the write as settled, and the CPI logs a warning that asks us to put protection back with the same `qm set` command. The same holds when PVE refused the last try, for example because the parker's config was locked, and the deadline ran out while the CPI waited to try again, because no write went out after that refusal. An earlier try that got no answer may still have applied, so we check the parker with `qm config` before we set protection by hand.

When an earlier step of the same operation has already failed in a way that leaves it uncertain, the CPI doesn't send the restore at all. The error then says that the protection restore was not attempted because the operation was already uncertain, so there is nothing to look for on the network or in PVE's logs. We still check the parker and put protection back the same way, and the record is reconciled for the earlier failure, not for the restore.

For a journal-managed disk, the CPI checks three things before it sends the restore. It reads the disk's storage definition, the cluster's identity, and the parker's configuration. When one of those reads fails, because PVE answered it with an error or because it ran past the restore's deadline, the CPI doesn't send the restore. The error then says that the protection restore `was not sent because the checks before it failed, so protection is still off`, or, when a read ran past the deadline, `was not sent because the checks before it did not finish within 35s, so protection is still off`. Either way it gives the same `qm config` and `qm set` commands, and it ends with the name of the check that couldn't finish, such as `managed disk backing could not be checked before the parker protection restore`. The read of the cluster's identity has its own 30-second timeout, which runs out before the restore's deadline, so a hung identity read always gets the `failed` ending. In this case we know protection is off, because the transfer's own write turned it off and nothing was sent after that. When the transfer completed, the CPI still records the disk on its new VM, and the record keeps the restore as a planned step, so the next call that touches the record reads the parker back, just as it does after a restore that was cut off. We put protection back with the `qm set` command and retry. When one of those reads does answer, but with a storage backing or a cluster identity that differs from the record, the CPI treats the operation as uncertain instead. The Director then sees `managed disk holder provenance was not verified; audit required`, which is not retriable, and the CPI log names the restore as not attempted because the operation was already uncertain.

### A cross-node attach left its migration mover behind

A cross-node attach moves a disk through a migration mover, which is a parker VM that holds only that disk and carries the `bosh-disk-mover` tag. The attach normally destroys the mover once the disk lands. When destroying it would do harm, the attach keeps the empty mover instead, and it logs a warning that names the mover, its node, the reason, and when it is safe to remove. `bosh task <id> --cpi` shows that warning.

```text
attach_disk: kept the migration mover because its protection restore was cut off, and the mover is where that restore gets checked and settled. Nothing in the CPI removes it later, so only after a retry of this attach has succeeded and the mover holds no disks, run qm set 90787 --protection 0 and then qm destroy 90787 on node pve02
```

The reason tells us when the mover can come out.

- The mover's protection restore was cut off

  The cut-off error tells us to check the mover's protection and put it back if it is off. For a journal-managed disk, the next call settles the restore by reading the mover back, so we put its protection back if it is off, and we remove the mover only after a retry of the attach has succeeded. If the mover was removed first, the retry still settles the step once the CPI finds the mover gone from the cluster and the disk on its VM, and it logs a warning that says so.

- An earlier request created the mover

  A journal-managed attach deletes only a mover that the same request created, so it keeps one that an earlier, interrupted request left behind. The warning then says `kept the migration mover because an earlier request created it, so this request's journal can't delete it`. We remove that mover once the call has gone through.

- The destroy failed

  The warning says `could not destroy the migration mover after the attach`, and its log record carries PVE's error in the `error` field. We remove the mover once the call has gone through.

Nothing in the CPI removes a mover it kept, so the mover stays until we remove it. `qm destroy` frees each volume that the VM's own configuration section, its `unusedN` entries, its vmstate, or its pending section names, but only when the volume's name carries the VM's VMID. With `--destroy-unreferenced-disks` it also frees every other volume on the enabled storages that carries that VMID, and the GUI's destroy dialog ticks that option by default. So before we destroy anything, we read the mover's current and pending values on the node the warning names, with `qm pending <vmid>`, and check that it holds no disk. The CPI's own destroy makes the same check, and it refuses a mover that has a `scsi`, `virtio`, `ide`, or `sata` line or any `unusedN` line. It doesn't look at `efidisk` or `tpmstate` lines, so we count those as disk lines too. If the output shows any of them, we stop and leave the mover as it is, and we reconcile the allocation as [Multi-storage allocation requires reconciliation](#multi-storage-allocation-requires-reconciliation) describes. When it shows none, we turn the mover's protection off and destroy it with `qm destroy <vmid>` from the command line, where `--destroy-unreferenced-disks` is off unless we pass it.

```bash
qm pending <vmid>
qm set <vmid> --protection 0
qm destroy <vmid>
```

### A crash-abandoned allocation keeps charging capacity

Every set-managed create charges the bytes that its in-flight siblings have already claimed, so an allocation left behind in an in-flight state goes on charging its bytes against every later create in the namespace. Nothing ages a record out of those states on its own. A CPI killed between writing its record and running its first step leaves a `planned` record, and that record keeps its claim until an operator resolves it.

The bound is the number of abandoned allocations rather than the number of VMs, so the usual symptom is a share that looks fuller to the planner than PVE says it is. In the worst case, a create cannot find a feasible member at all.

The journal audit names such a record.

```sh
cpi storage-journal audit --config /path/to/cpi.json
```

Each record summary carries `charging`, which is true for exactly the states that charge bytes, and it carries `CreatedAt` and `UpdatedAt` in RFC 3339. The `charging_summary` object beside the record listing holds the number of charging records, the identifier of the oldest one, and its age, so we do not have to read a long audit row by row. When nothing is charging, that summary reports a count of zero and names no record. When a record's age runs to hours or days and its `UpdatedAt` has not moved since it was created, we are looking at the shape a crashed CPI leaves behind.

Nothing clears such a record automatically, and that is deliberate, because deciding that an in-flight allocation is abandoned rather than merely slow is a human's call. The `reconciliation_required` state exists for exactly that wait. Reconcile the record the way this section describes, by reading it, settling what the allocation left behind on PVE, and then moving or cleaning it with the commands in [Audit and recover storage allocations](storage-journal-operations.md).

[Charge in-flight siblings against a placement](multi-storage-placement.md#charge-in-flight-siblings-against-a-placement) lists which record states charge and which do not.

### A disk delete is refused after its volume is already gone

An orphaned disk can fail `delete_disk` with the following error, even though PVE no longer holds the volume.

```text
allocation 65a2e32a-0ec7-4dd8-bfc3-8ba70f2dfcf3 requires reconciliation at lifecycle delete_disk completion audit failed; no alternate allocation was attempted
```

The error is not retriable, so the Director keeps the orphan on its list. Each pass of its orphan cleanup, each `bosh clean-up --all`, and each `bosh delete-disk` then fails the same way. The allocation is left in `reconciliation_required`. From this release, a retry can clear that state, as the cases below describe. In the output of `storage-journal audit --summary`, the allocation shows a `record:` line and an `evidence:` line. The evidence line has a `kind` of `disk`, and its `holder_vmid` is the ID of the VM the disk was last attached to. The same error text also appears when the completion audit fails for another reason, such as an audit read that failed, so we confirm the cause from that evidence line before we act.

By the time we see this error, the CPI has already deleted the volume that the Director asked it to delete. What remains is a note in the description of the VM the disk was last attached to. That note is the disk's allocation entry, and the audit treats it as proof that something of the allocation remains. Up to 0.8.1, only a `detach_disk` that finished the move on its first attempt removed the note, so a disk whose detach needed a second attempt left it behind.

A disk reaches that state in the following way. A `detach_disk` for the disk fails with a retriable error after the CPI has already deleted the disk's slot on the VM, and the deploy fails with it. The volume is then on no slot of that VM, and the CPI holds a record of the transfer on a parker VM. When a later `detach_disk` finds that record, as it can on a rerun of the deploy, it finishes the move to the parker. Up to 0.8.1, that call returns as soon as it sees the disk on a parker, so it never reaches the step that removes the note. On 0.8.0 the second `detach_disk` has to come within an hour of the first, because that release removes the parker's transfer record once it is an hour old, so only some disks reach this state there. Later the Director orphans the disk while the VM it came from still exists, and its `delete_disk` deletes the volume and then finds the note. From this release, whenever a `detach_disk`, `attach_disk`, `delete_disk`, or disk attach in `create_vm` finds the move to the parker landed, it removes the note from the VM the move took the disk off, before it moves or deletes the disk. So a disk whose calls have all run under this release reaches that state in one of three ways. First, another writer can put the note back, as we describe below. Second, the note can be filed under a volume name that the VM still holds, and the CPI leaves it there on purpose, because that name now belongs to whatever disk sits on it. Third, the note can name some other volume with no recorded move that links that volume to this disk, and then the CPI leaves it in place and logs a warning that the source VM's allocation entry doesn't match the landed disk. This release also lets a `delete_disk` remove our disk's notes from every VM that carries them and holds the disk nowhere, after the audit's first pass and before its second, so a note that an earlier release left behind heals on the next retry. That heal works even when the allocation is already in `reconciliation_required`, and even when we attached the disk to other VMs after it left the one with the note. The third way above is the exception, because a note that names another volume with no recorded move still makes a retry refuse. The fallback of recreating the VM, which we describe below, still applies to it. The cases below say when a retry still refuses.

A change to the disk type or the disk size is one way the Director orphans a disk while its VM lives on. With `director.enable_cpi_resize_disk` and `director.enable_cpi_update_disk` off, as they are by default, the Director creates a new disk, migrates the data, detaches the old disk, and orphans it, and the instance keeps its VM throughout.

That VM still exists, which is what lets the audit find the note. Deleting a VM deletes its description, and the note with it, so a disk whose VM is already gone does not meet this refusal.

Before we recreate any VM for this refusal, we check that the cause isn't another volume on the disk's old name. We take the `volume` that the allocation's `evidence:` line names, which is the name the disk had on that VM, and we list the disk and `unusedN` keys of the VM with `qm config`. On shared storage such as Ceph or NFS, any VM on any node can hold that name, so we search every VM's configuration in the cluster instead, for example with `grep -l '<volume>' /etc/pve/nodes/*/qemu-server/*.conf`, and we run `qm config` on each VM it lists. When no key of any of those VMs names that volume, the recovery later in this section holds. If any VM holds that volume on a disk slot, we don't recreate the VM that carries the note. Deleting that VM can destroy a volume named for it, and that volume now holds another disk's data. If the volume sits on a VM only as an `unusedN` entry, a retry won't clear it, so we follow [A disk delete is refused while another volume sits under its old name](#a-disk-delete-is-refused-while-another-volume-sits-under-its-old-name).

What we do next depends on which VM holds the volume on a disk slot. On node-local storage, a VM counts here only while it runs on the node where the CPI moved our disk off the VM that carries the note, and the last case below covers a VM that has migrated to another node since.

- The VM that carries the note holds the volume itself

  This is the VM that the allocation's `evidence:` line names as its `holder_vmid`. We retry the `delete_disk`, and from this release the retry ends `deleted`. After the audit's first pass and before its second, the retry removes our disk's own note from that VM, along with any attached-disk note that names our disk's CID and isn't filed under a volume that VM still holds. It leaves every note of the other disk exactly as it was. The retry judges the volume by its notes alone. So if we have attached our disk's own data back to that VM under the old name with no serial, for example from a parker's `unusedN` entry, the record closes, and that data stays on the VM as a volume the CPI no longer tracks.

- A different VM holds the volume

  We retry the `delete_disk`, and from this release the retry ends `deleted`. The VM that carries the note holds our disk nowhere, so the retry removes our disk's own notes from it after the audit's first pass and before its second. The VM that holds the volume keeps all of its notes, even when they mention the old name, because they're filed under the other disk's stable ID and allocation, and the retry removes only notes that are filed under ours. The one case that doesn't end `deleted` is a disk whose old name is also its birth name, when the other VM holds that name on a slot with no serial. The identity check refuses that delete before the heal runs, and [Another entry names a disk's birth volume](#another-entry-names-a-disks-birth-volume) describes the way out.

- The storage is node-local, and the VM that holds the volume has migrated to another node since the move

  A retry doesn't clear the refusal. Before the retry reads any VM, it checks what the audit found. On node-local storage that check treats a VM that holds the old name as not holding the disk only when the VM sits on the node where the move ran, because the volume the move renamed is on that node and nowhere else. A VM that has migrated fails that check, so the retry leaves every VM's notes as they are and refuses. This release leaves these records in `reconciliation_required`, and a later change heals them.

From this release on, a retry of `delete_disk` removes our disk's notes from every VM that carries them and holds the disk nowhere. It does that only when the audit finds no VM holding the disk and no volume of the disk on any storage, so while any VM still holds the disk, every VM keeps its notes. The retry also reads every one of those VMs before it writes to any of them, so a hold that only one VM's read turns up, such as a snapshot, keeps the notes on all of them. A VM still holds the disk when one of the disk's names appears in its current config, in its pending changes, on an `unusedN` entry, or in one of its snapshots, or when it has a drive with the disk's serial. The one exception is the old name on a disk slot, when a recorded move took the disk off that VM under that name. That holds whether the slot is in the VM's config or in one of its snapshots, because PVE may have given the name to another disk since. On node-local storage, a VM that holds that name in its config and has migrated off the node where that move ran isn't an exception, so every VM keeps its notes, as the last case above describes. A VM that holds that name only on an `unusedN` entry still holds the disk, because the entry could hold the disk's own data.

The retry also leaves every VM's notes alone while a step of the record's active attempt has no outcome yet, as after an attach that was cut off. A step of an attempt that the journal has already closed doesn't count, such as the create step that `create_disk` leaves behind when PVE rejects its first request at validation and `create_disk` then retries. A read that fails ends the call retriable, and the next call reads each VM again. When a retry still refuses with an `evidence:` line whose `holder_vmid` is a VM, we look at that VM with `qm config` and `qm pending`. We also list its snapshots with `qm listsnapshot` and read each one with `qm config <VMID> --snapshot <name>`, so that we find what still names the disk. We settle that first, for example by letting the Director finish the operation that was cut off, or by removing a snapshot we no longer need, and then we retry.

After we upgrade to this release, a disk whose VM still carries its note heals on its next `detach_disk`, `attach_disk`, `delete_disk`, or `create_vm` that attaches it, as long as its allocation isn't in `reconciliation_required` yet and the disk hasn't been attached anywhere since it left that VM. That call finds the VM through the parker's record of the move and removes the note before it moves or deletes the disk. The call fails with a retriable error when it can't remove the note, which happens when the read of that VM fails, when one of the reads the CPI makes just before the write fails, when the VM is locked, or when PVE refuses the write and a fresh read still shows every note the write meant to remove. The disk stays on its parker, and we leave the allocation ready for the Director to retry instead of putting it in `reconciliation_required`. When a `create_vm` can't remove the note, it also disposes of the VM it built, unless `pve.debug.keep_failed_vms` asks us to keep it. So while the failure lasts, each placement attempt and each Director retry of `create_vm` builds a VM and disposes of it, just as when a parker lock wait runs out. A disk that an earlier release left in this shape and that we then attached to another VM and detached again keeps the note on the first VM through these calls, because they reach only the VM of the disk's last move. A retry of `delete_disk` removes that note once the volume is gone, as the cases above describe.

The removal also doesn't take that VM's own lock. So a writer that rewrites the VM's whole description without a digest, such as `set_vm_metadata`, can write back an older copy with the note in it. A `delete_disk` that finds that copy at its completion audit has already deleted the volume, so it ends in this same state with the same evidence line. A retry of that `delete_disk` removes the note again, even when the allocation is already in `reconciliation_required`. Once the volume is gone, no other CPI command removes the note from a VM that still exists. `adopt` cannot help because it needs the disk on storage. `cleanup` refuses on the same evidence, and `finalize-cleanup` refuses while any audit evidence names the allocation. If a retry still refuses after we have settled every hold we found, or the storage is node-local and the VM has migrated, deleting or recreating the VM the disk was last attached to is what clears the note, which for a Director means `bosh recreate` of that instance. Deleting the VM is not blocked by the note, because the CPI reads a VM's disk notes only for volumes that the VM's slots still name. We have read this in the code and have not run it against a lab. Once the VM is gone, the audit shows no evidence for the allocation, and either of the following closes the record.

- A retry of `delete_disk`

  The Director's scheduled orphan cleanup calls it again every 30 minutes for an orphan older than `director.disks.max_orphaned_age_in_days`, and `bosh -d <deployment> delete-disk <cid>` calls it sooner.

- A `finalize-cleanup` decision

  ```sh
  cpi storage-journal finalize-cleanup \
    --config /path/to/cpi.json \
    --allocation-id ALLOCATION_UUID \
    --decision-id incident-1234-cleanup
  ```

Recreating the VM through the Director keeps the instance's persistent disk, because `bosh recreate` detaches the disk, replaces the VM, and attaches the disk to the new VM. We delete a VM directly in PVE only when no instance depends on it, because that destroys its guest. When the Director is about to recreate the VM anyway, we can let the recreate do the work.

### A disk delete is refused while another volume sits under its old name

A `delete_disk` can fail with the same `completion audit failed` error when another volume has taken the disk's old name. When the CPI moves a disk onto a parker, PVE gives the disk a new name, and the next volume that lands on that VM can take the old one. If a VM holds that volume on a disk slot, the audit no longer counts it as ours once the journal shows the move that renamed our disk. A volume that sits under the old name only as an `unusedN` entry still counts as ours, because it could be a leftover of our disk's data or another disk caught in its own unfinished detach, and the name alone can't tell us which. So the allocation stays in `reconciliation_required` until we sort the volume out. In the output of `storage-journal audit --summary`, the allocation's `evidence:` line has a `kind` of `disk`, a `volume` with the disk's old name, and a `holder_vmid` of `none`.

We start by finding out whose data the volume holds. `qm config` on the VM that the volume's name points at shows the `unusedN` entry. If the volume belongs to another disk on that VM, we attach it back to its owner on a slot, or we let the Director's own operation for that disk finish its detach. If the volume holds a leftover of our disk's data that we no longer need, we remove it. Deleting an `unusedN` entry with `qm set VMID --delete unusedN` destroys the volume, so we do that only once we know nobody needs its data. After that, we rerun the delete with `bosh -d <deployment> delete-disk <cid>`, or we wait for the Director's next orphan cleanup. When that VM carries no note of our disk, or the rerun removes our disk's note from it because the VM holds our disk nowhere, the allocation ends `deleted`. If the rerun still refuses and `storage-journal audit --summary` shows an `evidence:` line for the allocation with that VM's ID as its `holder_vmid`, that VM still holds the disk in some form, so we settle what it holds, as the section above describes, and retry.

### An operation fails with an allocation audit refusal

**Symptom**

A `create_vm`, `create_disk`, `delete_vm`, or `delete_disk` fails, or a `storage-journal` decision is refused, and the error opens with the operation, the word `refused`, and a count of audit findings. Every refusal has the same shape.

```text
<operation> refused: <counts>; <finding>; <finding>; <finding>; and <remainder>; see "An operation fails with an allocation audit refusal" in docs/troubleshooting.md of bosh-proxmox-cpi-release; run '<audit command>' for the full report
```

Here is the refusal a Director raises when two VMs on node-local storage have been live-migrated outside BOSH.

```text
create_vm refused: 2 audit conflicts; VM 4626 on pvupvecf102, recorded pvupvecf101 (node_mismatch), allocation 180f7d1e-08e3-437d-8163-7c9bdfe00dc9; not a move: volume local-lvm:vm-4626-disk-0 is node-local; VM 7014 on pvupvecf103, recorded pvupvecf102 (node_mismatch), allocation 2a4b0f7c-1c2d-4e5f-8a9b-0c1d2e3f4a5b; not a move: volume local-lvm:vm-7014-disk-0 is node-local; see "An operation fails with an allocation audit refusal" in docs/troubleshooting.md of bosh-proxmox-cpi-release; run 'sudo -u vcap /var/vcap/packages/pve_cpi/bin/cpi storage-journal audit --summary --config /var/vcap/jobs/pve_cpi/config/cpi.json' for the full report
```

The counts come first and name `audit conflict`, `VM-scan issue`, or `audit issue`. The error then lists at most three findings, and it takes one finding for each VM or volume before it lists a second one for any of them, so the several conflicts of one moved VM can't hide another VM. A conflict appears in a short form that leads with the VM or volume it concerns and names its allocation, as in `VM 4626 on pvupvecf102, recorded pvupvecf101 (node_mismatch), allocation 180f7d1e-08e3-437d-8163-7c9bdfe00dc9`, and an issue appears in full. The exception is a disk with too many holders, whose short form leads with `disk allocation <id>`. The one short form that can name no allocation is a volume that two VMs share, when only its name ties it to our namespace. When the error leaves findings out, it counts them by kind and names the VMs and volumes that their short forms lead with, as in `and 2 more audit conflicts (VM 7015, VM 7018), 1 more VM-scan issue`. A finding that leads with neither, such as an offline node or an issue whose text opens with `known volume`, is counted but not named. The error then points at this section and ends with the command that prints the full report. When the CPI could not work out the audit command at startup, it ends with "run storage-journal audit --summary as the journal owner on the host that runs this CPI" instead, and the pointer to this section still comes just before it.

The operation at the front tells us which call refused and which findings it refuses on.

| Error opens with | Raised by | Refuses on |
|---|---|---|
| `create_vm refused` | `create_vm` admission | Any conflict, or any issue that left the VM scan incomplete |
| `VM allocation readback refused` | A Director retry of `create_vm` whose allocation already has a record | Any conflict, or any issue that left the VM scan incomplete |
| `create_vm retry refused` or `unsubmitted VM generation inspection refused` | A `create_vm` retry that plans a new attempt or inspects one that was never submitted | Any conflict or issue |
| `storage allocation admission refused` | `create_disk` | Any conflict |
| `create_disk retry refused` | A `create_disk` retry | Any conflict or issue |
| `VM cleanup refused`, `post-destroy VM resource cleanup refused`, or `VM cleanup completion refused` | `delete_vm`, or a `create_vm` retry that cleans up a failed attempt | Any conflict or issue |
| `delete_disk refused` | `delete_disk` | Any conflict or issue |

So a storage listing that failed on one node blocks `delete_vm` and `delete_disk` but not `create_vm`. An offline node blocks every call in the table except `create_disk` admission, which refuses only on a conflict. That exception ends once anything has moved. While the VM scan is incomplete, the audit can't accept any move, so every VM, disk, or parker in the namespace that was moved outside BOSH stays a conflict, and `create_disk` refuses on it too. The refusal names the node the scan missed, as in `not a move: the VM scan is incomplete because pve3 is reported offline, so another sighting could be hidden`. The `storage-journal` actions `cleanup`, `adopt`, and `finalize-cleanup` run audit gates of their own, including the ones for retained VM cleanup. They print the same summary and the pointer to this section inside `allocation decision refused (<class>: <summary>; see "An operation fails with an allocation audit refusal" in docs/troubleshooting.md of bosh-proxmox-cpi-release)`, without the command, because we are already running it.

**Diagnosis**

`bosh tasks` keeps only the first 75 or so characters of a CPI error, which is enough for the operation, the counts, and the start of the first finding. `bosh task <id>` prints the whole message. The CPI's own log for that task is in `bosh task <id> --cpi`, and every refusal writes one Error record there, named `allocation audit gate refused`. That record carries `operation`, `vm_scan_complete`, `complete`, `conflict_count`, `vm_scan_issue_count`, and `issue_count`, along with the full `conflicts`, `vm_scan_issues`, `issues`, and `observed_moves` lists, each joined with ` | `.

For the full report, we run the command the refusal names on the host that runs the CPI. On a Director that means the Director VM, where the journal belongs to `vcap` and no `cpi` is on the `PATH`.

```bash
sudo -u vcap /var/vcap/packages/pve_cpi/bin/cpi storage-journal audit --summary --config /var/vcap/jobs/pve_cpi/config/cpi.json
```

The `sudo -u vcap` matters. The CPI itself runs as `vcap`, but we log in to the Director as another user, so a CPI that runs from `/var/vcap/` always prints the command with `sudo -u` and the journal's owner. Plain `sudo` runs the audit as root, and the journal opens only for its owner, so the CLI stops with `journal <path> is owned by vcap; rerun as that user:` followed by the command to use. Under `bosh create-env` the CPI runs on our workstation as our own user, so the command the refusal prints has no `sudo -u` and uses the workstation's paths.

`--summary` prints one line per finding, so we can read the report on a Director that has no `jq`. The first line gives the overview, and every other line starts with its kind. Here is the summary from a Director where one VM moved with a node-local disk and another moved on shared storage.

```text
audit: complete=false vm_scan_complete=true generation_index_healthy=true cluster_continuity=true records=42
conflict: remote allocation 180f7d1e-08e3-437d-8163-7c9bdfe00dc9 (VM 4626) is outside recorded mutation targets: observed on pvupvecf102, recorded pvupvecf101 (node_mismatch); a migration outside BOSH is the usual cause; not accepted as a move because volume local-lvm:vm-4626-disk-0 is node-local
issue: known volume local-lvm:vm-4626-disk-0 on storage "local-lvm" on node "pvupvecf102" disagrees with recorded physical target: allocation 180f7d1e-08e3-437d-8163-7c9bdfe00dc9 recorded pvupvecf101 (node_local_elsewhere)
observed move: vm allocation 5b1c9a47-2f0e-4d4b-9b3e-7a61e0c2d8f1 (VM 7015) moved from pvupvecf102 to pvupvecf103
runbook: see "An operation fails with an allocation audit refusal" in docs/troubleshooting.md of bosh-proxmox-cpi-release
charging: none
```

In this report, VM 4626 moved with a node-local disk and stays a conflict, while VM 7015 moved on shared storage and was accepted. A `conflict:` line means the audit saw something that contradicts the journal. A `vm-scan issue:` line means the VM scan could not finish, so the audit cannot prove that no other copy of a VM exists. An `issue:` line means some other read failed, or that what the audit read disagrees with the journal in a way that is not a conflict, like the `(node_local_elsewhere)` line above or malformed disk provenance. An `observed move:` line is a move the audit accepted, and it never blocks anything. A `runbook:` line follows the findings whenever the audit raised a conflict or an issue, and it points back at this section. A `planned step:` line appears for each planned step in the journal. A `generation index:` line appears only when the journal's index is unhealthy, and a `skipped disabled storages:` line appears only when the audit skipped one. The command exits 1 when the audit or its VM scan is incomplete, when the index is unhealthy, or when cluster continuity is lost, and any conflict marks the audit incomplete. The findings name the allocation, the VMID, the recorded node, and the observed node, so each one matches one of the cases below.

On releases before 0.9.0, a `create_vm` or `create_disk` could refuse because another create ran at the same moment and the audit saw that create's new VM or volume before the record behind it. The refusal then named `carries unknown allocation <id>`, `recorded no node (not_in_step)`, or a VM configuration that `does not exist`, and an audit we ran afterwards came back clean. In that case we rerun the deploy, and 0.9.0 no longer refuses this way.

**An accepted move on shared storage**

When the cluster operator migrates a VM, a persistent disk's holder, or a parker VM to another node, and everything it holds sits on shared storage the new node can see, the audit accepts the move. There is no refusal, and the summary lists the move instead.

```text
observed move: vm allocation <allocation id> (VM <vmid>) moved from <recorded node> to <observed node>
```

A persistent disk that moved with its VM prints a `disk allocation` line, and a parked disk that moved with its parker prints a `parker allocation` line. The JSON report lists the same moves under `observed_moves`.

The audit accepts a VM move only when all of these hold:

- The VM scan is complete, so no offline node can hide a second copy. When it isn't, the refusal names the node the scan missed, as in `the VM scan is incomplete because pve3 is reported offline`, and it lists every node that was missed when more than one is offline, as in `the VM scan is incomplete because pve3, pve4 are reported offline`.

- The record belongs to our namespace and is in `ready_to_return` or `adopted`, which means the Director already holds the VM's CID. A record that `delete_vm` has since moved into `planned`, `observed`, or `reconciliation_required` also qualifies, but only when the latest delete admission that verified the VM's ownership accepted the same move of the same VMID to the same node.

- The VMID is a target of the record's active attempt, the allocation marker was sighted exactly once, and the VM carries the record's agent digest.

- Every volume in the VM's configuration belongs either to this allocation or to a disk allocation whose provenance the VM carries, and every snapshot configuration could be read.

- Every volume the VM holds, including a hibernation `vmstate` and the volumes of each snapshot, sits on storage that meets the storage conditions below. So does every volume the active attempt recorded.

A moved disk or parker needs the holder VM to be sighted exactly once in a complete VM scan, the volume to be in that VM's configuration, and its storage to meet the same conditions. Each storage has to meet all of these conditions:

- It is shared now, and it was already shared when the owning plan was frozen.

- Its backing is the one the owning plan froze.

- Its node list is empty or includes the new node.

- The new node's listing of that storage, taken in this same audit, shows the volume itself. A successful listing that lacks the volume is not enough, because a `dir` storage whose mount has dropped lists the empty directory underneath. When that listing fails instead, the audit neither accepts nor refuses the move. It raises an `issue:` line that carries `move undecided (listing_failed)` and the listing it could not read, as in `move undecided (listing_failed): storage "<name>" on <node> could not be listed, so volume <volid> is unproven there`, so `create_vm` and `create_disk` go ahead while `delete_vm` and `delete_disk` wait for an audit that can read the listing.

The CPI never records a step for an accepted move in the journal. It decides every move afresh on every audit, and when an operation retains verification evidence, that evidence lists the moves its audit saw. The calls that run later read the same decision. Resume and adoption accept the VM on its new node, and `delete_vm` stops and destroys it there, retains its ephemeral disks there, and heals the provenance of any persistent disk it detaches. While that delete runs, and after it fails partway, the audit keeps reading the VM as moved because the delete admission accepted the move. Other deployments can still create VMs in the meantime, and the next `delete_vm` for that VM finishes the job. A rerun of the deploy makes that call, and for a VM that a create-swap-delete deploy orphaned, the Director's orphaned-VM cleanup makes it on its own schedule. When a crash leaves a stop or destroy step unsettled, a plain `delete_vm` retry refuses and names that step, as in `cleanup has unresolved mutation evidence; step <step id> (vm.delete.destroy) is planned`, just as it does for a VM that never moved, and `storage-journal cleanup` then links that step to the moved VM through the same admission. A `detach_disk` first rewrites the disk's provenance on the VM so that it names the VM's current node, and it does this before the disk moves to its parker. Parker reuse, attaching from a parker, and `delete_disk` of a parked disk accept a moved persistent-disk parker the same way.

For a VM, a persistent disk, or a persistent-disk parker, there is nothing to fix. We can leave it where it is or migrate it back. Downgrading the CPI below this release brings the conflicts back, because an older CPI does not know the rule. The retention parker that `delete_vm` leaves behind for retained ephemeral disks is the exception, and the entry on a retention parker moved by a bulk migrate, further down, covers it.

A few checks stay strict even on shared storage. A VM whose CID the Director never received is not accepted as moved, so `storage-journal cleanup` of it refuses with `allocation decision refused (cleanup_historical_audit: 1 audit conflict; VM <vmid> on <node>, recorded <node> (node_mismatch), allocation <allocation id>; not a move: the record is in state <state>, not ready_to_return or adopted; see "An operation fails with an allocation audit refusal" in docs/troubleshooting.md of bosh-proxmox-cpi-release)`. A `create_disk` whose hinted VM moves while the disk is planned refuses with `create_disk: hinted VM <cid> migrated outside observed nodes: now on <node>, observed <nodes>; no allocation submitted`, and simply retrying the deploy settles that one. ISO cleanup also stays strict about its node, and so does cleanup of a retention parker.

The rule has one blind spot. The CPI trusts the `shared` flag on a `dir` or `lvm` storage. If a storage was declared shared from the start but is really a local directory on each node, and the new node happens to hold a file of the same name, the audit cannot tell that file from the real volume.

**A move that involves node-local storage**

When any volume involved sits on node-local storage, the move stays a conflict, and the conflict says why. A moved VM raises this conflict.

```text
remote allocation <allocation id> (VM <vmid>) is outside recorded mutation targets: observed on <new node>, recorded <recorded node> (node_mismatch); a migration outside BOSH is the usual cause; not accepted as a move because volume <volid> is node-local
```

A moved persistent disk raises this one.

```text
disk ownership provenance disagrees with actual holder node: disk allocation <allocation id> (volume <volid>) held by VM <vmid> on <new node>, provenance names <recorded node>; a migration outside BOSH is the usual cause; not accepted as a move because volume <volid> is node-local
```

The new node's listing of that local storage now shows the volume too, which adds an issue ending in `(node_local_elsewhere)`, as in the summary above. Other reasons after "not accepted as a move because" name the condition that failed, such as `storage "<name>" of volume <volid> is not available on <node>`, `volume <volid> was not listed on storage "<name>" on <node>`, or `the backing of storage "<name>" changed since the plan for volume <volid> was frozen`.

That last reason has a trap. A `dir` storage's backing includes its node list, so adding the new node to a restricted `dir` storage to make the volume visible changes the backing, and the move is refused again for that reason.

The usual cause is a migration outside BOSH, often a live migration with `--with-local-disks` during maintenance. PVE logs a migration on the source node, which is the recorded node, so we look there to see who moved the VM.

```bash
pmx pve task list --node <recorded node> --vmid <vmid> --typefilter qmigrate --userfilter <user>
```

The fix is to move the VM back to the node the journal records.

```bash
pmx pve qemu migrate <vmid> --target-node <recorded node> --online --with-local-disks
```

PVE can give a disk a different name on the target node, so we check afterwards that each volid still matches the journal. We compare the VM's configuration with the volumes the journal recorded for its allocation, and for each persistent disk's allocation. The example path is the usual Director journal directory, and ours is whatever `pve.storage_allocation_journal_dir` names.

```bash
pmx pve qemu config get <vmid> --node <recorded node>
sudo -u vcap find /var/vcap/store/pve_cpi/allocations -name 'allocation-<allocation id>.json' \
  -exec grep -oE '"(intended_volume|volids)":(\[[^]]*\]|"[^"]*")' {} +
```

If the names match, we run the audit again and the conflicts are gone. If a volid changed, the journal no longer describes the VM, and we reconcile that allocation as [Multi-storage allocation requires reconciliation](#multi-storage-allocation-requires-reconciliation) describes before deploying again.

**A parker moved by a bulk migrate**

Parker VMs are ordinary stopped VMs that hold parked persistent disks, so a bulk migrate or `pvenode migrateall` during a patch cycle moves them along with everything else. On shared storage the audit accepts the move and prints a `parker allocation` line. On node-local storage the parked disk raises the `disk ownership provenance disagrees with actual holder node` conflict shown above, with the parker's VMID as the holder. The fix is the same as for any node-local move. We migrate the parker back to the node its provenance names, with `--with-local-disks`, and check its volids afterwards.

**A retention parker moved by a bulk migrate**

When `delete_vm` keeps a VM's ephemeral disks, it moves them onto a retention parker and leaves the record in `vm_deleted_retained`. That parker's entry carries no allocation ID, so the audit never decides a move for it, and the summary shows neither a conflict nor an `observed move:` line for it. The explicit cleanup that finally deletes those disks still reads the parker on the node it was created on. If a bulk migrate moved the parker, even on shared storage, `storage-journal cleanup` refuses with `allocation decision refused (cleanup_resource_cleanup: <description>)`. The description names the failed read of the parker on its recorded node, and it is usually an HTTP status with PVE's message.

The retention steps in the VM's journal record name the parker's VMID and its recorded node. We read them on the Director like this.

```bash
sudo -u vcap find /var/vcap/store/pve_cpi/allocations -name 'allocation-<allocation id>.json' \
  -exec grep -oE '"kind":"lifecycle_delete_vm_retain_ephemeral_QEMU_Create","target":\{[^}]*\}' {} +
```

The fix is to migrate the retention parker back to its recorded node and then retry the cleanup.

```bash
pmx pve qemu migrate <parker vmid> --target-node <recorded node>
```

**A VM or volume the journal has no record for**

```text
VM <vmid> on <node> carries unknown allocation <allocation id>
volume <volid> on <node> carries unknown allocation <allocation id>
```

The full report gives the same conflict as `remote allocation <allocation id> is missing from retained journal; audit required: observed <VM or volume>`. The VM's Notes, or the volume's name, carry an allocation of our namespace that this journal has no record of. The audit reads the journal a second time after its scan, so a `create_vm` or `create_disk` that wrote its record during the scan no longer causes this. The one exception is a second read that fails, which the CPI logs as a warning that says `storage allocation audit could not read the journal again; its unsettled conflicts stand`. So we run the audit again first, and when the conflict is gone, we rerun the deploy.

A conflict that stays means the resource belongs to an allocation that this journal never saw or has lost. Two Directors or two cpi-config contexts that share one `pve.storage_placement_namespace` can cause it, and so can a journal restored from a backup that is older than the allocation. Before we change anything, we read the full report to see which allocation it names, and we check which Director or context the VM's Notes or the volume's name belong to. Then we reconcile as [Multi-storage allocation requires reconciliation](#multi-storage-allocation-requires-reconciliation) describes. [Storage-set authorities across clusters](multi-cluster.md#storage-set-authorities-across-clusters) explains how each writer gets its own namespace. We never delete the VM or volume to clear the refusal, because it may be another writer's live resource.

**A VM or volume of an allocation the journal has closed**

```text
<VM or volume> belongs to <deleted or cleaned> allocation <allocation id>
```

The full report gives it as `terminal allocation <allocation id> still has remote provenance; audit required: <deleted or cleaned> record, observed <VM or volume>`. The journal recorded this allocation as deleted or cleaned, and PVE still holds a VM or volume that carries it. The most likely cause is a VM that somebody restored from a backup after BOSH deleted it. No live BOSH VM or disk carries a closed allocation, so once we're sure nobody restored the resource on purpose, we remove it by hand on PVE. `qm destroy` frees each volume that the VM's own configuration section, its `unusedN` entries, its vmstate, or its pending section names, but only when the volume's name carries the VM's VMID. With `--destroy-unreferenced-disks` it also frees every other volume on the enabled storages that carries that VMID, and the GUI's destroy dialog ticks that option by default. For a VM, we first read `qm pending <vmid>` on its node and check that none of the volumes it lists is one that the Director or another VM still uses, and then we destroy it with `qm destroy <vmid>` from the command line, where `--destroy-unreferenced-disks` is off unless we pass it. After the removal, the audit stops reporting the conflict.

**A VM or volume whose allocation has another kind or namespace**

```text
<VM or volume> disagrees with the journal identity of allocation <allocation id>
```

The full report gives it as `remote allocation <allocation id> disagrees with journal identity: record kind <kind> in namespace "<namespace>", observed <vm or volume> evidence <VM or volume>`. The journal's record for this allocation names a different kind of resource, or a different namespace, from the one PVE shows. The CPI doesn't write a marker or a volume name like that, so most likely somebody edited the VM's Notes or renamed a volume by hand. For a VM, we compare the record's kind and namespace in the full report with the VM's Notes, and we restore the Notes block exactly as the CPI wrote it, as the entry on malformed provenance below describes. For a volume, we change nothing. The evidence comes from the volume's name, which no edit to the Notes can correct, so we reconcile the allocation as [Multi-storage allocation requires reconciliation](#multi-storage-allocation-requires-reconciliation) describes.

**A VM or volume from a closed attempt**

```text
<VM or volume> is from a closed attempt of allocation <allocation id>
```

The full report gives it as `allocation <allocation id> has resources from a closed attempt: <VM or volume>, active attempt <n>`. A `create_vm` or `create_disk` retry closed an earlier attempt of this allocation and started a new one, and PVE still holds a VM or volume that the earlier attempt built. The CPI removes those when it closes an attempt, so this one survived a removal that failed or was cut off. We check that it isn't the VM or disk the Director uses, which `bosh vms --details` and `bosh disks` show. `qm destroy` frees each volume that the VM's own configuration section, its `unusedN` entries, its vmstate, or its pending section names, but only when the volume's name carries the VM's VMID. With `--destroy-unreferenced-disks` it also frees every other volume on the enabled storages that carries that VMID, and the GUI's destroy dialog ticks that option by default. For a VM, we then read `qm pending <vmid>` on its node, and we destroy the VM by hand only when none of the volumes it lists is one of the Director's disks, using `qm destroy <vmid>` from the command line, where `--destroy-unreferenced-disks` is off unless we pass it. If the Director does use it, the journal and PVE disagree about which attempt is live, and we reconcile the allocation as [Multi-storage allocation requires reconciliation](#multi-storage-allocation-requires-reconciliation) describes.

**A VM whose marker names another agent**

```text
VM <vmid> on <node> carries an agent digest that differs from allocation <allocation id>
```

The full report gives it as `VM allocation <allocation id> has inconsistent agent provenance: VM <vmid> on <node> carries a different agent digest`. The VM's marker names this allocation but a different BOSH agent. The CPI doesn't write that either, so most likely somebody edited the VM's Notes or copied the marker block from another VM. We restore the block as the entry on malformed provenance below describes, with the agent digest that the record holds, and the conflict clears.

**A VM the record targets that lacks our marker**

```text
VM <vmid> on <node> lacks the marker of allocation <allocation id>
```

A live record says that this VMID on this node is ours, and the VM there doesn't carry our marker. The full report gives it as `recorded VM target for allocation <allocation id> lacks matching ownership provenance`, followed by the reason, which is `carries no allocation marker`, `carries a malformed allocation marker`, `carries a marker from namespace "<namespace>"`, or `carries the marker of allocation <other id>`. A malformed marker also raises a VM-scan issue, which the entry on malformed provenance below describes. Either somebody edited the VM's Notes, and we restore the block, or BOSH's VM is gone and an unrelated VM took its VMID on the same node. In that second case we leave the unrelated VM alone and reconcile the allocation, as [Multi-storage allocation requires reconciliation](#multi-storage-allocation-requires-reconciliation) describes.

**A persistent disk with more than one holder**

```text
disk allocation <allocation id> has <n> holders: <holders>
```

This is the one short form that leads with the allocation instead of a VM or volume. The full report gives it as `disk allocation <allocation id> has multiple active holders or duplicate stable tokens: <holders>`, and each holder reads `volume <volid> on <node> (VM <vmid>)`. Two drives claim the same persistent disk, either because they carry the same disk serial or because each holds the volume the disk's record and provenance name. That happens when somebody copies a VM's configuration with its drive serials, or attaches the disk to a second VM by hand. A volume whose name PVE reused for a later disk no longer causes it. This conflict doesn't come alone. When two VMs hold one volume, the shared-volume conflict below comes with it, and when a copied VM carries the serial on a volume of its own, that volume raises a `not_in_step` conflict as well. Taking the drive away from the VM that shouldn't hold it clears all of them. We find that VM in the list of holders, we stop it or confirm that it is already stopped, and we remove its drive line by hand, as the entry on a volume that two VMs share describes below, without deleting the volume, because the other holder still uses it. When the second drive is a different volume of that VM's own that carries only a copied serial, we can remove just the `serial=` option from its line instead, which keeps that VM's own disk attached.

**A VM-deleted allocation with something outside its kept disks**

```text
<VM or volume> is outside the retained artifacts of allocation <allocation id>
```

The full report gives it as `VM-deleted allocation <allocation id> has provenance outside retained artifacts`, followed by what the audit observed. `delete_vm` destroyed this allocation's VM and kept its ephemeral disks on a retention parker, so the record is in `vm_deleted_retained`. PVE now shows something of this allocation other than those kept disks. A kept disk on node-local storage whose retention parker moved to another node can cause it, and we migrate the parker back as the entry on a retention parker moved by a bulk migrate above describes. The VM itself, restored from a backup, is the other cause. `qm destroy` frees each volume that the VM's own configuration section, its `unusedN` entries, its vmstate, or its pending section names, but only when the volume's name carries the VM's VMID. With `--destroy-unreferenced-disks` it also frees every other volume on the enabled storages that carries that VMID, and the GUI's destroy dialog ticks that option by default. Once we're sure nobody restored it on purpose, we read `qm pending <vmid>` on its node, check that none of the volumes it lists is one of the kept disks, and remove the VM by hand with `qm destroy <vmid>` from the command line, where `--destroy-unreferenced-disks` is off unless we pass it. The kept disks stay on the parker, and the audit accepts the state again.

**A sighting that none of the record's steps explains**

```text
<VM or volume> on <node>, recorded <node> (<reason>), allocation <allocation id>
```

The full report gives it as `remote allocation <allocation id> (<VM or volume>) is outside recorded mutation targets: observed on <node>, recorded <node> (<reason>)`. The move entries above cover the reasons `node_mismatch` and `node_local_elsewhere`. The other reasons say why no step of the record matched what the audit saw.

- `not_in_step`

  No step of the record names this VMID or volume, and `recorded no node` means the record has no step at all. A copy of one of our VMs, made with its Notes, causes it, and so does a sibling create whose record the audit's second journal read couldn't see. We run the audit again, and if the conflict stays, we look for a VM whose Notes carry a marker it shouldn't.

- `external`

  Every step of the record targets something it doesn't own outright, such as a retention parker, so none of them can claim the VM or volume. A record with one ordinary step beside its external ones reports `not_in_step` instead. We reconcile the allocation, as [Multi-storage allocation requires reconciliation](#multi-storage-allocation-requires-reconciliation) describes.

- `storage_unknown`

  PVE no longer defines the volume's storage, or the volume ID can't be read, or the record's frozen plan has no definition for it. When PVE dropped the definition, we restore it under its original name, and the audit accepts the state again.

- `storage_changed`

  The step that names the volume recorded another storage as its target than the one in the volume's own name. The CPI's own steps always record a volume's own storage, and PVE can't change what a record says, so this most likely means that the record was edited or written by something other than this release. We change nothing on PVE for it, and we reconcile the allocation.

- `backing_changed`

  The storage under that name now has a different backing from the one the plan froze, such as another server, export, path, or pool. The full report also shows an issue that names the recorded and the current backing. We put the storage definition back the way it was, and the audit accepts the state again. A `dir` storage's backing includes its node list, so adding a node to it counts as a change too.

**A volume of ours that two VMs share**

```text
volume <volid> is attached to <n> VMs: VM <vmid> on <node>, VM <vmid> on <node>, allocation <allocation id>
```

The full report gives it as `volume <volid> is referenced by more than one VM, so each can write the same disk: <holders>`. The volume is one of ours because its name carries our namespace, a live record names it, or a VM's disk provenance or drive serial ties it to one of our allocations. At least one of the VMs listed references it without owning it, usually through a hand-copied configuration or a disk that someone attached to a second VM by hand. For a persistent disk, the refusal leads with the holder conflict from the entry above and gives this one second. When only the volume's name ties it to us, the short form ends after the last VM with no allocation, and when the storage listing shows the volume, a conflict that reads `carries unknown allocation` comes first. A VM allocation's own volume shows only this one. Guests outside our namespace that share a disk or a passed-through device among themselves never raise this conflict, and neither does an ISO that many VMs mount.

To find the VM that really holds the disk, we start from the Director's own pairing of VM and disk CIDs, as [Auditing parked disks with scripts/disk-audit](operations.md#auditing-parked-disks-with-scriptsdisk-audit) describes. Then we take the extra reference out of the configuration of the VM that shouldn't hold the disk, by editing the configuration file by hand. First we stop that VM, or confirm that it is already stopped, because removing the line doesn't unplug the disk from a running guest, so both VMs keep writing it until that guest stops. Then, on the node that holds that VM, we remove only that drive's line from `/etc/pve/qemu-server/<vmid>.conf`. When the file's `[PENDING]` section names the volume too, we remove that line as well. A snapshot section that names the volume puts the reference back when we roll back to that snapshot, so we either delete that snapshot or make a note of it so that nobody rolls back to it. Last, we confirm with `qm pending <vmid>` that the line is gone and that no `unusedN` entry names the volume.

```bash
qm pending <vmid>
```

We don't use `qm set <vmid> --delete <slot>` or `qm disk unlink` for this. Either one can leave an `unusedN` entry for a volume that the VM owns by its name, and when that entry is removed through PVE, PVE deletes the volume, even though the other VM still uses it. If one of those commands has already left an `unusedN` entry that names the volume, we remove that line by editing the configuration file too, and we never run `qm set <vmid> --delete unusedN`. Then we run `qm pending <vmid>` again. The audit doesn't count an `unusedN` entry as a reference, so a leftover entry doesn't keep the conflict, and the audit says nothing about it. So we check with `qm pending` ourselves, and we count an `unusedN` entry as a reference whenever we decide to destroy a VM. `qm destroy` frees each volume that the VM's own configuration section, its `unusedN` entries, its vmstate, or its pending section names, but only when the volume's name carries the VM's VMID. With `--destroy-unreferenced-disks` it also frees every other volume on the enabled storages that carries that VMID, and the GUI's destroy dialog ticks that option by default. When we do destroy a VM, we run `qm destroy <vmid>` from the command line, where `--destroy-unreferenced-disks` is off unless we pass it. We never delete the volume to clear the refusal.

**A node reported offline**

```text
some cluster nodes could not be inspected: <node>, <node> (reported offline by /cluster/status)
```

This is a VM-scan issue, so it refuses every audit-gated call, `create_vm` included. The one exception is `create_disk` admission, which refuses only on a conflict, and even that exception ends once anything in the namespace has moved outside BOSH. While a node can't be scanned, the audit accepts no move, so every move outside BOSH, on any storage, raises a conflict that ends in `not a move: the VM scan is incomplete because <node> is reported offline, so another sighting could be hidden`. Once the node is back, the audit accepts a move on shared storage again when it meets the move rules. The audit cannot prove that an allocation has no second copy on a node it cannot read. A related issue, `cluster VM enumeration failed: could not list guests on node(s) <nodes>`, means the node was not reported offline but its guest listing failed. When one answer from PVE explains the failure, the issue reads `could not list guests on node <node> (<cause>)`, as in `(HTTP 403: Permission check failed)`. A failed guest listing leaves the audit with no guests at all, so `create_vm` refuses on the VM-scan issue while `create_disk` admission still goes ahead. We bring the node back, or we restore its API if it is up but unreachable. If the node is gone for good, we remove it from the cluster the way PVE documents, and the next audit no longer counts it.

**An unreadable VM configuration**

```text
VM <vmid> configuration could not be inspected on <node>: <error>
```

This is also a VM-scan issue. `<error>` is a safe description of what went wrong, which is an HTTP status with PVE's own message, `request timed out`, `connection to <host>:<port> failed`, `context deadline exceeded`, `context canceled`, or `unclassified error`. The message ends in `PVE returned an empty configuration` when the read succeeded but held nothing. We read the configuration ourselves with `pmx pve qemu config get <vmid> --node <node>`. An HTTP 403 means the CPI's token lacks `VM.Audit` on that VM, and an empty or unreadable configuration usually means a damaged file in `/etc/pve/qemu-server/` on that node. On releases before 0.9.0, a `does not exist` error for a VM that is no longer listed means the VM was deleted during the scan.

**Unproven visibility**

```text
cluster-wide VM and storage audit visibility is unproven: allocation audit requires propagated VM.Audit at /vms
```

The text after the colon says what the audit could not prove. When the token lacks a grant, it names the missing privilege and ACL path, for example `allocation audit requires Sys.Audit at /access`, or `allocation audit visibility is restricted at <path>` when an ACL narrows what the token can see. When one of the check's own reads failed, it names the read, the path, and the kind of failure instead, as in `could not read effective permissions at /access (context deadline exceeded)` or `could not read ACL entries at /access/acl (HTTP 500: <PVE message>)`. When PVE answered with something the CPI couldn't parse, it reads like `PVE returned malformed effective permissions at /vms`. A failed read says nothing about the token's grants, so we look at the PVE API on that node before we change any permissions. PVE filters listings by permission, so a listing the token cannot fully see proves nothing about absence. We grant what the message names, as [Multi-storage audit visibility](pve-api-permissions.md#multi-storage-audit-visibility) describes. The variants that end in `PVE client cannot prove audit visibility (CPI defect)` or `the audit visibility reader is unavailable (CPI defect)` are CPI bugs rather than cluster problems, so we keep the CPI log from `bosh task <id> --cpi` for them.

**Malformed provenance**

```text
VM <vmid> has malformed allocation provenance on <node>: <parse error>
VM <vmid> has malformed disk provenance on <node>: <parse error>
```

The first is a VM-scan issue about the `[bosh_storage_allocation]` block in the VM's Notes, and it refuses `create_vm`. The second is about the `bosh_disk_allocations` or `bosh_parked_disks` provenance, and it names the carrier or key that failed. It refuses `delete_vm`, `delete_disk`, the `create_vm` and `create_disk` retries that plan a new attempt, and the `storage-journal` decisions. It does not refuse `create_vm` admission, `create_disk` admission, or a Director retry that reads back an existing allocation. The parse error is one of the CPI's fixed texts, such as `ambiguous storage allocation provenance` or `malformed JSON provenance`. Both usually mean somebody edited the VM's Notes by hand. We read them with `pmx pve qemu config get <vmid> --node <node>` and restore the block exactly as the CPI wrote it, from a backup of the VM configuration if we have one. We should not delete the block to clear the refusal, because the marker is how the CPI recognizes its own VM.
