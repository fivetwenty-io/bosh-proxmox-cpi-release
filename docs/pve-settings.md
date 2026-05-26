# Proxmox VE Settings Required by `bosh-pve-cpi`

Prerequisites on the PVE host before `bosh create-env` succeeds. One-time per cluster. Each section gives both the UI path and an equivalent API call.

Examples below assume:

```bash
PVE_HOST=pve-0.taile80fe.ts.net:8006
PVE_TOKEN='PVEAPIToken=root@pam!ocfp-bosh-cpi-root=00000000-0000-0000-0000-000000000000'
```

Replace the token secret before running.

## 1. Enable Local Storage (`disable=0`)

If `local` is flagged disabled in PVE, every stemcell upload fails with a storage-not-active error. Enable it.

UI:

1. Datacenter → Storage → `local` → Edit.

2. Uncheck **Disable**.

3. OK.

API:

```bash
curl -sk -X PUT -H "Authorization: $PVE_TOKEN" \
  -d 'disable=0' \
  https://$PVE_HOST/api2/json/storage/local
```

Verify:

```bash
curl -sk -H "Authorization: $PVE_TOKEN" \
  https://$PVE_HOST/api2/json/storage/local | jq '.data.disable'
```

Expect `0` or `null` (absent). `1` means still disabled.

## 2. Enable `Import` Content on Stemcell Storage

The CPI uploads stemcells as qcow2 disk images and references them via the `import-from=` parameter when creating VMs. PVE only accepts the `import-from=` directive when the source storage advertises the **Import** content type.

