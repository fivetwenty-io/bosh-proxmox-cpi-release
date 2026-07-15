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
| `VM.Config.Disk` | `/vms` | `attach_disk.go`, `detach_disk.go`, `update_disk.go`, `resize_disk.go`, `create_disk.go`, `delete_disk.go` |
| `VM.Config.Network` | `/vms` | `create_vm.go`, `create_network.go`, `delete_network.go` (NIC attach/detach) |
| `VM.Config.Options` | `/vms` | `set_vm_metadata.go`, `set_disk_metadata.go`, `tags.go` (description + tags) |
| `VM.Config.Cloudinit` | `/vms` | `create_vm.go` (ConfigDrive delivery via the `cloudinit` config field) |
| `VM.PowerMgmt` | `/vms` | `create_vm.go` (start), `delete_vm.go` (stop), `reboot_vm.go` (soft mode: graceful `/status/reboot` with `/status/reset` fallback; hard mode: `/status/reset`; stopped VM started via `/status/start`) |
| `VM.Snapshot` | `/vms` | `snapshot_disk.go`, `delete_snapshot.go` |
| `Datastore.Allocate` | `/storage/<pool>` for each of `vm_storage`, `disk_storage`, `stemcell_storage`, `iso_storage` | `delete_disk.go`, `delete_stemcell.go` (`DELETE /nodes/{n}/storage/{s}/content/{volume}`) |
| `Datastore.AllocateSpace` | same four storage paths | `create_disk.go`, `create_vm.go` (`import-from=<storage>:import/...` disk allocation) |
| `Datastore.AllocateTemplate` | `stemcell_storage`, `iso_storage` | `create_stemcell.go` (`POST .../upload` with `content=import`), ConfigDrive ISO upload (`content=iso`) |
| `Datastore.Audit` | same four storage paths | `create_stemcell.go`, `get_disks.go`, `has_disk.go` (list/check volume) |
| `SDN.Allocate` | `/sdn` | `create_network.go`, `delete_network.go` (zone create/delete check it on `/sdn/zones` and `/sdn/zones/{zone}`, covered by a propagated `/sdn` grant), `create_vm.go` (`advertised_routes` subnet injection) — required by default (`network_mode: sdn`); only a `network_mode: bridge` opt-out avoids it |
| `Sys.Console` | `/` | HA-rule writes — `POST`/`DELETE /cluster/ha/resources`, `POST`/`DELETE /cluster/ha/rules` driven by `placement.pin_az_via_ha_rules`, `anti_affinity.use_ha_rules`, and DLB (`placement.dlb` configured) — see note below |
| `Pool.Allocate` | `/pool/<poolid>` | `cluster_lock.go` (`POST`/`DELETE /pools` — create/delete sentinel pools `bosh-lock-*` when `cluster_lock_mode: pool`), `create_stemcell.go` (`PUT /pools/{poolid}` via `AddVM` — assign the template VM to `pve.stemcell_template_pool` when set) — opt-in, only when one of these two features is enabled. `pve.vm_pool` does **not** need this privilege: it assigns membership via the `pool` parameter on the create/clone request itself, checked as `VM.Allocate` on `/pool/{pool}` (see §7), not via a separate `PUT /pools` call |
| `Pool.Audit` | `/pool/<poolid>` | `GetPoolComment` (`GET /pools`/`GET /pools/{poolid}`) — pool-existence and comment reads backing the same two opt-in features above |
| `Sys.Modify` | `/` | `placement_dlb.go` (`PUT /cluster/options` setting `crs=ha=dynamic,...`) — opt-in, only when `placement.dlb.manage_cluster_crs: true`; when false (default) the CPI only reads `/cluster/options` (`Sys.Audit`, already granted) and logs a warning instead of writing |

**`SDN.Allocate` note:** required by default — SDN is the default network mode, and with defaults the CPI creates and deletes the turnkey vxlan zone, its vnets, and subnets, all gated on `SDN.Allocate` (the zone endpoints check it on `/sdn/zones` and `/sdn/zones/{zone}`, which a propagated `/sdn` grant covers). Only deployments that opt out with `network_mode: bridge`, never mark a network `managed: true`, and set no `advertised_routes` can omit this privilege. Grant it on `/sdn`:

