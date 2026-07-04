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
| Minimum privilege (recommended) | `bosh@pve` (custom role `BoshOperator`) | `bosh@pve!<id>`, `privsep=0` | Bounded by `BoshOperator` ACL on `/`, `/vms`, and the configured storage paths | Production, shared clusters, any deployment that outlives the lab |
| Quick-start | `root@pam` | `root@pam!<id>`, `privsep=0` | Full datacenter administrator | Throwaway lab where the BOSH director already shares trust boundary with the cluster |

Both paths produce a usable token. The minimum-privilege path is recommended everywhere except short-lived labs because the `root@pam` token with `privsep=0` is equivalent to handing the BOSH director full datacenter administration. The quick-start path is documented in [pve-settings.md §3](pve-settings.md#3-disable-privilege-separation-on-the-api-token).


## 2. Privileges the CPI actually uses

Derived from the API endpoint inventory under `src/pve_cpi/internal/cpi/handlers/` and the auth-header construction in `src/pve_cpi/internal/pve/client.go`. Each row lists a PVE 9.1 privilege, the ACL path on which it must be granted, and the CPI methods that drive it. Table verified against the PVE API schema (`apidoc.json`) as of commit `cfd1b12`.

| Privilege | ACL path | CPI methods (handler file) |
|---|---|---|
| `Sys.Audit` | `/` | All `info`, `has_vm`, `has_disk`, task polling, `GET /cluster/status`, `/cluster/resources`, `/cluster/storage`, `/cluster/config/nodes`, `/nodes` (most handlers); HA-rule reads (`GET /cluster/ha/rules`, `/cluster/ha/resources`) when HA placement is enabled |
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
| `SDN.Allocate` | `/sdn` | `create_network.go`, `delete_network.go`, `create_vm.go` (`advertised_routes` subnet injection) — required when `network_mode: sdn` or `advertised_routes` is used |
| `Sys.Console` | `/` | HA-rule writes — `POST`/`DELETE /cluster/ha/resources`, `POST`/`DELETE /cluster/ha/rules` driven by `placement.pin_az_via_ha_rules`, `anti_affinity.use_ha_rules`, and DLB (`placement.dlb` configured) — see note below |
| `Pool.Allocate` | `/pool/<poolid>` | `cluster_lock.go` (`POST`/`DELETE /pools` — create/delete sentinel pools `bosh-lock-*` when `cluster_lock_mode: pool`), `create_stemcell.go` (`PUT /pools/{poolid}` via `AddVM` — assign the template VM to `pve.stemcell_template_pool` when set) — opt-in, only when one of these two features is enabled |
| `Pool.Audit` | `/pool/<poolid>` | `GetPoolComment` (`GET /pools`/`GET /pools/{poolid}`) — pool-existence and comment reads backing the same two opt-in features above |
| `Sys.Modify` | `/` | `placement_dlb.go` (`PUT /cluster/options` setting `crs=ha=dynamic,...`) — opt-in, only when `placement.dlb.manage_cluster_crs: true`; when false (default) the CPI only reads `/cluster/options` (`Sys.Audit`, already granted) and logs a warning instead of writing |

**`SDN.Allocate` note:** only required when the CPI manages SDN objects. Deployments that use pre-existing bridges or that never call `create_network`/`delete_network` and set no `advertised_routes` do not need this privilege. Grant it on `/sdn`:

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

**`delete_vm` and `skiplock`:** `delete_vm` issues `DELETE /nodes/{n}/qemu/{vmid}` with `skiplock=true` so a locked or still-running VM is destroyed without a separate unlock step. The endpoint itself checks `VM.Allocate` on `/vms/{vmid}` (already granted), but `skiplock` is **not** governed by any privilege: PVE restricts the flag to the literal `root@pam` user and rejects it for every other user regardless of role or ACL. To let `delete_vm` clear locked VMs, authenticate the CPI as `root@pam` (or an API token owned by `root@pam` with privilege separation disabled). A least-privilege `bosh@pve` user cannot be granted `skiplock` through any role; without it, `delete_vm` still succeeds on unlocked, stopped VMs but fails on locked ones.

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

3. Privileges: select `VM.Allocate`, `VM.Audit`, `VM.Config.Disk`, `VM.Config.Network`, `VM.Config.Options`, `VM.Config.Cloudinit`, `VM.PowerMgmt`, `VM.Snapshot`, `Datastore.Allocate`, `Datastore.AllocateSpace`, `Datastore.AllocateTemplate`, `Datastore.Audit`, `SDN.Allocate` (if using SDN features), `Sys.Console` (if using HA placement features or DLB), `Pool.Allocate` and `Pool.Audit` (if using `cluster_lock_mode: pool` or `stemcell_template_pool`), and `Sys.Modify` (if using `placement.dlb.manage_cluster_crs`).

4. Create.

API:

```bash
# Base role — VM, disk, and storage operations.
curl -sk -X POST -H "Authorization: $PVE_TOKEN" \
  --data-urlencode 'roleid=BoshOperator' \
  --data-urlencode 'privs=VM.Allocate,VM.Audit,VM.Config.Disk,VM.Config.Network,VM.Config.Options,VM.Config.Cloudinit,VM.PowerMgmt,VM.Snapshot,Datastore.Allocate,Datastore.AllocateSpace,Datastore.AllocateTemplate,Datastore.Audit,SDN.Allocate,Sys.Console,Pool.Allocate,Pool.Audit,Sys.Modify' \
  https://$PVE_HOST/api2/json/access/roles
```

If your deployment does not use SDN features (`network_mode: bridge` only, no `advertised_routes`), HA placement features or DLB (`pin_az_via_ha_rules: false`, `use_ha_rules: false`, no `placement.dlb`), resource pools (`cluster_lock_mode: pool` unset, `stemcell_template_pool` unset), or CPI-managed cluster CRS (`placement.dlb.manage_cluster_crs: false`/unset), you may omit `SDN.Allocate`, `Sys.Console`, `Pool.Allocate`/`Pool.Audit`, and `Sys.Modify`, respectively. The minimum required set for a bridge-only deployment without HA placement, DLB, or pools is `VM.*,Datastore.*` as listed in the privilege table above. Note that `delete_vm`'s `skiplock` flag is separate from this role: it is gated on the `root@pam` user, not on any privilege (see the `delete_vm` note above), so granting `BoshOperator` does not enable it.

### 3c. Grant ACLs

Four ACL grants are always needed — cluster-wide audit, VM operations, and one storage grant per configured storage pool — plus two conditional grants (root-path, resource pool) that apply only when the corresponding opt-in feature is used.

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
| `/sdn` | `BoshOperator` | Required for `SDN.Allocate` — only when `network_mode: sdn` or `advertised_routes` is used |
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

# SDN — required for network_mode: sdn and advertised_routes.
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
