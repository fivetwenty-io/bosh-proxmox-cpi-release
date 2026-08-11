# Proxmox VE Settings Required by `bosh-pve-cpi`

Prerequisites on the PVE host that must be in place before `bosh create-env` succeeds. One-time per cluster. Each section gives both the UI path and an equivalent API call.

Examples below assume:

```bash
PVE_HOST=pve-0.taile80fe.ts.net:8006
PVE_TOKEN='PVEAPIToken=root@pam!ocfp-bosh-cpi-root=00000000-0000-0000-0000-000000000000'
```

Replace the token secret before running.

## 1. Enable Local Storage (`disable=0`)

If `local` is disabled in PVE, every stemcell upload fails with a storage-not-active error. Enable it.

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

This section covers the `root@pam` quick-start token. For the recommended non-root setup — a dedicated `bosh@pve` user with a custom `BoshOperator` role — see [pve-api-permissions.md](pve-api-permissions.md). The `privsep=0` requirement applies to both paths; only the trust boundary differs.

PVE API tokens default to **Privilege Separation = on**, giving the token its own (empty) ACL, distinct from the parent user — even when the parent is `root@pam`. Every ACL-gated call the CPI makes then fails with `403`; the stemcell upload and the `import-from=<storage>:import/<file>` clone that consumes it are typically the first casualties.

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

Expect `0`. The token now inherits the parent user's full ACLs, and every CPI call — stemcell upload and `import-from=` included — is authorized as the parent user.

### Fix B — Grant ACL to the token (works, but heavier)

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

This authorizes every ACL-gated API, which covers the CPI completely: the CPI references stemcells via the volume form `import-from=<storage>:import/<file>`, never a raw filesystem path, so PVE's root-only restriction on path-form arguments does not apply to it — see [pve-api-permissions.md §6](pve-api-permissions.md#6-caveat-import-from-and-privilege-separation). The trade is a second object to maintain: the token's ACL must be kept in sync with what the CPI needs, on top of the token itself.

**TL;DR:** Use Fix A — one flag, same effect, nothing extra to maintain.

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

## 4. IOMMU Enablement for PCI Passthrough

**This section applies only if you use `pci_passthroughs` in `create_vm` cloud_properties.** Standard BOSH deployments that do not pass host PCI devices to VMs can skip it.

PCI passthrough requires the host CPU's IOMMU (Input-Output Memory Management Unit) to be enabled. IOMMU lets the kernel isolate PCI device DMA access to the guest VM's address space. Without it, the hypervisor cannot safely expose the device to a guest.

### 4a. Enable IOMMU in the kernel boot parameters

Edit `/etc/default/grub` on each PVE node that will host VMs with PCI passthrough:

For Intel CPUs:

```
GRUB_CMDLINE_LINUX_DEFAULT="quiet intel_iommu=on iommu=pt"
```

For AMD CPUs:

```
GRUB_CMDLINE_LINUX_DEFAULT="quiet amd_iommu=on iommu=pt"
```

The `iommu=pt` flag enables pass-through mode, avoiding IOMMU translation overhead for devices not passed to guests. Apply the change and reboot:

```bash
update-grub
reboot
```

Verify IOMMU is active after reboot:

```bash
dmesg | grep -e DMAR -e IOMMU | head -10
```

Expect lines such as `DMAR: IOMMU enabled` (Intel) or `AMD-Vi: AMD IOMMUv2 loaded` (AMD).

### 4b. Load VFIO kernel modules

VFIO is the kernel framework that mediates PCI device ownership between host and guest:

```bash
# Append to /etc/modules
echo -e "vfio\nvfio_iommu_type1\nvfio_pci\nvfio_virqfd" >> /etc/modules
update-initramfs -u -k all
```

### 4c. Enable IOMMU in the PVE BIOS passthrough mode

In the PVE web UI:

1. Select the node → System → BIOS.

2. Ensure **IOMMU** (also labelled **VT-d** on Intel or **AMD-Vi** on AMD) is enabled in the firmware settings. This is a firmware toggle, not an OS toggle — the kernel parameter in §4a activates the OS-side driver, but the firmware must expose the hardware capability first.

> **Operator note:** PCI passthrough is incompatible with live migration. The CPI enforces this automatically: any VM created with `pci_passthroughs` receives a strict HA node-affinity pin that prevents the PVE HA manager from migrating it. IOMMU group isolation is the operator's responsibility — the CPI validates that the requested PCI address exists on the target node but does not inspect IOMMU group membership.

## Cluster topology limitations

### Single-node vs. multi-node PVE

The CPI reads `config.node` for node-scoped operations (bridge create/delete, VM placement when no cloud-property override is supplied). On a single-node cluster this works transparently because there is only one node to target. On multi-node clusters, ensure `config.node` names a reachable node that hosts (or will host) the resources the CPI manages.

VM-scan operations (e.g., `has_vm`, `get_disks`) search across all cluster nodes and do not depend on `config.node`.

### Bridge network node affinity

Linux bridges are per-node configuration objects. The CPI creates a bridge on the node resolved at `create_network` time (`cloud_properties.node` if supplied, otherwise `config.node`) and deletes it from `config.node` at `delete_network` time.

**Operator requirement:** do not change `config.node` between `create_network` and `delete_network` for the same network CID. If `config.node` changes in between, `delete_network` targets the wrong node and the bridge on the original node is left behind. Clean it up manually:

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

The `sdn_auto_manage_zone` CPI config flag controls whether the CPI creates or deletes SDN zones on your behalf. It does **not** invent zone names or relax the requirement that the zone name be provided.

| Flag value | Zone absent from PVE | Zone name not supplied by operator |
|---|---|---|
| `false` (default) | `create_network` returns an error | `create_network` returns an error |
| `true` | CPI creates the zone in PVE | `create_network` returns an error — a name is still required |

The operator must always supply the zone name via `cloud_properties.zone` or `config.sdn_zone`. Setting `sdn_auto_manage_zone: true` only allows the CPI to create a zone that does not yet exist in PVE, and to delete a zone that becomes empty after `delete_network`.