```bash
curl -sk -X PUT -H "Authorization: $PVE_TOKEN" \
  --data-urlencode 'path=/sdn' \
  --data-urlencode 'users=bosh@pve' \
  --data-urlencode 'roles=BoshOperator' \
  -d 'propagate=1' \
  https://$PVE_HOST/api2/json/access/acl
```

**`Sys.Console` note:** PVE checks `Sys.Console` on `/` for HA resource and rule writes — `POST`/`DELETE /cluster/ha/resources` and `POST`/`DELETE /cluster/ha/rules` (the matching `GET` reads check `Sys.Audit`, already granted on `/`). These endpoints are called when `placement.pin_az_via_ha_rules: true`, `anti_affinity.use_ha_rules: true`, or DLB is configured (`placement.dlb` present with `enabled: true` or an `az_name` sentinel in use — `delete_vm.go` gates its HA cleanup path on `AntiAffinityUseHaRulesEnabled() || DLBConfigured()`); a deployment that leaves all three off needs neither `Sys.Console` nor the HA-read grant. The privilege names are taken from the PVE API schema (`apidoc.json`: HA writes → `["perm", "/", ["Sys.Console"]]`, HA reads → `["perm", "/", ["Sys.Audit"]]`).

**`Pool.Allocate`/`Pool.Audit` note:** these privileges gate the PVE resource-pool API and are only needed when `cluster_lock_mode: pool` (cross-process advisory locking via sentinel pools) or `stemcell_template_pool` (template-VM pool assignment) is set — a deployment that uses neither leaves pools untouched and needs no pool ACL at all. `PUT /pools/{poolid}` (`AddVM`, used by `create_stemcell.go`) additionally requires whatever permission-modification right governs the object being added to the pool; for VM members this is covered by `VM.Allocate` on `/vms`, already granted in §3 below, so no extra grant is needed beyond `Pool.Allocate` itself.

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

3. Privileges: select `VM.Allocate`, `VM.Audit`, `VM.Config.Disk`, `VM.Config.Network`, `VM.Config.Options`, `VM.Config.Cloudinit`, `VM.PowerMgmt`, `VM.Snapshot`, `Datastore.Allocate`, `Datastore.AllocateSpace`, `Datastore.AllocateTemplate`, `Datastore.Audit`, `SDN.Allocate` (required by default; omit only for a `network_mode: bridge` opt-out), `Sys.Console` (if using HA placement features or DLB), `Pool.Allocate` and `Pool.Audit` (if using `cluster_lock_mode: pool` or `stemcell_template_pool`), and `Sys.Modify` (if using `placement.dlb.manage_cluster_crs`).

4. Create.

API:

```bash
# Base role — VM, disk, and storage operations.
curl -sk -X POST -H "Authorization: $PVE_TOKEN" \
  --data-urlencode 'roleid=BoshOperator' \
  --data-urlencode 'privs=VM.Allocate,VM.Audit,VM.Config.Disk,VM.Config.Network,VM.Config.Options,VM.Config.Cloudinit,VM.PowerMgmt,VM.Snapshot,Datastore.Allocate,Datastore.AllocateSpace,Datastore.AllocateTemplate,Datastore.Audit,SDN.Allocate,Sys.Console,Pool.Allocate,Pool.Audit,Sys.Modify' \
  https://$PVE_HOST/api2/json/access/roles
```

`SDN.Allocate` belongs in the default grant — SDN is the default network mode. A deployment that explicitly opts out with `network_mode: bridge` (and no `advertised_routes`), does not use HA placement features or DLB (`pin_az_via_ha_rules: false`, `use_ha_rules: false`, no `placement.dlb`), resource pools (`cluster_lock_mode: pool` unset, `stemcell_template_pool` unset), or CPI-managed cluster CRS (`placement.dlb.manage_cluster_crs: false`/unset) may omit `SDN.Allocate`, `Sys.Console`, `Pool.Allocate`/`Pool.Audit`, and `Sys.Modify`, respectively. The minimum set for that documented bridge-only opt-out without HA placement, DLB, or pools is `VM.*,Datastore.*` as listed in the privilege table above. Note that `delete_vm`'s `skiplock` flag is separate from this role: it is gated on the `root@pam` user, not on any privilege (see the `delete_vm` note above), so granting `BoshOperator` does not enable it.

