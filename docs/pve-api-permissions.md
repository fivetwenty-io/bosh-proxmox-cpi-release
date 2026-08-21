# PVE API Token + Minimum-Privilege `bosh` User

How to create the Proxmox VE API token the CPI authenticates with, and how to scope a dedicated `bosh@pve` user to the smallest privilege set that still lets every CPI call succeed.

This doc covers Proxmox VE 9.1. It complements [pve-settings.md](pve-settings.md), which covers the cluster-side toggles (storage `disable=0`, `import` content type, token privilege separation) that must also be in place.

Examples below assume:

```bash
PVE_HOST=pve-0.example.tld:8006
PVE_TOKEN='PVEAPIToken=root@pam!setup=00000000-0000-0000-0000-000000000000'
```

The `PVE_TOKEN` here is an admin token used **only for the one-time setup**. The token you wire into the CPI is the `bosh@pve!...` token produced in step 4 below.

## 1. Two paths, one recommendation

| Path | User | Token | Blast radius | When to use |
|---|---|---|---|---|
| Minimum privilege (recommended) | `bosh@pve` (custom role `BoshOperator`) | `bosh@pve!<id>`, `privsep=0` | Bounded by `BoshOperator` ACL on `/`, `/vms`, `/sdn`, and the configured storage paths | Production, shared clusters, any deployment that outlives the lab |
| Quick-start | `root@pam` | `root@pam!<id>`, `privsep=0` | Full datacenter administrator | Throwaway lab where the BOSH director already shares trust boundary with the cluster |

