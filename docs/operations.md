# Operations Runbook

This runbook covers day-2 operations and diagnostics for an already-deployed BOSH PVE CPI. For symptom-first failure triage, see [Troubleshooting](troubleshooting.md).

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

The `ok_to_retry` field tells the Director whether to re-queue the entire CPI call. `RetriableCloudError` sets it `true`; `CloudError` sets it `false`. Storage-lock timeouts and transient transport faults surface as retriable errors. Permission failures (HTTP 403), snapshot guard rejections, and VMID exhaustion are terminal.

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
| `"type":"Bosh::Clouds::RetriableCloudError"` | Director will auto-retry | Monitor frequency |
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
# BOSH VMs carry director--, deployment--, and job-- tags
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
qm config <vmid> | grep -E '^(scsi|unused)'
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

## Filing a bug report

Collect the following before opening an issue.

### Version information

```bash
# CPI version — on the Director VM
/var/vcap/jobs/pve_cpi/bin/cpi --version
# Output format: "bosh-pve-cpi <version> (<commit>, built <date>)"

# pve-apiclient-go version — in the release source
grep pve-apiclient-go /path/to/bosh-pve-cpi-release/src/pve_cpi/go.mod
# Current release: v3.2.7

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
# Build a timestamped dev tarball under dev_releases/bosh-pve-cpi/
make dev-release

# Capture the path from the RELEASE_TGZ= line printed at the end
RELEASE_TGZ=$(scripts/create-release dev 2>&1 | grep '^RELEASE_TGZ=' | cut -d= -f2-)
```

Or equivalently:

```bash
scripts/create-release dev
# Output ends with:
#   Set BOSH var: --var release_artifact_path=/abs/path/to/bosh-pve-cpi-dev-YYYYMMDDHHMMSS.tgz
#   RELEASE_TGZ=/abs/path/to/bosh-pve-cpi-dev-YYYYMMDDHHMMSS.tgz
RELEASE_TGZ=/abs/path/to/bosh-pve-cpi-dev-YYYYMMDDHHMMSS.tgz
```

### Build a versioned (final) tarball

```bash
make release VERSION=1.2.3
# Tarball written to releases/bosh-pve-cpi/bosh-pve-cpi-1.2.3.tgz
RELEASE_TGZ="$(pwd)/releases/bosh-pve-cpi/bosh-pve-cpi-1.2.3.tgz"
```

### Pass the path to BOSH

For `bosh create-env`:

```bash
bosh create-env manifests/bosh/bosh.yml \
  --state manifests/bosh/state.json \
  --vars-file manifests/bosh/vars.yml \
  --var release_artifact_path="$RELEASE_TGZ" \
  ...
```

For `bosh int` (manifest interpolation only):

```bash
bosh int manifests/bosh/bosh.yml \
  --vars-file manifests/bosh/vars.yml \
  --var release_artifact_path="$RELEASE_TGZ"
```

`manifests/bosh/vars.yml.example` shows the corresponding variable declaration:

```yaml
pve_cpi_release_path: ((release_artifact_path))
```

Copy `vars.yml.example` to `vars.yml` and leave `pve_cpi_release_path` as the `((release_artifact_path))` placeholder. Supply the actual path via `--var release_artifact_path=...` at runtime. Never commit `vars.yml` — it is listed in `.gitignore`.

### Hygiene check

`make release-hygiene` asserts that no `bosh-pve-cpi-*.tgz` file exists at the repository root. This gate catches accidental tarball dumps in the working tree before they enter a commit.

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

### Default (`local`) and its trust boundary

The default `iso_storage` value is `local`, which maps to the node-local directory storage at `/var/lib/vz/`. Anyone with read access to that storage pool — including any PVE user with `Datastore.AllocateSpace` or `Datastore.AllocateTemplate` privilege, or any root-level process on the PVE node — can mount the ISO and extract the credentials it contains.

When the CPI configures a VM against the `local` pool, it emits a warning in the CPI log:

```
iso_storage=local; ConfigDrive ISOs are readable by anyone with access to the PVE node-local storage. Recommend dedicated pool. See docs/operations.md ISO storage section.
```

Treat this warning as a deploy-time signal to move to a dedicated pool. The warning fires once per CPI process, not once per VM.

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

The CPI creates persistent disks as PVE storage volumes. The disk format and the storage pool type must be compatible. LVM and ZFS pools require raw volumes; directory-backed and network-backed pools default to `qcow2`.

### `disk_format` property

Set `disk_format` in the `cloud_properties` of a `persistent_disk_type` when the target pool requires a specific format:

| Pool type | Required `disk_format` | Notes |
|---|---|---|
| `lvm`, `lvmthin` | `raw` | LVM does not support qcow2 layers |
| `zfspool` | `raw` | ZFS zvols are raw block devices |
| `dir`, `nfs`, `cifs` | omit (defaults to `qcow2`) | qcow2 is the pool default; raw is also valid |
| `rbd` (Ceph) | omit (defaults to `raw`) | RBD volumes are always raw; the field is ignored |

### Example manifest snippet

```yaml
disk_types:
  - name: 50GB
    disk_size: 51200
    cloud_properties:
      disk_format: raw   # required for lvm, lvmthin, and zfspool pools
```

For pools that do not require `raw`, omit `disk_format` entirely:

```yaml
disk_types:
  - name: 50GB
    disk_size: 51200
    cloud_properties: {}
```

See [Configuration — Storage properties](configuration.md) for the full set of `cloud_properties` fields and their defaults.

---

## Parked Disk Strategy

When `detached_disk_strategy: parked` is enabled, the CPI holds detached disks on dedicated parker VMs rather than leaving them as free-floating storage volumes. Parker VMs carry the `bosh-parker` tag and occupy VMIDs in the range **90000–90999**. Each parker can hold up to 31 disks across its SCSI slots. See [Persistent Disk Lifecycle Strategy](persistent-disk-strategy.md) for the full mechanics and trade-off discussion.

### Auditing parked disks with `scripts/disk-audit`

`scripts/disk-audit` is a Python 3 operator tool that queries the PVE API directly and classifies every persistent disk volume in the disk-band (default 9000–29999) across all nodes. Run it from the repo root or any host with PVE API access.

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
| `free-floating` | Volume in storage but no VM holds it — potential orphan; triggers exit 1 |
| `unknown` | Volume found in storage but VMID cannot be determined from the volid pattern |

The script prints warnings to stderr when:

- Parked disks exist but `detached_disk_strategy` in the config file is not `"parked"`.

- Empty parker VMs are found (0 disks held). Each empty parker is a teardown candidate.

### Recovering empty parker VMs

The script prints a `qm destroy` command for each empty parker VM. Verify before running:

```bash
# Confirm the parker holds no disks
qm config <parker-vmid> | grep -E '^(scsi|virtio|ide|sata)'
# Proceed only when the above is empty
qm destroy <parker-vmid> --purge 1
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