### 3c. Grant ACLs

Five ACL grants are needed with defaults — cluster-wide audit, VM operations, SDN, and one storage grant per configured storage pool — plus two conditional grants (root-path, resource pool) that apply only when the corresponding opt-in feature is used. The SDN grant drops out only for a `network_mode: bridge` opt-out.

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
| `/sdn` | `BoshOperator` | Required for `SDN.Allocate` — needed by default (`network_mode: sdn`); omit only for a `network_mode: bridge` opt-out with no `advertised_routes` |
| `/storage/<vm_storage>` | `BoshOperator` | From `pve.vm_storage` |
| `/storage/<disk_storage>` | `BoshOperator` | From `pve.disk_storage` |
| `/storage/<stemcell_storage>` | `BoshOperator` | From `pve.stemcell_storage` (defaults to `vm_storage`) |
| `/storage/<iso_storage>` | `BoshOperator` | From `pve.iso_storage` (defaults to `local`) |
| `/pool/<poolid>` | `BoshOperator` | Required for `Pool.Allocate`/`Pool.Audit` — only when `cluster_lock_mode: pool` or `stemcell_template_pool` is set |

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

# SDN — required by default (network_mode: sdn); omit only for a
# network_mode: bridge opt-out with no advertised_routes.
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

# Resource pool — required for Pool.Allocate/Pool.Audit.
# Omit if cluster_lock_mode: pool and stemcell_template_pool are both unused.
curl -sk -X PUT -H "Authorization: $PVE_TOKEN" \
  --data-urlencode 'path=/pool/<poolid>' \
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
pve_api_token: 'PVEAPIToken=bosh@pve!bosh-cpi=<secret-from-3d>'
```

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


The "Fix B is insufficient" warning in [pve-settings.md §3](pve-settings.md#fix-b--grant-acl-partial-not-sufficient-alone) applies only to the **path form** `import-from=/absolute/filesystem/path`, which PVE restricts to the `root` Unix account regardless of API privileges. The CPI never uses the path form, so the minimum-privilege setup in this doc does not hit that restriction.

In short: a `bosh@pve` token with `privsep=0` and the §3 ACLs can upload stemcells and clone from them without `root@pam`.

## 7. Shared-cluster variant: scoping VM mutation to a resource pool

Every grant in §3 that lands on `/vms` is cluster-wide: on a cluster that also runs VMs the CPI does not manage, a `bosh@pve` token with those ACLs can mutate every guest, not only the ones it created. `pve.vm_pool` (see [Configuration](configuration.md)) closes that gap by assigning every CPI-created VM to a dedicated resource pool at create/clone time, so the six `/vms`-scoped privileges below can be granted on `/pool/<bosh-pool>` instead.

### How the scoping works

Confirmed against the PVE API schema (`apidoc.json`), not merely inferred:

- **Create** (`POST /nodes/{node}/qemu`) checks `VM.Allocate` on `/vms/{vmid}` **or** on `/pool/{pool}` when the request supplies a `pool` parameter. `pve.vm_pool` makes the CPI's create calls always supply that parameter, so a token with `VM.Allocate` granted only on `/pool/<bosh-pool>` can create new VMs into that pool.
- **Clone** (`POST /nodes/{node}/qemu/{vmid}/clone`) checks the same `VM.Allocate` on `/vms/{newid}` **or** `/pool/{pool}` for the *new* VMID, plus `VM.Clone` on `/vms/{vmid}` for the **template's** own VMID — see the gap below.
- **Every other VM operation** (`config` read/write, `status/*` power management, disk attach/detach, snapshots, etc.) checks its privilege on `/vms/{vmid}` for the *existing* VM. PVE's resource-pool permission model (documented in the PVE Administration Guide's Permission Management chapter) additionally honors any ACL granted on `/pool/{poolid}` for VMs that are members of that pool — so once a VM is a `<bosh-pool>` member (which it is, from the moment `create_vm` created it with `pool=<bosh-pool>`), a `/pool/<bosh-pool>`-scoped grant covers every subsequent operation on it too.

### The gap: cloning still needs template access

The clone endpoint's `VM.Clone` check runs against the **template's own VMID**, not the new VM's. A template is a member of `pve.stemcell_template_pool` (if set) or no pool at all — never `pve.vm_pool`, and the two are rejected as equal at config load specifically so they cannot be conflated. A `bosh@pve` token scoped only to `/pool/<bosh-pool>` cannot clone from a template it has no grant on.

Two ways to close this, pick one:

- Grant `VM.Clone` (bundled in `BoshOperator`) on `/pool/<stemcell-template-pool>` too, if `stemcell_template_pool` is set — a second, narrower pool-scoped grant, still far short of cluster-wide `/vms`.
- Grant `VM.Clone` on `/vms` specifically (leaving the other five VM.\* privileges pool-scoped) — templates are CPI-owned infrastructure, not guest VMs, so a `VM.Clone`-only cluster-wide grant is a much smaller blast radius than the full `/vms` grant in §3.

The reduced-ACL table below assumes `stemcell_template_pool` is set and takes the first option.

### Reduced ACL table

Replaces the `/vms` row from §3c's table; every other row (root-path, SDN, storage, and the `/pool/<poolid>` row already present for `cluster_lock_mode: pool`/`stemcell_template_pool`) is unchanged.

| Path | Role | Notes |
|---|---|---|
| `/pool/<bosh-pool>` | `BoshOperator` | Replaces `/vms`. Covers `VM.Allocate` (create/clone into the pool) and every other VM.\* privilege for VMs that are members of `<bosh-pool>` — i.e. every VM this CPI creates once `pve.vm_pool: <bosh-pool>` is set |
| `/pool/<stemcell-template-pool>` | `BoshOperator` | Additional grant closing the cloning gap above — required whenever `stemcell_template_pool` is set alongside `vm_pool`. Omit and grant `VM.Clone` on `/vms` instead if you took the second option above |

Prerequisite: `<bosh-pool>` must exist before the first `create_vm` call — the CPI does not create it (unlike the cluster-lock sentinel pools or `stemcell_template_pool`, both of which the CPI creates on demand). Create it once with an admin token:

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

- **Existing VMs are not retroactively covered.** `pve.vm_pool` only stamps pool membership on VMs `create_vm` provisions *after* the setting takes effect. A VM created before enabling it is not a pool member, and a token scoped only to `/pool/<bosh-pool>` cannot manage it — either leave `/vms` granted alongside the pool scope until every pre-existing VM is replaced, or manually add old VMIDs to the pool as a one-time migration step.
- **Changing `vm_pool` does not move existing members.** A VM stays in the pool it was created under even if the config value later changes; re-pointing `pve.vm_pool` at a new pool only affects VMs created afterward.
- **`delete_vm`'s `skiplock` behavior is unaffected either way** — it remains gated on the `root@pam` user regardless of pool scoping (see the `delete_vm` note in §2).
- **Verify before relying on this in production.** The create/clone pool-fallback is directly confirmed against the PVE API schema; the propagation to config/status/disk/snapshot operations is PVE's documented general pool-permission behavior but was not exercised against a live cluster as part of this change. Run the §5 smoke test against a `bosh@pve` token scoped *only* to the reduced ACL table above (temporarily drop the `/vms` grant) before committing to it, and re-run it after any PVE upgrade.
- **Pool membership on delete.** `DELETE /nodes/{node}/qemu/{vmid}?purge=1`'s own API description covers cleanup of backup/replication/HA job references and VM-specific permissions; it does not explicitly document resource-pool membership cleanup. A deleted VM's stale pool membership entry (if one persists) carries no capability risk — there is no VM behind that VMID to act on — but do not rely on it disappearing from `pvesh get /pools/<bosh-pool>` output without checking your PVE version.
