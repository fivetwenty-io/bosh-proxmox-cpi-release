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
  | jq 'del(.password, .api_token, .registry_password)'
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

`pve.log_level` is config-only — there is no runtime environment variable override. Valid values are `debug`, `info`, `warn`, and `error`; the default is `info`. Log output is JSON-formatted (slog) and each entry carries the `request_id` and `method` from the active RPC context.

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
| `registry` | POSTs settings to a BOSH registry endpoint | Legacy environments with an existing registry |
| `noagent` | Skips agent delivery | Specialised workloads that do not run the BOSH agent |

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

# SDN (when using network_mode: sdn)
pvesh get /cluster/sdn/vnets
pvesh get /cluster/sdn/zones
```

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

# Persistent-disk volumes use synthetic VMIDs 9000–9999
pvesm list <disk_storage> | grep '^vm-9'

# Stemcell import files
pvesm list <stemcell_storage> --content import | grep bosh-stemcell

# ConfigDrive ISOs
pvesm list <iso_storage> --content iso | grep config.iso
```

### Cleaning up orphaned VMs

**Danger: `qm destroy --purge` is irreversible and destroys every disk volume referenced in the VM config, including `unusedN` slots. Always inspect the config before running it.**

Persistent disks use synthetic VMIDs 9000–9999 and survive `qm destroy` only if they are absent from the VM config at destroy time (that is, if `detach_disk` has already removed them). If a VM was torn down without a proper `detach_disk`, persistent disks may still appear in `unusedN` slots and will be deleted by `--purge`.

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

Collect all of the following before opening an issue.

### Version information

```bash
# CPI version — on the Director VM
/var/vcap/jobs/pve_cpi/bin/cpi --version
# Output format: "bosh-pve-cpi <version> (<commit>, built <date>)"

# pve-apiclient-go version — in the release source
grep pve-apiclient-go /path/to/bosh-pve-cpi-release/src/pve_cpi/go.mod
# Current release: v3.1.7

# PVE version — on the PVE host
pveversion -v
# or: pvesh get /version

# BOSH Director and CLI versions
bosh env | grep -E 'Version|CPI'
bosh --version
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
  | jq 'del(.password, .api_token, .registry_password)' \
  > cpi-config-redacted.json
```

6. PVE storage configuration — on the PVE host:

```bash
cat /etc/pve/storage.cfg
pvesm status
```