Both paths produce a usable token. The minimum-privilege path is recommended everywhere except short-lived labs because the `root@pam` token with `privsep=0` is equivalent to handing the BOSH director full datacenter administration. The quick-start path is documented in [pve-settings.md §3](pve-settings.md#3-disable-privilege-separation-on-the-api-token).


## 2. Privileges the CPI actually uses

Derived from the API endpoint inventory under `src/pve_cpi/internal/cpi/handlers/` and the auth-header construction in `src/pve_cpi/internal/pve/client.go`. Each row lists a PVE 9.1 privilege, the ACL path on which it must be granted, and the CPI methods that drive it. Table verified against the PVE API schema (`apidoc.json`) as of commit `cfd1b12`.

| Privilege | ACL path | CPI methods (handler file) |
|---|---|---|
| `Sys.Audit` | `/` | All `info`, `has_vm`, `has_disk`, task polling, `GET /cluster/status` (also feeds VXLAN peer derivation for CPI-created zones), `/cluster/resources`, `/cluster/storage`, `/cluster/config/nodes`, `/nodes` (most handlers); HA-rule reads (`GET /cluster/ha/rules`, `/cluster/ha/resources`) when HA placement is enabled; `GET /cluster/firewall/options` — `create_vm.go`'s once-per-process datacenter firewall master-switch probe, triggered only when the VM being created requests `security_groups`, `pve.vm_firewall`, or `allowed_address_pairs` (see [Configuration — Firewall](configuration.md#firewall)); a probe failure (missing `Sys.Audit`, etc.) logs a warning and never fails `create_vm` |
| `VM.Allocate` | `/vms` | `create_vm.go`, `delete_vm.go` (`POST /nodes/{n}/qemu`, `DELETE /nodes/{n}/qemu/{vmid}`) |
| `VM.Audit` | `/vms` | `has_vm.go`, `get_disks.go`, `has_disk.go`, `reboot_vm.go` (`GET /nodes/{n}/qemu/{vmid}/status/current` power-state pre-check), every config-read path (`GET /nodes/{n}/qemu/{vmid}/config`) |
| `VM.Clone` | `/vms` | `create_vm.go` (`POST /nodes/{n}/qemu/{vmid}/clone` — checked against the **source template's** VMID; the new VMID is covered by `VM.Allocate`), `create_stemcell.go` replica cloning |
| `VM.Migrate` | `/vms` | `disk_migrate.go` (`POST /nodes/{n}/qemu/{vmid}/migrate` — cross-node disk migration moves the disk on a mover VM) |
| `VM.Config.Disk` | `/vms` | `attach_disk.go`, `detach_disk.go`, `update_disk.go`, `resize_disk.go`, `create_disk.go`, `delete_disk.go` |
| `VM.Config.CDROM` | `/vms` | `create_vm.go` (ConfigDrive ISO attach — the `ide2 ... media=cdrom` config write is checked per-option as `VM.Config.CDROM`, not `VM.Config.Disk`) |
| `VM.Config.Network` | `/vms` | `create_vm.go`, `create_network.go`, `delete_network.go` (NIC attach/detach) |
| `VM.Config.CPU` | `/vms` | `create_vm.go` (`cores`, `cpu` type, `numa` — PVE checks each config option's own privilege class at submit time, so VM creation needs these even though the endpoint-level check is `VM.Allocate`) |
| `VM.Config.Memory` | `/vms` | `create_vm.go` (`memory`, `balloon`) |
| `VM.Config.HWType` | `/vms` | `create_vm.go` and `create_stemcell.go` (`scsihw`, `tablet`, `machine`, `bios`, `agent` — per-option check as above) |
| `VM.Config.Options` | `/vms` | `set_vm_metadata.go`, `set_disk_metadata.go`, `tags.go` (description + tags); also PVE's own tag validation on any config write that carries `tags` |
| `VM.Config.Cloudinit` | `/vms` | `create_vm.go` (ConfigDrive delivery via the `cloudinit` config field) |
| `VM.PowerMgmt` | `/vms` | `create_vm.go` (start), `delete_vm.go` (stop), `reboot_vm.go` (soft mode: graceful `/status/reboot` with `/status/reset` fallback; hard mode: `/status/reset`; stopped VM started via `/status/start`) |
| `VM.Snapshot` | `/vms` | `snapshot_disk.go`, `delete_snapshot.go` |
| `Datastore.Allocate` | `/storage/<pool>` for each of `vm_storage`, `disk_storage`, `stemcell_storage`, `iso_storage` | `delete_disk.go`, `delete_stemcell.go` (`DELETE /nodes/{n}/storage/{s}/content/{volume}`) |
| `Datastore.AllocateSpace` | same four storage paths | `create_disk.go`, `create_vm.go` (`import-from=<storage>:import/...` disk allocation) |
| `Datastore.AllocateTemplate` | `stemcell_storage`, `iso_storage` | `create_stemcell.go` (`POST .../upload` with `content=import`), ConfigDrive ISO upload (`content=iso`) |
| `Datastore.Audit` | same four storage paths | `create_stemcell.go`, `get_disks.go`, `has_disk.go` (list/check volume) |
| `SDN.Allocate` | `/sdn` | `create_network.go`, `delete_network.go` (zone create/delete check it on `/sdn/zones` and `/sdn/zones/{zone}`, covered by a propagated `/sdn` grant), `create_vm.go` (`advertised_routes` subnet injection) — opt-in, needed only when `network_mode: sdn` is set (or a network is marked `managed: true`, or `advertised_routes` is used); the default (`network_mode: bridge`) makes zero SDN calls and needs no grant |
| `SDN.Use` | `/sdn/zones/localnetwork` (propagated; covers every local bridge) or `/sdn/zones/<zone>/<vnet>` for SDN vnets | `create_vm.go` NIC attach — **required even in the default `network_mode: bridge`**: PVE checks `SDN.Use` on the bridge for every `net{i}` config write by a non-root identity, treating plain Linux bridges as members of the implicit `localnetwork` zone. Not bundled in `BoshOperator`; granted via the small dedicated `BoshBridgeUse` role in §3c |
| `Sys.Console` | `/` | HA-rule writes — `POST`/`DELETE /cluster/ha/resources`, `POST`/`DELETE /cluster/ha/rules` driven by `placement.pin_az_via_ha_rules`, `anti_affinity.use_ha_rules`, and DLB (`placement.dlb` configured) — see note below |
| `Pool.Allocate` | `/pool` (parent, propagated — per-deployment pool names are dynamic) | `pool.go`'s `EnsurePoolExists` (`POST /pools`) — create-if-missing for every template-rendered per-deployment pool (`pve.vm_pool_template`, default `bosh-{director}-{deployment}`) and the static `pve.vm_pool` fallback (default `bosh`) at `create_vm` time, for `pve.stemcell_template_pool` (default `bosh-templates`) at `create_stemcell` time, and again at `set_vm_metadata` time when pool reconciliation moves a VM (`PUT /pools/{poolid}` with `allow-move=1`); `cluster_lock.go` (`POST`/`DELETE /pools` — create/delete sentinel pools `bosh-lock-*`: when `cluster_lock_mode: pool`, and on every park and unpark, which serializes the parker's protection window under `bosh-lock-vm-<vmid>` — advisory there, so a denial degrades to an unserialized window with a warning rather than a failure); `delete_vm.go`'s empty-pool reaper (`DELETE /pools/{poolid}`, on by default via `pve.pool_reap_empty`). Required by default; set `vm_pool_template`, `vm_pool`, and `stemcell_template_pool` all to `""` and leave `cluster_lock_mode`/`pool_reap_empty` unused to avoid needing it. This privilege alone is sufficient — no `Permissions.Modify` grant is needed; see the note below |
| `Pool.Audit` | `/pool` (parent, propagated) | `GetPoolComment` (`GET /pools`/`GET /pools/{poolid}`) — probed at CPI startup by `preflightPoolAccess` (`cmd/cpi/main.go`) against the configured static pools; read by `cluster_lock.go` to check a lock pool's owner/expiry when stealing a lock, and again on every release to confirm the sentinel still belongs to the releasing caller (under `cluster_lock_mode: pool`, and on every park and unpark), and by `delete_vm.go`'s default-on empty-pool reaper (`pve.pool_reap_empty`) to check a pool's provenance comment before deleting it. Required by default, on the same condition as `Pool.Allocate` above — the startup preflight reads each static pool before the CPI serves its first request, and a denial (HTTP 401/403) is fatal |
| `Sys.Modify` | `/` | `placement_dlb.go` (`PUT /cluster/options` setting `crs=ha=dynamic,...`) — opt-in, only when `placement.dlb.manage_cluster_crs: true`; when false (default) the CPI only reads `/cluster/options` (`Sys.Audit`, already granted) and logs a warning instead of writing |

**`SDN.Allocate` note:** opt-in — `network_mode: bridge` is the default, and a default deployment makes zero SDN calls, so this grant is unneeded unless `network_mode: sdn` is set, a network is marked `managed: true`, or `advertised_routes` is used. Any of those turns on the turnkey vxlan zone (and its vnets and subnets) or an operator-created zone/vnet, all gated on `SDN.Allocate` (the zone endpoints check it on `/sdn/zones` and `/sdn/zones/{zone}`, which a propagated `/sdn` grant covers). Grant it on `/sdn` only for those deployments:

```bash
curl -sk -X PUT -H "Authorization: $PVE_TOKEN" \
  --data-urlencode 'path=/sdn' \
  --data-urlencode 'users=bosh@pve' \
  --data-urlencode 'roles=BoshOperator' \
  -d 'propagate=1' \
  https://$PVE_HOST/api2/json/access/acl
```

**`SDN.Use` note:** this one is easy to miss because it applies to the *default* network mode. Since PVE 8, every `net{i}` bridge assignment by a non-root identity is checked for `SDN.Use` on the bridge's SDN path; a plain Linux bridge (`vmbr0`) resolves to `/sdn/zones/localnetwork/<bridge>`. None of the built-in audit roles and none of the privileges in `BoshOperator` cover it, so a token built from this doc's §3 grants alone fails `create_vm` at NIC attach with `Permission check failed (/sdn/zones/localnetwork/vmbr0, SDN.Use)`. §3c adds a dedicated single-privilege `BoshBridgeUse` role granted on `/sdn/zones/localnetwork` with propagation, which covers every local bridge on every node. Verified live on PVE 9.2.4.

**Create-time tag check note:** PVE validates the `tags` field of any VM config write with a check for `VM.Config.Options` that runs **only against `/vms/{vmid}`** — unlike the create endpoint's own `VM.Allocate` check, it has no `/pool/{pool}` alternative, and in `POST /nodes/{n}/qemu` it runs inside the create worker *before* the new VM is registered into the requested pool (verified in the PVE 9.2.4 source: `PVE/API2/Qemu.pm` calls `assert_tag_permissions` ahead of `add_vm_to_pool`). A token whose VM privileges come only from pool grants therefore can never create a VM with tags in the create call itself, while tagging the same VM immediately afterward succeeds because pool membership then satisfies the `/vms/{vmid}` check. The CPI accounts for this: template and clone creation submit without `tags` and apply the identity tags in a follow-up config write. The one exception is `stemcell_strategy: import` (per-VM or global), whose fresh-create path still passes tags at create and therefore needs a real `/vms` grant; the default `stemcell_strategy: template` is unaffected (see §7).

**`Sys.Console` note:** PVE checks `Sys.Console` on `/` for HA resource and rule writes — `POST`/`DELETE /cluster/ha/resources` and `POST`/`DELETE /cluster/ha/rules` (the matching `GET` reads check `Sys.Audit`, already granted on `/`). These endpoints are called when `placement.pin_az_via_ha_rules: true`, `anti_affinity.use_ha_rules: true`, or DLB is configured (`placement.dlb` present with `enabled: true` or an `az_name` sentinel in use — `delete_vm.go` gates its HA cleanup path on `AntiAffinityUseHaRulesEnabled() || DLBConfigured()`); a deployment that leaves all three off needs neither `Sys.Console` nor the HA-read grant. The privilege names are taken from the PVE API schema (`apidoc.json`: HA writes → `["perm", "/", ["Sys.Console"]]`, HA reads → `["perm", "/", ["Sys.Audit"]]`).

**`Pool.Allocate` note:** this privilege gates pool creation and deletion via `POST`/`DELETE /pools`. It is needed by default, because every pool the CPI uses is create-if-missing: the template-rendered per-deployment pools (`pve.vm_pool_template`, default `bosh-{director}-{deployment}`), the static `pve.vm_pool` fallback (default `bosh`), and `pve.stemcell_template_pool` (default `bosh-templates`) are each created the first time they are needed, tagged with a `managed by bosh-pve-cpi` provenance comment. Because the per-deployment names are dynamic, grant `Pool.Allocate` at the `/pool` parent with propagation rather than on individual `/pool/<poolid>` paths — a per-poolid grant cannot cover a pool that does not exist yet. A deployment that sets `vm_pool_template: ""`, `vm_pool: ""`, and `stemcell_template_pool: ""`, and does not use `cluster_lock_mode: pool` or `pool_reap_empty`, should still grant `Pool.Allocate` when disks are parked: parking and unparking take a `bosh-lock-vm-<vmid>` sentinel pool to serialize the parker's protection window. A denial there is not fatal — the window runs unserialized and logs a warning — but two concurrent windows on one parker can then leave a volume referenced by an `unusedN` key nothing else sees. `PUT /pools/{poolid}` (`AddVM`, used by `create_stemcell.go` to assign the template VM, and `MoveVMToPool` with `allow-move=1`, used by `set_vm_metadata`'s pool reconciliation) additionally requires whatever permission-modification right governs the object being added to the pool; for VM members this is covered by `VM.Allocate` on `/vms`, already granted in §3 below, so no extra grant is needed beyond `Pool.Allocate` itself. **`Permissions.Modify` is not required** for any part of the CPI's pool lifecycle (create, assign, or delete) — verified against a live PVE 9.2.4 cluster with a token restricted to exactly the privileges in this table (no `Permissions.Modify`): pool creation, one-shot pool assignment on VM create, and the empty-pool reaper's delete all succeeded. PVE surfaces pool-create, pool-move, and non-empty pool-delete conflicts as HTTP 500 with descriptive text rather than the 409/404 a REST client might expect for those conditions — the CPI recognizes them by matching known substrings in the error text (`already exists`, `belongs already to pool`, `is not empty`, `does not exist`) rather than by status code.

**`Pool.Audit` note:** required by default, on the same condition as `Pool.Allocate`. Before serving its first request the CPI runs `preflightPoolAccess` (`cmd/cpi/main.go`), which issues `GET /pools/{poolid}` (`GetPoolComment`) against each configured pool layer — `pve.vm_pool` (default `bosh`) and `pve.stemcell_template_pool` (default `bosh-templates`) — to prove its identity can read a pool path before a real `create_vm`/`create_stemcell` hits an opaque denial. A classified permission error (HTTP 401/403) is fatal: the CPI logs `pools preflight failed` and exits, so a deployment missing this grant never starts. Other failures (network fault, PVE 5xx, timeout) are logged at Warn and treated as transient, and a pool that does not exist yet is not a failure — it is the normal zero-config first boot, and the pool is created lazily on first use.

The probe reads rather than writes because there is no side-effect-free way to test `Pool.Allocate` — PVE only enforces that privilege on a mutating call. A clean preflight therefore proves read access only, which is why the failure message names both grants: fix them together rather than discovering the `Pool.Allocate` gap separately on the first create.

Beyond startup, `GetPoolComment` is also read by the default-on empty-pool reaper (`pve.pool_reap_empty`, checking a pool's provenance comment before deleting it) and when `cluster_lock_mode: pool` (`cluster_lock.go` reading a lock pool's owner/expiry when stealing or verifying a hold).

The preflight can only probe the static pool names; the per-deployment names `pve.vm_pool_template` renders do not exist until their first `create_vm`, so a missing `/pool` grant for them surfaces as a named, non-retriable `create_vm` error directing the operator to grant `Pool.Allocate` at `/pool` or set `pve.vm_pool_template: ""`.

The grant drops out only when `vm_pool_template`, `vm_pool`, and `stemcell_template_pool` are all set to `""` — the preflight then makes zero PVE calls — `cluster_lock_mode: pool`/`pool_reap_empty` are unused, and nothing parks disks.

> **Upgrade note.** Earlier releases documented `Pool.Audit` as opt-in, and the create-if-missing path genuinely never read pool state: `EnsurePoolExists` called only `POST /pools`, treating PVE's "already exists" error as its idempotency check. A role built to that minimal grant will now **fail to start** after upgrading, with `pools preflight: PVE denied read access to pool "bosh"`. Fix it in place without touching the rest of the role:
>
> ```bash
> pveum role modify BoshOperator --privs Pool.Audit --append 1
> ```

**`Sys.Modify` note:** required only when `placement.dlb.manage_cluster_crs: true` — the CPI then writes the cluster-wide CRS (Cluster Resource Scheduler) setting via `PUT /cluster/options` so PVE actively load-balances HA-managed VMs. The default (`manage_cluster_crs` unset or `false`) never writes this endpoint; the CPI only reads it (`Sys.Audit`, already granted on `/`) to warn when `crs` is not set to `ha=dynamic,...`.

**`delete_vm` and `skiplock`:** `delete_vm` issues `DELETE /nodes/{n}/qemu/{vmid}` with `skiplock=true` so a locked or still-running VM is destroyed without a separate unlock step. The endpoint itself checks `VM.Allocate` on `/vms/{vmid}` (already granted), but `skiplock` is **not** governed by any privilege: PVE restricts the flag to the literal `root@pam` user authenticated via **password**, and rejects it for every other identity regardless of role or ACL — including an API token owned by `root@pam`, since a token's authenticated identity always carries a `!<token-id>` suffix that never equals the bare `root@pam` PVE's check requires. To let `delete_vm` clear locked VMs, authenticate the CPI as `root@pam` with a password (not a token). A least-privilege `bosh@pve` user cannot be granted `skiplock` through any role; without it, `delete_vm` still succeeds on unlocked, stopped VMs but fails on locked ones.

The four storage fields are defined in `src/pve_cpi/internal/config/config.go:25-48`. If your deployment uses fewer than four distinct pools (for example, `disk_storage = vm_storage`), grant ACLs only on the distinct pool names — duplicates are harmless but unnecessary.

## 3. Minimum-privilege path (recommended)

Each step shows the UI path and the equivalent API call.

### 3a. Create the realm user `bosh@pve`

The `pve` realm is Proxmox's built-in realm — no PAM/Linux account is needed. The CPI never uses the password after token issuance; the password value is irrelevant beyond satisfying the API. The CPI authenticates with the token.

UI:

1. Datacenter → Permissions → Users → Add.

2. User name: `bosh`.

3. Realm: `Proxmox VE authentication server` (`pve`).

4. Password: any sufficiently random string (record it but it is unused).

5. Enabled: checked.

6. OK.

API:

```bash
curl -sk -X POST -H "Authorization: $PVE_TOKEN" \
  --data-urlencode 'userid=bosh@pve' \
  --data-urlencode 'password=<long-random-string>' \
  -d 'enable=1' \
  https://$PVE_HOST/api2/json/access/users
```

### 3b. Create the `BoshOperator` custom role

The role bundles every non-audit privilege from §2. `Sys.Audit` is granted separately on `/` via the built-in `PVEAuditor` role, keeping `BoshOperator` focused on VM and storage mutation.

UI:

1. Datacenter → Permissions → Roles → Create.

2. Name: `BoshOperator`.

3. Privileges: select `VM.Allocate`, `VM.Audit`, `VM.Clone`, `VM.Migrate`, `VM.Config.Disk`, `VM.Config.CDROM`, `VM.Config.Network`, `VM.Config.CPU`, `VM.Config.Memory`, `VM.Config.HWType`, `VM.Config.Options`, `VM.Config.Cloudinit`, `VM.PowerMgmt`, `VM.Snapshot`, `Datastore.Allocate`, `Datastore.AllocateSpace`, `Datastore.AllocateTemplate`, `Datastore.Audit`, `Pool.Allocate` and `Pool.Audit` (both required by default — `vm_pool` and `stemcell_template_pool` both default to a create-if-missing pool name, and the CPI's startup preflight reads each one; omit both only when both pool names are set to `""`), `SDN.Allocate` (opt-in — only if `network_mode: sdn` is set, a network is marked `managed: true`, or `advertised_routes` is used), `Sys.Console` (if using HA placement features or DLB), and `Sys.Modify` (if using `placement.dlb.manage_cluster_crs`).

4. Create.

API:

```bash
# Base role — VM, disk, and storage operations.
curl -sk -X POST -H "Authorization: $PVE_TOKEN" \
  --data-urlencode 'roleid=BoshOperator' \
  --data-urlencode 'privs=VM.Allocate,VM.Audit,VM.Clone,VM.Migrate,VM.Config.Disk,VM.Config.CDROM,VM.Config.Network,VM.Config.CPU,VM.Config.Memory,VM.Config.HWType,VM.Config.Options,VM.Config.Cloudinit,VM.PowerMgmt,VM.Snapshot,Datastore.Allocate,Datastore.AllocateSpace,Datastore.AllocateTemplate,Datastore.Audit,SDN.Allocate,Sys.Console,Pool.Allocate,Pool.Audit,Sys.Modify' \
  https://$PVE_HOST/api2/json/access/roles

# Bridge-attach role — SDN.Use is deliberately kept out of BoshOperator so
# the broad role never lands on SDN paths; this one-privilege role goes on
# the localnetwork zone (or specific vnets) in §3c. Required even for the
# default network_mode: bridge.
curl -sk -X POST -H "Authorization: $PVE_TOKEN" \
  --data-urlencode 'roleid=BoshBridgeUse' \
  --data-urlencode 'privs=SDN.Use' \
  https://$PVE_HOST/api2/json/access/roles
```

`Pool.Allocate` and `Pool.Audit` both belong in the default grant — the per-deployment pools `pve.vm_pool_template` renders (default `bosh-{director}-{deployment}`), the static `pve.vm_pool` fallback (default `bosh`), and `pve.stemcell_template_pool` (default `bosh-templates`) are all create-if-missing, and the CPI's startup preflight reads each static pool before serving a request. `SDN.Allocate`, `Sys.Console`, and `Sys.Modify` are opt-in only, since `network_mode: bridge` is the default and a default deployment makes zero SDN calls. A deployment that sets `network_mode: sdn` (or marks a network `managed: true`, or uses `advertised_routes`) needs `SDN.Allocate`; one that uses HA placement features or DLB needs `Sys.Console`; one that enables `placement.dlb.manage_cluster_crs` needs `Sys.Modify`. The two pool privileges drop out together, and only when `vm_pool_template`, `vm_pool`, and `stemcell_template_pool` are all set to `""` (with `cluster_lock_mode: pool` and `pool_reap_empty` unused). The minimum set for a bridge-mode deployment with no resource pools, no HA placement, and no DLB is `VM.*,Datastore.*` as listed in the privilege table above, plus `SDN.Use` on the bridge path via `BoshBridgeUse` — that one applies to every deployment that attaches a NIC, bridge mode included. Note that the `VM.Config.*` list is wider than the CPI's own config writes suggest: PVE checks each config option's privilege class at submit time, so a VM create that sets `cores`, `memory`, and `scsihw` needs `VM.Config.CPU`, `VM.Config.Memory`, and `VM.Config.HWType` even though the CPI never updates those fields afterward (each was confirmed by a live `Permission check failed` denial when omitted). Note that `delete_vm`'s `skiplock` flag is separate from this role: it is gated on the `root@pam` user, not on any privilege (see the `delete_vm` note above), so granting `BoshOperator` does not enable it.

### 3c. Grant ACLs

Five ACL grant categories apply by default — cluster-wide audit, VM operations, the bridge-attach grant (`SDN.Use` on the `localnetwork` zone — needed even in the default `network_mode: bridge`), one storage grant per configured storage pool, and the resource-pool grant at the `/pool` parent (needed by default: per-deployment pools are created on demand with dynamic names, so a per-poolid grant cannot cover them) — plus two conditional categories: SDN (only when `network_mode: sdn` is set, a network is marked `managed: true`, or `advertised_routes` is used) and root-path (only when HA placement, DLB, or CPI-managed cluster CRS is used). The resource-pool grant drops out only when `vm_pool_template`, `vm_pool`, and `stemcell_template_pool` are all set to `""` and `cluster_lock_mode: pool`/`pool_reap_empty` are unused.

UI (repeat for each row below):

1. Datacenter → Permissions → Add → **User Permission**.

2. Path: as shown.

3. User: `bosh@pve`.

4. Role: as shown.

5. Propagate: checked.

6. OK.

| Path | Role | Notes |
|---|---|---|
| `/` | `PVEAuditor` | Built-in role; grants `Sys.Audit` cluster-wide |
| `/` | `BoshOperator` | Required for `Sys.Console` (HA-rule management) — only if using HA placement features or DLB; also required for `Sys.Modify` (`PUT /cluster/options`) — only if `placement.dlb.manage_cluster_crs: true` |
| `/vms` | `BoshOperator` | All VM and disk mutation |
| `/sdn/zones/localnetwork` | `BoshBridgeUse` | `SDN.Use` for NIC bridge attach — required for **every** deployment, including the default `network_mode: bridge`; propagation covers each local bridge (`vmbr0`, ...) on every node. For SDN vnets, grant it on `/sdn/zones/<zone>` (or the specific vnet path) instead |
| `/sdn` | `BoshOperator` | Required for `SDN.Allocate` — opt-in, needed only when `network_mode: sdn` is set, a network is marked `managed: true`, or `advertised_routes` is used. `network_mode: bridge` (the default) needs no grant here |
| `/storage/<vm_storage>` | `BoshOperator` | From `pve.vm_storage` |
| `/storage/<disk_storage>` | `BoshOperator` | From `pve.disk_storage` |
| `/storage/<stemcell_storage>` | `BoshOperator` | From `pve.stemcell_storage` (defaults to `vm_storage`) |
| `/storage/<iso_storage>` | `BoshOperator` | From `pve.iso_storage`. With `pve.iso_storage_follow_vm_storage` (default true) the effective ISO pool is `pve.vm_storage` when that pool has `iso` content and is shared — no extra grant needed. Grant this only for a pinned `iso_storage` pool distinct from `vm_storage` |
| `/pool/<poolid>` | `BoshOperator` | Required for `Pool.Allocate` and `Pool.Audit` by default — needed for the `vm_pool` (`bosh`) and `stemcell_template_pool` (`bosh-templates`) create-if-missing pools, which the CPI's startup preflight also reads; omit only when both are set to `""` |

API:

```bash
# Cluster-wide audit.
curl -sk -X PUT -H "Authorization: $PVE_TOKEN" \
  --data-urlencode 'path=/' \
  --data-urlencode 'users=bosh@pve' \
  --data-urlencode 'roles=PVEAuditor' \
  -d 'propagate=1' \
  https://$PVE_HOST/api2/json/access/acl

# Root-path BoshOperator for Sys.Console (HA-rule management) and
# Sys.Modify (PUT /cluster/options for DLB's manage_cluster_crs).
# Omit if HA placement features, DLB, and manage_cluster_crs are all
# unused. (skiplock delete_vm is gated on the root@pam user, not on
# this role — see the delete_vm note.)
curl -sk -X PUT -H "Authorization: $PVE_TOKEN" \
  --data-urlencode 'path=/' \
  --data-urlencode 'users=bosh@pve' \
  --data-urlencode 'roles=BoshOperator' \
  -d 'propagate=1' \
  https://$PVE_HOST/api2/json/access/acl

# All VMs.
curl -sk -X PUT -H "Authorization: $PVE_TOKEN" \
  --data-urlencode 'path=/vms' \
  --data-urlencode 'users=bosh@pve' \
  --data-urlencode 'roles=BoshOperator' \
  -d 'propagate=1' \
  https://$PVE_HOST/api2/json/access/acl

# Bridge attach — required for every deployment that attaches a NIC,
# including the default network_mode: bridge. Plain Linux bridges live
# under the implicit localnetwork SDN zone.
curl -sk -X PUT -H "Authorization: $PVE_TOKEN" \
  --data-urlencode 'path=/sdn/zones/localnetwork' \
  --data-urlencode 'users=bosh@pve' \
  --data-urlencode 'roles=BoshBridgeUse' \
  -d 'propagate=1' \
  https://$PVE_HOST/api2/json/access/acl

# SDN — opt-in, only when network_mode: sdn is set, a network is
# marked managed: true, or advertised_routes is used. Omit for the
# default network_mode: bridge.
curl -sk -X PUT -H "Authorization: $PVE_TOKEN" \
  --data-urlencode 'path=/sdn' \
  --data-urlencode 'users=bosh@pve' \
  --data-urlencode 'roles=BoshOperator' \
  -d 'propagate=1' \
  https://$PVE_HOST/api2/json/access/acl

# Per-storage. Repeat for vm_storage, disk_storage, stemcell_storage, iso_storage.
for s in local-lvm local-lvm nfs-shared local; do
  curl -sk -X PUT -H "Authorization: $PVE_TOKEN" \
    --data-urlencode "path=/storage/$s" \
    --data-urlencode 'users=bosh@pve' \
    --data-urlencode 'roles=BoshOperator' \
    -d 'propagate=1' \
    https://$PVE_HOST/api2/json/access/acl
done

# Resource pools — required for Pool.Allocate and Pool.Audit by default:
# vm_pool_template (bosh-{director}-{deployment}), vm_pool (bosh), and
# stemcell_template_pool (bosh-templates) all create their pool on demand,
# and the CPI reads each static one at startup (a denial is fatal, so a
# role without Pool.Audit will not boot). The grant goes on the /pool
# parent with propagation because the per-deployment names are dynamic;
# PVE has no wildcard ACL path. Omit the whole grant only if
# vm_pool_template: "", vm_pool: "", and stemcell_template_pool: "" are
# all set and cluster_lock_mode: pool / pool_reap_empty are unused.
curl -sk -X PUT -H "Authorization: $PVE_TOKEN" \
  --data-urlencode 'path=/pool' \
  --data-urlencode 'users=bosh@pve' \
  --data-urlencode 'roles=BoshOperator' \
  -d 'propagate=1' \
  https://$PVE_HOST/api2/json/access/acl
```

Substitute your own pool names. Duplicates (`local-lvm` listed twice when `vm_storage` and `disk_storage` share a pool) are idempotent.

### 3d. Generate the API token

UI:

1. Datacenter → Permissions → API Tokens → Add.

2. User: `bosh@pve`.

3. Token ID: `bosh-cpi` (or any descriptive id).

4. **Privilege Separation: unchecked** (see §3e for why).

5. Expire: leave blank, or set per your rotation policy.

6. Add.

7. **Copy the displayed secret immediately** — Proxmox shows it once and never again.

API:

```bash
curl -sk -X POST -H "Authorization: $PVE_TOKEN" \
  -d 'privsep=0' \
  https://$PVE_HOST/api2/json/access/users/bosh@pve/token/bosh-cpi
```

The response body contains `data.value` — that is the one-time secret. The full token credential is `PVEAPIToken=bosh@pve!bosh-cpi=<secret>`.

Issue one token per consuming integration — a dedicated token id (`bosh-cpi` above) for this CPI, a separate one for any other BOSH director, script, or service that talks to the same PVE cluster, even when they share the `bosh@pve` user. PVE tokens revoke independently by token id: deleting `bosh@pve!bosh-cpi` invalidates only that one consumer, leaving every other token issued under `bosh@pve` untouched. A token shared across integrations turns a routine rotation (compromised credential, departing operator, scheduled renewal) into a coordinated outage across every consumer of that one credential, and forces a full audit of every place the shared secret might have leaked instead of a single, known blast radius.

### 3e. Why `privsep=0` is safe here

PVE API tokens default to **Privilege Separation = on**. With `privsep=1`, the token has its own empty ACL distinct from the parent user; you must then repeat every grant from §3c as an **API Token Permission** instead of a User Permission. That works, but doubles the number of ACL entries to maintain.

Setting `privsep=0` makes the token inherit the parent user's ACL. The blast radius is bounded by what `bosh@pve` itself can do — the `BoshOperator` privileges plus `Sys.Audit`, scoped to `/vms` and the configured storage paths. This is safe because the user is already minimally privileged.

Contrast with [pve-settings.md §3](pve-settings.md#3-disable-privilege-separation-on-the-api-token), where `privsep=0` on a `root@pam` token exposes the entire datacenter. The setting is the same; the trust boundary is different.

### 3f. Wire into `vars.yml`

In `manifests/bosh/vars.yml`:

```yaml
pve_user:      bosh@pve
pve_password:  ""
pve_api_token: 'bosh@pve!bosh-cpi=<secret-from-3d>'
```

The canonical token format is the bare `<user>@<realm>!<token-id>=<secret>` form. The CPI also accepts the Authorization-header form (`PVEAPIToken=bosh@pve!bosh-cpi=<secret>`) and strips the prefix on load, so pasting either shape works.

The CPI requires exactly one of `pve_password` or `pve_api_token` — leave the unused field as an empty string so BOSH var interpolation resolves it (see [bosh-create-env.md — Authentication](bosh-create-env.md#authentication)).

## 4. Quick-start `root@pam` path (lab only)

For an ephemeral lab where the BOSH director and the Proxmox cluster share the same trust boundary, you can skip the `bosh@pve` setup and use a `root@pam` API token directly. The steps are:

1. Datacenter → Permissions → API Tokens → Add → user `root@pam`, token id of your choice, **Privilege Separation unchecked**.

2. Wire the resulting token into `vars.yml` as `pve_api_token`.

3. Confirm `privsep=0` per [pve-settings.md §3](pve-settings.md#3-disable-privilege-separation-on-the-api-token).

This path is appropriate only for short-lived labs. It grants the BOSH director full datacenter administration, including access to every other VM and storage pool on the cluster.

## 5. Verification

Run these from the workstation after the token is in hand. `BOSH_TOKEN` here is the new `bosh@pve` token — replace it before running.

```bash
BOSH_TOKEN='PVEAPIToken=bosh@pve!bosh-cpi=<secret>'

# Cluster audit — exercises Sys.Audit on /.
curl -sk -o /dev/null -w "cluster/status: %{http_code}\n" \
  -H "Authorization: $BOSH_TOKEN" \
  https://$PVE_HOST/api2/json/cluster/status

# VM listing — exercises VM.Audit on /vms.
curl -sk -o /dev/null -w "cluster/resources type=vm: %{http_code}\n" \
  -H "Authorization: $BOSH_TOKEN" \
  "https://$PVE_HOST/api2/json/cluster/resources?type=vm"

# Storage listing — exercises Datastore.Audit on each /storage/<pool>.
for s in <vm_storage> <disk_storage> <stemcell_storage> <iso_storage>; do
  curl -sk -o /dev/null -w "storage/$s: %{http_code}\n" \
    -H "Authorization: $BOSH_TOKEN" \
    https://$PVE_HOST/api2/json/storage/$s
done

# Node enumeration — used by the CPI for node selection and task polling.
curl -sk -o /dev/null -w "nodes: %{http_code}\n" \
  -H "Authorization: $BOSH_TOKEN" \
  https://$PVE_HOST/api2/json/nodes
```

Expect `200` on every line. Mapping of non-2xx codes:

- `401` — token secret is wrong, or the token id no longer exists. Re-issue per §3d.

- `403` — privilege gap. The path in the failing line tells you which ACL is missing. Re-check §3c for that path.

- `501` — endpoint not implemented at the URL you requested. Check the URL; the privilege exists but the path is wrong.

Substitute your own storage pool names in the loop. Run `bosh create-env` only after every smoke-test line returns `200`.


## 6. Caveat: `import-from=` and privilege separation

The CPI uploads stemcells as qcow2 files and references them via the **volume form** `import-from=<storage>:import/<file>.qcow2` (`src/pve_cpi/internal/cpi/handlers/create_vm.go`). The volume form is ACL-gated — it requires `Datastore.AllocateSpace` on the source storage but is **not** restricted to the `root@pam` account.


PVE additionally restricts the **path form** `import-from=/absolute/filesystem/path` to the `root` Unix account regardless of API privileges — see [pve-settings.md §3](pve-settings.md#3-disable-privilege-separation-on-the-api-token). The CPI never uses the path form, so the minimum-privilege setup in this doc does not hit that restriction.

In short: a `bosh@pve` token with `privsep=0` and the §3 ACLs can upload stemcells and clone from them without `root@pam`.

## 7. Shared-cluster variant: scoping VM mutation to a resource pool

Every grant in §3 that lands on `/vms` is cluster-wide: on a cluster that also runs VMs the CPI does not manage, a `bosh@pve` token with those ACLs can mutate every guest, not only the ones it created. `pve.vm_pool` (see [Configuration](configuration.md)) closes that gap by assigning every CPI-created VM to a dedicated resource pool at create/clone time, so the `/vms`-scoped privileges can be granted on `/pool/<bosh-pool>` instead.

**This single-pool model requires `pve.vm_pool_template: ""`.** The default `pve.vm_pool_template` (`bosh-{director}-{deployment}`) creates a pool per deployment with names that do not exist until first use, and PVE has no wildcard ACL path — a single `/pool/<bosh-pool>` grant cannot cover them. On a reduced-ACL cluster (one `/pool/<name>` grant, no parent-path grants), set `pve.vm_pool_template: ""` explicitly so every VM lands in the one static `pve.vm_pool`; the CPI fails `create_vm` with an error naming exactly this fix when its token cannot create a per-deployment pool. To keep per-deployment pools under a scoped token instead, move the pool grants to the `/pool` parent with propagation — broader than a single pool path, but still far short of `/vms`.

### How the scoping works

Confirmed against the PVE API schema (`apidoc.json`), not merely inferred:

- **Create** (`POST /nodes/{node}/qemu`) checks `VM.Allocate` on `/vms/{vmid}` **or** on `/pool/{pool}` when the request supplies a `pool` parameter. `pve.vm_pool` makes the CPI's create calls always supply that parameter, so a token with `VM.Allocate` granted only on `/pool/<bosh-pool>` can create new VMs into that pool.
- **Clone** (`POST /nodes/{node}/qemu/{vmid}/clone`) checks the same `VM.Allocate` on `/vms/{newid}` **or** `/pool/{pool}` for the *new* VMID, plus `VM.Clone` on `/vms/{vmid}` for the **template's** own VMID — see the gap below.
- **Every other VM operation** (`config` read/write, `status/*` power management, disk attach/detach, snapshots, etc.) checks its privilege on `/vms/{vmid}` for the *existing* VM. PVE's resource-pool permission model (documented in the PVE Administration Guide's Permission Management chapter) additionally honors any ACL granted on `/pool/{poolid}` for VMs that are members of that pool — so once a VM is a `<bosh-pool>` member (which it is, from the moment `create_vm` created it with `pool=<bosh-pool>`), a `/pool/<bosh-pool>`-scoped grant covers every subsequent operation on it too.

### The gap: cloning still needs template access

The clone endpoint's `VM.Clone` check runs against the **template's own VMID**, not the new VM's. A template is a member of `pve.stemcell_template_pool` (if set) or no pool at all — never `pve.vm_pool`, and the two are rejected as equal at config load specifically so they cannot be conflated. A `bosh@pve` token scoped only to `/pool/<bosh-pool>` cannot clone from a template it has no grant on.

Two ways to close this, pick one:

- Grant `VM.Clone` (bundled in `BoshOperator`) on `/pool/<stemcell-template-pool>` too, if `stemcell_template_pool` is set — a second, narrower pool-scoped grant, still far short of cluster-wide `/vms`.
- Grant `VM.Clone` on `/vms` specifically (leaving the other VM.\* privileges pool-scoped) — templates are CPI-owned infrastructure, not guest VMs, so a `VM.Clone`-only cluster-wide grant is a much smaller blast radius than the full `/vms` grant in §3.

The reduced-ACL table below assumes `stemcell_template_pool` is set and takes the first option.

### Reduced ACL table

Replaces the `/vms` row from §3c's table; every other row (root-path, bridge-attach, SDN, storage) is unchanged — the `BoshBridgeUse` grant on `/sdn/zones/localnetwork` in particular is still required, since NIC attach checks `SDN.Use` on the bridge path regardless of pool scoping.

| Path | Role | Notes |
|---|---|---|
| `/pool/<bosh-pool>` | `BoshOperator` | Replaces `/vms`. Covers `VM.Allocate` (create/clone into the pool), `Pool.Allocate` (create-if-missing for `<bosh-pool>` itself — the CPI creates it on demand if it does not already exist; `Permissions.Modify` is not needed, see §2), and every other VM.\* privilege for VMs that are members of `<bosh-pool>` — i.e. every VM this CPI creates once `pve.vm_pool: <bosh-pool>` is set |
| `/pool/<stemcell-template-pool>` | `BoshOperator` | Additional grant closing the cloning gap above — required whenever `stemcell_template_pool` is set alongside `vm_pool`. Omit and grant `VM.Clone` on `/vms` instead if you took the second option above |
| `/pool/bosh-lock-*` | `Pool.Allocate`, `Pool.Audit` | Sentinel-pool locks. A `/pool/<bosh-pool>`-scoped grant does not cover them: the lock pools live at their own paths. Parking and unparking create and delete `bosh-lock-vm-<vmid>` on every call. Without this grant those windows run unserialized rather than failing, so the grant is strongly recommended rather than required. PVE has no wildcard ACL path, so the grant has to go on a parent: `/pool`, which covers every pool, or `/`, which covers everything |

**Live-validated.** We exercised this exact table against a PVE 9.2.4 cluster with a `bosh-d3@pve` token (privsep=0) holding *only* the grants above plus §3c's audit, bridge-attach, storage, and `/pool` rows — no `/vms` grant at all — and ran the full 16-step CPI lifecycle (`create_stemcell` through `delete_stemcell`, including disk create/attach/resize/snapshot/detach/delete and both reboot modes) with `detached_disk_strategy: free`. Every step passed: the create/clone pool-fallback works, and PVE's pool-permission propagation covers every subsequent config, status, disk, and snapshot operation on pool-member VMs, including template freeze and final cleanup. Re-run the §5 smoke test after any PVE upgrade all the same.

No prerequisite pool-creation step is required: `pve.vm_pool` is create-if-missing, so the CPI creates `<bosh-pool>` itself the first time `create_vm` needs it, provided the token holds `Pool.Allocate` on `/pool/<bosh-pool>` per the table above. To pre-create the pool anyway — for example to set a custom comment before the first deploy — use an admin token:

```bash
curl -sk -X POST -H "Authorization: $PVE_TOKEN" \
  --data-urlencode 'poolid=<bosh-pool>' \
  --data-urlencode 'comment=BOSH CPI managed VMs' \
  https://$PVE_HOST/api2/json/pools
```

Then grant the ACL:

```bash
curl -sk -X PUT -H "Authorization: $PVE_TOKEN" \
  --data-urlencode 'path=/pool/<bosh-pool>' \
  --data-urlencode 'users=bosh@pve' \
  --data-urlencode 'roles=BoshOperator' \
  -d 'propagate=1' \
  https://$PVE_HOST/api2/json/access/acl
```

### Caveats

- **Template-layer VMs are reconciled; everything else is not.** VMs whose pool came from the rendered `pve.vm_pool_template` are moved by `set_vm_metadata` on the next `bosh deploy` whenever the template's current render of their persisted create-time tokens differs from their actual pool, and a legacy VM (created before pool provenance existed) sitting in the static `pve.vm_pool` is adopted into that flow the same way. VMs whose pool came from a call-level `cloud_properties.pool`, a `vm_type` profile, or the static `pve.vm_pool` layer are never moved by a config change: they stay in the pool they were created under, and re-pointing `pve.vm_pool` at a new name only affects VMs created afterward.
- **VMs with no pool membership at all are not retroactively covered.** A VM created when every pool layer was `""` is not a member of any pool, and a token scoped only to `/pool/...` paths cannot manage it — either leave `/vms` granted alongside the pool scope until every such VM is replaced, or manually add old VMIDs to a pool as a one-time migration step.
- **Set `detached_disk_strategy: free`.** The default `parked` strategy anchors every created and detached disk to parker VMs in a dedicated VMID band, and parkers are deliberately created outside every resource pool — so a pool-scoped token cannot attach to, detach from, or configure them (`create_disk` fails at the park step with a `/vms/<parker-vmid>` denial). Under the reduced table, either set `pve.detached_disk_strategy: free` or keep a real `/vms` grant.
- **Cross-node disk migration is unavailable.** `disk_migrate.go` moves a disk by attaching it to a mover VM (same out-of-pool band as parkers) and migrating that VM (`VM.Migrate` on `/vms/<mover-vmid>`). A pool-scoped token cannot drive it; deployments that rely on cross-node persistent-disk moves need the `/vms` grant.
- **`stemcell_strategy: import` is unavailable.** Its fresh-create path passes `tags` in the create call, and PVE's create-time tag check accepts only a `/vms/{vmid}` grant (see the create-time tag check note in §2). The default `template` strategy creates untagged and tags after pool registration, which pool grants cover.
- **`delete_vm`'s `skiplock` behavior is unaffected either way** — it remains gated on the `root@pam` user regardless of pool scoping (see the `delete_vm` note in §2).
- **Read visibility stays cluster-wide.** The `/` `PVEAuditor` grant means the token can still *list and read* every VM and storage object on the cluster (`/cluster/resources` returns all guests); the pool scoping bounds mutation, not visibility. The CPI needs that read breadth for node selection, task polling, and VMID-collision scans.
- **Grants on not-yet-existing pools work.** PVE accepts an ACL on `/pool/<name>` before the pool exists, so the reduced table can be granted up front and the CPI's create-if-missing pool creation then succeeds under the reduced token itself (verified live — the CPI created its `stemcell_template_pool` with its provenance comment on first use).
- **Pool membership on delete.** `DELETE /nodes/{node}/qemu/{vmid}?purge=1`'s own API description covers cleanup of backup/replication/HA job references and VM-specific permissions; it does not explicitly document resource-pool membership cleanup. A deleted VM's stale pool membership entry (if one persists) carries no capability risk — there is no VM behind that VMID to act on — but do not rely on it disappearing from `pvesh get /pools/<bosh-pool>` output without checking your PVE version.