This must be enabled on the storage pool configured as `pve.stemcell_storage`. For multi-node clusters that storage pool must be shared (NFS, CIFS, CephFS, etc.); see [Configuration — Stemcell Storage](configuration.md#stemcell-storage) for details.

UI:

1. Datacenter → Storage → select the storage configured as `pve.stemcell_storage` → Edit → Content.

2. Check **Import** (in addition to existing checks).

3. Recommended companion checks: `backup`, `vztmpl`, `iso`.

4. OK.

API (sets the full content list — include every type you want kept):

```bash
curl -sk -X PUT -H "Authorization: $PVE_TOKEN" \
  --data-urlencode 'content=import,backup,vztmpl,iso,snippets' \
  https://$PVE_HOST/api2/json/storage/local
```

Verify from the workstation:

```bash
curl -sk -H "Authorization: $PVE_TOKEN" \
  https://$PVE_HOST/api2/json/storage/local | jq '.data.content'
```

Output must include `"import"`.

Or on the PVE node:

```bash
pvesm status -storage local
grep '^dir: local' -A4 /etc/pve/storage.cfg
```

The `content` line must include `import`.

## 3. Disable Privilege Separation on the API Token

This section covers the `root@pam` quick-start token. For the recommended non-root setup — a dedicated `bosh@pve` user with a custom `BoshOperator` role — see [pve-api-permissions.md](pve-api-permissions.md). The `privsep=0` requirement applies to both paths; the trust boundary is what differs.

PVE API tokens default to **Privilege Separation = on**. That gives the token its own (empty) ACL, distinct from the parent user — even when the parent is `root@pam`. The CPI then fails any call that needs full root authority, most visibly `--import-from=<path>` (filesystem-path arguments are user-bound, not ACL-bound).

### Fix A — Disable Privilege Separation (recommended)

UI:

1. Datacenter → Permissions → API Tokens.

2. Edit the token used by the CPI (e.g. `root@pam!ocfp-bosh-cpi-root`).

3. Uncheck **Privilege Separation**.

4. OK.

API:

```bash
curl -sk -X PUT -H "Authorization: $PVE_TOKEN" \
  -d 'privsep=0' \
  https://$PVE_HOST/api2/json/access/users/root@pam/token/ocfp-bosh-cpi-root
```

Verify:

```bash
curl -sk -H "Authorization: $PVE_TOKEN" \
  https://$PVE_HOST/api2/json/access/users/root@pam/token/ocfp-bosh-cpi-root \
  | jq '.data.privsep'
```

Expect `0`. The token now inherits the parent user's full powers. Required for `import-from=` and any other path-bound operation.

### Fix B — Grant ACL (partial, not sufficient alone)

If you must keep Privilege Separation on, grant the token Administrator on `/`:

UI:

1. Datacenter → Permissions → Add → **API Token Permission**.

2. Path: `/`.

3. Token: `root@pam!ocfp-bosh-cpi-root`.

4. Role: `Administrator`.

5. Propagate: checked.

API:

```bash
curl -sk -X PUT -H "Authorization: $PVE_TOKEN" \
  --data-urlencode 'path=/' \
  --data-urlencode 'tokens=root@pam!ocfp-bosh-cpi-root' \
  --data-urlencode 'roles=Administrator' \
  -d 'propagate=1' \
  https://$PVE_HOST/api2/json/access/acl
```

This unlocks ACL-gated APIs, but **not** `--import-from=<path>`. PVE restricts arbitrary-filesystem-path arguments to the `root` user account (or a token with Privilege Separation disabled acting as root). Stemcell upload will still fail.

**TL;DR:** Use Fix A. Fix B alone is insufficient for stemcell import.

## Full Verification

After all three changes, smoke-test from the workstation:

```bash
curl -sk -H "Authorization: $PVE_TOKEN" \
  https://$PVE_HOST/api2/json/storage/local \
  | jq '{disable: .data.disable, content: .data.content}'

curl -sk -H "Authorization: $PVE_TOKEN" \
  https://$PVE_HOST/api2/json/access/users/root@pam/token/ocfp-bosh-cpi-root \
  | jq '.data.privsep'
```

Expect:

- `disable`: `0` or `null`

- `content`: includes `"import"`

- `privsep`: `0`

If the calls return `401`, the token secret is wrong. If any field is off, re-run the matching section above.

## Cluster topology limitations

### Single-node vs. multi-node PVE

The CPI reads `config.node` for node-scoped operations (bridge create/delete, VM placement when no cloud-property override is supplied). On a single-node cluster this works transparently because there is only one node to target. On multi-node clusters, ensure `config.node` names a node that is reachable and that hosts (or will host) the resources the CPI manages.

VM-scan operations (e.g., `has_vm`, `get_disks`) search across all cluster nodes and do not depend on `config.node`.

### Bridge network node affinity

Linux bridges are per-node configuration objects. The CPI creates a bridge on the node resolved at `create_network` time (`cloud_properties.node` if supplied, otherwise `config.node`) and deletes it from `config.node` at `delete_network` time.

**Operator requirement:** do not change `config.node` between `create_network` and `delete_network` for the same network CID. If `config.node` is changed in between, `delete_network` will target the wrong node and the bridge on the original node will be left behind. Clean it up manually:

```bash
# On the original node
pvesh set /nodes/<original-node>/network --iface <bridge-name>
# or via API
curl -sk -X DELETE -H "Authorization: $PVE_TOKEN" \
  https://$PVE_HOST/api2/json/nodes/<original-node>/network/<bridge-name>
curl -sk -X PUT -H "Authorization: $PVE_TOKEN" \
  https://$PVE_HOST/api2/json/nodes/<original-node>/network
```

This limitation does not affect the SDN path; SDN vnets are cluster-global and are not tied to a specific node.

### SDN `sdn_auto_manage_zone` scope

The `sdn_auto_manage_zone` CPI config flag controls whether the CPI creates or deletes SDN zones on your behalf. It does **not** invent zone names.

| Flag value | Zone absent from PVE | Zone name not supplied by operator |
|---|---|---|
| `false` (default) | `create_network` returns an error | `create_network` returns an error |
| `true` | CPI creates the zone in PVE | `create_network` returns an error — a name is still required |

The operator must always supply the zone name via `cloud_properties.zone` or `config.sdn_zone`. Setting `sdn_auto_manage_zone: true` only allows the CPI to create a zone that does not yet exist in PVE, and to delete a zone that becomes empty after `delete_network`. It does not relax the requirement that the zone name be provided.
