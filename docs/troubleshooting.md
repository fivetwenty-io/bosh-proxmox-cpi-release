# Troubleshooting

This is a symptom-first triage runbook. Start from the failure you see, follow the diagnosis steps, apply the fix. For log access commands, ISO verification, orphan cleanup procedures, and preflight checks, see the [Operations Runbook](operations.md).

## Reading CPI errors

The CPI surfaces two error types in BOSH task output:

- **`Bosh::Clouds::CloudError`** (`ok_to_retry: false`) — terminal failure; the Director will not retry. Operator action required.

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

The CPI attempts an automatic recovery first: if it is authenticated as the `root@pam` superuser — either directly with `pve.user: root`, `pve.realm: pam` (or `pve.user: root@pam`) and a password, or via an API token issued to the `root@pam` user (`pve.api_token: root@pam!<token-id>=<secret>`) — it retries the failing stop/destroy call once with PVE's `skiplock` parameter, which bypasses the guest-config lock check. PVE honors `skiplock` **only** for `root@pam`; it rejects the parameter for every other identity, including a least-privilege token issued to any other user, regardless of the ACL roles or privileges granted to that user. When the CPI is *not* authenticated as `root@pam`, or the `skiplock` retry itself fails, the error above is the final, actionable outcome and manual recovery is required:

```bash
qm unlock <vmid>
```

Run this on the PVE node that hosts the VM (the node named in the error message). Once unlocked, the BOSH Director's next retry of `delete_vm` (or the original `create_vm`, if the lock was hit during a rollback) succeeds normally — no CPI restart or manifest change is needed.

**Interaction with `debug.keep_failed_vms` and the `bosh-create-failed` tag**

When a `create_vm` rollback (`cleanupVM`) hits this condition and cannot clear the lock (not `root@pam`, or the `skiplock` retry also failed), the VM is left running and orphaned — it was never meant to be preserved, but ended up stuck regardless of the `pve.debug.keep_failed_vms` setting (see [CPI Methods — `create_vm`](cpi_methods.md#create_vm) for that flag's normal, opt-in preserve-for-inspection behavior). The CPI tags it `bosh-create-failed` on a best-effort basis (when the failing rollback has a BOSH deploy identity to tag with) so an operator can find it the same way:

```bash
pvesh get /cluster/resources --type vm | jq '.[] | select(.tags != null and (.tags | contains("bosh-create-failed"))) | {vmid:.vmid, node:.node, tags:.tags}'
```

A VM found this way needs the same `qm unlock <vmid>` fix before it can be destroyed or adopted.

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

The CPI classifies this condition as retriable and injects an operator hint into the error message (`` cluster has lost quorum; mutations are blocked until quorum returns — check `pvecm status` ``) so the raw 5xx is not left anonymous in task output. Because quorum loss is a minutes-scale condition — waiting for a node to reboot or a network partition to heal takes far longer than a worker-pool hiccup — the CPI retries it on the storage-lock backoff curve (2 seconds → 30 seconds, 10 attempts, ±30% jitter) rather than the shorter transient-transport curve (1 second → 15 seconds, 8 attempts) used for ordinary 5xx errors. If quorum returns within that window, the retry succeeds transparently and the BOSH task shows no visible interruption beyond the added latency; if quorum does not return in time, the error escalates to the task log as a `RetriableCloudError` and the BOSH Director's own re-drive logic takes over on the next deploy or task retry.

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
| `"type":"Bosh::Clouds::RetriableCloudError"` | Transient fault, director will auto-retry | No action unless retries routinely exhaust |
| `"type":"Bosh::Clouds::CloudError","ok_to_retry":false` | Terminal failure, operator action required | Read the `message` field and consult the relevant section above |
