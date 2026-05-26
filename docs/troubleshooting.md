# Troubleshooting

This is a symptom-first triage runbook. Start from the failure you see, follow the diagnosis steps, apply the fix. For log access commands, ISO verification, orphan cleanup procedures, and preflight checks, see the [Operations Runbook](operations.md).

## Reading CPI errors

The CPI surfaces two error types in BOSH task output:

- **`Bosh::Clouds::CloudError`** (`ok_to_retry: false`) — terminal failure. The director will not retry. Operator action required.

- **`Bosh::Clouds::RetriableCloudError`** (`ok_to_retry: true`) — transient fault. The director auto-retries the CPI call. If it exhausts retries, the error escalates to the task log as a permanent failure.

Errors appear in the BOSH task debug log. To view them:

```bash
bosh task <id> --debug
bosh task <id> --debug 2>&1 | grep -E '"method":"|"error":|pve:'
```

For instructions on accessing the CPI log file on the Director VM, see the [Operations Runbook](operations.md).

## Authentication and permission failures

### 401 Unauthorized or token rejected

**Symptom**

A failed call wrapped by the CPI, where the text after the `PVE API error:` prefix is the message PVE returned and will vary by PVE version:

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

A failed call wrapped by the CPI, where the text after the `PVE API error:` prefix is the permission message PVE returned for the denied path:

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
config validation failed: agent_mode must be one of cloudinit|registry|noagent, got "X"
```

**Fix**

Set `pve.agent_mode` to `cloudinit`, `registry`, or `noagent`. The default is `cloudinit`.

### VMID range too low

**Symptom**

```text
config validation failed: vmid_range_start must be ≥100
```

**Fix**

Set `pve.vmid_range_start` to 100 or higher in your manifest.

### Registry endpoint missing

**Symptom**

```text
config validation failed: registry_endpoint is required when agent_mode=registry
```

**Fix**

Set `registry.endpoint`, `registry.user`, and `registry.password` in your manifest when using `agent_mode: registry`. See [Configuration](configuration.md).

### Unknown field in config

**Symptom**

```text
config: decode failed: <json error>
```

**Diagnosis**

The CPI uses `DisallowUnknownFields` when parsing `cpi.json`. A typo in a property name causes an immediate decode failure.

**Fix**

Compare the failing `cpi.json` against the property list in [Configuration](configuration.md). Correct the misspelled field name and redeploy.

## VM creation failures

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
no free VMID in range [100, 5999]: all 5900 IDs exhausted
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

Delete unused VMs to free VMIDs, or widen the range by setting `pve.vmid_range_end` to a higher value (maximum 9999; the 9000–9999 band is reserved for persistent-disk VMIDs).

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

## Stemcell upload and import failures

### Stemcell storage is local-only on a multi-node cluster

**Symptom**

```text
create_stemcell: stemcell storage "X" is local-only but the cluster has N nodes; stemcell_storage must be a shared storage pool (NFS, Ceph, CIFS, etc.) accessible from all cluster nodes
```

**Fix**

Set `pve.stemcell_storage` to a storage pool that is shared across nodes (`shared=1` in `storage.cfg`). Acceptable types: `nfs`, `cifs`, `glusterfs`, `cephfs`, `dir` with shared=1. See [Configuration](configuration.md#stemcell-storage).

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
ParseStemcellCID: legacy integer CID "5042" not supported in direct-qcow mode; clear the stemcell entry from BOSH state and re-upload to regenerate the CID
```

**Diagnosis**

