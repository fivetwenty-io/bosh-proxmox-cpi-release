# Operations Runbook

This runbook covers day-2 operations and diagnostics for an already-deployed BOSH Proxmox CPI. For symptom-first failure triage, see [Troubleshooting](troubleshooting.md).

---

## Diagnostic entry points

### CPI logs

**Director-managed deploys (`bosh deploy`)**

The CPI binary runs inside the Director VM. All CPI log output goes to stderr, which the job template wires to `/var/vcap/sys/log/bosh/cpi/pve.log`.

From your operator workstation:

```bash
# Full debug log for a specific task (includes raw JSON-RPC envelopes)
bosh task <id> --debug

# Show only CPI errors and method names
bosh task <id> --debug 2>&1 | grep -E '"method":"|"error":|pve:'

# Stream the most recent task
bosh task --recent=1 --debug
```

From the Director VM (requires jumpbox SSH — see [Deploying a BOSH Director](bosh-create-env.md#7-ssh-into-the-director-vm)):

```bash
ssh -i /tmp/jumpbox.key jumpbox@<director-ip>
sudo tail -f /var/vcap/sys/log/bosh/cpi/pve.log
sudo cat /var/vcap/sys/log/director/director.log   # director-side CPI call trace
```

**Bootstrap deploys (`bosh create-env`)**

During `bosh create-env`, the CPI binary runs locally on your workstation. Logs stream to stderr only; no persistent log file exists at this stage.

```bash
bosh create-env bosh.yml ... --debug 2>&1 | tee create-env.log
```

### CPI configuration

The rendered CPI configuration lives at `/var/vcap/jobs/pve_cpi/config/cpi.json` on the Director VM. This is the source of truth for what the running CPI sees — not the manifest template.

```bash
sudo cat /var/vcap/jobs/pve_cpi/config/cpi.json
```

To view without credentials:

```bash
sudo cat /var/vcap/jobs/pve_cpi/config/cpi.json \
  | jq 'del(.password, .api_token)'
```

### Reading a CPI request/response

The CPI reads JSON-RPC requests from stdin and writes responses to stdout. The Director pipes both. Running `bosh task <id> --debug` shows the raw envelopes on `D` (debug) lines.

Request format:

```json
{"method":"create_vm","arguments":[...],"context":{"director_uuid":"...","request_id":"..."}}
```

Successful response:

```json
{"result":"vm-cid-value","error":null,"log":""}
```

Error response:

```json
{"result":null,"error":{"type":"Bosh::Clouds::CloudError","message":"...","ok_to_retry":false},"log":""}
```

The `ok_to_retry` field tells the Director whether the call may be re-driven with identical arguments. Storage-lock timeouts and transient transport faults surface with it set; permission failures (HTTP 403), snapshot guard rejections, and VMID exhaustion are terminal. The Director acts on the flag for `create_vm` only, where a transient failure arrives as `Bosh::Clouds::VMCreationFailed` and the create step retries up to its `max_vm_create_tries` budget (default 5). Transient failures from every other method arrive as `Bosh::Clouds::CloudError` with the flag attached for operator visibility, because the Director has no retry loop for those methods.

### Log level

`pve.log_level` is config-only — no runtime environment variable override exists. Valid values are `debug`, `info`, `warn`, and `error`; the default is `info`. Log output is JSON-formatted (slog); each entry carries the `request_id` and `method` from the active RPC context.

To enable verbose logging, set the level in your deployment manifest and redeploy:

```yaml
properties:
  pve:
    log_level: debug
```

### Agent modes

`pve.agent_mode` controls how the CPI delivers BOSH agent settings to a newly created VM.

| Mode | Mechanism | When to use |
|---|---|---|
| `cloudinit` (default) | Builds an ISO 9660 ConfigDrive labeled `config-2`, uploads to `iso_storage`, attaches on `scsi30` | All standard deploys |
| `noagent` | Skips agent delivery | Specialised workloads that do not run the BOSH agent |
| `auto` | Always selects ConfigDrive/cloudinit regardless of stemcell API version | Manifests that want explicit future-proof mode selection |

For `cloudinit`, the ISO path on PVE storage is `<iso_storage>:iso/vm-<vmid>-config.iso`. `iso_storage` must be a file-based storage type (`dir`, `nfs`, or `cifs`) — block-based types (`lvm`, `lvmthin`, `zfspool`) cannot accept ISO uploads. The default value is `local`.

To confirm an ISO is present:

```bash
pvesm list local --content iso | grep config.iso
# or directly on a dir-type local storage:
ls /var/lib/vz/template/iso/ | grep config.iso
```

---

## PVE-side diagnostics

### VM state

```bash
qm status <vmid>       # running / stopped / unknown
qm config <vmid>       # full VM config: SCSI slots, NICs, tags
qm list                # all VMs on node with status
```

### Storage

```bash
pvesm status                                    # all storage pools: active/inactive, free/used
pvesm status -storage <name>                    # single pool
pvesm list <storage>                            # all volumes
pvesm list <storage> --content iso              # ISO files (ConfigDrive location)
pvesm list <storage> --content import           # stemcell qcow2 import files
```

See [PVE Storage Locking](pve-storage-locking.md) for lock mechanics and the retry strategy.

### Storage capacity: utilization bands and the CPI ceiling gate

`pvesm status` (above) reports raw free/used bytes per pool. Those numbers alone do not say when a pool is becoming risky — that depends on the storage backend's behavior as it fills, and on any external watermarks (like Ceph's) already in play on the same pool.

**Copy-on-write pools degrade progressively as they fill.** qcow2, thin-provisioned LVM, and ZFS all allocate space on write rather than up front, and their allocation/fragmentation cost grows with occupancy:

That "allocate on write" behavior for ZFS assumes the pool's `sparse` flag is on — PVE's own zfspool storage default is `sparse` unset (thick): every zvol the CPI creates there reserves its full requested size immediately, the same as a thick LVM pool, not on demand. A pool that looks thin by nominal capacity can already be fully committed by reservation alone, which throws off the utilization-pct headroom math below. The CPI logs this once per pool per process (Info level, naming the pool) the first time `create_disk` resolves a thick-provisioning zfspool while it already has the storage type in hand — see `pvesm status <storage>` (or check `sparse` in `/etc/pve/storage.cfg`) to confirm a given pool's actual posture, and the pool's zfspool config to switch it to `sparse: 1` if thin provisioning is what you want.

- Below roughly 50% utilization: normal.

- From roughly 50%: allocation and fragmentation overhead becomes noticeable — new writes and snapshot operations get measurably slower.

- By roughly 80%: degradation is severe — thin-provisioning metadata operations, snapshot merges, and qcow2 cluster allocation all compete for shrinking contiguous free space.

- At 100% (pool full): the backend stops accepting writes. Guests on that pool see I/O errors or hang. Recovery — freeing enough contiguous space for the backend to resume normal allocation — can take **days**, not minutes, especially on ZFS and thin-LVM where reclaiming space from deleted blocks is itself a background process.

**Ceph enforces its own watermarks on top of this.** A Ceph-backed pool (rbd) separately applies OSD-level watermarks, by default:

- `nearfull` at 85% — Ceph logs a cluster health warning; still accepts writes.

- `backfill-full` at 90% — Ceph refuses new backfill/rebalance operations on the affected OSDs.

- `full` at 95% — Ceph refuses new writes cluster-wide on the affected pool.

Ceph's defaults leave comparatively little headroom between "healthy" and "writes refused" — a few percentage points of overshoot on a large cluster can be observed and absorbed, but the same overshoot on a small cluster (few OSDs, little averaging) can jump past a watermark in a single large `create_vm` or `create_disk` burst. A stricter CPI-side ceiling (e.g. 70-75%) is sensible on small clusters for exactly this reason.

**The CPI ceiling gate (`pve.storage.max_utilization_pct`) is a proportional early-warning band that sits below both of the above.** Set it below the CoW "badly degraded" band and below Ceph's `nearfull` watermark — the recommended operational value is 80 — so the CPI's own placement, disk-creation, and resize logic reacts before either failure mode engages, rather than after. It checks the SAME storage status `pvesm status` reports (used/total bytes) at four points:

- **create_vm placement** — a candidate node is rejected (enforce) or logged (warn) when its `vm_storage` pool would cross the ceiling after adding the new VM's disk footprint.

- **create_disk** — the resolved disk pool is checked before allocation.

- **resize_disk** — the resize delta is checked before the resize call.

- **snapshot_disk** — Warn-only regardless of mode: snapshot growth is unbounded (a snapshot's actual disk cost depends on how much the guest subsequently writes, which cannot be known ahead of time), so this only warns when the pool is already above the ceiling; it never blocks a snapshot.

**This gate composes with, and is independent of, the absolute-bytes headroom filter** (`pve.placement.reserve_storage_headroom` / `pve.placement.storage_headroom_mb`, create_vm placement only). The two answer different questions and can both be active on the same deployment:

- The headroom filter asks: "does this node have at least `disk footprint + fixed margin` bytes free, right now?" — a fixed-byte floor, useful regardless of how large the pool is.

- The utilization-pct gate asks: "would this operation push the pool's fill fraction past a proportional ceiling?" — scales with pool size and tracks the degradation bands and Ceph watermarks above, which are themselves proportional.

A large pool can pass the headroom filter (plenty of bytes free) while still crossing an 80% utilization ceiling; a small pool near its ceiling might fail the headroom filter first. Running both catches both cases.

Diagnostics:

```bash
pvesm status                       # current used/avail/total per pool — same data the gate reads
journalctl -u pvedaemon --since "1 hour ago" | grep "utilization ceiling"
                                    # CPI Warn/error entries name the pool, node, projected
                                    # percentage, and configured ceiling
```

See [Configuration Reference](configuration.md) for the `pve.storage.*` property table and [DLB-Aware Placement](dlb-aware-placement.md) for how this gate interacts with placement scoring and the absolute headroom filter.

### Task logs

```bash
pvesh get /nodes/<node>/tasks --limit 50          # recent task list
pvesh get /nodes/<node>/tasks/<upid>/log          # log for a specific UPID

# Filesystem task logs
ls -lt /var/log/pve/tasks/

# Grep by VMID
grep -r "vm <vmid>" /var/log/pve/tasks/ 2>/dev/null | tail -20
```

### Storage locks

Per-storage lockfiles live at `/var/lock/pve-manager/pve-storage-<storage-name>`.

```bash
ls /var/lock/pve-manager/    # list all current lockfiles
```

A lockfile present after all PVE tasks complete is stale. See [Unsticking a locked storage](#unsticking-a-locked-storage) for the safe removal sequence.

### Daemon health

```bash
# Worker counts
ps -ef | grep '[p]vedaemon worker' | wc -l
ps -ef | grep '[p]veproxy worker'  | wc -l

# Configuration
grep MAX_WORKERS /etc/default/pvedaemon /etc/default/pveproxy

# Service status
systemctl status pvedaemon pveproxy --no-pager | grep -E '(Active|Main PID)'

# Daemon logs from recent window
journalctl -u pvedaemon --since "1 hour ago" | grep -E "worker|error|WARN"
journalctl -u pveproxy  --since "1 hour ago" | grep -E "error|WARN"
```

The default `MAX_WORKERS` is 3 for both daemons. For Cloud Foundry-class deploys (10–40 concurrent `create_vm` calls), target 6–8. See [PVE Host Tuning](pve-host-tuning.md) for sizing guidance.

### Network and bridge

```bash
ip link show vmbr0                               # bridge exists and is UP
pvesh get /nodes/<node>/network                  # PVE-side network config

# SDN (the default network mode)
pvesh get /cluster/sdn/vnets
pvesh get /cluster/sdn/zones
```

### SDN VXLAN operations

With the vxlan default, the CPI creates the turnkey zone `bosh` on first `create_network` and every vnet inside it is a cluster-wide overlay segment. The commands below inspect and verify that fabric.

```bash
# Zone and vnet state, including pending (staged, unapplied) changes.
pvesh get /cluster/sdn/zones --pending 1
pvesh get /cluster/sdn/vnets --pending 1
pvesh get /cluster/sdn/vnets/<vnet>/subnets

# Commit staged SDN changes by hand (the CPI does this itself and awaits
# the task; manual apply is for recovering from an interrupted operator edit).
pvesh set /cluster/sdn

# Per-node realization: every vnet becomes a same-named bridge on each node.
ip -d link show <vnet>          # shows the bridge and its derived MTU
bridge fdb show | grep 4789     # VXLAN forwarding entries toward peer nodes
```

Firewall prerequisites between all cluster nodes (and any dedicated underlay interfaces named in `pve.sdn_vxlan_peers`):

- UDP 4789 node-to-node — the VXLAN tunnel itself. Blocked 4789 leaves same-node VM traffic working and cross-node traffic dead; see [Troubleshooting — cross-node VXLAN](troubleshooting.md#cross-node-vm-traffic-dead-same-node-fine-vxlan).

- TCP 179 (BGP) — only for opt-in EVPN zones, between nodes and their route reflectors/controllers. The EVPN controller and its peering are operator infrastructure; the CPI never creates them. See the PVE SDN documentation for route-reflector topology.

MTU verification — the overlay MTU must be the underlay MTU minus roughly 50 bytes of VXLAN encapsulation (PVE derives 1450 from a 1500 underlay automatically, and the CPI hands each virtio NIC `mtu=1` so guests inherit it):

```bash
ip -d link show <vnet>                 # expect 1450 on a 1500 underlay
# From a guest: largest non-fragmenting payload on a 1450 path is 1422.
ping -M do -s 1422 <peer-vm-ip>        # must pass
ping -M do -s 1472 <peer-vm-ip>        # must fail with "message too long"
```

If small packets pass and large packets hang instead of failing cleanly, see [Troubleshooting — SDN MTU](troubleshooting.md#small-packets-pass-large-packets-hang-sdn-mtu).

Throughput between nodes, once ICMP and MTU both check out:

```bash
# On the receiving node:
iperf3 -s

# On the sending node:
iperf3 -c <receiving-node-ip> -t 10
```

A large gap between this cross-node figure and a same-node `iperf3` run (VM-to-VM on one node, no VXLAN encapsulation) points at the underlay link or its MTU rather than at SDN itself.

### SDN VLAN operations

With `sdn_zone_type: vlan`, each managed network becomes one vnet carrying one 802.1Q VLAN ID, realized as a per-node bridge over the underlay named in `pve.network_bridge`. The tag lives on the vnet — VMs attach by bridge name alone — so VLAN membership audits reduce to one command.

Pre-flight, before the first VLAN deploy — unlike VXLAN, the VLAN path depends on physical-fabric configuration the CPI cannot create or verify:

```bash
# 1. The underlay bridge exists on EVERY node and is VLAN-aware.
grep -A3 "iface vmbr0" /etc/network/interfaces
# expect: bridge-vlan-aware yes
#         bridge-vids 2-4094        (or at least the VLAN IDs in use)

# 2. The switch ports feeding each node's underlay uplink trunk the VLANs
#    in use (802.1Q trunk mode; check the switch, not the node).
```

Verification after `create_network`:

```bash
# Zone carries the underlay bridge; each vnet carries its VLAN ID as "tag".
pvesh get /cluster/sdn/zones --pending 1
pvesh get /cluster/sdn/vnets --pending 1

# Per-node realization: the vnet is a bridge named after itself.
ip -d link show <vnet>

# Tagged frames actually leave the node (run while pinging from a VM on the vnet):
tcpdump -ni <underlay-uplink> -e vlan <id>
```

A vnet whose traffic dies at the node boundary while same-node VM-to-VM traffic works is almost always a switch trunk or `bridge-vlan-aware` gap — see [Troubleshooting — VLAN vnet](troubleshooting.md#vm-on-a-vlan-vnet-has-no-connectivity).

VLAN ID hygiene: explicit `cloud_properties.vnet_tag` values are the recommended path (IDs come from the fabric's VLAN plan); the auto-allocation band for vlan zones defaults to 2000–2999 and must stay within the 4094 cap. Reserve the band in the fabric's VLAN plan if auto-allocation is used at all.

### Memory ballooning

The CPI writes `balloon: 0` on every VM and template it creates, so the balloon device is disabled by default — BOSH sizes VMs deterministically, and auto-ballooning would reclaim guest memory beneath the Director's assumptions. Verify on any CPI-created VM:

```bash
qm config <vmid> | grep '^balloon'
# balloon: 0
```

To enable auto-ballooning fleet-wide, set `pve.balloon` to a MiB floor; per instance group, set `cloud_properties.balloon`. The sentinel `pve-default` restores PVE's own default (device enabled, balloon = memory). Changes apply on VM recreation, not in place.

Before enabling, review the cluster's overcommit posture: auto-ballooning only relieves pressure when host memory is actually oversubscribed, and a ballooned guest whose BOSH jobs were sized to the manifest's full `memory` value can OOM under reclaim. If ballooning is enabled anywhere, also check KSM (`ksmtuned`) settings — both mechanisms trade guest performance for host headroom, and stacking them multiplies the reclaim pressure.

### Backups (PBS) interplay

Several CPI operations power-cycle a guest: `delete_vm`, a hard reboot (`pve.reboot_mode: hard`, or the soft-reboot fallback to it), and any Director recreate — including a stemcell update, which the Director always implements as delete-then-create rather than an in-place change. Each of these restarts the guest's QEMU process. Proxmox Backup Server's incremental backups depend on a dirty-bitmap tracked inside that running QEMU process, so a power cycle always drops it — the next PBS backup after one re-reads the full disk instead of only the blocks that changed, regardless of how small the actual change was.

Schedule PBS backup windows after a deploy has settled, not during one: a deploy that recreates several instances forces a full re-read for each instance that lands mid-window, inflating both the backup's duration and the read load the backend storage sees. The PBS task log makes the shift visible directly — `dirty-bitmap status: created new` in a guest's backup task log means that run was a full re-read; `dirty-bitmap status: OK` means it stayed incremental.

On a shared cluster where PBS also backs up hand-provisioned VMs alongside CPI-managed ones, the CPI's own provenance markers make backup-job selection precise: every CPI-managed VM carries the `director--` tag (see [Post-deploy verification](#post-deploy-verification) for the `pvesh` filter), and CPI-managed VMs occupy dedicated VMID bands — `100`–`8999` for VMs, `30000`–`30999` for stemcell templates — that a PBS backup job's VMID-range selector can target directly. See [Configuration — VMID ranges](configuration.md#vmid-ranges) for the full band table.

---

## Health checks

### Pre-deploy checklist

**PVE API smoke tests** — all endpoints must return HTTP 200. From the [PVE API Permissions](pve-api-permissions.md#5-verification) verification section:

```bash
BOSH_TOKEN='PVEAPIToken=bosh@pve!bosh-cpi=<secret>'
PVE_HOST='<pve>:8006'

curl -sk -o /dev/null -w "cluster/status: %{http_code}\n" \
  -H "Authorization: $BOSH_TOKEN" https://$PVE_HOST/api2/json/cluster/status

curl -sk -o /dev/null -w "cluster/resources: %{http_code}\n" \
  -H "Authorization: $BOSH_TOKEN" \
  "https://$PVE_HOST/api2/json/cluster/resources?type=vm"

for s in <vm_storage> <disk_storage> <stemcell_storage> <iso_storage>; do
  curl -sk -o /dev/null -w "storage/$s: %{http_code}\n" \
    -H "Authorization: $BOSH_TOKEN" https://$PVE_HOST/api2/json/storage/$s
done
```

**Storage health:**

```bash
pvesm status                                          # all pools active
pvesm status -storage <stemcell_storage>              # adequate free space
pvesm list <stemcell_storage> --content import        # import content type works
```

**Bridge existence:**

```bash
ip link show <network_bridge>
pvesh get /nodes/<node>/network | jq '.[] | select(.iface == "<bridge>")'
```

**pvedaemon and pveproxy workers:**

```bash
grep MAX_WORKERS /etc/default/pvedaemon /etc/default/pveproxy
ps -ef | grep '[p]vedaemon worker' | wc -l
systemctl is-active pvedaemon pveproxy
```

**Storage lock clean:**

```bash
ls /var/lock/pve-manager/    # should be empty or only ephemeral activity
```

**VMID range headroom:**

```bash
pvesh get /cluster/resources --type vm | jq '[.[].vmid] | length'
# Compare to (vmid_range_end - vmid_range_start) from cpi.json
```

**`iso_storage` must be file-based:**

```bash
pvesh get /storage/<iso_storage> | jq '.data.type'
# Must be "dir", "nfs", or "cifs" — not "lvm", "lvmthin", or "zfspool"
```

**`stemcell_storage` must be file-based and shared (multi-node clusters):**

```bash
pvesh get /storage/<stemcell_storage> \
  | jq '{type:.data.type, shared:.data.shared, content:.data.content}'
# type: dir/nfs/cifs/glusterfs/cephfs
# shared: 1 on multi-node clusters
# content: must include "import"
```

See [Configuration — Stemcell Storage](configuration.md#stemcell-storage) for the full constraint matrix.

### Post-deploy verification

```bash
# Director reachable
bosh env

# All VMs healthy — no "unresponsive agent"
bosh vms --all

# All instances and processes healthy
bosh -d <deployment> instances --ps

# No cloud-check problems
bosh -d <deployment> cloud-check

# PVE side — all BOSH VMs running
pvesh get /cluster/resources --type vm \
  | jq '.[] | select(.tags != null and (.tags | contains("director--"))) | {vmid:.vmid, name:.name, status:.status}'
```

### Retry log interpretation

Watch these patterns in `/var/vcap/sys/log/bosh/cpi/pve.log`:

| Pattern | Meaning | Action |
|---|---|---|
| `pve: storage lock timeout, retrying ... attempt=N` | Normal if N < 5 | If N > 5 routinely, split storages or throttle the director |
| `pve: transient transport fault, retrying` | Normal pvedaemon worker cycle | If frequent, raise MAX_WORKERS |
| `"type":"Bosh::Clouds::VMCreationFailed","ok_to_retry":true` | Transient `create_vm` failure; the Director retries the create | Monitor frequency |
| `"type":"Bosh::Clouds::CloudError","ok_to_retry":true` | Transient fault from a method the Director does not retry | Retry the deploy; investigate if frequent |
| `"type":"Bosh::Clouds::CloudError","ok_to_retry":false` | Terminal — operator action needed | See [Troubleshooting](troubleshooting.md) |

Storage-lock retries use exponential backoff: 2 s × 1.5^n with ±30% jitter, capped at 30 s, for up to 10 attempts. Transient transport retries use 1 s × 1.5^n capped at 15 s for up to 8 attempts. See [PVE Storage Locking](pve-storage-locking.md) and [PVE Host Tuning](pve-host-tuning.md) for structural remediation.

---

## Recovery procedures

### Reconciling with cloud-check

`bosh cloud-check` calls `has_vm`, `has_disk`, and `get_disks` on the CPI to reconcile the Director database against the actual PVE state. See [CPI Methods Reference](cpi_methods.md#has_vm) for what each method queries. The Director then offers remediation: deleting orphaned records, recreating missing VMs, or reattaching disks.

```bash
# Interactive — prompts for each detected problem
bosh -d <deployment> cloud-check

# Automatic — applies the safest available remediation
bosh -d <deployment> cloud-check --auto
```

Run `cloud-check` after any failed deploy or partial cleanup before attempting a fresh `bosh deploy`.

### Identifying orphans

```bash
# BOSH VMs carry director--, deployment--, instance-group--, job--, and index-- tags
pvesh get /cluster/resources --type vm \
  | jq '.[] | select(.tags != null) | {vmid:.vmid, name:.name, tags:.tags}'

# Persistent-disk volumes use synthetic VMIDs 9000–29999
pvesm list <disk_storage> | grep '^vm-'

# Stemcell import files
pvesm list <stemcell_storage> --content import | grep bosh-stemcell

# ConfigDrive ISOs
pvesm list <iso_storage> --content iso | grep config.iso
```

### Cleaning up orphaned VMs

**Danger: `qm destroy --purge` is irreversible and destroys every disk volume referenced in the VM config, including `unusedN` slots. Always inspect the config before running it.**

Persistent disks use synthetic VMIDs 9000–29999 and survive `qm destroy` only if they are absent from the VM config at destroy time (that is, if `detach_disk` has already removed them). If a VM was torn down without a proper `detach_disk`, persistent disks may still appear in `unusedN` slots and will be deleted by `--purge`.

Inspect before destroying:

```bash
qm config <vmid> | grep -E '^(scsi|unused)[0-9]+:'
```

Identify any `vm-9NNN` volumes in the output. If they belong to a still-tracked persistent disk, detach them via BOSH or `bosh cck` first, or copy the volume ID for manual preservation before proceeding.

Then destroy:

```bash
qm stop <vmid> --skiplock 1 2>/dev/null
qm destroy <vmid> --purge 1
```

### Cleaning up orphaned disks

Identify candidates:

```bash
pvesm list <disk_storage> | grep 'vm-9'
```

**Danger: verify a disk is not referenced by any VM config before removing it.**

```bash
grep -r 'vm-9NNN' /etc/pve/nodes/*/qemu-server/*.conf
# Proceed only if the above returns no output.
```

For LVM backends:

```bash
lvremove -f <vg-name>/vm-9NNN-disk-0
```

For ZFS backends:

```bash
zfs list -t all | grep vm-9NNN
zfs destroy <pool>/vm-9NNN-disk-0
```

For `dir` or NFS backends:

```bash
pvesm free <storage>:images/vm-9NNN-disk-0.qcow2
```

### Cleaning up orphaned stemcells and ConfigDrive ISOs

**Danger: `pvesm free` permanently deletes the volume. Confirm the stemcell is absent from the Director's state and that no running VM still uses the ISO before freeing it.**

A stemcell import is a single qcow2 image. List it and confirm the Director no longer references the stemcell (`bosh stemcells`) before freeing:

```bash
pvesm list <stemcell_storage> --content import | grep bosh-stemcell
pvesm free <stemcell_storage>:import/<filename>.qcow2
```

ConfigDrive ISOs belong to a specific VM. Confirm that VM is gone (or no longer references the ISO via `qm config <vmid>`) before freeing:

```bash
pvesm list <iso_storage> --content iso | grep config.iso
pvesm free <iso_storage>:iso/vm-<vmid>-config.iso
```

### Unsticking a locked storage

PVE's storage lock timeout is 30 seconds and is not user-configurable. A lockfile that persists after all PVE tasks complete is stale — typically left behind by a kernel OOM or process crash.

**Danger: removing an active lockfile corrupts the storage manager's serialisation guarantee. Only remove it after confirming no PVE task is running that holds it.**

Step 1 — confirm no running tasks:

```bash
pvesh get /nodes/<node>/tasks | jq '[.[] | select(.status=="running")]'
# Proceed only if the output is an empty array: []
```

Step 2 — confirm the file has no flock holder:

```bash
flock -n /var/lock/pve-manager/pve-storage-<name> echo "free" \
  || echo "locked by PID $(lsof /var/lock/pve-manager/pve-storage-<name> 2>/dev/null | awk 'NR>1{print $2}')"
# Proceed only if "free" is printed.
```

Step 3 — remove the stale lockfile:

```bash
rm /var/lock/pve-manager/pve-storage-<name>
```

See [PVE Storage Locking](pve-storage-locking.md) for full lock mechanics.

---

### Resetting the endpoint cache

We reset the cache when a finding tells us to, such as a cluster identity change that the CPI refused under `verify_ssl: false`, or when a node's certificate changed and the stored pins no longer fit. The files are plain JSON that we can read as `vcap`. The cache files hold no credential, although the directory secret that signs them does. A reset costs one rediscovery and nothing else, so it is safe to do on a live Director. We should expect the directory even when discovery is off. A pool with more than one endpoint and a usable cache directory writes the directory, its secret file, and the down-mark cache files, and a pool with a single endpoint writes nothing.

First we find the directory. The CPI chooses it in three layers, and the first layer that applies wins.

1. The value of `pve.endpoint_cache_dir`, when we set it.

2. The `endpoint-cache` directory beside `pve.storage_allocation_journal_dir`. With the documented journal path `/var/vcap/store/pve_cpi/allocations`, that is `/var/vcap/store/pve_cpi/endpoint-cache`. The job's pre-start script creates it, and the CPI creates it again at run time when the process runs as the journal directory's owner and the parent belongs to that user and sits on the journal's mount. Choosing this layer turns on the allocation journal, though, because setting the journal directory does. The journal then also needs `pve.storage_placement_namespace` and an enrolled namespace, and without them `create_vm` and `delete_vm` refuse. For a durable cache without a journal, we set `pve.endpoint_cache_dir` instead.

3. The `pve_cpi-endpoint-cache.<uid>` directory under the temporary directory, which the CPI creates itself.

A Debug line at the start of a call names the choice, in the form `pve endpoints: cache directory <path>, beside the allocation journal directory`. When the call created the directory, the line ends with `, created by this call`. We set `pve.log_level: debug` to see it, and `pve-ops ha` judges the same layers without creating anything.

The directory holds these files, and the first two kinds belong to the endpoint cache.

- `<host>-<16 hex characters>.json`
  The cache for one cluster. The name carries the first configured endpoint's host and a digest of the endpoint list, the TLS settings, the CA, and the credential. It holds the discovered nodes and their pins, the cluster identity, the down marks, and the cold-start hint.

- `<host>-<16 hex characters>.lock` and `<host>-<16 hex characters>.discover.lock`
  The lock files that serialize writes and discovery for that cluster.

- `ha_profile-<host>-<16 hex characters>.json` and its lock files
  The HA profile cache, which holds slow-changing facts for 60 seconds and has nothing to do with endpoints.

- `ha-recovery-episodes.json` and `ha-recovery-episodes.lock`
  The HA recovery episode record, which the CPI writes even when discovery is off.

- `ha-recovery-deferrals.json` and `ha-recovery-deferrals.lock`
  The HA recovery deferral record, which holds the last time a `delete_vm` deferred each VM for fence, recovery, an unknown HA state, or lost quorum, so a later delete can let a VM that PVE HA restarted settle.

- `spread-claims.json` and `spread-claims.lock`
  The spread claim record. While a `create_vm` with `pve.placement.anti_affinity.enabled` runs, it holds a claim here on the node it picked, so a sibling that scores the nodes at the same moment counts it. The create gives its claim up when `create_vm` returns. On Linux a claim stops counting once the process that made it has exited, but only when that process ran in the same boot and the same PID namespace as the create that reads the claim. A claim from another PID namespace, such as another container, or from another host that shares the directory, lapses at its create's deadline, and so does every claim that a crash leaves behind on a host other than Linux. Removing the file costs only the spread of the creates that are running.

- `secret` and `secret.lock`
  The directory secret that signs the files. It never goes into a support bundle or a ticket.

To reset only the endpoint cache, we remove the files whose names start with the cluster's host and end in `.json`, `.lock`, or `.discover.lock`, and we leave the rest alone. To reset everything, we remove every file in the directory but not the directory itself.

```bash
# Run as root on the Director, with the directory from the Debug line or pve.endpoint_cache_dir.
dir=/var/vcap/store/pve_cpi/endpoint-cache
ls -l "$dir"
rm -f "$dir"/pve1.example.com-*.json "$dir"/pve1.example.com-*.lock "$dir"/pve1.example.com-*.discover.lock
```

We delete the files and keep the directory. When we remove one of the first two directories, the CPI creates it again on the next call only when [the creation rule](endpoint-failover.md#when-the-cpi-creates-a-missing-directory) allows it. Otherwise a Director that loses its `endpoint-cache` directory falls through to the temporary layer for every call, and the cache stays out of the place we chose until the next pre-start script runs and creates the directory again. Deleting the temporary directory is harmless, because the CPI creates it on the next call.

What the CPI does next depends on what we removed.

- The endpoint cache file
  The next call starts with the configured endpoints and no discovered nodes, down marks, or cold-start hint. A call that has more than one configured endpoint writes down marks again as soon as it meets a dead endpoint, even with discovery off. When discovery is on, the first configured endpoint to answer finds no file, so the CPI discovers in the background and writes a fresh file before the process exits, waiting up to 3 seconds for the write. The calls after that start with the discovered nodes. Under `verify_ssl: false` the first discovery is trust on first use again, so we reset only when we trust the network path to the seed.

- The directory secret
  The CPI creates a new one on the next call. A cache file's name depends on the secret, so the endpoint cache starts a new file under a new name, and the old file ages out after 30 days. The HA recovery episode record keeps its name, fails verification, and starts empty. The log shows `endpoint cache: replaced the directory secret, which had the wrong length` only when a secret exists with the wrong length, and not after a plain removal.

- The HA recovery episode record
  A VM that is in HA recovery restarts its grace window. That delays the handover to BOSH and breaks nothing else.

- The HA recovery deferral record
  A delete that reaches a VM which PVE HA has just restarted no longer waits for it to settle, which is how earlier releases behaved. The next deferral for fence or recovery starts the record again.

- A file that is damaged, too large, or written by another release
  The CPI ignores it, logs `endpoint cache: the cache file is not usable and is ignored` at Debug under `auto` and at Warn under `on`, and replaces it on the next write. We do not need to delete it by hand. A file with the wrong owner, a group or other permission bit, or a symbolic link in its place is different. The CPI runs that call without discovery and does not replace the file, so we remove it by hand.

The CPI also removes cache files that nobody has touched for 30 days when it writes, so a changed endpoint list or CA leaves nothing behind for long.

## PVE HA

This section is for operators who set `pve.ha.mode` to `auto` or `managed`, and for anyone who finds HA objects on a cluster after switching back to `off`. [HA and Resurrection](ha-and-resurrection.md) explains the model, and [Configuration, HA](configuration.md#ha) lists the properties. The commands below run on a PVE node as root unless they say otherwise.

### Raise `agent_timeout` before turning HA on

Before we set `pve.ha.mode` to `auto` or `managed`, we raise the Director's `hm.intervals.agent_timeout` to 300 seconds, which is above the two to four minutes PVE HA takes to fence a failed node and start its VMs elsewhere. With the default of 60 seconds, the resurrector starts its retries before PVE HA has finished, and its `delete_vm` defers on each one. The CPI lets a VM that PVE HA restarts settle for five minutes before a retried delete may proceed, as [After PVE HA restarts the VM](ha-and-resurrection.md#after-pve-ha-restarts-the-vm) describes, so an agent that answers in that time keeps the VM. A longer timeout spares that loop in the common case. It does not cover an outage that outlasts it, such as a `hold` VM's node that stays down until we bring it back, and there the settle wait is what keeps the restarted VM. [HA and Resurrection](ha-and-resurrection.md#set-agent_timeout-above-the-ha-recovery-time) gives the timing, and [A VM that PVE HA recovered is deleted and recreated](troubleshooting.md#a-vm-that-pve-ha-recovered-is-deleted-and-recreated) has the ops file.

### Reading the cluster with `pve-ops ha`

The `pve-ops ha` subcommand is a read-only doctor. It never writes to PVE, and it runs in every mode, `off` included. There are five exceptions. The first is `--widen-replicas --apply`, which runs the CPI's own replica widening pass instead of the doctor, as [Widening replica pins after a deploy](#widening-replica-pins-after-a-deploy) describes. The second is `--fix-anti-affinity --apply`, which writes the rules of each anti-affinity group whose rules differ from the layout the CPI plans, as [Fixing an anti-affinity layout](#fixing-an-anti-affinity-layout) describes. The third is `pve-ops ha rebalance --apply`, which live-migrates this Director's HA VMs back into a good spread, as [Rebalancing HA VMs after node maintenance](#rebalancing-ha-vms-after-node-maintenance) describes. The fourth is `pve-ops ha drain <node> --apply`, which stops each VM that can't leave a node in maintenance, through PVE HA. The fifth is `pve-ops ha undrain <node> --apply`, which starts those VMs again and then runs the rebalance, as [Draining and undraining a node with `pve-ops ha drain`](#draining-and-undraining-a-node-with-pve-ops-ha-drain) describes. The doctor itself runs the rebalance as a dry run to judge the spread, and that dry run writes nothing. Each of those five commands is a dry run until we add `--apply`. On the Director VM we run the subcommand with the full path, because `pve-ops` is not on `PATH`. The `--config` flag is optional there, because the command looks for the configuration in `--config`, then in `$PVE_CPI_CONFIG`, and then at `/var/vcap/jobs/pve_cpi/config/cpi.json`.

```bash
/var/vcap/packages/pve_cpi/bin/pve-ops ha
```

The command takes these flags.

- `--config PATH`
  This names the CPI configuration file. The rendered `cpi.json` is the usual value, and it is the default when neither the flag nor `$PVE_CPI_CONFIG` is set.

- `--request PATH`
  This names a CPI request, or a bare context object, whose context picks a cpi-config entry. The request's Director UUID also filters ownership markers, so the report skips another Director's rules and resources. Without a UUID, every marker counts.

- `--cpi-config PATH`
  This names a BOSH cpi-config as JSON, and the command then checks every `pve` entry and the exports they share. The file holds either `{"cpis": [...]}` or a bare list of entries. It is not the rendered `cpi.json`, which describes one cluster and belongs to `--config`, and a file that holds the rendered keys gets an error that says so. A YAML cpi-config converts with `yq -o=json . cpi-config.yml > cpi-config.json`. The flags `--request` and `--cpi-config` cannot be used together. A BOSH cpi-config does not carry the Director's UUID, so we give it in a `director_uuid` key on an entry or on the document, or with `--director-uuid`, and the doctor uses it to skip another Director's markers, VMs, and replication jobs, the same way `--request` does. The entry's key wins over the document's key, and the document's key wins over the flag. An entry with no UUID is judged by its VM ID band alone, which means the doctor also judges a VM that another Director created inside that band. The doctor reports that once for the run rather than once for each entry. It gives a single advice finding, and that finding names every entry without a UUID. The finding goes on the report of the first such entry whose cluster the doctor could read, because an entry it couldn't read judged nothing, and when it read none of them, the finding goes on the first such entry. The doctor also logs one warning that names the same entries. When several entries of one Director reach the same cluster, the doctor reports each VM's findings once, on the first entry in the document whose VMID band holds the VM. It reports the Director's rules, HA resources, and rebalance findings once as well. A pin, a resource, or a planned move names one VM, so it goes to the entry that reports that VM, and one that no entry's band holds goes to the first of the Director's entries on the cluster, as does a rule that names no single VM, such as a group rule. The doctor passes over an entry whose cluster it couldn't read, so the next entry that holds the VM reports it instead.

- `--json`
  This prints the report as JSON.

- `--widen-replicas`
  This runs the replica widening pass in place of the doctor. Without `--apply` it is a dry run that claims and writes nothing. It needs the Director's UUID from `--request` or `--director-uuid`, and it cannot be combined with `--cpi-config`.

- `--apply`
  With `--widen-replicas`, this writes the widened pins.

- `--director-uuid UUID`
  With `--widen-replicas`, this names the Director whose VMs the pass may widen when no `--request` names it. When both are given, they must name the same Director.

The report starts with the profile line that the CPI logs, with the headroom result added at its end. It then lists the nodes, the CPU migration domains, and the findings, and it ends with a count.

```
ha: mode=auto effective=full topology=mixed pve=9.2.4 rules=yes crm=master@pve1 quorum=yes votes=3/3 survives_one=yes qdevice=no nodes=3/3 sys_console=yes crs=static rebalance_on_start=no headroom=ok(42%)
nodes:
  pve1 online votes=1 health=ok placeable=yes watchdog=armed pve=9.2.4 cpu=Cascadelake-Server
  pve2 online votes=1 health=ok placeable=yes watchdog=unknown pve=9.2.4 cpu=Cascadelake-Server
  pve3 online votes=1 health=ok placeable=yes watchdog=unknown pve=9.2.4 cpu=Cascadelake-Server
cpu migration domains:
  domain 1: Cascadelake-Server (flags aes,avx2) on pve1, pve2, pve3
findings:
  none
result: 0 problem(s), 0 advice
```

The node lines show what the node's HA agent reports. The `lrm=` part gives its mode and state, and `watchdog=armed` means the agent still holds its lock and runs its watchdog. A node that is draining for maintenance keeps its watchdog armed, with `lrm=mode=maintenance/state=active`. When the drain finishes, the agent enters the state `maintenance`, releases its lock, and closes the watchdog, and the line then reads `watchdog=idle` with `drain=complete`.

The headroom field reads `headroom=ok(42%)` when every single-node failure leaves memory free on the survivors, where the percentage is the smallest margin over all such failures. It reads `short(<node>)` or `short(<node>+N)` when a failure leaves a VM with nowhere to go. The name is the first node whose loss does that, and N counts the other nodes that are short. It reads `unavailable` when the CPI could not read what it needs. The number is the same memory-only estimate that `create_vm` uses, so a result of `ok` does not promise that every VM restarts.

Each finding is either a problem or advice. The command exits with 0 when the report holds no problem, with 1 when it holds a problem or a source it could not read, and with 2 for a usage error. An unreadable source counts as a problem with the kind `unchecked`, so a clean run proves that every check ran. The finding kinds fall into these groups.

- Cluster and quorum
  `observe_only`, `node_down`, `entry_clock_unreadable`, `quorum_fragile`, `qdevice_advice`, `advice`, `crs_mode`, and `notes_marker_near_miss`.

  The `notes_marker_near_miss` advice names a node and a line in its notes that looks like the maintenance marker but isn't one, such as `- tags: maintenance` or `tags = maintenance`. The CPI ignores such a line, so the node stays in placement. The advice says to write the line as `tags: <tag>` at the start of its own line, which is the form that takes the node out. The CPI logs the same line as a warning on each `create_vm` that reads the node's notes. Under `--cpi-config`, the advice appears once for each Director and cluster, on the Director's first entry on the cluster, because every entry of one Director on a cluster reads the same notes. When the doctor couldn't read that entry's cluster, the next entry gives the advice instead.

- Rules and resources
  `rule_problem`, `rule_unmarked`, `pin_missing`, `soft_pin_resource_affinity`, `anti_affinity_skipped`, `anti_affinity_split`, `anti_affinity_short`, `anti_affinity_ambiguous`, `aa_rule_full`, `aa_layout_drift`, `aa_hold_lapsed`, `aa_pin_restore`, `aa_pattern_trimmed`, `aa_pattern_blocks_drain`, `resource_ignored`, `managed_without_resource`, `failback_on_replica`, `failback_drift`, `rebalance_pending`, and `spread_skewed`.

  The four `aa_` kinds apply to groups whose rules or HA resources carry the CPI's group marker.

  The `aa_rule_full` problem names a rule that holds as many members as its domain has nodes, so a drain of one node leaves a member with no free node. It is advice instead when the group's pattern sets `drain_safe: false`. Its text names `pve-ops ha --fix-anti-affinity --apply`. The `aa_layout_drift` advice says that the group's rules differ from the layout the CPI plans, and its detail lists each write the plan would make, such as `update bosh-aa-<hash12> with vm:100,vm:101,vm:102`. It is a problem when the CPI could not plan the group at all. The plan treats a place that a delete holds for the recreate of its index as taken, so a live `hold=` record in the group's marker never reads as drift, as [HA and Resurrection](ha-and-resurrection.md#what-the-cpi-registers) explains. The `aa_hold_lapsed` advice names a hold that a rule or an HA resource still records after it expired or after a member with its index took the place. Such a hold keeps no place, and the group's next rule write drops it, as does the fix command. The `aa_pin_restore` advice names a member's pin that still records, in its `bosh-prio=` field, the priorities a join flattened, while the member is in no resource-affinity rule. The group's next rule write gives the priorities back, as does the fix command, whose dry run lists the write as `restore the priorities <nodes> on bosh-na-<vmid>`. The advice says so when a live hold of the group defers the restore, and it says the record is dropped instead when the pin changed after the join, as [HA and Resurrection](ha-and-resurrection.md#what-the-cpi-registers) explains. The `aa_pattern_trimmed` advice says that the pattern asks for rules that drain safety leaves out. The `aa_pattern_blocks_drain` advice says that the pattern sets `drain_safe: false`, so a drain of one node can leave a member with no node to move to. After an upgrade, a rule that an older release filled to the number of nodes shows as `aa_rule_full` until the group's next `set_vm_metadata` rewrites it or we run the fix command, as [Fixing an anti-affinity layout](#fixing-an-anti-affinity-layout) shows.

  A group from an older CPI, whose rules carry no group marker, still gets `anti_affinity_skipped`, `anti_affinity_split`, and `anti_affinity_short`, now judged against the cap of one fewer member than the domain has nodes. The `anti_affinity_short` text says that the member is missing until `set_vm_metadata` or `pve-ops ha --fix-anti-affinity --apply` adds it.

  The `failback_on_replica` problem names a `replicate` VM whose HA resource has `failback` on, because PVE would move such a VM back to its old node after a failover and run the reversed replication job, whose first run destroys the writes that existed only on that node. The `failback_drift` advice names any other resource of this Director whose `failback` differs from the value the CPI writes for it, which is on for a VM whose every volume sits on shared storage and off otherwise or when `pve.ha.failback` is `false`. The next `set_vm_metadata` or `reboot_vm` on the VM sets the flag, and so does `ha-manager set vm:<vmid> --failback 0` or `--failback 1`. For a VM that only one node can take, such as one whose strict pin names one node, one with a PCI passthrough device, or one with a host CPU type that one node alone runs, the CPI writes `failback` off, so the doctor reports such a VM when its flag reads on. The doctor skips a resource that the narrowing barrier holds, a resource whose fresh `bosh-failback-hold` token says an attach is moving the VM, and a VM whose config, storages, rules, or replication jobs it couldn't read. It also skips a VM whose HA decision fails or refuses HA for a reason that may pass, and parkers, which stay stopped and which no call ever reaches. A pin that records a change to the VM's replicated volumes counts as the `replicate` policy, as it does in the CPI. The doctor also skips a resource that carries a `bosh-rebalance-to` mark, because a rebalance holds its `failback` off on purpose. Under `--cpi-config`, the doctor also skips a VM whose flag is the one another entry on the same cluster and VMID band writes. No VM record names the entry that created it, so a band whose entries set `pve.ha.failback` differently reports only the flags that none of them writes.

  The `rebalance_pending` problem names a resource of this Director that carries a `bosh-rebalance-to=<node>` mark while no rebalance holds the run lock `bosh-lock-ha-rebalance`, which means a run stopped part way and left the VM's `failback` off. The finding names the `pve-ops ha rebalance --apply` command that finishes or undoes the move. When the doctor can't read the run lock, it reports the marks as `unchecked`. The `spread_skewed` advice appears when a dry run of the rebalance would move a VM back into its AZ, when it would make a separation move, or when the committed memory of the fullest node and the emptiest node lies more than 15 points apart. The separation phase makes two kinds of move. It moves a member of an anti-affinity rule off a node that holds another member of the same rule, and it moves a member of an anti-affinity group off a node that holds more of the group's members than its share. A group's share of a node is the number of members its rules name, divided by the number of nodes those members may run on, rounded up. So three members of one group on one node count even when no rule holds two of them, and the advice names both kinds of move. It states each reason only when it applies. It counts the VMs outside their AZ when there are any, counts the separation moves when there are any, gives the spread only when it lies above the start threshold, and always names the dry run command. The dry run lists at most 10 moves unless `--max-moves` raises that cap, so when the plan holds more, the advice gives the total and adds `--max-moves` with that total to the command, which then lists every move the advice counts. Under `--cpi-config`, each entry counts only the moves of the VMs it reports on, and only the Director's first entry on the cluster gives the memory spread. The command it names then adds `--request`, because the doctor plans with the entry's settings, and `pve-ops ha rebalance` takes an entry's settings from the context of a request. The doctor runs that dry run only when the request names a Director and its own reads of the HA resources, the service states, the rules, and the guests all succeeded, and it reports nothing when the dry run's own reads fail, so the advice never fails the run.

- CPU and local storage
  `cpu_unknown`, `cpu_host_multi_domain`, `cpu_pin_crosses_domain`, `cpu_domain_too_small`, `local_unprotected`, `hold_blocks_maintenance`, `maintenance_stranded`, and `recovery_stranded`.

  Under `pve.ha.local_policy: none`, a VM with node-local volumes and no HA resource is reported as `local_unprotected` advice and never as a problem, because the operator chose to run it without HA and a note is the right weight for that choice.

  The `maintenance_stranded` finding names each started HA VM that is still running on a node in maintenance, once per VM. It is a problem when the doctor can name a reason the VM can't leave. The doctor judges that against the nodes the HA manager would move the VM to, which are the other online nodes that are not in PVE HA maintenance. A node that carries only the maintenance marker in its notes still counts, and so does a node the CPI won't place on for its own reasons, because the HA manager knows nothing of either. The reason is one of three things. Either no other online node is out of HA maintenance, or a strict node-affinity pin names none of those nodes, or anti-affinity rules already hold a partner on every one of them that the pin allows. A partner is another member of an enabled negative resource-affinity rule that names the VM, so a member of the same group that sits in another rule, or in no rule at all, doesn't count, and neither does a partner that the HA manager reports stopped, disabled, or ignored. A soft node-affinity pin never appears as a reason, because PVE moves a soft-pinned VM to the next priority class once its node is in maintenance. When the VM is still on the node and the doctor can name no reason, the finding is advice that says PVE is expected to move the VM, and we read it again after the HA manager's next round before we act on it. A `hold` VM is named by `hold_blocks_maintenance` instead, and neither finding appears once the node's HA agent has finished draining.

  A VM with a `host` or `max` CPU type on a cluster with several CPU migration domains gets `cpu_host_multi_domain` as a problem when it runs under HA with no enabled node-affinity pin, because HA may then move it to a node whose CPU differs. Every text that suggests setting `pve.cpu_type` to a named model says to run `bosh recreate` afterward as well, because the CPI never changes the CPU type of an existing VM.

- Replication
  `replica_on_partner`, `replica_awaiting_sync`, `replica_pin_unshaped`, `replica_not_recoverable`, `replica_job_stale`, `replica_job_failing`, `replica_schedule_unjudged`, `replica_marker_encoded`, `replica_orphan_volume`, `replica_recorded_orphan`, `replica_gone_vm_volume`, `replica_superseded_copy`, and `replica_journal_unconfigured`.

- Headroom
  `headroom_short` and `headroom_unavailable`.

- Leftovers and gaps
  `birth_tag_stale`, `readd_tag`, `owner_tag_missing`, `off_mode_resource`, `off_mode_rule`, `parker_resource_orphan`, `parker_pin_orphan`, `guests_not_enumerated`, `cache_directory`, and `unchecked`.

  The `parker_resource_orphan` and `parker_pin_orphan` problems name an HA resource or a pin that carries this Director's parker marker and names a VMID in the parker band that no guest holds. A pin counts only when it is a node-affinity rule that names the parker's resource and nothing else. The doctor reports them only under the parked strategy, only when the request carries a Director UUID, and only when every node answered the guest listing and the cluster's VM list could be read. The parker sweep removes such a leftover once it proves the parker gone, so a finding that stays across sweeps means the sweep can't prove it. That happens when the cluster isn't quorate, when a member is offline, when a node the pin names doesn't answer, or when the HA profile doesn't let the CPI write HA state. Each finding names the `ha-manager` command that removes the leftover by hand.

The doctor doesn't send a per-node read that it already knows can't be answered, because a read addressed to a node that is gone waits out the retry budget, and the doctor makes one for each replication job, storage, and VM config. It skips a read when cluster status lists the node offline, and when a read of the node failed with a transport fault earlier in the same run. It reports each skipped read as `unchecked`, with a detail that reads `node <node> was not asked, because <reason>`. With `--cpi-config`, every entry that reaches the same cluster shares what the earlier entries learned. The doctor tells clusters apart by the stamp of the cluster's root CA, the same identity the CPI scopes its own per-cluster records by, and it falls back to a hash of the entry's endpoints when the stamp cannot be read. So two clusters that name their nodes alike do not share what each learned, and an entry with neither identity shares nothing. While the cluster has lost quorum, a guest listing that failed for one entry is reported again for the next entry on that cluster instead of being repeated. A failure that PVE answered, such as a missing privilege, doesn't mark the node. The doctor still writes nothing.

  The `cache_directory` advice covers the directory that holds the endpoint cache, the HA profile cache, and the HA recovery episode record. The CPI chooses it in layers, which are `pve.endpoint_cache_dir`, then `endpoint-cache` beside the allocation journal directory, then a per-user directory under the temporary directory that the CPI creates. The CPI also creates a missing directory of the first two layers when its parent belongs to the CPI's user and sits on the mount of a directory that persists, and the doctor says whether it would or names the reason it would not. The doctor asks the same question for each layer without creating anything, and it judges as the CPI's user, which it reads from the owner of the allocation journal directory, searching every directory the CPI would pass through. It reports an override that does not exist, is a symbolic link, is not a directory, or grants group or other access, and any layer that another user owns or that sits below a directory the CPI's user cannot search. It also says when it could not judge a layer. A refusal from a layer that applies leaves the CPI with no recovery record, so a grace window never starts and the dead-node handover cannot begin. A missing override that the CPI does not create sends the record to the next layer that works, and creating it later moves the record and restarts a running grace window once. The endpoint cache and the HA profile cache do not fall back this way. With a missing override they run that call without a cache, and discovery is off for it. [Troubleshooting](troubleshooting.md#the-ha-doctor-reports-a-cache-directory-problem) lists each refusal and its fix.

Entries of a `--cpi-config` document that share one cluster and one VMID band stamp the same `bosh-aa-<hash12>` group tags, and nothing on a VM says which entry made it. When those entries agree on whether they write anti-affinity rules, the doctor judges the groups as usual, or skips them when every entry has `pve.ha.anti_affinity` off. When they disagree, it reports one `anti_affinity_ambiguous` advice for the band, and the advice names the peer entries. The doctor does not judge a group that has no rule in that case, because an entry with rules off may have made it. It still reports `anti_affinity_short` for a group whose rule exists, since only an entry with rules on writes a rule. The group tag does not name a band, and an entry in another band may stamp the tag without writing rules, so a group whose members all lie in the bands of entries with rules off is not judged at all.

Each group is judged by one entry only, the entry whose placement it belongs to, because entries that share a cluster can map AZs differently and so plan one group over different nodes. The doctor gathers a group's members from the VMID bands of every entry on the cluster, so a group whose members lie in the bands of two entries is still one group, and the doctor judges it once, over all its members. It picks the judging entry from the entries that write anti-affinity rules, that serve the group's Director, and whose VMID band holds a member. It plans the group under each of those entries' settings, the way that entry's own run would, because the CPI writes a group's rules from the plan of the entry that placed it, so that entry's plan keeps the rules as they stand. An entry whose plan could be made wins over one whose plan could not, and then the entry whose plan deletes the fewest of the group's rules wins. On a tie it takes the entry whose `placement.az_map` holds the strict pins of the most members, then the one whose AZs that hold them are smallest, which is how a plan picks a member's domain. Next it takes the entry whose `placement.az_map` holds an AZ that names exactly the nodes the most members' soft pins prefer, because only an entry with that AZ and `pin_az` on writes such a preference, and a pin that a join flattened counts with the priorities its `bosh-prio=` record keeps. After that it takes the one whose plan writes the fewest changes, and then the first entry in the document. So a pin restore that waits for a group is reported on the entry that maps each node to an AZ of its own, and not on an earlier entry with no map whose plan writes the same. So when an entry pins a group to a two-node AZ, where the CPI writes no rule, an entry without that AZ in its map doesn't plan the group over the cluster's three nodes and doesn't report the rule it would create as `aa_layout_drift`. And a `hold` group whose members each carry a strict pin to a node of their own is judged by the entry whose plan holds the group's rule, even when another entry maps each of those nodes to an AZ of its own and would split the rule. When the judging entry's plan would still delete a rule that another of the group's entries plans, the entries map the group's nodes differently, and the doctor reports an `anti_affinity_ambiguous` advice that names the rule and the other entry. It doesn't judge the group's layout and doesn't offer the delete. We fix that by serving the group through one entry, or by giving the entries one `placement.az_map`. A group that no entry with rules on can claim is judged by the first entry in the document whose band holds one of its members, as described above.

Under `--cpi-config`, the doctor plans each group that carries the CPI's group marker with the `pve.ha.anti_affinity_pattern` default of the entries that write its rules, not with the default of the entry whose run reports it. Those entries are the ones on the same cluster that write anti-affinity rules and whose VMID band holds at least one of the group's members. Two entries are on the same cluster when they name the same `pve_host`, or when the clusters they reach have the same `pve-root-ca` stamp, which the doctor reads once for each entry before it checks any of them. So an entry that reaches the cluster through another node still counts. When the doctor can't read an entry's stamp, it falls back to that entry's host. The default matters only for a group that records no pattern, because a pattern recorded on a member's HA resource or on the group's rules wins over every default. Every member that this release creates records its pattern, and the CPI records the pattern on the rules of an older group the first time it plans that group. When the entries that write a group's rules set different defaults and the group records no pattern, the layout the CPI plans depends on which entry serves the call. The doctor then reports an `anti_affinity_ambiguous` advice for the group that names each entry with its default, and it doesn't judge the group's layout. We fix that by setting `ha_anti_affinity` on the group's `vm_type`, or by giving the entries one default.

The `entry_clock_unreadable` finding names the entry node, and it covers an entry node whose own clock read fails. A node does that while systemd shuts it down, and it also does it when `systemd-timedated` is broken. When the CPI borrowed another member's clock, the HA view is still good, so the finding is advice. The doctor reads the entry node's clock once more before it reports, so a one-off server error on a healthy node reports nothing. When no member's clock answered, the HA view is unreadable and the profile is observe-only, so the finding is a problem. In both cases we check `timedatectl` and `systemd-timedated` on the entry node, or we put another node first in `pve.endpoints`.

A `replicate` VM shows `replica_awaiting_sync` as advice while its job works but the pin has not yet widened. Once a sync is verified, the pin widens after the next CPI call that takes the HA rules lock, and the advice names `pve-ops ha --widen-replicas --apply` for widening it now. It shows `replica_not_recoverable` as a problem only when no usable job exists, the job is disabled, or the first sync is more than twice its schedule plus the clock-skew bound overdue. A schedule the doctor cannot measure, such as a weekday list, gets `replica_schedule_unjudged` and is never called stale or healthy. A `replicate` VM whose strict pin names only one node but still carries its `bosh-replica=` token gets `replica_pin_unshaped` as advice. The CPI never writes that shape, and the widen sweep leaves it alone because the token reads as a pin it did not narrow. The advice names the VM and the pin, and the repair is to remove the token from the pin's comment and then run `pve-ops ha --widen-replicas --apply`.

PVE percent-encodes a replication job's comment again each time one pvescheduler pass writes `replication.cfg` more than once, which happens when HA recovers several replicated guests onto one node. The CPI's marker `bosh-cpi:v1` then reads `bosh-cpi%3Av1` or `bosh-cpi%253Av1` through the API. The CPI and the doctor undo those rounds before they read the marker, so the job stays this Director's, and the doctor reports it as the advice `replica_marker_encoded`. The finding names the `pvesr update <job> --comment '<marker>'` command that writes the marker back in its plain form, with the comment quoted so a shell reads it as one word, which matters because an older CPI no longer recognizes the encoded one. The CPI writes the plain form back itself on the next `set_vm_metadata` or `reboot_vm` on the VM, whether or not an allocation journal is configured. It rewrites only the VM's own job, only when the job's decoded marker names this Director, and it changes nothing but the comment. When the allocation journal holds a new generation of the job that it has not confirmed yet, the CPI leaves the comment to the write of that generation, which also writes the plain form. BOSH sends `set_vm_metadata` on every deploy that touches the VM, and a job whose VM gets neither call keeps the encoded marker until it does, because the CPI adds no pass that rewrites other jobs. The CPI and the doctor undo up to 2048 rounds, which is more than a comment within PVE's 4096-character limit can carry, so even a failover that moves many replicated guests onto one node in one pass leaves the jobs readable as this Director's.

With `--cpi-config`, the command also reports three kinds of overlap. The first is overlapping VMID bands on one export. The second is a `next-id` range that overlaps a band or the `next-id` range of another cluster on the same export. The third is a `next-id` range that overlaps the cluster's own bands when another checked cluster mounts one of its exports. A possible shared backing, which is one export path behind different server strings, is advice and does not fail the run. The command exits with 1 for any failing finding. PVE's default `next-id` range of 100 to 999999 overlaps every band, so a cluster on a shared export keeps getting the finding until the operator sets a range outside the bands, as [Shared-storage rules](multi-cluster.md#shared-storage-rules) says.

### Widening replica pins after a deploy

A `replicate` VM's pin names only its own node until a sync that started more than five minutes after the VM's last volume change is verified. The CPI widens such pins in a sweep after every successful call that takes the HA rules lock, but a deploy's last VMs often finish before any sync can count, and no later call may come for a while. After a deploy that adds `replicate` VMs, we wait until each new job has completed a scheduled sync, and then we widen the pins on the Director by hand. The command runs as `vcap`, because the pass opens the allocation journal, which opens only for its owner. It also needs the Director's UUID, because the pass widens only the pins that Director owns, and `bosh env --json` prints it in the `uuid` column.

```bash
sudo -u vcap /var/vcap/packages/pve_cpi/bin/pve-ops ha --widen-replicas --director-uuid <uuid>
```

That first run is a dry run. It reads the cluster, claims nothing, and writes nothing, and it lists every `replicate` VM of the Director whose pin still names its source alone, with what the pass would do and the reason.

```
Replica pin widening for Director 6c8e1d2a-4b3f-4e5a-9c7d-0f1e2d3c4b5a (a dry run, so nothing was written; add --apply to widen)
  VM 1042 on pve1: would widen to pve2, because the job's last sync follows the pin's last change; the check under the VM's journal record decides
  VM 1043 on pve1: skipped, because the VM's volumes changed at 2026-11-19T10:02:40Z, less than five minutes ago, so no sync can count yet
0 widened, 1 would widen, 1 skipped, 0 failed, and 0 deferred
```

A dry run reads no journal record and takes no lock, so a VM it lists as `would widen` can still be skipped by the real run. While an `attach_disk` changes a VM's volumes, the pin records the latest time the attach can land, which can lie ahead of the clock, and the VM is skipped because `an attach is changing the VM's volumes and may land as late as` that time. We then run the same command with `--apply`, and the pass prints `widened` with the job and the time of the sync that covers the VM. The pass now tries each VM's journal record once and makes the same check that `reboot_vm` and `set_vm_metadata` make, which turns `failback` off on the VM's HA resource right before the pin names the partner. A VM that the pass skips keeps its `failback` setting. A VM whose failback write fails is reported as `failed` and keeps the pin on its own node. Before it writes, it tries a short claim of its own on the VM's VMID once and, inside it, the HA rules lock once, so it waits on no lock, and then it widens each pin that a verified sync covers. A VM whose record, rules lock, or VMID another call holds is skipped with the reason, and so is a VM whose volumes or pin changed while the pass ran, a VM that is migrating or recovering, and a VM whose HA resource is ignored. Unlike the sweep, the command applies no caps and no time limit of its own, and an interrupt or SIGTERM stops it and releases the locks it holds. We run the command again later for a skipped VM, or we let the next call that takes the HA rules lock widen it. The command exits with 0 when no VM failed, and with 1 when a VM failed or the pass stopped early, for example because the token lacks the `Pool.Allocate` and `Pool.Audit` privileges that the VMID claims need. With `--json` it prints the Director's UUID, whether it applied, the results with an outcome and a reason for each VM, and the count of deferred VMs. For a Director with several `cpi_config` entries, we pass `--request` with a context that names the entry and the Director's UUID, once for each entry, because the pass reads one cluster.

### Fixing an anti-affinity layout

A member joins its group's anti-affinity rules at `set_vm_metadata`, and a delete plans the rest of its group again. After an upgrade, a rule that an older release filled to the number of nodes stays that way until the group's next `set_vm_metadata` or `delete_vm`, and so does a group whose rules an earlier build laid out by the order its members joined in rather than by index. The CPI's layout depends only on the members' indexes, the places that deletes hold, the node count, and the group's pattern, as [HA and Resurrection](ha-and-resurrection.md#what-the-cpi-registers) explains, so the next plan rewrites such a group once and the plan after it writes nothing. When we want the rules right now, we run `pve-ops ha --fix-anti-affinity`, which plans each anti-affinity group of one Director again, the way the CPI plans it, and reports each group. It needs the Director's UUID, from `--director-uuid` or from `--request`, and `bosh env --json` prints it in the `uuid` column. The usage line is as follows.

```text
pve-ops ha --fix-anti-affinity [--group RULE] [--apply] [--config PATH] [--request PATH | --director-uuid UUID] [--json]
```

The first run is a dry run, and it writes nothing.

```bash
sudo -u vcap /var/vcap/packages/pve_cpi/bin/pve-ops ha --fix-anti-affinity --director-uuid <uuid>
```

It prints each group with its member count, its pattern, and each write the plan would make.

```text
Anti-affinity layout for Director 11111111-2222-3333-4444-555555555555 (a dry run, so nothing was written; add --apply to write)
  Group web of deployment cf (bosh-aa-0123456789ab), 4 members, pattern block: the rules differ from the planned layout, and the fix would write these changes
    update bosh-aa-0123456789ab with vm:100,vm:101,vm:102
0 in layout, 1 would be fixed, 0 fixed, 0 failed, and 0 not planned
```

Each group ends in one of five outcomes. `in_layout` means the rules already match the plan. `would_fix` means a dry run found a difference, and a lapsed hold that the rules still record counts as one, listed as `drop the lapsed hold of index N from <carrier>`. `fixed` means the run wrote the group's rules. `failed` means a write failed. `not_planned` means the CPI could not plan the group. That includes a group whose plan would put members in a rule of the whole cluster's domain while their strict pins leave them, between them, no more nodes than the rule has members. No AZ of the entry's `placement.az_map` holds those pins, so another entry whose map does most likely placed the group, and the fix doesn't write a rule that would leave no node free for a drain. The same holds for a rule whose members' strict pins this entry's `placement.az_map` puts in different domains, because the CPI only ever writes a rule among members of one domain, so another entry whose map puts them in one domain wrote it, and the fix doesn't delete it. The fix also leaves a group alone when its plan would delete or rewrite a rule whose name this entry's `placement.az_map` never gives a rule of its members' domain. The block rules of each domain take turns in their numbering with the map's other AZs, and a pattern rule's number names its domain, so such a name means that another entry whose map defines other AZs most likely wrote the rule. A rule written before 0.12.0, or before this entry's `placement.az_map` changed, carries such a name too, and the group's next `set_vm_metadata` or `delete_vm` renames it. A rule whose members each hold a strict pin to a node of their own is not such a group, because the `hold` local policy writes those pins and the plan judges that rule as it stands. The doctor run against the same entry reports such a group as an `aa_layout_drift` problem with the same reason, and we check the group under `--cpi-config` and fix it with the request of the entry that placed it. When the plan is right, we run the same command with `--apply`, and it writes each group whose rules differ, under the HA rules lock. The option `--group bosh-aa-<hash12>` limits the run to one group.

The command refuses to run when `pve.ha.mode` or `pve.ha.anti_affinity` is off, or when scheduler-soft anti-affinity is disabled. We can't combine it with `--widen-replicas` or `--cpi-config`, and `--group` needs `--fix-anti-affinity`. It exits with 1 when a group failed or could not be planned. With `--json` it prints the results as JSON. For a Director with several `cpi_config` entries, we pass `--request` with a context that names the entry and the Director's UUID, once for each entry, because the pass reads one cluster.

### When `delete_vm` defers

A deferred `delete_vm` fails the Director's task with a retriable error that names the VM, its HA state, and its node's state. The Director does not wait for a deferred delete forever. When its retries run out, the `scan_and_fix` task fails with the defer message, and the Health Monitor asks again on its next cycle. If many instances defer at once, the Health Monitor's meltdown limit can stop resurrection for the whole deployment, so we resolve a defer rather than wait it out.

1. Find the defer message with `bosh task <id>` or `bosh tasks --recent`.

2. Run `pve-ops ha` against that cluster to see the node states and every CPI-owned HA resource.

3. Act on the state.
   For `fence` on a node that is still running, restore corosync or power the node off, so that PVE can fence it and recover the VM. For a dead node that PVE cannot fence, or a VM that is not HA-managed, follow [the dead-node procedure](#the-dead-node-procedure).

4. Run `bosh cck`, or wait for the next resurrection, and the delete then completes.

A defer whose message says `entry node clock unreadable` means that no member's clock answered through the entry node. The CPI already reads another member's clock when the entry node's own clock read fails with a 5xx, so the message means that every online member failed as well, or that the entry node failed in a way that never borrows a clock, such as a failure to reach it or a 4xx. We check the entry node and `pveproxy` on the other members, and then we retry. A `bosh recreate` that failed on such a defer can be run again as it is. The deferred delete changed nothing in PVE, and a disk that `detach_disk` had already moved to a parker stays there until the retried recreate attaches it to the new VM.

### The dead-node procedure

A delete on a dead node cannot complete through the normal path, because every call about the VM goes to its node. After the grace window, the CPI finishes the delete itself for a legacy VM on shared storage, as [HA and Resurrection](ha-and-resurrection.md#when-the-grace-window-expires-on-a-dead-node) describes. This procedure covers every other case, which includes a VM that is not HA-managed, a `hold` VM, a VM that the allocation journal records, a VM with a node-local volume, and a cluster whose token lacks `Sys.Console`.

Only follow it once the node is certain to be powered off. A node that is only partitioned may still run the guest, and moving its config then puts two writers on one disk.

1. Confirm that the node is powered off, through IPMI or at the machine.
   A missing ping is not proof, because a partitioned node still runs its guests.

2. Check that no BOSH task is running against the instance, with `bosh tasks`.
   A running `reboot_vm`, `set_vm_metadata`, or `attach_disk` can register the VM's HA resource again while it finishes, so we let it end or cancel it before we remove anything.

3. For a VM that PVE HA manages, remove its HA resource from a quorate node, and wait until `ha-manager status` no longer lists it.
   The CPI decides what to do with a delete from the CRM's own record of the service. After a fence, that record holds `vm:<vmid>` in `recovery` on the dead node, and moving the config by hand does not change it. A retried `delete_vm` would then keep deferring, and the dead-node completion would refuse, even though the config is on a live node. Once the resource is gone and the CRM has dropped the service, the CPI sees an unmanaged VM on a live node and the delete proceeds. A VM that is not HA-managed has no resource, so we skip this step for it.

   ```bash
   ha-manager remove vm:<vmid>
   ```

   We remove the resource only because step 1 proved that the node is off. The CPI registers a resource for an existing VM only inside a change that has already removed it, such as a pin narrowing or a clearing of an HA error, so with no task running, nothing puts the resource back.

4. For a VM whose volumes are on shared storage, or whose ephemeral disk can be thrown away, move the config from a quorate node.

   ```bash
   mv /etc/pve/nodes/<dead>/qemu-server/<vmid>.conf /etc/pve/nodes/<live>/qemu-server/
   ```

   The next `delete_vm` retry finds the VM on the live node and completes. It removes any node-affinity pin the VM still has. Afterward we run `pve-ops ha`, and we delete any rule that it reports as a `rule_problem` for the removed VM.

   A volume on the dead node's local storage is not removed by this delete. Step 7 covers it.

5. For a VM whose persistent disk lives on the dead node, either bring the node back or decide that the data is lost.
   When the data is lost, remove the disk reference with `bosh cck`.

   The destroy runs on the live node, and that node cannot free a volume that sits on the dead node's local storage. A storage that is defined for every node, such as a shared LVM volume group name, looks active on the live node too, and the destroy looks in that node's own copy, finds no such volume, and skips it. PVE does not warn about such a volume, and the destroy finishes without it. The volume stays on the dead node, and we remove it in step 7. The CPI logs a warning when it deletes a VM whose volume sits on a node-local storage and the VM's node does not hold that volume. The warning names the VM, the volume ID, and the storage, and when the storage is limited to certain nodes it names the likely node too. We save those lines from the deleting task's CPI log, so that step 7 has a list.

6. Run `bosh cck`, or wait for the next resurrection.

7. Once the dead node is back and in the cluster, remove the volumes that the destroy left behind on it.
   We take the volume IDs from the CPI's warnings. When the log is gone, we list the node's local storages instead, for example with `pvesm list <storage> --vmid <vmid>`, and look for volumes named for the deleted VM.

   For each volume, we first confirm that no VM config in the cluster still references it, and that the VM ID no longer exists.

   ```bash
   grep -l -e '<volume name>' -e '<vmid>' /etc/pve/nodes/*/qemu-server/*.conf
   pvesh get /cluster/resources --type vm | grep -w <vmid>
   ```

   Both commands must print nothing. A config that still names the volume means that some VM owns it, and we leave it alone. Then, on the node that holds the volume, we remove it.

   ```bash
   pvesm free <volid>
   ```

   We free the volume on the node that holds it, because a local storage answers only on its own node. The destroy leaves these volumes behind in silence, so a cluster that skips this step keeps the space for good.

### Clearing an HA error by hand

PVE refuses every state request for a resource in `error` except `disabled`. The CPI clears the error itself when it holds `Sys.Console` on `/`. Without that privilege, it returns a retriable error that tells us which command to run.

For a delete, or for a VM that is already stopped, we set the resource to `disabled`, wait until `ha-manager status` shows it `stopped`, and retry.

```bash
ha-manager set vm:<vmid> --state disabled
```

For a running VM that BOSH keeps, `disabled` would stop the guest, so we remove the resource and add it again, which leaves the guest running.

```bash
ha-manager remove vm:<vmid>
ha-manager add vm:<vmid> --state started
```

The VM has no HA protection between those two commands, and a hand-made resource carries no pin or ownership marker, so we restore its rules afterward. A resource added by hand gets `failback` on, which is PVE's default, and the CPI never changes a resource without its marker. So for a `replicate` VM, a `hold` VM, or any VM with a volume on node-local storage, we add `--failback 0` to the `ha-manager add` command. When the CPI does the same work, it tags the VM `bosh-ha-readd-<unix seconds>` until the registration finishes. A tag that lingers past the create timeout plus `pve.ha.motion_wait_sec` means the registration did not finish, and the doctor reports it as `readd_tag`. We then add the resource with the command above, restore the VM's rules, and remove the tag.

### Draining and undraining a node with `pve-ops ha drain`

A rolling maintenance sticks when a VM can't leave its node, because PVE waits on the node's HA agent until the last service has gone. The `pve-ops ha drain <node>` and `pve-ops ha undrain <node>` commands take that wait away. The drain lets PVE HA move every VM that can leave, and it stops each VM that can't through PVE HA, so the node empties of running HA VMs and the maintenance finishes. The undrain starts those VMs again and then runs the same rebalance that `pve-ops ha rebalance --apply` runs. Both commands act only on the VMs of the Director we name, and we run them as `vcap` on the Director, for the same reason the rebalance needs that user.

PVE offers no API call that turns HA node maintenance on or off, and the CPI reaches PVE only through the API. So the maintenance itself stays our step. We run `ha-manager crm-command node-maintenance enable <node>` on a PVE node before the drain, and `ha-manager crm-command node-maintenance disable <node>` on a PVE node after the work. The drain and undrain commands each check the maintenance state. A `drain --apply` refuses with exit 3 until the HA manager reports the node in maintenance, and an `undrain --apply` refuses with exit 3 while the node is still in maintenance or its notes still carry one of the tags in `pve.placement.maintenance_node_tags`. It also refuses with exit 3 when it can't read the node's notes, because it then can't rule the marker out, and we run it again once the node answers. Each refusal prints the command or the edit we need. A notes marker alone doesn't satisfy the drain, because PVE moves nothing for it. PVE takes a while to report what we just did, so with `--apply` each command waits up to 90 seconds, which `--maintenance-wait` sets, before it refuses. The drain waits for the HA manager to report the maintenance we enabled, and the undrain waits for the CRM to leave the maintenance we disabled, which took 30 to 40 seconds in a lab run. The wait holds no lock and writes nothing, it polls with the same reads the gates use, and it ends early on Ctrl-C or when the run's own time limit passes. It also ends at once when the gate refuses for a reason that PVE catching up can't change, such as a node with no health entry or a notes marker on an undrain. Its polls read each node's notes only once, and the run reads them again after the wait. When the wait runs out, the refusal still exits with 3, and it names the CRM state, the LRM mode, and the LRM state it last read. A dry run never waits.

We always read the plan first, and the dry run works before maintenance starts. The dry run reads the cluster, takes no lock, and writes nothing.

```bash
sudo -u vcap /var/vcap/packages/pve_cpi/bin/pve-ops ha drain <node> --director-uuid <uuid>
```

The output lists each of the Director's VMs on the node, and it says for each whether PVE HA will move it or whether it can't leave and why.

```
HA drain of node pve2 for Director 6c8e1d2a-4b3f-4e5a-9c7d-0f1e2d3c4b5a on cluster prod-east (token bosh-cpi@pve!****), a dry run, so nothing stopped; add --apply to stop the VMs that can't leave
pve-ops 0.12.0 with the configuration /var/vcap/jobs/pve_cpi/config/cpi.json
Gates
  pve.ha.mode is auto, and the HA profile lets the CPI write HA state
  quorum is held
  node pve2 is online, and the other 2 nodes are online, out of HA maintenance, and able to take work from PVE HA
  for --apply, the HA manager doesn't report node pve2 in maintenance; run `ha-manager crm-command node-maintenance enable pve2` on a PVE node first
  the HA manager's master is pve1, and its status is fresh
  the allocation journal holds no open record
  no CPI task or lock is active
  no other rebalance holds the run lock
  the drain decided the fate of every VM of this Director on node pve2
VMs on pve2
  VM 1042 (vm-8a1f2c3d), leaves for pve1 or pve3
  VM 1050 (vm-c07a5e19), can't leave, because its strict node-affinity rule bosh-na-1050 names no other node that can take it: stop planned
  VM 1061 (vm-f31b9a77), stays, because it has no HA resource, so maintenance won't move it, and the drain won't stop it; it goes down when the node reboots
1 VM can leave, 1 VM can't, and 0 stopped
Turn resurrection off for `cf` until `pve-ops ha undrain pve2 --apply` finishes, for example with `bosh update-resurrection off`, because the Health Monitor sees the agents of the stopped VMs go quiet and would recreate them.
```

The drain decides which VMs can't leave from PVE's own rules and the cluster's facts. It starts with the nodes that are online and able to take work from PVE HA, without the drained node, and it narrows that set with each strict node-affinity rule that names the VM, the node-local storage the VM uses and the targets of its replication jobs, every negative resource-affinity rule, and every positive resource-affinity group the VM belongs to. A VM whose set ends empty can't leave. A node that only the maintenance tag in its notes keeps out of placement stays in the set, and so does a node whose notes the CPI couldn't read, because the HA manager doesn't read node notes and moves VMs to such a node. The drain names each such node in its `nodes` gate and in a warning, so we know that PVE may move VMs there even though the CPI places no new VM on it. A VM with no HA resource is listed with a warning, because maintenance never moves it and the drain never stops it, so it goes down with the node when we reboot. We deal with such a VM by hand, as the sections below describe. A VM whose HA resource already requests a state other than `started` stays where it is, and the output says why.

Sometimes the drain can't tell whether a VM can leave. That happens when the cluster's storage list couldn't be read, when the list doesn't name a storage that holds one of the VM's volumes, or when the VM belongs to a positive resource-affinity group with a VM the drain can't decide. It also can't decide a VM whose positive group holds a running VM on the node that isn't one of this Director's, such as another Director's VM, because the drain never reads that VM's config. We remove such a VM from the group, or move it off the node ourselves, before we run the drain with `--apply`. The VM's line then says that it can't be decided and why, and the `fates` gate refuses with exit 3 before anything stops. A rerun decides again from a fresh read, so we run the drain again once the read works.

With `--apply`, the drain takes the run lock `bosh-lock-ha-rebalance`, so a drain, an undrain, and a rebalance never run at once on one cluster. It then works through these steps.

1. It reads the cluster and checks the gates again from a fresh read.

2. It waits for PVE HA to move every VM that can leave. The plan's soft pin warnings name only the VMs still on the node at the last read, so a VM that has already left draws none. It reads the cluster every 5 seconds, and it stops waiting once no such VM is left on the node and no HA service is migrating or relocating. A poll that can't decide a VM's fate keeps the wait going, and a VM that is still undecided when the settle timeout runs out makes the `fates` gate refuse with exit 3, with nothing stopped. A VM that can leave but is still on the node when the settle timeout runs out ends the run with exit 1, and the report names the nodes that could take it, because something other than the rules is holding it. One case is different. When a VM's soft node-affinity rule puts only the drained node in its highest priority class, the plan warns about it, because PVE HA should move it to the rule's next class. If every VM still on the node at the timeout is such a VM, and nothing is moving, the drain counts them as VMs that can't leave and stops them in the next step.

3. For each VM that can't leave, it reads the HA resource again under the VM's journal record and claim, and it confirms that the VM is still on the node and still requests `started`. In one write, it sets the resource's state to `stopped` and adds the comment token `bosh-drain-stopped=<node>`. It keeps the ownership marker and every other token in the comment. It never calls `qm stop`, so PVE HA itself shuts the guest down.

4. It waits until the HA manager reports every stopped VM as `stopped`, and then it prints the resurrection reminder.

The reminder names the deployments of the stopped VMs, and we turn resurrection off for them before we run the drain with `--apply`, for example with `bosh update-resurrection off`. Without that, the Health Monitor sees the silent agents of the stopped VMs and recreates them. The dry run prints the same reminder, because that is the moment it matters.

The undrain mirrors the drain. We disable maintenance on the node, and we run the undrain as a dry run first if we want to read the list of VMs that carry the node's token.

```bash
sudo -u vcap /var/vcap/packages/pve_cpi/bin/pve-ops ha undrain <node> --director-uuid <uuid> --apply
```

With `--apply`, it first waits up to the `--maintenance-wait` time for the CRM to leave maintenance, and then it takes the run lock and checks the gates with the node out of maintenance. It finds every resource of this Director whose comment carries a drain token that names the node, wherever the VM runs now. In one write for each of them, it sets the state back to `started` and drops the token. It waits until the HA manager reports each of those VMs as `started` and no HA service is migrating or relocating, so the failback moves that PVE makes after maintenance have ended too. A VM in `error`, or one that hasn't started in time, ends the run with exit 1, and the rebalance doesn't run. Unless we pass `--no-rebalance`, the undrain then runs the rebalance with the same code path as [Rebalancing HA VMs after node maintenance](#rebalancing-ha-vms-after-node-maintenance), and its report carries the rebalance's report. Last, it prints a reminder to turn resurrection back on, for example with `bosh update-resurrection on`.

Both commands take these flags, and the undrain takes the extra flags that follow. The node is the one positional argument, and it may come before or after the flags.

- `--apply`
  This makes the changes. Without it each command is a dry run.

- `--director-uuid UUID` or `--request PATH`
  One of these names the Director whose VMs the run acts on, as for the rebalance. The commands refuse `--cpi-config` and `--widen-replicas` with exit 2.

- `--config PATH`
  This names the CPI configuration file, as it does for `pve-ops ha`.

- `--maintenance-wait DURATION`
  With `--apply`, this bounds how long the command waits for PVE to report the maintenance we just enabled, for the drain, or disabled, for the undrain. It is 90 seconds by default. A value of 0 refuses at once, as the commands did before, and a dry run never waits. It must not be negative.

- `--settle-timeout DURATION`
  For the drain, this bounds each of its two waits, the one for the VMs that can leave and the one for the stops, and it is 30 minutes by default because a node can hold many VMs. For the undrain, it bounds the wait for the started VMs, it becomes the rebalance's settle timeout, and it is 10 minutes by default. Both must be at least 1 minute.

- `--json`
  This prints the report as JSON, with the Director's UUID, the masked credential, the gates, the warnings, each VM with its fate, reason, target nodes, and outcome, the deployments, the reminder, the rebalance's report for an undrain, whether the run applied, the reason it halted, and the exit code.

- `--no-rebalance`
  This is an undrain flag. It starts the VMs and skips the rebalance.

- `--max-moves N`, `--max-duration DURATION`, `--start-threshold FRACTION`, `--stop-threshold FRACTION`, and `--restore-only`
  These are undrain flags, and the undrain passes them to the rebalance unchanged, so they mean what the rebalance section says.

The text output shows the gates, then one line per VM, a summary, and the reminder. A drain line says that the VM leaves for one or more nodes, or that it can't leave and why, or that it stays and why, and an applied run adds the outcome. An undrain line names the node the VM sits on now and its outcome. The outcomes are as follows.

- `stopped` and `started`
  The drain stopped the VM through PVE HA, or the undrain started it.

- `stopped_earlier`
  An earlier drain of this node already stopped the VM, so this run counts it and doesn't write.

- `planned`
  A dry run plans the stop or the start, or an applied run did not reach it.

- `skipped`
  The VM's HA resource changed so that the stop or start no longer applies, for example because the VM left the node.

- `busy`
  Another call held the VM's journal record or VMID claim, so a rerun has to stop or start it.

- `failed`
  A write failed, a VM that could leave stayed, or the HA manager reports the VM in `error`.

- `left_pending`
  PVE HA had not finished the stop or the start when the settle timeout ran out. The token stays on the VM.

The commands exit with these codes.

- `0`
  The run finished. That includes a completed dry run and a drain whose node now holds no running VM of this Director.

- `1`
  A read or a write failed, a VM that could leave stayed, a stop or a start didn't settle in time, the run was interrupted, or the rebalance it ran ended with 1. A rerun goes on from the tokens.

- `2`
  A flag was wrong or missing, or the node was not named exactly once.

- `3`
  A gate refused the run, including the maintenance gate, and nothing was left part way. The gates are the rebalance's, with one change. Every node except the named one must be online, out of PVE HA maintenance, and able to take work, because draining a second node while another is down is not a rolling maintenance. A node that only its notes keep out of placement passes this gate, because PVE HA still moves VMs to it. On an undrain such a node doesn't stop the starts, and the rebalance that follows them refuses with exit 3 until the notes no longer keep the node out, the same way `pve-ops ha rebalance` refuses. The undrain warns about that before it starts anything. The named node must be online. The gate that waits for the cluster to settle doesn't apply, because a drain causes migrations and waits for them itself.

A rerun is how we resume. The token goes on in the same write that stops the VM and comes off in the same write that starts it, so a crash never leaves a VM that is stopped with no token or started with a stale one. A drain that stopped some VMs and then died leaves each stopped VM with its token, and the rerun lists those VMs as stopped by an earlier drain and stops whatever is left. A stop that conflicts or finds the record busy leaves the VM running with no token, the run finishes the other stops and exits with 1, and the rerun stops that VM. A stop that doesn't finish in time keeps its token, PVE goes on with it, and the rerun waits for it. A crash during the undrain leaves each VM either stopped with its token or started without it, and the rerun starts what is left, waits, and runs the rebalance, which resumes its own marks. A VM that we start by hand during the maintenance keeps its token, and the undrain then only drops the token. [Troubleshooting](troubleshooting.md#a-drain-refuses-with-the-maintenance-gate) covers the failures we meet most often.

### Node maintenance with `hold` VMs

PVE HA cannot evacuate a `hold` VM, because its strict pin forbids every other node. The maintenance request on the node therefore never completes while the VM runs. The drain described above stops such a VM for us, and we use the manual steps below only when we can't run the drain. Before we put a node into maintenance, we stop its `hold` VMs with the command below, and we start them again afterward. A soft `bosh stop` stops only the jobs and leaves the guest running, and `bosh stop --hard` deletes the VM, so a recreated VM with a node-local disk would have to land back on the node that holds the disk. The `ha-manager` command avoids both problems.

```bash
ha-manager set vm:<vmid> --state stopped
```

When the maintenance is over, we start the VM again.

```bash
ha-manager set vm:<vmid> --state started
```

The doctor lists a `hold` VM as `hold_blocks_maintenance` only while its HA state is `started` and its node is in maintenance with the drain still running. A `hold` VM whose HA service we have stopped does not block the drain, so the finding goes away as soon as we stop it, and it stays away once the node's HA agent has finished draining. A VM under `local_policy: none` is not HA-managed, so maintenance mode does not touch it. It keeps running until the node reboots, and then it goes down with the node.

### Node maintenance when a VM cannot leave the node

The `hold` procedure above is one case of a wider rule, and `pve-ops ha drain` handles every case of it for the VMs of one Director, as [Draining and undraining a node with `pve-ops ha drain`](#draining-and-undraining-a-node-with-pve-ops-ha-drain) describes. This section stays as the fallback, and it is the only way to deal with a VM that has no HA resource or that belongs to another Director. PVE waits forever on a node in maintenance for any HA service that cannot leave it. The maintenance request itself does not wait. The enable command returns at once, and the node's resource manager enters maintenance mode within a few seconds. What waits is the drain, because the resource manager keeps its agent lock and its watchdog until the last service has left the node. That wait blocks a reboot while the node is in maintenance and the restart of the local resource manager that the `pve-ha-manager` package upgrade runs. The resource manager logs that it ignores the shutdown policy while maintenance is on, and it never stops or fences the VM, so the node sits there until we release the service by hand. Two kinds of VM do this.

- **A member of a full anti-affinity group**

  Every other node that could take the VM already holds another member of its group, so the negative rule leaves the VM nowhere to go. A rule that an older release filled to the number of nodes does this, and a rule written by this release holds one fewer member than the nodes, so it does not.

- **A VM with a strict single-node pin**

  That means a `hold` VM, or a `replicate` VM whose pin has not widened to its partner yet. The pin forbids every other node, so HA cannot migrate it.

Before we enable maintenance, we run `pve-ops ha` and read its findings. A `hold` VM shows up as `hold_blocks_maintenance`. Any other started VM that cannot leave shows up as `maintenance_stranded`, with a reason that says either that its pin names no other usable node or that every other node that could take it holds a partner from its anti-affinity rules. The doctor names each VM once. Both findings only fire once the node is in maintenance, and they keep firing for as long as the drain is stuck on the VM, so we read them again right after we enable maintenance, while the drain is still running. They clear once the VM stops or leaves. The group and pin findings do not list the VMs that would block a drain, so before we enable it we check each hold VM, each `replicate` VM, and each anti-affinity group against the node count ourselves. We then clear every blocker before we start the upgrade or the reboot.

For a member of a full anti-affinity group, we pick one of these.

- Run `pve-ops ha --fix-anti-affinity`, and add `--apply` once the plan looks right, so that the group's rules hold one fewer member than the nodes they cover, as [Fixing an anti-affinity layout](#fixing-an-anti-affinity-layout) shows. A rule that an older release filled to the number of nodes shows as `aa_rule_full`.

- Run `bosh recreate` on the member so that it lands on another node, or scale the group below the node count.

- Stop the member through HA with the command below, and start it again once the node is back.

For a `hold` VM or a `replicate` VM that has not widened, we stop it through HA and accept the downtime. For a `replicate` VM, we can instead wait until its job has completed a sync that started more than five minutes after the VM's last volume change, run `pve-ops ha --widen-replicas --apply` as [Widening replica pins after a deploy](#widening-replica-pins-after-a-deploy) shows, and then HA migrates it like any other VM.

```bash
ha-manager set vm:<vmid> --state stopped
```

After the node is back and out of maintenance, we start the VM again.

```bash
ha-manager set vm:<vmid> --state started
```

Stopping a resource through HA changes nothing in the CPI's metadata, so the VM keeps its pin, its rules, and its place in the BOSH inventory. The Director still sees an instance whose agent has gone quiet, though, and the resurrector may act on it. We turn resurrection off for the deployment before we stop VMs this way, or we expect a recreate, and we turn it back on after the VMs run again.

If a wedge has already started, for example a `pve-ha-manager` upgrade that is stuck in its restart of the resource manager, we do not kill anything on the node. We stop each blocking resource through HA with the first command above. The drain then finishes within seconds, the stuck restart or reboot goes on by itself, and `pve-ops ha` stops reporting the node. We start the VMs again with the second command once the node has left maintenance.

### Keeping the CPI off a node with the node's notes

HA maintenance is our first choice for taking a node out of service, because it drains the node and keeps new VMs off it. When we want the CPI to place nothing on a node without HA maintenance, such as on a cluster without PVE HA or for a node that should keep running its VMs, we add a marker line to the node's notes. The notes are the text of the Notes panel for the node in the PVE web interface. The CPI reads them as the `description` of `GET /nodes/{node}/config`, and it treats the node as in maintenance when a line of the form `tags: <list>` shares a tag with `pve.placement.maintenance_node_tags`, which defaults to `["maintenance"]`. This is the line we add.

```
tags: maintenance
```

We can set it from a PVE node with this command.

```bash
pvesh set /nodes/<node>/config --description 'tags: maintenance'
```

The `--description` option replaces the whole notes. When the notes already hold other text, we either edit them in the node's Notes panel in the web interface, or we read them first with `pvesh get /nodes/<node>/config --output-format json` and write back the full text with the marker line added. The line may sit anywhere in the notes, and it has to start with the key. A Markdown list item such as `- tags: maintenance`, a heading, a quote, and a form such as `tags = maintenance` never count. When a line like that names a configured tag, `create_vm` logs a warning that names the node and the line, because the node stays in placement. The list after the colon splits on commas, semicolons, and whitespace, and the key and the tags match without regard to case. Brackets around the list, quotes around a tag, a period at the end of the line, and a byte order mark before the notes are ignored, so `tags: [maintenance]` and `tags: "maintenance"` count. The match errs toward keeping the CPI off the node, so a line such as `tags: not maintenance` counts, and so does a marker line inside a fenced code block.

The marker keeps new VMs off the node, and it moves nothing, because PVE HA doesn't drain a node for it. Removing the line, or the tag from it, puts the node back into placement on the next `create_vm`. Placement in `create_vm` reads the notes, and so does the HA profile that `set_vm_metadata`, `attach_disk`, `delete_vm`, and `pve-ops` resolve before they write HA rules. The profile counts a node whose notes it cannot read as unable to take work, so an anti-affinity rule has room for fewer members while that node doesn't answer. Each CPI call reads a node's notes at most once, and only for nodes that are otherwise placeable, so an offline or HA-maintenance node costs no read. A node whose read fails is asked at most twice in the call, and a create pinned to a `target_node` reads that node's notes alone. A node whose notes the CPI cannot read is left out of placement, because the CPI cannot tell whether the node is in maintenance. The read needs `Sys.Audit` on `/`, which the CPI token already has. Setting `pve.placement.maintenance_node_tags` to `[]` turns the marker off, and then the CPI reads no notes.

The `pve-ops` HA doctor shows `notes_tag=<tag>` on the node's line. It doesn't count a node that only has the marker as an HA maintenance node that PVE drains, and it reports an `unchecked` problem with the source `node-notes` when a node's notes could not be read.

### A create that pins a node in maintenance

A `create_vm` call whose `cloud_properties.target_node` names a node in maintenance fails with a retriable error, and the CPI never places that VM on another node instead. The error names the node and says to retry once it leaves maintenance. The CPI judges the node the same way placement does, so a node counts when the HA view reports it in maintenance and when its notes carry one of the tags in `placement.maintenance_node_tags`, and the error then says that the node is in maintenance and that its notes carry the maintenance tag. When the CPI cannot read the pinned node's notes, it refuses with a retriable error that says it cannot tell whether the node is in maintenance. The check runs in every `pve.ha.mode`, including `off`. A node that is fenced or otherwise unable to take new work is refused with a retriable error as well, and its message names the reason.

When the CPI cannot read the node health at all, it fails the create with a retriable error too, because it cannot rule maintenance out. Placement reads node health a second time when the first read fails. When `pve.ha.mode` is `auto` or `managed`, or the effective `pve.placement.maintenance_node_tags` list is not empty, a second failure stops the create with a retriable error as well, because placement cannot tell which nodes are in maintenance. An unset list gives `["maintenance"]`, so it counts as not empty. With HA writes off and the list set to `[]`, placement warns and carries on, since it has other nodes to choose from, but a pinned node has no alternative. A failed read of a node's notes is retried once in the same way, but a second failure leaves only that node out of placement, and the create goes on with the nodes that answered. The create stops with a retriable error when no candidate node is left, when the node is the pinned `target_node`, or when the VM's local disk pins it to that node, because the node comes back once it answers. During a rolling maintenance, the Director retries the call, and it goes through once the node is back. If we would rather have the VM land on any healthy node, we remove `target_node` and let placement choose.

### Where VMs go when a maintenance ends

When a node leaves maintenance, PVE moves a VM back to it only when the VM's HA resource has `failback` on. The CPI turns the flag on for each VM whose volumes all sit on shared storage, so those VMs go back to their nodes by live migration as each maintenance ends, and a rolling maintenance across every node leaves the cluster about as balanced as it started. A `hold` VM, a `replicate` VM, and any other VM with a volume on node-local storage keep `failback` off. A `replicate` VM that a maintenance moved to its partner stays there, and its pin keeps both nodes, as [HA and Resurrection](ha-and-resurrection.md#the-replicate-data-loss-window) describes. After the last node of a rolling maintenance comes back, `pve-ops ha` reports the `spread_skewed` advice when VMs sit outside their AZ, anti-affinity members share a node, or the nodes' committed memory is far apart, and [Rebalancing HA VMs after node maintenance](#rebalancing-ha-vms-after-node-maintenance) shows how we move them back.

With `pve.ha.failback: false`, no CPI VM moves back, and the node we drain last ends up nearly empty. We then move VMs back ourselves with `ha-manager migrate vm:<vmid> <node>`. Before a rolling maintenance, we run `pve-ops ha` and read any `failback_drift` advice, because a VM that an earlier release registered keeps `failback` off until a call touches it. A `bosh recreate` of the instance, or any `set_vm_metadata` or `reboot_vm` on the VM, sets the flag. [Failback](ha-and-resurrection.md#failback) covers what PVE does with the flag.

Turning the flag on can move a VM at once. When we upgrade from a release that wrote `failback` off, the first `set_vm_metadata` or `reboot_vm` on a shared VM turns the flag on, and if the VM runs on a node other than the top-priority node of its pin, PVE live-migrates it to that node right away. To keep those moves inside a maintenance window, we set `pve.ha.failback: false` before the upgrade, and we set it back to `true` or remove it during the window. The next call that touches each VM then turns the flag on, and any deploy that touches the instance sends such a call.

A manual `ha-manager migrate vm:<vmid> <node>` of a VM with `failback` on doesn't last when another online node has a higher priority in the VM's pin, because the HA manager moves the VM back. To move such a VM for good, we give the target node the highest priority in the pin, for example with `ha-manager rules set node-affinity bosh-na-<vmid> --nodes <target>:2,<other>:1`, and PVE then moves the VM by itself. For a short move, we can run `ha-manager set vm:<vmid> --failback 0` first, but the next `set_vm_metadata` or `reboot_vm` turns the flag on again and the VM goes back. [Failback](ha-and-resurrection.md#failback) explains both.

### Replicate failover and the old node's writes

After a `replicate` VM fails over, PVE reverses its replication job, so that the job runs from the partner back to the old node. The job's next run receives with `zfs recv -F`, which rolls the old node's dataset back to the last common snapshot. Any write that existed only on the old node, which means every write since the last successful sync, is destroyed by that first run. The CPI registers every `replicate` VM's HA resource with `failback` off, whatever `pve.ha.failback` says, so the VM stays on the partner when the old node returns. The next `reboot_vm` or `set_vm_metadata` call that touches the VM narrows its pin to the partner alone, and in the same step the CPI asks the job to sync from the partner. From then on the VM behaves like a `hold` VM on the partner, and the destructive first sync can start at the CPI's next touch of the VM, and not only on the job's own schedule.

When those writes do not matter, we do nothing, and replication resumes in the new direction by itself. When they do matter, we save them first.

1. Before any BOSH call touches the VM, and before the old node rejoins the cluster, disable the VM's replication job from a quorate node.

   ```bash
   pvesr disable <job>
   ```

2. Bring the old node back, and save the writes from the live dataset on it. The lost writes exist only in that live dataset. The newest snapshot there is the last replication snapshot, which is the common base the partner already holds, so sending or cloning it recovers nothing. We take a new snapshot of each of the VM's volumes on the old node first, with a name that does not start with `__replicate_`, and then we send or clone that snapshot.

   ```bash
   zfs snapshot <pool>/vm-<vmid>-<name>@rescue
   zfs send <pool>/vm-<vmid>-<name>@rescue | ssh <host> zfs recv <dataset>
   ```

   Once the copy sits somewhere else, we destroy the rescue snapshot, so that the last replication snapshot is again the newest one when the job's next receive runs.

   ```bash
   zfs destroy <pool>/vm-<vmid>-<name>@rescue
   ```

3. Enable the job again.

   ```bash
   pvesr enable <job>
   ```

   The job's next sync rolls the old node's live dataset back to the common snapshot and overwrites it from the partner, so the writes survive only in the copy we saved in step 2. A job that was never disabled does the same on its first run after the old node returns, which is why step 1 comes first.

The doctor reports a `replicate` VM that runs on its partner as `replica_on_partner`, which tells us about the reversed job. After a failover, or after a move the CPI hasn't reconciled yet, that finding is a problem, so the doctor exits with 1 until the condition clears.

A maintenance migration to the partner is not a failover, and it needs none of these steps. The next `reboot_vm` or `set_vm_metadata` call that verifies a sync from the partner back to the old node keeps both nodes in the pin and names the old node as the new partner, and the doctor's `replica_on_partner` finding clears then. Until that sync is verified, the CPI treats the move as a possible failover, narrows the pin, and says in its warning that it can't tell the two apart. Once a sync from the partner has run that covers the VM's volumes on the old node, and the old node's task list shows the migration, the doctor reports `replica_on_partner` as advice rather than a problem. The doctor runs the same sync check the CPI runs, and it judges only the job whose marker names this Director, so the two reach the same verdict. When two jobs carry that marker, the doctor reports a problem rather than picking one. When no migration task explains the move, the doctor reports it as a problem and says that writes were lost. When the VM started outside a migration since the pin's last change, which an HA recovery does, or when the doctor can't read the task lists or the task logs it needs, it says that writes may have been lost. Reading the task lists and their logs needs `Sys.Audit` on the nodes. [HA and Resurrection](ha-and-resurrection.md#the-replicate-data-loss-window) describes the checks.

### Removing orphan replicas

`delete_vm` retires a `replicate` VM's job before it destroys the VM. When the partner is offline, or the job is still there after `pve.ha.motion_wait_sec`, the CPI deletes the job with `force=1`. That call drops the job configuration without removing the partner's copy. The CPI logs the partner and the volume IDs that stay behind, and it records them in the allocation journal when the VM had a journal record. The doctor reports an unattended copy as `replica_orphan_volume` or `replica_recorded_orphan`. A copy whose job and VM are both gone, and that the journal never recorded, gets the advice `replica_gone_vm_volume` when its VMID sits inside the CPI's VM band, its name follows the CPI's volume naming, and no job or guest config on the cluster refers to it. The doctor infers that ownership from the band and the name, so we check the volume before we free it, and [Removing the volume of a VM that no longer exists](troubleshooting.md#removing-the-volume-of-a-vm-that-no-longer-exists) gives the commands in order. The doctor skips that check when it could not read the guests of every node. It also reports an older copy that a later retirement will remove as the advice `replica_superseded_copy`.

We remove an orphan by hand, once nothing needs the copy. We check that the VM it belonged to is gone or runs elsewhere, and then we free the volume on the node that holds it.

```bash
pvesm free <storage>:<volume>
```

### Turning HA off after it was on

Setting `pve.ha.mode: off` makes the CPI write no more HA state, but it does not remove the resources and rules that the CPI or an older release created. Until we remove them, the CPI still stops and deletes those VMs correctly, because the delete path reads the HA state and stops an HA-managed VM through PVE HA whatever the mode is. The doctor lists what is left as `off_mode_resource` and `off_mode_rule`. While they remain, a new parker still never takes a VMID that one of them names, because a parker's VMID allocation reads the HA resources and rules in every mode. We remove each leftover in the PVE UI, or with the following commands.

```bash
ha-manager remove vm:<vmid>
ha-manager rules remove <rule-id>
```

Removing a resource does not stop its guest.

### Clusters that share an export

Several clusters that mount one export share a volume namespace, so they need disjoint VMID bands and disjoint `next-id` ranges. [Shared-storage rules](multi-cluster.md#shared-storage-rules) covers the rules, including how the CPI refuses to delete a volume that another cluster owns. We run `pve-ops ha --cpi-config` against the cpi-config to check the pair.

`qm rescan` on one cluster can add another cluster's volume to a VM's config as an `unusedN` entry. We never remove such an entry with `qm set --delete`, because that frees the volume and destroys the other cluster's disk. We edit the VM's config file under `/etc/pve/qemu-server/` and remove the line instead.

---

## Applying or changing a cpi-config on a live Director

A cpi-config gives a Director several named CPI entries, and each entry reaches one cluster or host. Once it is active, the Director refuses every deploy against a cloud-config whose AZs don't name an entry. We apply a new or changed cpi-config in this order, and [Multi-Cluster Deployments](multi-cluster.md#applying-a-cpi-config-on-a-live-director) explains the model behind it.

We never change the `host` or the `node` of an entry that serves live instances, and we never repoint a live AZ at another entry. For a move to another cluster we add a new entry and bind a new AZ to it. [What we see if an entry is repointed anyway](multi-cluster.md#what-we-see-if-an-entry-is-repointed-anyway) shows the failure we'd get otherwise.

1. Set each entry's credential group in the vars that `manifests/cpi-config.yml` reads.
   In the shipped file, `pve-az1` reads `pve_user`, `pve_password`, and `pve_api_token`, and `pve-az2` reads `pve_az2_user`, `pve_az2_password`, and `pve_az2_api_token`. A group is either an API token or a user with a password, and we never complete one entry's group with another entry's user, password, or token.

2. Run `scripts/bosh update-cpi-config`.
   We should see the Director accept the cpi-config, a reminder to re-apply the cloud-config, and then a light stemcell upload with `--fix`, which registers the stemcell with every entry and builds each cluster's cache template.

3. Re-apply the cloud-config with `scripts/bosh ucc`, or with `scripts/cf cloud-config` for CF.
   We should see a line saying that the cpi-config is active and that the AZ-to-CPI binding is layered. If a deploy fails before this step with "AZ 'z1' must specify a CPI when CPI config is defined", [the troubleshooting entry](troubleshooting.md#the-director-rejects-a-deploy-with-must-specify-a-cpi) covers it.

4. Run `bosh cpi-config`.
   We should see every entry we expect, under the names the cloud-config uses.

5. Run `bosh stemcells`.
   We should see the stemcell registered once for each entry, shown as separate rows with each entry's name in the CPI column. A heavy stemcell needs `scripts/bosh -n upload-stemcell <tarball>`, because the wrapper's own `upload-stemcell` builds only the light one. [Light and heavy uploads through the wrapper](multi-cluster.md#light-and-heavy-uploads-through-the-wrapper) covers what that does to the shared export.

6. Run a deploy that uses an AZ on each entry, or `bosh -d <deployment> instances`.
   We should see each VM come up in the cluster that its AZ's entry reaches.

We know the change worked when the cloud-config update and the deploy both pass, `bosh stemcells` lists every entry, and each entry has its own cache template on its own cluster. When `bosh clean-up --all` later fails on the stemcell rows, [the troubleshooting entry](troubleshooting.md#the-first-bosh-clean-up---all-fails-on-stemcell-rows) says to run it again.

Before we remove an entry from the cpi-config, we delete the deployments that use its AZs and run `bosh clean-up --all` while the entry is still there. The Director deletes each stemcell row through that row's entry, so a row whose entry has gone stops the clean-up, as [the troubleshooting entry](troubleshooting.md#bosh-clean-up---all-fails-with-cpi-not-found-in-cpi-config) describes.

## Stemcell lifecycle across directors

When two or more BOSH directors share one PVE cluster — a management director and one or more environment directors, or several environment directors side by side — they can all depend on the same stemcell. This section covers what happens to a stemcell's cache template and backing qcow2 file as each director independently uploads and cleans it up. See [Design Decisions, D3](design-decisions.md#d3-stemcell-refcounting-per-director-reference-sets) for the reference-tracking design behind this.

A template tagged `bosh-stemcell-node-<node>` or `bosh-stemcell-storage-<storage-id>` is a replica. Replicas hold no reference of their own. From this release, `delete_stemcell` sweeps every replica of the stemcell whose last reference just dropped, whichever director built it. Earlier releases preserved a replica whose provenance named another director, and left it behind. A replica that still backs a linked clone makes `delete_stemcell` fail with the replica's VMID and node, and the retry resumes once those VMs are gone.

### Which stemcell is a VM running?

A VM the CPI creates carries a `stemcell--<name>-<version>` tag naming the stemcell it booted from, and the same identity in its Notes under the `bosh_stemcell` key. That covers every VM built from a stemcell the CPI uploaded, and the exceptions are listed at the end of this section. A Director never sends the stemcell in its `set_vm_metadata` payload, so before this the answer lived only inside the guest. Now PVE can answer it for every VM, including one a `bosh create-env` Director created, without waiting for the first metadata sync that writes the readable `stemcell:` line described below.

The tag uses the CPI's usual tag alphabet, so the version's dot becomes a dash, and the stemcell `bosh-openstack-kvm-ubuntu-noble-1.585` is tagged `stemcell--bosh-openstack-kvm-ubuntu-noble-1-585`. The Notes record keeps the exact version, along with the full stemcell CID, its kind, and its content sha8.

```bash
# Every VM still on a given stemcell
pvesh get /cluster/resources --type vm \
  | jq -r --arg want stemcell--bosh-openstack-kvm-ubuntu-noble-1-585 \
      '.[] | select(.tags != null) | select(.tags | gsub(","; ";") | split(";") | index($want)) | "\(.vmid) \(.name)"'

# Group the whole cluster by stemcell, to see what a bump has left behind
pvesh get /cluster/resources --type vm \
  | jq -r '.[] | select(.tags != null) | .tags | gsub(","; ";") | split(";")[] | select(startswith("stemcell--"))' \
  | sort | uniq -c

# The exact version, CID, kind, and sha8 for one VM
pvesh get /nodes/<node>/qemu/<vmid>/config --output-format json \
  | jq -r '.description' | grep -o '<!--BOSH:.*-->' | sed 's/<!--BOSH://; s/-->//' | jq '.bosh_stemcell'
```

Match on a whole tag rather than on a substring. A `contains` test for `stemcell--...-1-585` also matches `stemcell--...-1-5850`, which is why both queries above split the field first.

Both records are written once, when the VM is created, and neither is rebuilt afterwards. A VM whose tag names an old stemcell has genuinely not been recreated since that stemcell was current, which is exactly the question a stemcell bump raises. VMs created before this release carry no tag and no record; recreate them and they pick both up.

Four limits are worth knowing. The identity is derived from the stemcell's qcow2 filename, so it is always present for a `:heavy:` stemcell the CPI uploaded, and it is present for a `:light:` one only when the operator named the file the way the CPI does, as `bosh-stemcell-<name>-<version>-<sha8>.qcow2`. A file named anything else gets no tag and no record rather than a guess. A derived label longer than 96 characters, or carrying anything outside `A-Za-z0-9._-`, is refused for the same reason. And the readable `stemcell:` line in the Notes is written by `set_vm_metadata`, so it appears only after the first metadata sync, which a Director and `bosh create-env` both make right after `create_vm`. The fourth is the 350-byte cap on a VM's whole tag string. A VM with enough advertised routes to fill it loses the stemcell tag first, because we would rather keep the route tags that `delete_vm` needs to withdraw an SDN subnet. The CPI warns when that happens, and the VM still answers the Notes queries above even though the tag query misses it.

### Which Director owns a VM?

A VM the CPI creates carries a `bosh-owner--<director_uuid>` tag right after `bosh-cpi`, naming the Director whose `create_vm` built it. We can't use the `director--` tag for this, because `set_vm_metadata` rebuilds that tag from the Director's name on every sync, and two Directors can share a name. A request that carries no `director_uuid` gets no owner tag, and the CPI logs a warning instead.

A VM created before this release has `bosh-cpi` and no owner tag. The next `set_vm_metadata` from a Director adds the tag, but only when the VM's recorded instance matches the instance the Director names, or when the VM records no instance yet. The recorded instance comes from the `id:` and `name:` lines in the VM's Notes and from its `<job>--<id>` tag. We compare the instance UUID alone, so a renamed instance group still matches, and a `bosh create-env` VM is compared by the index at the end of its name. An index that only a tag records is compared only with another index, so an operator tag such as `web--7` on a `web` VM never blocks the Director's own instance. An index in the Notes `name:` line comes from a real `bosh create-env` sync, so it stays comparable, and a Director's metadata that reaches a create-env VM is refused. When nothing the VM records can be compared with the instance the Director names, for example when the only record is a UUID-valued tag that no `job--` or `instance-group--` tag ties to the VM, the call goes ahead and the CPI adds no owner tag. A UUID-valued tag like that, naming a different instance, also stops the owner tag from being added even when the rest of the VM's record matches. Stemcell templates and disk parkers never get one either, because several Directors may share them.

That comparison runs on every `set_vm_metadata`, whether or not the VM has an owner tag. When the VM records a different instance than the one the Director names, the call fails with a non-retriable error and the VM keeps its name, tags, and Notes. This usually means a VM CID now points at a different VM, for example after a `cpi_config` entry or an AZ moved to another cluster, so check the CID against the cluster before retrying. When the CPI can't read the VM's config, the call fails as retriable, because it can't run the comparison. The exception is a CPI with a storage allocation journal configured, where that read failure was already a non-retriable error before this release and still is. Metadata that names no instance skips the check.

The `bosh-owner--`, `bosh-cid--`, `bosh-cluster--`, and `stemcell--` prefixes belong to the CPI. A `cloud_properties.tags` or disk tag key that would render one of them, or any of the prefixes `set_vm_metadata` rebuilds, is dropped with a warning that names the key. An instance group whose name would render one of them gets no `<job>--<id>` tag.

```bash
# Every VM a given Director created
pvesh get /cluster/resources --type vm \
  | jq -r --arg want bosh-owner--<director_uuid> \
      '.[] | select(.tags != null) | select(.tags | gsub(","; ";") | split(";") | index($want)) | "\(.vmid) \(.name)"'
```

Every call that acts on a VM checks this tag first. A call from one Director never stops, reboots, retags, attaches to, detaches from, snapshots, or destroys a VM whose owner tag names another Director. The same holds for disk parkers, stemcell templates, containers, and VMs that carry no BOSH marker at all. `has_vm` answers `false` for such a VM, so the Director never adopts another Director's VM. A recreate that follows stops at the `delete_vm` refusal until we drop the stale record with cloud-check's "Delete VM reference" or `bosh delete-deployment --force`. The fast-path `delete_vm` sweep reaps the stalled deletes that carry this Director's owner tag, plus legacy stragglers that carry no owner tag, and it leaves another Director's for that Director to finish. [Configuration](configuration.md#vm-ownership-guard) describes `pve.vm_ownership_guard`, and [Troubleshooting](troubleshooting.md#a-call-refuses-a-vm-this-director-does-not-own) covers the refusal and its escapes.

### Which cluster does a VM CID name?

A VM this release creates gets a CID such as `101-0a1b2c3d-4e5f6a7b`. The middle part is the cluster stamp, which is the first eight hex characters of the SHA-256 of the cluster's root CA. The last part is a token that's new for every VM. The VM carries both as tags, `bosh-cluster--0a1b2c3d` and `bosh-cid--4e5f6a7b`, right after its owner tag. A call whose CID names a different cluster than the VM records is refused. So is a call whose VMID is absent from this cluster when the CID's stamp names another cluster, because the VM may still be running there. A VM that carries a different token took over the VMID after the CID's VM was deleted, so the call answers `VMNotFound` and leaves it alone. A VM created before the upgrade keeps its bare CID until a `bosh recreate` gives it a stamped one. A CID with the stamp `00000000` comes from a create that couldn't read the root CA, and when its VM is gone it gets the same answers a bare CID gets. Its token still counts, though. When a VM with a different token holds its VMID, the call answers `VMNotFound` and leaves that VM alone, even on a cluster other than the one that created the CID's VM, because the stamp can't say which cluster that was.

```bash
# Decode a CID, then find the VM that carries its token
pve-ops decode 101-0a1b2c3d-4e5f6a7b
pvesh get /cluster/resources --type vm \
  | jq -r --arg want bosh-cid--4e5f6a7b \
      '.[] | select(.tags != null) | select(.tags | gsub(","; ";") | split(";") | index($want)) | "\(.vmid) \(.node) \(.name)"'

# This cluster's own stamp, read on any of its nodes
pvesh get /nodes/$(hostname)/certificates/info --output-format json \
  | jq -r '.[] | select(.filename == "pve-root-ca.pem") | .fingerprint' \
  | tr -d ':' | tr 'A-F' 'a-f' | cut -c1-8
```

`pve.vm_cid_format: bare` makes the CPI return bare CIDs again while it keeps writing both tags. [Configuration](configuration.md#vm-ownership-guard) describes it, and [Troubleshooting](troubleshooting.md#a-call-refuses-a-cid-that-names-another-cluster) covers the refusal.

### Rotating a cluster's root CA

Replacing a cluster's root CA, for example after the old one expired or leaked, changes the cluster's stamp. Every stamped CID created before the rotation then names a stamp this cluster no longer reports. Calls on those VMs keep working while the VMs exist, because each VM's `bosh-cluster--` tag still matches its CID. Once one of those VMs is gone, though, its CID looks like it names another cluster, so `delete_vm` refuses it rather than answering success. We record the old stamp before rotating, so nothing waits on the refusal:

1. Read the current stamp with the `pvesh` command in [Which cluster does a VM CID name?](#which-cluster-does-a-vm-cid-name) and note it.

2. Add it to `pve.cluster_stamp_aliases` at the job level, or as `pve_cluster_stamp_aliases` in the `cpi_config` entry for this cluster, and redeploy the CPI.

3. Rotate the CA, then read the stamp again to confirm it changed.

An alias can stay as long as any CID created before the rotation is still in a Director's records. A `bosh recreate` gives a VM a CID with the new stamp. When the refusal already happened, its message names both stamps, and we add the CID's stamp the same way. We never add a stamp that belongs to another cluster we run, because its CIDs would then count as this cluster's, and `delete_vm` would answer success for a VM that may still be running there.

### `bosh delete-vm` on a VM with no instance goes to the default CPI

`bosh delete-vm <cid>` for a VM that no instance references can't tell the Director which `cpi_config` entry the VM came from. The Director sends the call to its default CPI with no entry properties, so the job-level cluster receives it. A stamped CID from another cluster is refused there, because of its stamp. A bare CID isn't, and the job cluster destroys whatever VM of ours holds that VMID, so we run `bosh delete-vm` with a bare CID only when we know the VM lives on the job cluster. We delete a VM on another cluster in PVE ourselves.

### Downgrading below stamped CIDs

0.9.3 and every earlier release can't parse a stamped CID. They fail every call on a stamped VM and can't delete a stamped snapshot, and `bosh create-env` can't delete a Director VM whose state file holds a stamped CID. Nothing is dropped silently, but nothing on a stamped VM works either. Journal records and parker provenance hold bare VMIDs, so they stay readable. Before rolling back, we replace every stamped CID in this order:

1. Set `pve.vm_cid_format: bare` and redeploy the CPI, so no new stamped CID is created.

2. Run `bosh unignore` on every instance that `bosh instances` shows as ignored, because `bosh recreate` skips ignored instances and their CIDs would stay stamped. We can ignore them again once step 3 is done.

3. Run `bosh recreate` on every deployment, so every VM gets a bare CID.

4. Run `bosh create-env --recreate` for each Director that `bosh create-env` manages, so its state file holds a bare CID.

5. Delete every snapshot whose CID is stamped, with `bosh delete-snapshot`, because the older release can't.

6. Check `bosh orphaned-vms` for stamped CIDs. A recreate that uses create-swap-delete orphans the old VM, so step 3 can leave stamped CIDs there. We delete each one with `bosh delete-vm <cid>`, or we wait until the Director's orphaned-VM cleanup removes it, because the older release can't delete them. [`bosh delete-vm` on a VM with no instance goes to the default CPI](#bosh-delete-vm-on-a-vm-with-no-instance-goes-to-the-default-cpi) applies to these VMs too.

7. Roll back the CPI release.

Setting `bare` before the first upgrade to a stamping release skips steps 2 through 6, because no stamped CID ever exists.

### Cycling PVE nodes during a rolling upgrade

When a cluster has more than one API endpoint for the CPI to try, we no longer repoint `pve.host` away from a node before we cycle it. An endpoint list in `pve.endpoints`, or the nodes that discovery adds to a single-host deployment that authenticates with an API token, gives the CPI somewhere else to go. A call that finds its first endpoint down walks to the next one, and every later request in that call starts on the endpoint that answered. With the default 5-second dial timeout and 10-second handshake timeout, a dead first endpoint costs a call at most 15 seconds and not a failed deploy. [Endpoint Failover](endpoint-failover.md) covers how the CPI chooses an endpoint, and [Configuration](configuration.md) covers the properties.

Two limits are worth knowing before we start the first node.

- A mutation that may already have reached a node stays on that node.
  If the node goes away while a clone, a disk create, or a destroy is in flight, the CPI does not replay it on another node, because the first node might still be running it. Any retry within that call stays on the same node, so the call fails or waits as it did before, and the Director's retry starts a new call that can use whichever endpoint answers. Reads and the ticket login do move.

- A pool with one endpoint behaves as it always did.
  A deployment that sets only `pve.host`, with discovery off or without an API token, has nowhere to go. We list at least two nodes in `pve.endpoints`, or we leave discovery on, before we rely on this.

We still cycle one node at a time, and we wait for the cluster to be quorate before we start the next one. For each node, we follow these steps.

1. Run `pve-ops ha drain <node>` as a dry run, and read which VMs leave and which can't.

2. Run `ha-manager crm-command node-maintenance enable <node>` on a PVE node.

3. Turn resurrection off for the deployments that the dry run names, for example with `bosh update-resurrection off`.

4. Run `pve-ops ha drain <node> --apply`, and wait for it to exit with 0.

5. Do the maintenance, such as the upgrade and the reboot, and wait until the node is back and the cluster is quorate.

6. Run `ha-manager crm-command node-maintenance disable <node>` on a PVE node.

7. Run `pve-ops ha undrain <node> --apply`, which starts the stopped VMs and rebalances.

8. Turn resurrection back on for the same deployments.

A VM with no HA resource is neither moved by maintenance nor stopped by the drain, so we stop it ourselves through the Director before step 5, or we accept that it goes down with the node. [Draining and undraining a node with `pve-ops ha drain`](#draining-and-undraining-a-node-with-pve-ops-ha-drain) describes both commands.

### Rebalancing HA VMs after node maintenance

A rolling maintenance moves every HA VM off each node in turn, and PVE moves a VM back only when its HA resource has `failback` on. So after the last node comes back, some VMs can sit outside their AZ, two members of an anti-affinity group can share a node, and the committed memory of the nodes can be far apart. The `pve-ops ha rebalance` command puts this Director's HA VMs back into a good spread. It works in three phases. First it moves a member of an anti-affinity rule off a node that holds another member of the same rule, and it moves a member of an anti-affinity group off a node that holds more than the group's share, which is the number of members the group's rules name divided by the number of nodes those members may run on, rounded up. Then it moves each VM that sits outside its AZ back into its AZ, and last it evens out committed memory among the VMs that may run on more than one node. Each move is a live migration through PVE HA, and it stays inside the VM's pin and AZ and obeys the anti-affinity rules.

We run it once the last node has left maintenance and `bosh tasks` shows no task running, because a CPI call and a move must not change the same VM. The command runs as `vcap` on the Director, because it opens the allocation journal, which opens only for its owner. It also needs the Director's UUID, which `bosh env --json` prints in the `uuid` column, because it moves only the VMs that Director owns.

```bash
sudo -u vcap /var/vcap/packages/pve_cpi/bin/pve-ops ha rebalance --director-uuid <uuid>
```

That first run is a dry run. It reads the cluster, takes no lock, and writes nothing. It prints the gates, the committed memory of each node now and after the plan, each move it plans with the reason, and each VM it leaves where it is.

```
HA rebalance for Director 6c8e1d2a-4b3f-4e5a-9c7d-0f1e2d3c4b5a on cluster prod-east (token bosh-cpi@pve!****), a dry run, so nothing moved; add --apply to move
pve-ops 0.12.0 with the configuration /var/vcap/jobs/pve_cpi/config/cpi.json
Gates
  pve.ha.mode is auto, and the HA profile lets the CPI write HA state
  quorum is held
  all 3 nodes are online, and none is in maintenance
  the HA manager's master is pve1, and its status is fresh
  no HA service is migrating or relocating
  the allocation journal holds no open record
  no CPI task or lock is active
  no other rebalance holds the run lock
Nodes, memory committed now and after the plan
  pve1   72%   50%
  pve2   55%   52%
  pve3   20%   45%
Moves
  1. VM 1042 (vm-8a1f2c3d) from pve1 to pve3, because VM 1042 is on pve1, and its AZ z3 is pve3
  2. VM 1077 (vm-4b9e0d21) from pve1 to pve3, because pve1 holds 61% of its memory and pve3 holds 33%, and the move narrows the spread of 28 points
Skipped
  VM 1050 (vm-c07a5e19) on pve1, because it is a replicate VM, and a move would reverse its replication job
2 moves planned, 0 made, 1 VM skipped, and 0 deferred, and the spread goes from 52 points to 7
```

The spread is the committed memory of the fullest node minus that of the emptiest, in points. Memory evening starts only when the spread passes the start threshold, which is 0.15 by default, and it stops once the spread falls below the stop threshold, which is 0.05. The command never moves a `replicate` VM, a `hold` VM, a DLB member, a parker, a VM with a node-local disk, a VM in a positive resource affinity rule, a stopped VM, a VM that an attach or a narrowing is moving under a fresh `bosh-failback-hold` token, or a VM of another Director. It counts the memory of every VM, moved or not.

We then run the same command with `--apply`. The run takes the run lock `bosh-lock-ha-rebalance`, so two runs never move VMs at once, and it makes one move at a time. The lock's lease has no renewal, so it covers the longest run and the move that may still run at its end, which includes twice `pve.ha.motion_wait_sec` for the settles before and after the migration. A run whose lease could run out during its next move stops before that move and warns that we should run the command again, and the rebalance that an undrain runs under the undrain's lock does the same. Before each move it checks every gate again from fresh reads and waits for the cluster to settle. For each move it turns the HA resource's `failback` off and writes the mark `bosh-rebalance-to=<node>` into the resource's comment in one write, and then it asks PVE HA to migrate the VM and waits for the VM to settle on the target. When the VM's pin prefers another node, the run rewrites the pin's priorities so the target comes first, which the move's reason states with `its pin is rewritten to prefer <node>`, because failback would otherwise send the VM back. It then restores the `failback` flag the CPI writes for the VM, drops the mark, and checks after two rounds of the HA manager that the VM stayed. When an attach or a narrowing has put a fresh `bosh-failback-hold` token on the resource while the VM moved, that write keeps `failback` off and leaves the token, because the hold's writer is still moving the VM. Right before it asks PVE HA for the migration, the run reads the resource again. When another writer has removed the mark or pointed it at another node, or has removed the resource, the run sends no request and leaves the resource as that writer left it, and it reports the move as `refused` because another writer took the VM. That read also refuses the move when a positive resource affinity rule now names the VM, because PVE HA would move the rule's other resources with it, outside the plan. PVE queues a migration before it reports the resources that move with the VM, so when a rule appears between that read and the request, the run can't take the move back. It finishes the move, names the other resources in the move's reason, and stops with exit code 1, and we then check the spread and run the command again. When the migration request gets no answer from PVE, because the connection dropped or a gateway timed out, the run waits for the move as if PVE had answered. PVE may have queued the request, so the run keeps the mark until two rounds of the HA manager show whether the VM moved. The run plans again after each move, so the moves it makes can differ from the first plan, and the output then says so.

The command takes these flags.

- `--apply`
  This makes the moves. Without it the command is a dry run.

- `--director-uuid UUID` or `--request PATH`
  One of these names the Director whose VMs the run may move. A request's context also picks a cpi-config entry, and the run reads that entry's cluster.

- `--config PATH`
  This names the CPI configuration file, as it does for `pve-ops ha`.

- `--max-moves N`
  This caps the moves of one run, and it is 10 by default. The summary counts the moves left over as deferred, and the next run makes them.

- `--settle-timeout DURATION`
  This bounds the wait for each move to settle, and the wait for the cluster to settle before each move. It is 10 minutes by default and at least 1 minute.

- `--max-duration DURATION`
  This bounds the run, and no move starts after it passes. It is 60 minutes by default.

- `--start-threshold FRACTION` and `--stop-threshold FRACTION`
  These set when memory evening starts and stops. The start threshold lies above 0 and at most 1, and the stop threshold is at least 0 and below the start threshold.

- `--restore-only`
  This runs only the separation and the AZ restore phases.

- `--json`
  This prints the report as JSON, with the Director's UUID, the masked credential, the gates, the warnings, the nodes, the first plan, the moves with their outcomes, the skipped VMs, the spread before and after, and the exit code.

The gates refuse a run, or stop it before its next move, when `pve.ha.mode` is `off` or the HA profile doesn't let the CPI write HA state, when the cluster lacks quorum, when a node is offline, in maintenance, or unable to take work, when the HA manager has no master or its status is stale, when an HA service is migrating or relocating, when the allocation journal holds an open record, when a CPI lock is held or the CPI's token user runs a task, or when another rebalance holds the run lock. A refused gate prints `REFUSED:` with the reason. A dry run whose gate refused still prints the plan an applied run would make once the gate clears.

The gates check the cluster at one moment, and they can't see every CPI call. A `create_vm` reads the cluster and picks a node before it takes its first lock or starts its first PVE task, so a create in that part of its work passes the CPI activity gate. Such a create can't change the VM a move changes, because it hasn't claimed a VMID or cloned a disk yet. Its choice of node can rest on the committed memory and the VM layout from before the move, though, so the spread can end up a little off. When the create then writes an anti-affinity rule that puts its new VM with a member the move just placed on the same node, PVE HA moves one of the two, and the run reports exit code 1 when that VM was its own. Writes to the HA rules check the digest of the rules file, so a create and a move that write rules at the same moment never overwrite each other. When the move's pin write finds the rules lock busy, it leaves the move part way for the next run to finish. A later `pve-ops ha` reports any skew that is left as `spread_skewed` advice. That's why we check that `bosh tasks` shows nothing running before an `--apply` run, and run it again once a deploy that started during the run has finished.

The command exits with these codes.

- `0`
  The run ended clean, and every move it made landed or PVE refused it with the VM left where it was.

- `1`
  A read or a move failed, the run was interrupted, or a move was left part way. The output names the cause, and a move left part way prints `left part way`.

- `2`
  A flag was wrong or missing.

- `3`
  A gate refused the run, or stopped it before its next move, and no move was left part way.

An interrupt or SIGTERM never cuts a write short. The run finishes the write in progress, releases the run lock, and stops before its next move. A move that a stop or a failure leaves part way keeps its mark, and the mark keeps the resource's `failback` off, so a `set_vm_metadata` or `reboot_vm` can't pull the VM back to its old node. `pve-ops ha` reports such a VM as the `rebalance_pending` problem. The next run with `--apply` finishes each marked move whose VM reached its target, undoes each one whose VM never left its node, and only then plans anything new, as [A rebalance left a move part way](troubleshooting.md#a-rebalance-left-a-move-part-way) describes.

On a bastion, the dry run works, but `--apply` refuses at the journal gate when the configuration names an allocation journal the bastion can't read. When the configuration names no journal, `--apply` runs from a bastion and prints a warning, because the run then can't see the Director's own journal records. That is safe only when the Director's own CPI has no journal configured either. For a Director with several `cpi_config` entries, we pass `--request` with a context that names the entry and the Director's UUID, once for each entry, because the run reads one cluster.

The undrain runs this same rebalance when it finishes, so after a drain we normally don't run the rebalance by hand. We run it alone when the undrain ran with `--no-rebalance`, when the rebalance ended with 1 or 3 inside the undrain, or after a maintenance that we did without the drain.

### Rolling back below endpoint lists

Rolling back to 0.9.7 from the release that adds endpoint lists meets four facts, and we deal with the first two before we start.

1. A manifest without `pve.host` does not render on the older release.

   The older template reads `pve.host` with no default, so a manifest that relies on `pve.endpoints` alone fails at deploy time. We keep `pve.host` set beside `pve.endpoints`, and the two must name the same host and port as the first list entry. Doing that from the first upgrade means the rollback needs no manifest change.

2. A `cpi_config` entry that names the new keys breaks on the older release.

   The older release does not know `pve_endpoints` or `pve_api_endpoint_discovery`. It rejects every call routed through such an entry with "context carries ... which the pve_cpi job spec does not define", and that takes the whole availability zone down. We keep those keys out of `cpi_config` entries until the new release has settled, or we remove them from the cpi config before we roll back. The job-level properties are harmless, because the older job spec does not define them and its template never reads them.

3. Cache files stay behind.

   The older release never reads them. Files in the temporary directory disappear with the ephemeral disk, and the `endpoint-cache` directory beside the allocation journal stays on the persistent disk where the older release never looks. We can delete either one. If we upgrade again with the same credential, any files that remain are still valid, though the sweep that removes files unmodified for 30 days may already have taken some of them.

4. The guard on removed HA keys plays no part.

   The template refuses removed `pve.ha` keys, and none of the endpoint properties is on that list, so this work adds nothing to that check on a rollback.

The stamped-CID steps in [Downgrading below stamped CIDs](#downgrading-below-stamped-cids) are separate, and they apply when the older release is 0.9.3 or earlier.

### `bosh clean-up` is safe across directors

Each cache template records the set of director UUIDs currently depending on it. Running `bosh clean-up` (or `bosh clean-up --all`) on one director only removes *that* director's own reference — it never touches another director's reference to the same template, even though both directors may be looking at the same PVE cluster. The cache template, and for a CPI-uploaded (`:heavy:`) stemcell its qcow2 file, survive until the last director referencing them releases its reference. An operator-managed (`:light:`) qcow2 is never removed by the CPI regardless of reference count.

The template's `director--` tags follow the same set. When a director's reference goes and other directors still hold the template, the same write removes that director's tag, and it adds the `bosh-stemcell-cache` tag if the template would otherwise be left with no generation marker. The tags therefore name the directors that hold the template now. When the last reference goes, its tag stays, because the template is about to be destroyed, and if PVE refuses the destroy, that tag keeps the template visible to the orphan prune, which runs only when `pve.stemcell.prune_orphans` is on. The same director's later `delete_stemcell` finds the template without the tag, through its sha8 and the generation marker, and resumes the destroy.

In practice, cleaning up one director's unused stemcells is always safe to run on its own schedule, independent of what any other director sharing the cluster is doing. On a Director with several cpi-config entries, the first `bosh clean-up --all` can still fail on the stemcell rows because of a Director-side race, and a rerun succeeds. See [The first bosh clean-up --all fails on stemcell rows](troubleshooting.md#the-first-bosh-clean-up---all-fails-on-stemcell-rows).

### `bosh-destroy-pending` retry semantics

If a template's last reference is released but the destroy itself fails — most commonly because the template still backs a linked clone — the CPI does not leave the reference record in an inconsistent state. It tags the template `bosh-destroy-pending` and returns an error naming the blocker:

```
delete_stemcell: template VM 30012 (node "pve-n1") still backs one or more linked-clone VMs;
delete or migrate those VMs, then retry delete_stemcell (the destroy is marked pending and will resume): ...
```

Delete or migrate the linked-clone VM(s) named in the error, then retry `delete_stemcell` (or let a later `bosh clean-up` retry it) — the CPI recognizes the `bosh-destroy-pending` tag and resumes the destroy directly rather than re-deriving whether this was really the last reference.

### `IsBaseVolumeInUse`: linked clones must go first

The underlying condition above is PVE refusing to delete a base volume that a linked clone still depends on. If you see this error while manually cleaning up templates via `qm destroy`, the fix is the same: find and remove the dependent VM(s) first.

```bash
# Find VMs cloned from a specific template (linked clones reference the base disk by volid)
grep -rl 'base-<template_vmid>-disk-0' /etc/pve/nodes/*/qemu-server/*.conf
```

### Clearing a stale director ref

When we rebuild a Director, it comes back with a new UUID. Its old UUID stays in `director_refs` on every stemcell cache template it registered, and no CPI call ever removes it, so `bosh delete-stemcell` and `bosh clean-up` can never free those templates. The `cpi stemcell-refs` command reports the refs on every cache template and removes one we name. It runs on the Director VM with the same `cpi.json` the CPI job uses, and it changes nothing until we pass `--apply`.

The command acts on the one cluster that the `cpi.json` we give it names. It can't see templates or VMs on another cluster, such as a second AZ that a `cpi_config` entry adds, and it can't see the owner tags on that cluster's VMs either. On a multi-cluster foundation we run the report and the prune once per cluster, each time with a `cpi.json` aimed at that cluster, and we check the owned VM count on every cluster before we prune.

We start with the report, which only reads:

```bash
sudo -u vcap /var/vcap/packages/pve_cpi/bin/cpi stemcell-refs report --config /var/vcap/jobs/pve_cpi/config/cpi.json
```

The report has three tables. The first lists every stemcell cache template with its node, VMID, name, and sha8, the refs in its description, its `director--` tags, whether the refs and tags disagree, and the linked clones we found. The description's refs decide whether a template stays, and the tags name the directors that hold it now, so the two agree once `--reconcile-tags` has run and the CPI has touched the template since. A disagreement before that isn't an error, because releases before this one never removed a departing director's tag, and a template that one `delete_stemcell` touches still carries its other stale tags until `--reconcile-tags` clears them. A template whose description is within 1024 bytes of PVE's 8192-byte limit gets a note with its size, and a line under the table counts those templates. The CPI refuses to add a director's ref when the description would pass the limit, so an upload that reuses such a template fails until we prune its stale refs. The second table lists every distinct ref with the templates that hold it, how many VMs that ref owns, and which templates a prune of that ref would free or leave blocked. A VM counts as owned when it carries the ref's `bosh-owner--` tag, and a disk parker counts when it carries the ref's `director--` tag, because parkers record their Director there instead. The free and blocked columns assume `--allow-owned-vms`, so they show what a prune would do once the owner check passes. While ownership is unknown, as the next paragraph describes, the owned count reads unknown and the free column reads blocked for every ref, because no prune can go ahead. A blank ref gets its own row with a note, because the prune can't name it. The third table, PRUNE --vmid CANDIDATES, lists the templates a `--vmid` prune could free, with the reason for each one it would refuse. A legacy template from before 0.9.2 is freeable there. It has a blank description and a legacy cache name, and it may carry sha8 or node tags, but it never carries a `director--` tag or the `bosh-stemcell-cache` tag. A template with a blank description that does carry a `director--` or `bosh-stemcell-cache` tag is marked "refs unknown" and is never freeable, because its ref record was lost and we can't tell who uses it. A template marked "untracked" has provenance JSON but no generation marker. It comes from a CPI generation that never recorded refs, so its empty ref set doesn't prove that nobody uses it. No `--director` prune and no plain `--vmid` prune destroys it, even when it is the last holder of a ref or a replica of a stemcell whose last main template goes. Once we know nothing uses it, we free it with `--vmid` and `--untracked`, as [Freeing an untracked template](#freeing-an-untracked-template) describes. The third table lists every untracked template too, including those that hold refs. Its FREE WITH column names the command that frees each freeable template, or the linked clones that block one the prune would refuse. That command carries the `--config` path the report ran with, so we can paste it as printed, and it is a dry run until we add `--apply`. The second table also lists, for each ref, the replicas a prune of that ref would keep with no ref, with the reason for each. With `--json` we get the same data as JSON. The report exits 1 when some guest's config couldn't be read, because the linked-clone column can't be trusted then.

The second table also helps when a ref matches no UUID that `bosh env` prints. A Director built with `bosh create-env` registers its stemcell refs under the `director_id` in its state file, and that value isn't the UUID `bosh env` prints, so such a ref isn't necessarily stale. When a ref's owner tag sits on a guest that also carries `director--bosh-init`, the NOTES column names that guest, such as `owned by create-env Director VM 4441 (bosh-init, deployment lab-bosh)`.

When no guest carries a ref's owner tag and ownership is known, the report prints one note under the table, however many refs it covers. The note says the ref may be the `director_id` of a create-env Director whose VM lives on another cluster, or that the ref may be stale because that Director was rebuilt. It asks us to compare the ref with the `director_id` in each create-env state file before we treat it as stale, and it names the refs it applies to, up to five and then a count of the rest. Because every guest in this cluster already carries an owner tag when the note prints, `adopt-owners` has nothing to stamp, so the note doesn't suggest it. A create-env Director VM that lacks its owner tag shows up under OWNERSHIP UNKNOWN instead, which prints the `adopt-owners` command for it.

The note never appears while ownership is unknown, and a blank ref, the `unknown-director` ref, and a ref that a disk parker holds never get it. Neither addition changes what a prune does or refuses. With `--json`, each ref carries `create_env_directors` and `possible_create_env_ref`, and the report carries `possible_create_env_ref_note`.

The owned count can only see VMs that carry an owner tag, and a VM created before 0.9.4 carries none until a later `set_vm_metadata` from its Director adds it. A live Director whose VMs all predate the tag would therefore count zero owned VMs, and its ref would look safe to prune. So the scan sorts every guest that isn't a template into one of three kinds:

- A disk parker carries the `bosh-parker` tag. It never makes ownership unknown.

- A BOSH guest carries `bosh-cpi`, a `director--` tag, a `deployment--` tag, or an owner tag, or its notes hold the `director:`, `deployment:`, and `name:` lines that `set_vm_metadata` writes. These are the same marks the CPI's owner check accepts as proof that a VM is a BOSH VM. A guest that also carries a stemcell marker counts here too.

- Any other guest is one that BOSH doesn't manage, and the owner check ignores it.

When any BOSH guest carries no owner tag, the report marks ownership as unknown for every ref, and it adds an OWNERSHIP UNKNOWN section. A guest whose config the scan couldn't read counts the same way, whatever tags the guest listing shows for it, because we can't see what it carries. The owner check and this rule both cover only the cluster the `cpi.json` names, so a VM on another cluster neither counts as owned nor makes ownership unknown here. The section explains why ownership is unknown, then lists the guests grouped by director name and deployment. Under each group that adopt-owners can stamp, it prints the `cpi stemcell-refs adopt-owners` command that covers the group, with a note that says where to find the Director UUID the command needs. The command names every deployment whose uncovered VMs carry that director name, because adopt-owners requires the full list, and we compare it with `bosh -e <env> deployments` on that Director before we run it. The command never fills in a UUID. It always prints the `<director-uuid>` placeholder, because an owner tag on some of a name's VMs doesn't prove whose the uncovered VMs are when two Directors share the name. The note says where the value comes from, which is `bosh -e <env> env` for a deployed Director and the `director_id` in the create-env state file for a `bosh-init` VM, and it names any deployments of the name whose VMs already carry an owner tag. A group that adopt-owners can't stamp gets the reason and the way out instead:

- A guest whose config couldn't be read needs the cause fixed, such as a node that is down or a permission the CPI's API token lacks, and then a fresh report.

- A guest without `bosh-cpi`, without a `director--` or `deployment--` tag, or marked as a BOSH VM only by the Director lines in its notes, needs us to confirm which Director runs it. We then recreate the instance with `bosh -d <deployment> recreate <instance>` on CPI 0.9.4 or later, which writes the full tag set with the owner tag, or we add the missing `bosh-cpi`, `director--<name>`, and `deployment--<deployment>` tags in PVE and run adopt-owners. A VM that BOSH doesn't manage loses its BOSH tags and the Director lines in its notes, or we delete it.

- A guest with two `director--` or two `deployment--` tags needs the tags that don't belong to it removed, and then adopt-owners or a recreate.

- A guest with a stemcell marker is usually left over from a failed stemcell upload, and we delete it once we know no deployment uses it.

- A guest whose `director--` tag holds a UUID looks like a disk parker that lost its `bosh-parker` tag, so we restore that tag or delete the VM.

- A create-env Director VM that shares its deployment name with another create-env VM that already carries an owner tag can't be told apart from it, so we recreate that Director with `bosh create-env` on CPI 0.9.4 or later, or delete the VM if that Director is gone.

With `--json` the same groups come back in `uncovered`, next to `ownership_unknown` and its reason.

Before we prune a ref, we make sure its Director is gone. The owned VM count is the first check, because a live Director's VMs carry its owner tag. For the `unknown-director` ref the report shows that count as unknown, because a VM created without a Director UUID carries no owner tag, so no count can prove that such a caller has stopped. We also compare the ref with `bosh -e <env> status` on every Director that shares the cluster.

The prune is a dry run until we pass `--apply`:

```bash
cpi stemcell-refs prune --config /path/to/cpi.json --director <old-uuid>
cpi stemcell-refs prune --config /path/to/cpi.json --director <old-uuid> --apply
```

The ref can be any non-empty string, so test refs such as `debug-repro` work too, and so does `unknown-director` with the extra flag the rules below describe. The command trims the ref, compares it with each template's description refs after trimming them as well, and removes the ref as the description spells it. It matches the tag as `director--` plus the sanitized ref. For each template that holds the ref, replicas included, the apply takes the same per-VMID lock `delete_stemcell` uses, reads the template again, and removes the ref and its tag. If that tag was the template's last generation marker and the template stays, the same write adds the `bosh-stemcell-cache` tag, so a later scan still treats the template as tracked and a `--vmid` prune can free it. When no ref remains, it destroys the template the way `delete_stemcell` does. The apply handles the stemcell's main templates first and its per-node and per-storage replicas after them. A replica left with no ref is destroyed only when it passes the checks a `--vmid` prune makes, which means its sha8 is known, it backs no linked clone, and no main template of its stemcell survives the prune. Otherwise the replica stays with an empty ref set. When the last main template of a stemcell goes, the apply also destroys the replicas that already held no ref and back no linked clone. It leaves alone a replica that held no ref but has its own action in the plan, such as a stale tag to remove or a pending destroy, and an untracked replica. The plan lists each of those as kept, with the reason and whether a `--vmid` prune can free it afterwards. The plan lists every replica it will change, destroy, or keep, with the reason. If the apply keeps a replica it planned to destroy, for example because the main template's destroy failed, the result shows the replica as kept with the reason and the action it actually took. A template that carries only the tag, without the ref in its description, gets the tag removed and is never destroyed.

These rules keep the prune safe:

- If any BOSH guest in the cluster carries no owner tag, or any guest's config can't be read, a `--director` prune refuses and exits 1, whatever ref it names. `--allow-owned-vms` doesn't override this, because we can't know which Director those guests belong to. We stamp them with `adopt-owners`, as [Stamping owner tags on older VMs](#stamping-owner-tags-on-older-vms) describes, and prune again.

- If any VM still carries the ref's owner tag, or any disk parker carries its `director--` tag, the command refuses and exits 1. We pass `--allow-owned-vms` only when we know those VMs are orphans.

- The command can't tell who still uses the `unknown-director` ref, because VMs created without a Director UUID carry no owner tag. It refuses to prune that ref unless we pass `--allow-owned-vms`, which we do only when we know no such caller still deploys from those templates.

- A template whose description doesn't parse as provenance JSON is reported and never changed. A blank description counts as zero refs only on a legacy template, which has a legacy cache name and neither a `director--` tag nor the `bosh-stemcell-cache` tag. On any other template a blank description means the refs are unknown, so the prune reports that template and never changes it.

- A template that backs a linked clone is never destroyed, and it keeps its last ref. The prune still removes a ref that isn't the last one, or a stale tag, from such a template, because that write doesn't touch its disks. We delete or migrate the clone before the last ref can go. If the scan couldn't read some guest's config, every destroy is blocked, because we can't prove that no clone exists.

- Inside the lock, the apply checks that the guest is still a template with the name, description, and tags the plan saw. If anything changed, for example because a Director registered against it, the apply skips it. Just before each destroy, the apply reads the template once more and skips the destroy if its `bosh-destroy-pending` tag is gone, because a Director that registers against a template removes that tag. The apply scans the cluster again instead of reusing an earlier dry run's plan, so it acts on what the cluster holds at that moment. The text output of `--apply` prints that fresh plan, with the owned VMs and any scan errors, before the results.

- A replica that still holds another ref after the prune keeps that ref and is never destroyed. A replica that backs a linked clone loses the ref and stays in place, and it doesn't block the destroy of its stemcell's main template, because the clone depends on the replica's own base volume and not on the main template's.

- When one template fails, the command reports it, carries on with the others, and exits 1.

- When the `--director` ref matches no template's refs or tags, the command prints a warning on stderr, so a mistyped ref doesn't pass for a clean cluster.

If a destroy fails, the template keeps its `bosh-destroy-pending` tag, and running the same prune again resumes the destroy, the same way [`bosh-destroy-pending` retry semantics](#bosh-destroy-pending-retry-semantics) describes for `delete_stemcell`. This holds for `--untracked` too, and its error names the exact command to rerun.

To free one template that holds no ref, such as a legacy template the report lists as freeable, we name its VMID:

```bash
cpi stemcell-refs prune --config /path/to/cpi.json --vmid 30012 --apply
```

The command acts only when the guest is a template, is recognizably a stemcell cache template, provably holds no ref, and backs no linked clone. It recognizes a cache template by its provenance JSON, by its cache tags, or by the `bosh-stemcell-` name prefix that legacy templates use. It frees a template that carries no generation marker only when the template has the legacy shape, which means a `bosh-stemcell-` name and a blank description, and it refuses every other template without a marker. A hand-built image that someone named with the `bosh-stemcell-` prefix and left with blank notes has that shape too, so we confirm the VMID before we pass `--apply`. It refuses a replica that holds refs of its own, a replica whose sha8 is unknown, and a replica whose stemcell still has a live template. When we name a main template and its replica in the same run, the replica's stemcell has no live template left once the run is done, so the replica is freed with that template. The plan shows the replica as a skip that names the template, the destroy of that template sweeps it, and the run doesn't count it as a refusal. If that template's sweep doesn't free the replica, because the template was already gone when the apply reached it, its destroy failed, or an earlier template of the same stemcell failed first, the run counts the replica as a refusal and names the template and the `--vmid` command that frees the replica on its own. It frees a legacy template, and it refuses an untracked one with a reason that names `--untracked`. We can repeat `--vmid` to name several templates, and the command then judges each one on its own, so a refusal of one doesn't stop the others. When the template we name is the last one of its stemcell, the command also destroys that stemcell's replicas that would each pass these same checks on their own. The dry run lists those replicas, and it lists the ones it keeps with the reason.

To bring every template's tags in line with its refs, we reconcile them:

```bash
cpi stemcell-refs prune --config /path/to/cpi.json --reconcile-tags
cpi stemcell-refs prune --config /path/to/cpi.json --reconcile-tags --apply
```

The reconcile removes every `director--` tag that no ref in the template's description maps to, on every stemcell cache template the scan finds, replicas included. It keeps each tag that a ref maps to, and when the removals would leave a template with no generation marker, the same write adds the `bosh-stemcell-cache` tag. It never removes a ref and never destroys a template, because the description's refs alone decide whether a template stays. It skips a template whose config can't be read, whose description doesn't parse, whose refs are unknown, whose destroy is already pending, or that holds no ref, and it lists each skip with its reason without counting it as a failure. Each write takes the same per-VMID lock and the same fresh read as the other prunes, so the apply reports a template a Director changed after the plan and leaves it alone. We run it once after upgrading from an earlier release, so the tags on older templates name only the directors that hold them now. A template that holds no ref keeps its tags, because those tags are what keep it reachable by its director's orphan prune, and we free it with a `--vmid` prune once we know nothing uses it.

The prune frees templates, not files. For a `:heavy:` stemcell the qcow2 under `import/` stays, because a Director that still lists the stemcell could build a new template from it. Once no Director lists the stemcell, `pve-ops stemcells --orphans` shows the file with no correlated template, and we remove it with `pvesm free`, as the example in [Operator caveats](light-stemcells.md#operator-caveats) shows.

The exit code is 0 for success, 1 for a runtime, config, or refusal error or for any template that failed, and 2 for a usage error. We pass exactly one of `--director`, `--vmid`, and `--reconcile-tags`. `--allow-owned-vms` works only with `--director`, and `--untracked` works only with `--vmid`. With `--json`, an `--apply` run always prints the apply result, and when the plan is refused that result has `refused` set and an empty `results` list.

#### Freeing an untracked template

An untracked template has provenance JSON but no generation marker, so it comes from a CPI generation that never recorded refs. Its empty ref set doesn't prove that nobody uses it, which is why no other prune destroys it. Once we know nothing uses one, we free it by naming its VMID with `--untracked`:

```bash
cpi stemcell-refs prune --config /path/to/cpi.json --vmid 30012 --untracked
cpi stemcell-refs prune --config /path/to/cpi.json --vmid 30012 --vmid 30013 --untracked --apply
```

The command frees a template only when all of these hold:

- The guest is a template whose name follows stemcell cache naming, `bosh-stemcell-<name>-<version>`, with or without the `-<sha8>` suffix.

- Its description is provenance that `create_stemcell` wrote, in one of two forms. Full provenance is a JSON object with the `name`, `version`, `sha8`, and `created` string fields, and it records the sha8 there along with a stemcell name and version. An older CPI that ran with provenance turned off wrote minimal notes instead, and it recorded the sha8 only in the `bosh-stemcell-sha-` tag. Those notes hold exactly the `name`, `version`, `sha8`, `created`, and `stemcell_refs` fields, the first four are empty strings, and `stemcell_refs` holds at least one entry, each of which is the template's own stemcell CID, `template:<vmid>`. The older CPI dropped `stemcell_refs` once its list was empty, so it never wrote an empty one. Notes with any other field, with a `null` where a string belongs, with an empty `stemcell_refs`, or with a `stemcell_refs` entry that names something else are refused, and the refusal names the field.

- It carries a `bosh-stemcell-sha-` tag, and the sha8 agrees everywhere it appears. Every CPI that wrote provenance also wrote that tag, so the command refuses a template without one. Every `bosh-stemcell-sha-` tag names the same sha8, and the description's sha8, when it records one, matches that tag. For full provenance, the name must be the one `create_stemcell` builds from the provenance's stemcell name and version, with the sha8 suffix, or without it for provenance from before the CPI added the suffix. Minimal notes record no name or version, so their template's name may end in its version or in the tag's sha8, but never in some other sha8. The refusal names the signal that is missing or doesn't match, so a hand-built image or a template with a contradictory identity is never freed.

- It carries no generation marker, which means no `bosh-stemcell-cache` tag and no `director--` tag. The command refuses a tracked template, and we free that one with `--vmid` alone.

- Its description holds no `director_refs`. When it holds some, the refusal lists them, and the command also refuses a blank or unparseable description.

- The scan read every guest's config, and no guest has a disk based on the template.

- The template isn't running.

The dry run lists each template with its VMID, name, and node, along with each disk volume the template owns, the storage that volume lives on, and its provisioned size. It ends with the total provisioned size of the volumes the destroys would remove. On thin or copy-on-write storage, such as lvm-thin or qcow2 files, a volume often uses less space than its provisioned size, so the destroys can give back less than that total. When PVE doesn't report a volume's size, that size reads as unknown and the total becomes a lower bound. With `--json`, the plan carries the same volumes, sizes, and totals.

When a template's minimal notes carry a `stemcell_refs` entry, the dry run shows it on its own line, such as `legacy ref template:30430 (retired ref count, self-reference that no released CPI resolves)`. That entry is the stemcell CID that a CPI built before v0.1.0 handed its Director. It names the template itself, no released CPI accepts it, and `--untracked` doesn't count it as a ref, so before we apply we only need to know that the Director that held it is gone. With `--json`, the action lists those entries in `legacy_refs`.

The apply takes the per-VMID lock, reads the template's config again, and checks every condition again against that read. It also asks PVE for the template's status, and it lists every guest in the cluster again to look for linked clones. That second clone scan has ten minutes to finish, which keeps the lock's hold well inside its expiry, and a scan that runs out of time leaves the template alone and reports the failure. When the scan finishes, the apply reads the config once more and compares its digest with the locked read's, so an edit made outside the CPI during the scan, such as one in the PVE UI, leaves the template alone. That check also covers a resumed destroy, whose stamp is already in place. Only then does it stamp `bosh-destroy-pending`, in a write guarded by that read's config digest, so a change that lands while the checks run makes PVE refuse the stamp. After the lock is released, it destroys the template the same way a `--vmid` prune does. The apply reports a template that changed after the plan and leaves it alone. When the template's config is no longer on the node the plan saw, the apply lists the cluster before it calls the template gone. If the template now sits on another node, the result names that node and asks us to rerun the prune, and the run exits 1.

When the destroy fails after the stamp, the template keeps `bosh-destroy-pending`, and the error says so and names the exact command to rerun, such as `cpi stemcell-refs prune --config /path/to/cpi.json --vmid 30012 --untracked --apply`. Only an earlier `--untracked` apply stamps an untracked template, so `--untracked` resumes that destroy. We fix whatever made the destroy fail, such as a linked clone or a storage error, and run that command. The dry run marks such a template as resuming the pending destroy, and the apply checks every condition again before it destroys the template.

We can repeat `--vmid`, and the command judges each template on its own. A refusal of one template doesn't stop the others, and the run exits 1 at the end when any template was refused or failed. The untracked prune never sweeps replicas, so we name each replica we want to free. `--untracked` needs `--vmid`, and it doesn't combine with `--director`, `--reconcile-tags`, or `--allow-owned-vms`. The report's PRUNE --vmid CANDIDATES table lists every untracked template. Its FREE WITH column names the `--untracked` command, with the report's `--config` path, for each template the prune would free, or the linked clones that block the ones it would refuse.

#### Stamping owner tags on older VMs

`cpi stemcell-refs adopt-owners` stamps one live Director's owner tag on its VMs that predate owner tags, so the owner check can count them and a prune of some other ref can go ahead. We name the Director by its UUID and its name, and we name every one of its deployments with `--deployment`. The command picks the guests that carry `bosh-cpi`, the `director--` tag that `set_vm_metadata` renders from that name, and the `deployment--` tag of one of those deployments, and that have no owner tag yet. It never stamps a template, a disk parker, a guest with a stemcell marker, or a guest whose BOSH marks sit only in its notes. Like the prune, it changes nothing until we pass `--apply`:

```bash
cpi stemcell-refs adopt-owners --config /path/to/cpi.json --director-uuid <uuid> --director-name <name> --deployment <a>,<b>
cpi stemcell-refs adopt-owners --config /path/to/cpi.json --director-uuid <uuid> --director-name <name> --deployment <a>,<b> --apply
```

**A wrong pair takes VMs away from their real Director.** Once a VM carries an owner tag, the CPI treats every other Director as a stranger to it, so the real owner's `delete_vm` and `set_vm_metadata` refuse that VM, as [A call refuses a VM this Director does not own](troubleshooting.md#a-call-refuses-a-vm-this-director-does-not-own) describes. We copy the UUID and the name from `bosh -e <env> env` on the Director we mean, and the deployment names from `bosh -e <env> deployments` on that same Director, and we read the dry run before we pass `--apply`. If we stamp the wrong UUID anyway, we remove that `bosh-owner--` tag from each affected VM by hand, through the PVE web UI or API, before the real Director next touches it.

`--deployment` is required on every run. We can repeat it or give a comma-separated list, and we copy the names from `bosh -e <env> deployments`, because that list is the one record of which deployments belong to the Director. Two Directors can share a name, so the `director--` tag alone never says which Director a VM belongs to. A deployment can have no VMs on this cluster, for example when they all run in another AZ's cluster, so a name that matches no guest with the director tag gets a warning that says it has no guests on this cluster, and the run goes on with the others. We check such a name against `bosh -e <env> deployments` in case it is a typo. When none of the names matches a guest, the command refuses. `--vmid <n>` narrows the run further to the guests we list, and we can repeat it. Each VMID must name a guest that carries the director tag and the tag of one of the named deployments, or the command refuses.

Before it stamps anything, the command checks the pair against every guest in the cluster, not only the ones it would stamp. It refuses the whole run and names the guests involved in any of these cases:

- A guest anywhere carries the UUID's owner tag while its director name or its deployment falls outside the name and deployments we gave. That UUID then already belongs to another Director, or we left out one of this Director's deployments. A VM that a deploy is still creating carries the owner tag before its `director--` and `deployment--` tags, so it trips this check too, and the reason says so. We run the command again once the deploy finishes.

- A guest in one of the deployments we named, with the director name we gave, carries a different owner tag. A deployment belongs to one Director, so the pair we gave contradicts what the cluster records.

- Two guests in one of the deployments we named record the same instance group and index, in their `instance-group--` and `index--` tags or in the matching lines of their notes, and at least one of them is a VM the run would stamp. One BOSH deployment holds one instance per group and index, so either two Directors share both the name and the deployment name, or one of the two is an orphaned VM or one left over from a failed recreate. The tags can't tell those apart. We compare the VMs with `bosh -e <env> -d <deployment> vms` and `bosh -e <env> orphaned-vms` on our Director and narrow the run to its live VMs with `--vmid`.

A guest with the same director name in a deployment we didn't name may carry a different owner tag, because that is how two Directors that share a name look. The command leaves those guests alone, and the dry run lists their deployments so we can add any that `bosh -e <env> deployments` lists for our Director. The command also refuses when the scan couldn't read some guest's config, because a guest it can't see might hold a conflicting owner tag.

The dry run prints the pair, then a block for each deployment we named. Each block lists the VMs it would stamp, the VMs that already carry the owner tag, and the VMs it skips, each with the reason. Next to each VMID it prints the BOSH instance as `<instance group>/<index>` when the VM records one, so we can compare each block with `bosh -e <env> -d <deployment> vms` on our Director before we pass `--apply`. A deployment with no guests on this cluster gets a warning line in place of its list. Conflicts and the deployments it leaves alone follow the blocks.

The apply handles each VM under the same per-VMID lock the other writes use. It reads the VM again and checks every condition the plan chose it on, and it writes the tags with that read's config digest, so a write that races another one is refused and retried. The owner tag joins the head of the tag list, behind `bosh-cpi` and next to the VM's CID and cluster stamps, which is where `set_vm_metadata` keeps it so that tag truncation can't reach it. A VM that gained the right owner tag in the meantime is reported as already owned. A VM that changed in any other way, including one that gained a conflicting owner tag, is reported as changed since the plan and left alone. When one VM fails, the command reports it, carries on with the others, and exits 1.

A Director that `bosh create-env` deployed needs more care. Every create-env Director VM carries `director--bosh-init`, because create-env names itself `bosh-init`, so the name alone can't tell two of them apart. The UUID that create-env sends is the `director_id` in the deployment's create-env state file, not the UUID that `bosh env` prints for the Director. For these VMs we pass `--director-name bosh-init`, that `director_id` as `--director-uuid`, the deployment name from the create-env manifest as `--deployment`, and the VM's VMID as `--vmid`:

```bash
cpi stemcell-refs adopt-owners --config /path/to/cpi.json --director-uuid <director_id> --director-name bosh-init --deployment <name> --vmid <n>
```

With `bosh-init` the command refuses unless the selection holds exactly one VM, and it refuses when the `director_id` already sits on another VM, because a create-env `director_id` owns exactly one VM. It also refuses while any other BOSH guest in the cluster still lacks an owner tag, so we adopt the deployed Directors first. Their VMs then carry the UUIDs that `bosh env` prints, and if we paste one of those UUIDs in place of the `director_id`, the cluster-wide check finds it on the deployed Director's VMs and refuses. That check has one gap. A Director with no deployments yet has no VMs that carry its UUID, so nothing in the cluster contradicts a create-env run that uses its `bosh env` UUID by mistake, and we rely on copying the `director_id` from the state file. The report's OWNERSHIP UNKNOWN section prints these commands with `--deployment` and `--vmid` for each create-env VM.

The exit code is 0 for success, 1 for a runtime, config, or refusal error or for any VM that failed, and 2 for a usage error, which includes a missing `--deployment`, an empty deployment name, and a UUID that isn't one. With `--json`, a dry run prints the plan and an `--apply` run prints the result with an outcome for each VM.

## Safe teardown ordering

Tearing down a multi-director environment in the wrong order can leave orphaned VMs holding IP addresses, or attempt to destroy a stemcell template another director still depends on. Follow this order:

1. **Delete every deployment** on each environment director (`bosh -e <env> delete-deployment -d <deployment>` for each), while that director is still up. Deleting deployments after their director is gone leaves orphaned VMs with no record of their IP reservations.
2. **Run `bosh clean-up --all` on each environment director** once its deployments are gone. This drops that director's stemcell references (see above), its orphaned disks, and any CPI-managed networks it created.
3. **Tear down the bootstrap/management director last** — the director you originally stood up with `bosh create-env`, which is typically also where any shared `:light:`/`:heavy:` stemcells were first uploaded and registered. Tearing it down before every environment director has finished its own clean-up risks removing a stemcell reference (or, on a shared cluster, PVE resources) that an environment director still needs mid-deploy.

### Post-teardown verification

After the last director is gone, confirm nothing was left behind. `pve-ops` ships on the Director VM at `/var/vcap/packages/pve_cpi/bin/pve-ops`, but it is not on `PATH` by default, so we either invoke it by full path or run `export PATH="/var/vcap/packages/pve_cpi/bin:$PATH"` first:

```bash
/var/vcap/packages/pve_cpi/bin/pve-ops stemcells --orphans --config <path-to-cpi.json>
python3 scripts/disk-audit --config <path-to-audit-config.json>
```

The first command surfaces stemcell qcow2 files with zero remaining director references, or cache templates whose backing file is gone — either is now safe to remove by hand if it was not already cleaned up automatically. The second inventories persistent-disk volumes, including any parked disks, that no longer correspond to a live VM.

### Known limitation: fast-path delete skips the pool reaper

`pve.fast_path_delete: true` optimizes `delete_vm` for latency and deliberately skips the empty-pool reaper step that the synchronous delete path runs. The reaper is on by default (`pve.pool_reap_empty`, default `true`), matching the per-deployment pools the `pve.vm_pool_template` default creates, but it only runs on the synchronous path: a resource pool emptied by a fast-path delete is reaped by a later synchronous-path delete, or manually:

```bash
pvesh delete /pools/<poolid>
```

This is a known, accepted trade-off of the fast path, not a bug — if your environment relies heavily on `fast_path_delete`, expect CPI-managed pools to accumulate until either a normal-path delete runs or you reap them by hand.

---

## Filing a bug report

Collect the following before opening an issue.

### Version information

```bash
# CPI version — on the Director VM
/var/vcap/jobs/pve_cpi/bin/cpi --version
# Output format: "bosh-proxmox-cpi <release version> (package <fingerprint>, built <date>)"
# A job config rendered without a release version prints the package line:
# "bosh-proxmox-cpi <fingerprint> (<commit>, built <date>)"
# The CPI also logs release_version and package_version once at startup.

# proxmox-apiclient-go version — in the release source
grep proxmox-apiclient-go /path/to/bosh-proxmox-cpi-release/src/pve_cpi/go.mod
# Current release: v3.8.6

# PVE version — on the PVE host
pveversion -v
# or: pvesh get /version

# BOSH Director and CLI versions
bosh env | grep -E 'Version|CPI'
bosh --version
```

---

## Release artifact workflow

This section describes how to build a CPI release tarball and pass its path to BOSH at deploy time. The tarball path is never written back to `manifests/bosh/vars.yml` automatically — operators must supply it explicitly.

### Build a dev tarball

```bash
# Build a timestamped dev tarball under dev_releases/bosh-proxmox-cpi/
make dev-release

# Capture the path from the RELEASE_TGZ= line printed at the end
RELEASE_TGZ=$(scripts/create-release dev 2>&1 | grep '^RELEASE_TGZ=' | cut -d= -f2-)
```

Or equivalently:

```bash
scripts/create-release dev
# Output ends with:
#   Set BOSH var: --var pve_cpi_release_path=/abs/path/to/bosh-proxmox-cpi-dev-YYYYMMDDHHMMSS.tgz
#   RELEASE_TGZ=/abs/path/to/bosh-proxmox-cpi-dev-YYYYMMDDHHMMSS.tgz
RELEASE_TGZ=/abs/path/to/bosh-proxmox-cpi-dev-YYYYMMDDHHMMSS.tgz
```

### Build a versioned (final) tarball

```bash
make release VERSION=1.2.3
# Tarball written to releases/bosh-proxmox-cpi/bosh-proxmox-cpi-1.2.3.tgz
RELEASE_TGZ="$(pwd)/releases/bosh-proxmox-cpi/bosh-proxmox-cpi-1.2.3.tgz"
```

### Pass the path to BOSH

For `bosh create-env`:

```bash
bosh create-env manifests/bosh/bosh.yml \
  --state manifests/bosh/state.json \
  --vars-file manifests/bosh/vars.yml \
  --var pve_cpi_release_path="$RELEASE_TGZ" \
  ...
```

For `bosh int` (manifest interpolation only):

```bash
bosh int manifests/bosh/bosh.yml \
  --vars-file manifests/bosh/vars.yml \
  --var pve_cpi_release_path="$RELEASE_TGZ"
```

`manifests/bosh/vars.yml.example` does not declare a `pve_cpi_release_path` key: only a comment notes that `scripts/bosh` supplies it via `-v` at deploy time. Supply it directly with `--var pve_cpi_release_path=...` as shown above; do not route it through an intermediate var. A `pve_cpi_release_path: ((some_other_var))` line in a vars file is not re-interpolated by `bosh` (vars-file values are used as literal substitution sources, not rendered themselves), so that pattern silently fails to resolve.

Copy `vars.yml.example` to `vars.yml` and leave `pve_cpi_release_path` unset there; supply it at runtime with `--var pve_cpi_release_path=...` as above. Never commit `vars.yml`; it is listed in `.gitignore`.

### Hygiene check

`make release-hygiene` asserts that no `bosh-proxmox-cpi-*.tgz` file exists at the repository root. This gate catches accidental tarball dumps in the working tree before they enter a commit.

```bash
make release-hygiene
# Exits 0 when clean; exits 1 and prints offending paths when any loose tarball is found.
```

Run this check in CI before any commit that touches BOSH release structure, or wire it into a pre-commit hook:

```bash
# .git/hooks/pre-commit (excerpt)
make release-hygiene || exit 1
```

### Logs to attach

1. Full CPI debug log for the failed task:

```bash
bosh task <id> --debug > task-<id>-debug.log 2>&1
```

2. Director log from the failure window — on the Director VM:

```bash
sudo grep -A5 -B5 "error\|ERROR" /var/vcap/sys/log/director/director.log \
  > director-extract.log
```

3. PVE daemon logs from the failure window — on the PVE host:

```bash
journalctl -u pvedaemon -u pveproxy \
  --since "YYYY-MM-DD HH:MM" --until "YYYY-MM-DD HH:MM" \
  > pve-daemon.log
```

4. PVE task log for each failed task UPID visible in the CPI log:

```bash
pvesh get /nodes/<node>/tasks/<upid>/log > pve-task-<upid>.log
```

5. Rendered CPI config with credentials redacted:

```bash
sudo cat /var/vcap/jobs/pve_cpi/config/cpi.json \
  | jq 'del(.password, .api_token)' \
  > cpi-config-redacted.json
```

6. PVE storage configuration — on the PVE host:

```bash
cat /etc/pve/storage.cfg
pvesm status
```

---

## ConfigDrive ISO storage

In `cloudinit` agent mode (the default), the CPI builds an ISO 9660 image containing the BOSH agent settings for each VM. These settings include the NATS mbus URL — which embeds credentials when the Director is deployed with a `nats://user:password@host:port` mbus string — and the blobstore credentials map. The ISO is uploaded to the storage pool named by `pve.iso_storage` and attached to the VM as a CD-ROM on `scsi30`. It remains on storage for the lifetime of the VM.

### When the ISO lands on node-local `local`, and its trust boundary

The spec default for `iso_storage` is `local`, but `pve.iso_storage_follow_vm_storage` (default `true`) treats a resolved `local` as "unset" and points the ConfigDrive pool at `pve.vm_storage` when that pool advertises `iso` content and is shared. The literal node-local `/var/lib/vz/` pool is used only when following is disabled, when `iso_storage` is pinned to another node-local pool, or when the fail-open fallback fires because `vm_storage` is unusable for ISOs. In those cases, anyone with read access to that storage pool — including any PVE user with `Datastore.AllocateSpace` or `Datastore.AllocateTemplate` privilege, or any root-level process on the PVE node — can mount the ISO and extract the credentials it contains.

When the CPI configures a VM against the `local` pool, it emits a warning in the CPI log:

```
iso_storage=local; ConfigDrive ISOs are readable by anyone with access to the PVE node-local storage. Recommend dedicated pool. See docs/operations.md ISO storage section.
```

Treat this warning as a deploy-time signal to move to a dedicated pool. The warning fires once per CPI process, not once per VM.

### Migration and HA availability, not only credential exposure

A node-local ISO pool has a second consequence beyond credential exposure: the ISO stays attached to the VM's CD-ROM slot for the VM's whole life, and PVE refuses to live-migrate — or HA-recover on another node — a VM whose CD-ROM volume sits on non-shared storage. If any of `pve.ha.dlb`, `pve.ha.pin_az`, or `pve.ha.anti_affinity` is enabled, `create_vm` separately logs a warning naming the non-shared `iso_storage` pool and the triggering feature, and `pve.require_shared_iso_for_ha: true` escalates that warning to a `create_vm` error. See [ConfigDrive — Migration and HA interaction](configdrive.md#migration-and-ha-interaction) for the full detail.

### Recommended configuration

Dedicate a separate PVE storage pool for ConfigDrive ISOs and grant access only to the PVE user account the CPI authenticates as. Suitable pool types are `dir`, `nfs`, and `cifs` — ISO uploads require a file-based pool.

Example pool options:

| Pool type | Notes |
|---|---|
| `dir` | Node-local directory at a non-default path; reduces exposure vs `/var/lib/vz/` but remains node-local |
| `nfs` | Shared; restrict NFS exports to the PVE node IP range |
| `cephfs` | Shared; ACL enforcement via Ceph caps; preferred for multi-node clusters |

Set the pool in the CPI manifest properties:

```yaml
properties:
  pve:
    iso_storage: bosh-isos
```

Verify the pool type is file-based:

```bash
pvesh get /storage/bosh-isos | jq '.data.type'
# Must be "dir", "nfs", or "cifs"
```

After moving to a dedicated pool, confirm the warning no longer appears in subsequent deploy logs.

### Orphan ISO cleanup

ConfigDrive ISOs are named `vm-<vmid>-config.iso`. The CPI removes the ISO during `delete_vm`. If a VM is destroyed outside BOSH (for example, by `qm destroy` directly), the ISO may linger as an orphan:

```bash
pvesm list <iso_storage> --content iso | grep config.iso
pvesm free <iso_storage>:iso/vm-<vmid>-config.iso
```

Confirm the corresponding VM is absent before freeing:

```bash
qm status <vmid>   # should return "no such guest"
```

---

## Error message hygiene

### Policy

Error messages returned to the BOSH Director in the JSON-RPC response envelope must not contain secrets. Secrets include passwords, API tokens, NATS mbus URLs that embed credentials (`nats://user:pass@host:port`), and blobstore credential maps. Filesystem paths and PVE storage names may appear in error messages — they are operational identifiers, not credentials — but trim full absolute paths to the basename or replace them with a logical identifier when they add no diagnostic value.

### Mechanism

The `cpierrors` package distinguishes two error surfaces:

| Constructor | Director sees | Use when |
|---|---|---|
| `cpierrors.Cloud(format, args...)` | The formatted message string | The message is safe to surface: it contains no secrets and is written for an operator |
| `cpierrors.Retriable(format, args...)` | The formatted message string | Same as Cloud but signals the Director to retry the CPI call |
| `cpierrors.Wrap(err, msg)` | `msg` only (the cause is not serialized into the RPC payload) | Wrapping an inner error that may carry secret-bearing context; `msg` is operator-safe, the full chain is preserved for the Go logger |

`Wrap` preserves the error type and `ok_to_retry` flag from the innermost `*cpierrors.Error` in the chain, so retriable classification is not lost when adding context. The `error.Error()` string (used by the logger) includes the full chain; `RPCPayload()` serializes only `msg`.

### Developer rule

When wrapping an error that may carry secret-bearing context — for example, an SDK `APIError` that includes raw HTTP response bodies or credential fields — write a generic, operator-safe message for the `Wrap` call and log the full chain at debug level separately:

```go
if err := pveClient.Nodes().Something(ctx, node, params); err != nil {
    logger.Debug("pve api error detail", log.Error(err))
    return cpierrors.Wrap(pve.WrapError(err),
        fmt.Sprintf("operation failed for node %s", node))
}
```

The message passed to `Wrap` is what the Director persists in its task log. It must identify the failing operation and the resource (node name, VMID, disk CID) but must not include credential values.

### Example: PVE authentication failure

A PVE API 401 response returns a generic operator-safe message in the BOSH error:

```
PVE authentication failed for node <node-name>
```

The full `APIError` — including HTTP response body, request URL, and any credential hint in the body — is logged at debug level only and does not appear in the Director's task log or error envelope.

To inspect the full error during a failed deploy:

```bash
bosh task <id> --debug 2>&1 | grep 'pve api error detail'
```

---

## Persistent disks: cloud_properties

The CPI creates persistent disks as PVE storage volumes, and the disk format has to suit the storage pool's type. Block storage such as LVM, ZFS, and Ceph RBD holds raw volumes, while directory-backed and network-backed pools can hold qcow2. When nothing sets a format, the CPI picks it from the pool's type, so most deployments never need to set one.

### `disk_format` property

We can set `disk_format` in the `cloud_properties` of a `persistent_disk_type`, but for most pools we leave it out and let the CPI choose:

| Pool type | `disk_format` | Notes |
|---|---|---|
| `lvmthin`, `zfspool`, `zfs`, `rbd` | omit (the CPI picks `raw`) | These pools hold only raw volumes. An explicit `qcow2` or `vmdk` is replaced with `raw`, and the CPI logs a warning. |
| `lvm` (thick) | omit (the CPI picks `raw`) | An explicit `qcow2` or `vmdk` is replaced with `raw` for a persistent disk, and the CPI logs a warning. |
| `iscsi`, `iscsidirect` | not supported | These types expose LUNs that already exist and can't allocate a new volume, so PVE refuses a `create_disk` there. |
| `dir`, `nfs`, `cifs`, `glusterfs`, `btrfs` | omit (the CPI picks `qcow2`) | qcow2 keeps copy-on-write and snapshots. `raw` and `vmdk` are also valid and are used as given. |

### Example manifest snippet

On any pool we can leave the format out, and the CPI picks the one that suits the pool:

```yaml
disk_types:
  - name: 50GB
    disk_size: 51200
    cloud_properties: {}
```

When we want a specific format on a file-backed pool, we set it explicitly:

```yaml
disk_types:
  - name: 50GB
    disk_size: 51200
    cloud_properties:
      disk_format: raw   # a raw file on dir, nfs, or cifs
```

A `disk_format` or `vm_disk_format` that is empty or `auto` counts as unset, the same as leaving the key out. That matters for Genesis kits and other generators that write `disk_format: ""` or `disk_format: auto` into every `disk_type` and `vm_type` instead of dropping the key.

### Where the format comes from, and why a pool can hold raw volumes you did not ask for

The CPI looks for a requested format in four places, and the first one that carries an explicit format wins. A `disk_format` in the call's own `cloud_properties` comes first, then a `disk_format` on a `disk_type` or `vm_type` profile declared in the CPI config, and then the `pve.vm_disk_format` job property, which a `cpi-config` entry may override per cluster as `pve_vm_disk_format`. VM root disks and ephemeral disks read `vm_disk_format` at the first two places and fall back to the same job property. An empty or `auto` value at any of the four places counts as unset, so the search moves on to the next place, and the CPI never sends an empty or `auto` format to PVE. Format names ignore case, so `QCOW2` means `qcow2`.

The `pve_vm_disk_format` override in a `cpi-config` entry works a little differently. An empty override means the entry does not override anything, so the job property stands for that cluster. An `auto` override clears the job property for that cluster, so its disks get the storage-type choice even when the job property names a format.

When none of the four places carries an explicit format, the CPI picks one from the type of the storage the disk lands on. It resolves the format separately for each disk after the CPI chooses that disk's storage, so a root disk on `local-lvm` and an ephemeral disk on an NFS pool can end up with different formats. The CPI applies these rules:

- File storage (`dir`, `nfs`, `cifs`, `glusterfs`, and `btrfs`) gets `qcow2`.

- Block storage (`lvm`, `lvmthin`, `zfspool`, `zfs`, and `rbd`) gets `raw`.

- For a root disk, any other type, or a storage whose type lookup failed, keeps the old `qcow2` default, and the CPI logs a warning. A failed lookup never fails the deploy.

- `create_vm` refuses an ephemeral disk on `iscsi` or `iscsidirect` storage before it clones or configures the VM, because those types expose LUNs that already exist and can't allocate a new volume. When the CPI can't read the ephemeral pool's type, the deploy goes on as before. A `create_disk` on those types still goes to PVE, and PVE refuses it.

- When the CPI plans the VM's storage, `create_vm` also refuses an ephemeral disk on ZFS over iSCSI (`zfs`) storage before it creates the VM, because an earlier release couldn't settle that disk's record after a downgrade. The CPI plans a VM's storage when a storage set places the VM, and also for every VM once `persistent_storage_set` is configured. Without a plan a `zfs` ephemeral disk gets `raw`.

Two rules apply on top of an explicit format. On file storage the CPI sends the format to PVE as given and bakes it into the volume filename, which is why a volume on such a pool is named `vm-<vmid>-disk-0.qcow2` or `vm-<vmid>-disk-0.raw`. On block storage the CPI replaces an explicit `qcow2` or `vmdk` with `raw` and logs a warning naming the storage, its type, and the requested format. A root disk on thick `lvm` is the one exception. There the CPI sends an explicit `qcow2` to PVE unchanged and lets PVE decide, because PVE 9 can hold it as a volume chain when the storage enables snapshots as volume chains, and the CPI records the `qcow2` it sent. Persistent and ephemeral disks on thick `lvm` get `raw` with a warning, like every other block type, and a persistent disk's CID records raw for any block-backed volume.

A thick `lvm` root changes one ephemeral case when no layer names a format. When the CPI plans the VM's storage, which it does when a storage set places the VM and for every VM once `persistent_storage_set` is configured, the plan records a single format, the root's `raw`, and an ephemeral disk on file storage follows it, so that ephemeral volume ends in `.raw`. Without a plan the same ephemeral disk resolves against its own pool and gets `qcow2`. Both work, because the agent formats the ephemeral disk either way, so a `.raw` ephemeral volume next to a thick `lvm` root on a planned VM is expected and doesn't point to a stale `disk_format`.

The CPI never downgrades a requested `qcow2` to `raw` on file storage. Proxmox supports qcow2 on NFS, and the CPI classifies NFS as file-backed everywhere it makes a format decision. So a raw volume on an NFS pool means an explicit raw reached the CPI from one of the four places above, and the volume's own `.raw` extension is the proof, because the extension always mirrors the format the CPI sent.

When a pool holds raw volumes we did not expect, we read the four places from the top. The usual cause is a stale `disk_format` in the `cloud_properties` of the `disk_type` the deployment actually uses, because that value beats the `pve.vm_disk_format` job property and every Genesis kit generates its own cloud-config from its own environment file. Setting the format on the director's environment file does not reach a deployment whose kit builds its own `disk_types`. We check the format the director is really sending with `bosh cloud-config` and look at the `cloud_properties` of the named `disk_type`, not at the CPI job properties.

Some disks keep the format they already have. Existing disks and their CIDs never change format, because the CPI resolves a format only when it creates a disk. A linked clone inherits its template's format, as PVE requires. Persistent disks that a `pve.storage` allocation plan places on a storage set land on shared NFS, the only storage type a set admits, so they use `qcow2` unless a layer asks for another format. That allocator accepts only file storage, and it refuses a plan on any other pool type before it records or submits anything, so the volume name, the format it requests, and the format it verifies always match the pool.

Two cases leave the choice to PVE. When no layer sets an explicit format and the CPI cannot classify the target pool's type, `create_disk` omits the format parameter so PVE applies that storage plugin's own default. On block storage `create_disk` also omits the parameter when the only format came from the job property or from the storage type, and PVE allocates raw there. Neither case needs a new PVE privilege, because the CPI reads the storage type from the same storage lookup it already makes.

See [Configuration — Storage properties](configuration.md) for the full set of `cloud_properties` fields and their defaults.

---

## Parked Disk Strategy

Under `detached_disk_strategy: parked` (the default), the CPI holds detached disks on dedicated parker VMs rather than leaving them as free-floating storage volumes. Parker VMs carry the `bosh-parker` tag and occupy VMIDs in the range **90000–90999**. Each parker can hold up to 31 disks across its SCSI slots. See [Persistent Disk Lifecycle Strategy](persistent-disk-strategy.md) for the full mechanics and trade-off discussion.

### Auditing parked disks with `scripts/disk-audit`

`scripts/disk-audit` is a Python 3 operator tool that queries the PVE API directly and classifies every persistent disk volume across all nodes. We count a volume as ours when its name carries a VMID in the disk-band (default 9000–29999), when a drive line in any guest's config holds its `bpd-` serial, when a sentinel in any guest's description names its full volid, or when it sits on a bus slot of a parker VM, because the CPI renames a stable-ID disk to the VMID of its guest or parker and the band alone would miss it. Storages that PVE lists as disabled or inactive are skipped, and disks on a skipped storage are not in the report. The script prints a `SKIPPED` line on stderr for each one, lists them under `Skipped storages` in the report header, and adds a `skipped_storages` array to the JSON output. A storage that is enabled but not active may hold our disks, so the report words that case as partial coverage. Run it from the repo root or any host with PVE API access.

**Setup — create a config file with PVE credentials:**

```json
{
  "host": "pve.example.com",
  "user": "root@pam",
  "api_token": "root@pam!bosh-cpi=<token-secret>",
  "verify_ssl": false
}
```

**Run a human-readable audit:**

```bash
python3 scripts/disk-audit --config /path/to/audit-config.json
```

**Run a machine-readable audit (JSON output):**

```bash
python3 scripts/disk-audit --config /path/to/audit-config.json --json
```

**Exit codes:**

| Code | Meaning |
|---|---|
| `0` | No free-floating disks — clean |
| `1` | One or more free-floating disks found — investigate before deleting |
| `2` | Usage error or fatal transport failure |

**Disk classifications in the report:**

| Classification | Meaning |
|---|---|
| `attached` | Volume held by a real (non-parker) VM on an active bus slot |
| `parked` | Volume held by a parker VM (tag `bosh-parker`, VMID 90000–90999); provenance pulled from parker VM description |
| `free-floating` | Volume in storage but no VM holds it, which makes it a potential orphan and triggers exit 1. A volume that only a sentinel note named is marked as found only by a sentinel note |
| `unknown` | Volume found in storage but VMID cannot be determined from the volid pattern |

A sentinel note can go stale, because a record keeps naming the volume under its old name until a transfer finishes, and PVE reuses volume names. When a free-floating volume was found only because a sentinel named it, the report marks it with the VM whose sentinel pointed at it and says to verify with `bosh disks --orphaned` before deleting. The JSON record carries `found_only_by_sentinel`, `sentinel_vmid`, and `sentinel_name`. When a guest still names that volume on an `unusedN` entry, the report names that guest as the holder and the JSON record carries its `holder_slot` and `held_by_unused_entry: true`, which means the volume is referenced by a guest and must not be deleted. Any free-floating record that a guest names on an `unusedN` entry carries the same holder and key, and the stderr `EXIT 1` line names the volume and the guest. The exit code stays 1 for these volumes. Every disk record in the JSON output carries a `discovered_by` list, which names the signals that found it, drawn from `band`, `serial`, `parker`, and `sentinel`.

The report also lists the parker VMs themselves, and that listing carries a `POOL` column naming the resource pool each parker belongs to, left blank for a parker that belongs to no pool. The parker warnings below name the same pool, and write `none` where the column is blank, so a finding says where the parker lives without a second lookup. The column is read from the cluster index, which lags a membership change by minutes, so a parker that was swept into its pool moments ago can still read as unpooled.

The report also lists every volume that more than one guest's config names, whether on an active slot or an unused entry. Earlier releases could leave a disk this way. When a transfer to a parker stopped partway, the volume could stay on the guest's unused entry, and a later detach or attach could then name the same volume from a parker or a second VM. The report shows each such volume once and follows it with every guest that names it, the slot, and whether that guest is a parker. It also marks the guest that owns the volume by name, and the JSON output carries the same findings under `multiply_referenced`.

The owner matters because PVE decides from the volume's name alone which VM owns it, reading the VMID from the start of the name's last segment, as in `vm-<vmid>-` or `base-<vmid>-`. Destroying the owner deletes the volume, and so does removing the owner's unused entry, even while another guest still names it. Removing a non-owner's entry, or destroying a VM that doesn't own the volume, only drops that reference and leaves the volume alone. When the owner exists but names the volume nowhere, destroying it with `destroy-unreferenced-disks` deletes the volume too, so a deployment that sets `pve.destroy_unreferenced_disks` needs the same care there. Until we know which guest really holds the disk, we leave every reference in place and destroy none of the guests involved.

To find the real holder, we start from the Director's own pairing. `bosh -d <deployment> instances --details` puts each instance's VM CID next to its disk CIDs. On the Director VM, `/var/vcap/packages/pve_cpi/bin/pve-ops locate <disk-cid>` lists the candidates. It names every guest whose active slot carries the disk's serial or its volid. It also names every unused entry that refers to the volume, whether by its original volid or by the name a matching slot gives it. When more than one guest turns up, locate warns on stderr, and it warns as well when it can't read a guest's config, because a candidate could then be missing. The same tool's `decode <vm-cid>` gives the VMID of the guest the Director believes holds the disk. That guest holds the disk when locate lists it on an active slot. When locate lists more than one guest on an active slot, the Director's VM CID decides which one holds the disk. When locate finds no active slot at all, the volume most likely sits on a guest's unused entry after a transfer that stopped partway, and [delete_vm refuses to destroy VM with attached unused disks](troubleshooting.md#delete_vm-refuses-to-destroy-vm-with-attached-unused-disks) gives the checks and the recovery for that case.

The report reads each guest's config in both of PVE's views, the running one and the one with pending changes applied, so a drive whose delete is only pending still counts as held. It doesn't count a reference kept only in a snapshot section, and it reads QEMU guests only, so a container that names a volume isn't counted either. It can also see only the guests the token can audit. When the token lacks `VM.Audit` on `/vms`, or when the script can't read the token's permissions, the script warns that the report may miss a second guest, and the JSON sets `multiply_referenced_visibility` to `limited` or `unknown`. A guest whose config didn't come back is listed in `multiply_referenced_unreadable_vmids`, and `multiply_referenced_complete` is `true` only when nothing is missing. None of this changes the exit code.

The script prints warnings to stderr when:

- Parked disks exist but `detached_disk_strategy` in the config file is set to `"free"`. These disks still drain — the parker band resolves under every strategy, so each unparks on its next `attach_disk` or `delete_disk` — but no new detaches will park.

- Empty parker VMs are found (no bus disk and no `unusedN` reference). Each empty parker is a teardown candidate.

- A parker carries an `unusedN` reference to a live volume, left by a sweep that did not complete. The warning names the `qm unlink` sequence that clears it. That parker is not a teardown candidate: `qm destroy --purge` frees the volume behind an `unusedN` entry as readily as one in a `scsiN` slot.

- A parker's config did not come back, so its contents are unknown and it is not reported as empty.

- One `bpd-` serial appears on two different volumes, which happens when someone clones a BOSH VM. The warning names both volumes and the guests that hold them.

- More than one guest names a volume. The warning names every guest and slot, says which deletion would free the volume, and points back at this section.

- The report of volumes named by more than one guest may be incomplete, because a guest's config didn't come back, the token lacks `VM.Audit` on `/vms`, or the token's permissions couldn't be read.

### Moving parkers after a parker pool rename

Under the parked strategy the CPI puts parkers into the pool `pve.parker_pool` names, and it does that after every successful park rather than only when it creates a parker. The sweep claims only the parkers that belong to no pool and carry this deployment's own prefix, and it never moves a parker out of a pool something else put it in. Renaming `pve.parker_pool` therefore leaves every existing parker where it is, and only the parkers created after the rename land in the new pool.

Moving the old ones is a manual step, through the Proxmox UI or `pvesh`. Take it once we are satisfied that the old pool holds nothing but parkers we mean to move, since the same command would move anything else we named:

```bash
# Confirm what the old pool still holds
pvesh get /pools/<old-pool>
# Move one parker across; allow-move is what lets PVE take a guest
# that already belongs to another pool
pvesh set /pools/<new-pool> --vms <parker-vmid> --allow-move 1
```

Run `python3 scripts/disk-audit --config /path/to/audit-config.json` afterwards and read the parker table's `POOL` column, allowing the cluster index its few minutes to catch up. Nothing is broken while the parkers are split across two pools. Every parker classifier keys on the `bosh-parker` tag and the VMID band rather than on pool membership, so a split pool costs us tidiness in the PVE UI and nothing else.

### Retiring parkers after a parker prefix change

Changing `pve.parker_prefix`, or changing `pve.vm_prefix` when the parker prefix inherits from it, takes the parkers we already have out of future reuse and pooling. A park reuses only a parker whose prefix matches the deployment's resolved prefix, and the pool sweep pools only those same parkers, so nothing chooses the old parkers again. The deployment creates fresh parkers beside them and fills those from the change onward, even when an old parker on the same node still has free slots.

Nothing renames a parker, and the CPI never deletes one, so the old parkers stay standing and keep holding the disks they already hold. Those disks stay reachable. Each one attaches, unparks, and deletes exactly as it did before the change, because every path that finds a disk where it already sits is prefix-agnostic. The old parkers therefore drain one disk at a time as the Director attaches or deletes what they hold, and each becomes a teardown candidate once it is empty. Run `python3 scripts/disk-audit --config /path/to/audit-config.json` to see which of them still hold anything, and follow [Recovering empty parker VMs](#recovering-empty-parker-vms) for the ones that come back empty, once the tag check below shows that each one is ours.

Until the old parkers are gone, every parker listing warns that parkers with another parker prefix sit inside this entry's parker band. That warning covers parkers that carry this Director's `director--` tag or no director tag at all. The CPI can't tell our leftovers from the parkers of a second cpi-config entry of this Director that shares the band, so it warns about both. The warning stops once the last old parker is removed, and [Parkers of another prefix sit inside the parker band](troubleshooting.md#parkers-of-another-prefix-sit-inside-the-parker-band) covers the shared-band case.

A parker of another prefix can also belong to another BOSH Director on the same cluster, because two Directors share the default parker band unless one of them changes it. Such a parker is never a leftover of ours, and we never remove it. The CPI gives it a separate warning, which says that another BOSH Director on this cluster may own the parker and can park disks on it at any time, so we must never remove it, and that each Director needs its own `parked_disk_vmid_range`. When the call carried no Director UUID, that warning reports `director_known=false` and covers every parker of another prefix with a `director--` tag, including parkers of our own other entries. Before we retire any parker, we read its tags with `pvesh get /nodes/<node>/qemu/<vmid>/config --output-format json | jq -r '.tags'` and compare the UUID in its `director--` tag with the UUID that `bosh env` prints. We retire only parkers that carry our UUID or no director tag, and [Parkers of another Director share the parker band](troubleshooting.md#parkers-of-another-director-share-the-parker-band) covers the rest.

An operator who wants the old parkers to keep filling sets `pve.parker_prefix` back to the old value explicitly. For a deployment that inherited the prefix from `pve.vm_prefix`, that value is `bosh`, because a parker created before the identity tag existed carries no `vm-prefix--` tag, and its `bosh-parker-<vmid>` name reads as the `bosh` prefix.

### A workload VM found in the parker pool

A workload VM lands in the parker pool when `pve.vm_pool_template` or a `vm_type` profile renders the same name the parker pool claims. The CPI refuses that. `create_vm` fails the request instead of creating a VM there, and the `set_vm_metadata` pool reconciler warns and skips rather than moving one in. A VM that an older release put there before those guards existed stays where it is.

The CPI does not move it back out either. The reconciler re-renders the pool name from the VM's persisted tokens, gets the parker pool back, refuses it, and returns without moving anything. The warning therefore comes back on every deploy, and the VM stays put. Nothing else is broken while it sits there, because every parker classifier keys on the `bosh-parker` tag and the VMID band rather than on pool membership.

Moving it is a manual step. Change the template or the profile first, so that the name it renders no longer collides and the same tokens do not send the next VM to the same place. Then put the VM in the workload pool it belongs in:

```bash
pvesh set /pools/<workload-pool> --vms <vmid> --allow-move 1
```

The audit reports these VMs. Run `python3 scripts/disk-audit --config /path/to/audit-config.json` and read the warnings on stderr. The audit counts every pool that holds a parker as a parker pool, and `--parker-pool <name>` names one that holds no parker yet.

### Recovering empty parker VMs

The script prints the removal commands for each empty parker VM. Verify before running. The digit anchor matters: every parker carries a `scsihw:` line, so a bare `^scsi` matches on an empty parker too and the check never clears.

```bash
# Confirm the parker holds no disks
qm config <parker-vmid> | grep -E '^(scsi|virtio|ide|sata|unused)[0-9]+:'
# Proceed only when the above is empty. Every parker carries protection=1,
# which PVE honors by refusing the destroy, so clear it in the same breath.
qm set <parker-vmid> --protection 0
qm destroy <parker-vmid> --purge
```

A parker VM that still holds disks must not be destroyed — doing so deletes those disks permanently. If the parked disk is no longer needed, delete it via BOSH (`bosh delete-disk <cid>`) first.

### Recovering free-floating disks

A `free-floating` disk exists in storage with no VM holding it. Before taking action:

1. Cross-reference the BOSH Director database — `bosh disks --orphaned` lists Director-tracked disks with no current deployment.

2. If the Director tracks the disk, it is a normal detached disk in the `free` strategy (or a parked disk whose parker VM was destroyed). Reattach via a `bosh deploy` or delete via `bosh delete-disk`.

3. If the Director does not track the disk and no deployment references it, it is a true orphan. Remove it using the same steps as [Cleaning up orphaned disks](#cleaning-up-orphaned-disks).

---

## Per-RPC Metrics

The CPI can append one JSON-lines record per CPI RPC to a file; it is disabled by default. Enable it in your deployment manifest:

```yaml
properties:
  pve:
    metrics:
      enabled: true
      file_path: /var/vcap/sys/log/bosh/cpi/pve-metrics.jsonl
```

Set `pve.metrics.enabled: true` and `pve.metrics.file_path` to an absolute path writable by the CPI process. See [Configuration — Per-RPC Metrics](configuration.md#per-rpc-metrics) for the full property reference.

### Record format

Each line is a self-contained JSON object:

```json
{"ts":"2026-06-15T12:00:00.123456789Z","method":"create_vm","duration_ms":1423.7,"outcome":"ok","request_id":"req-abc123"}
```

| Field | Type | Description |
|---|---|---|
| `ts` | RFC3339Nano UTC string | Timestamp of the call completion |
| `method` | string | CPI method name (for example, `create_vm`, `attach_disk`) |
| `duration_ms` | float | Wall-clock duration of the CPI call in milliseconds |
| `outcome` | string | `"ok"` when the call succeeded; `"error"` when it returned an error |
| `request_id` | string | BOSH Director request ID from the JSON-RPC context |

Write failures are non-fatal — the CPI logs a warning and completes the RPC normally. The file is opened, written, and closed once per call; no file descriptor is held across calls.

### Querying the metrics file

```bash
# Method latency summary (requires jq)
jq -r '[.method, (.duration_ms | tostring)] | join("\t")' \
  /var/vcap/sys/log/bosh/cpi/pve-metrics.jsonl \
  | sort | awk '{sum[$1]+=$2; n[$1]++} END{for(m in sum) printf "%-30s avg=%.1f ms  calls=%d\n", m, sum[m]/n[m], n[m]}' \
  | sort -k1

# All errors in the last 100 calls
tail -100 /var/vcap/sys/log/bosh/cpi/pve-metrics.jsonl \
  | jq 'select(.outcome == "error")'

# p99 latency for create_vm
grep '"method":"create_vm"' /var/vcap/sys/log/bosh/cpi/pve-metrics.jsonl \
  | jq -r '.duration_ms' | sort -n | awk 'END{print NR"th percentile:", $0}'
```

## Storage allocation recovery

Use the [storage journal runbook](storage-journal-operations.md) to enroll a namespace, inspect retained CIDs, restore authority, repair its index, and record adoption or completed cleanup. These commands require the durable journal and fresh PVE observations. An absent resource does not establish that an earlier task has finished.

The existing opt-in metrics exporter includes `cpi.storage.allocations`, `cpi.storage.fallbacks`, `cpi.storage.rejections`, `cpi.storage.candidate_rejections`, and `cpi.storage.reconciliations`. Request rejection and candidate rejection are separate counters. Labels identify configured sets, roles, strategy versions, or fixed reconciliation outcomes; allocation IDs and backend error text are excluded.
