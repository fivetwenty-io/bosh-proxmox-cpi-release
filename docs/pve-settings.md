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