The BOSH state database holds a legacy integer CID from a previous CPI version. The current CPI uses a content-addressed string CID.

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
attach_disk: local-backend disk <cid> lives on node X but VM <vmid> runs on node Y — local-storage disks cannot cross nodes
```

**Fix**

Local-backend disks are pinned to a node and cannot be attached to a VM on a different node. Use a shared storage backend (`nfs`, `cephfs`, `cifs`, etc.) for `pve.disk_storage` if you need cross-node disk attachment. See [Persistent Disks](persistent-disks.md).

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

### delete_vm refuses to destroy VM with attached unused disks

**Symptom**

```text
delete_vm: refusing to destroy VM <N> — persistent volumes still attached as unused slots on storage "X": [unusedN=<volid>] (call detach_disk first)
```

**Diagnosis**

The BOSH director's view of disk state has drifted from PVE state. The VM config still contains `unusedN` slots referencing live persistent volumes.

**Fix**

Run `bosh -d <deployment> cloud-check` to reconcile state. The director will offer to detach the disks and clean up the record. If the deployment is unrecoverable, detach the disks manually via `bosh -d <deployment> detach-disk` before deleting the VM. See the [Operations Runbook](operations.md) for recovery procedures.

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

### SDN zone or vnet not found

**Symptom**

PVE returns 404 or "Zone not found" when the CPI attempts to create a network.

**Diagnosis**

```bash
pvesh get /cluster/sdn/zones
pvesh get /cluster/sdn/vnets
```

**Fix**

When using `pve.network_mode: sdn`, the SDN zone referenced by `pve.sdn_zone` must exist before deployment. Create it in the PVE web UI or enable `pve.sdn_auto_manage_zone: true` to let the CPI manage it. Ensure `libpve-network-perl` is installed on all cluster nodes and the token has `SDN.Allocate` on `/sdn`. See [Configuration — SDN network management](configuration.md#sdn-network-management) and [create_network](cpi_methods.md#create_network).

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

## Storage lock contention and transient transport faults

These two failure classes are handled by CPI retry logic and are usually absorbed without operator action.

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

The CPI retries up to 10 times with exponential backoff starting at 2 seconds, capped at 30 seconds, with ±30% jitter. If all 10 attempts fail, the operation surfaces as a `RetriableCloudError` and the director re-queues it.

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

## Task timeouts

**Symptom**

```text
AwaitTask <upid>: context cancelled
```

or a PVE task that does not complete within the CPI's deadline.

**Behavior**

The standard CPI task deadline is 300 seconds. Stemcell import and VM disk import use a 600-second deadline. These are not operator-configurable.

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
| `pve.vmid_alloc_attempts` | `10` (create_vm) / `5` (create_disk VMID conflict) | Maximum retries for VMID allocation before giving up |
| `pve.reboot_timeout` | `60s` | How long the CPI waits for a soft-reboot to complete |
| `pve.reboot_mode` | `soft` | `soft` uses ACPI shutdown; `hard` forces power off |
| `pve.allow_disk_ops_with_snapshots` | `false` | When `true`, bypasses the snapshot guard on attach, detach, and resize |
| `pve.require_snapshot_check_pass` | `false` | When `false`, the CPI proceeds (with a warning) if the snapshot-check API call fails; when `true`, any API failure on the snapshot check is a hard error |
| `pve.vmid_range_start` | `100` | Lower bound of the VMID range the CPI allocates from |
| `pve.vmid_range_end` | `5999` | Upper bound of the VMID range the CPI allocates from |
| `pve.log_level` | `info` | Log verbosity: `debug`, `info`, `warn`, or `error` |

## Interpreting retry log lines

Use these patterns to distinguish normal retry noise from actionable failures.

| Log pattern | Meaning | Action |
|---|---|---|
| `pve: storage lock timeout, retrying op=<op> attempt=N max_attempts=10` | Storage lock contention, CPI is retrying | Watch trend; if `attempt` > 5 routinely, split storages or throttle deploys |
| `pve: transient transport fault, retrying` | `pvedaemon` worker recycled mid-request | If frequent, raise `MAX_WORKERS`; see [PVE Host Tuning](pve-host-tuning.md) |
| `"type":"Bosh::Clouds::RetriableCloudError"` | Transient fault, director will auto-retry | No action unless retries routinely exhaust |
| `"type":"Bosh::Clouds::CloudError","ok_to_retry":false` | Terminal failure, operator action required | Read the `message` field and consult the relevant section above |
