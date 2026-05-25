# CPI Methods Reference

The BOSH PVE CPI implements the full BOSH CPI v2 specification: 21 canonical methods plus `update_disk`, a PVE-specific extension inherited from the prior implementation.

The CPI communicates over JSON-RPC on stdin/stdout. Each invocation handles one request and exits. Logs go to stderr.

---

## BOSH CPI v2 Differences from v1

The following changes apply when using CPI v2 (i.e., when the stemcell's `api_version` field is 2):

- `configure_networks` is removed. Networks are configured only at `create_vm` time.

- `create_vm` returns `[vm_cid, networks_with_mac]` — an array — instead of the v1 bare string `vm_cid`.

- `attach_disk` returns disk hints (e.g., `{"path": "/dev/sdd"}`) instead of void.

- Registry is optional. When a stemcell has `api_version: 2`, the CPI injects agent settings into the VM's cloud-init metadata at `create_vm` time. The agent reads from the IaaS metadata service and falls back to the registry only if configured.

---

## Info

### `info`

**Type:** Required v2

**Args:** none

**Returns:** `Hash` — `{ "api_version": 2, "stemcell_formats": ["openstack-qcow2", "openstack-raw", "pve-qcow2", "general-qcow2", "general-raw"] }`

**Errors:** none

**Notes:** The Director calls `info` first to determine the CPI's API version and supported stemcell formats. This CPI always returns `api_version: 2`.

The CPI advertises `openstack-qcow2` and `openstack-raw` because OpenStack qcow2/raw stemcells are byte-compatible with what PVE imports via `qm importdisk` — PVE treats the format name opaquely and only the on-disk image bytes matter. Operators running existing `bosh-openstack-kvm-*` stemcells can therefore upload them directly with no conversion. The `pve-*` and `general-*` aliases remain accepted for forward compatibility.

---

## Stemcell

### `create_stemcell`

**Type:** Required v2

**Args:**

- `args[0]` (String): `image_path` — absolute path to the extracted stemcell image file on the local filesystem
- `args[1]` (Hash): `cloud_properties` — parsed `cloud_properties` section from `stemcell.MF`

**Returns:** `String` — `stemcell_cid`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure or storage error

**Notes:** The CPI uploads the disk image to `stemcell_storage` as a qcow2 file under the `import` content type. If the image is a gzip+tar tarball (as produced by the BOSH stemcell builder), the disk image is extracted before upload. The CPI computes a SHA-256 of the disk image and builds a content-addressed filename:

```
bosh-stemcell-<name>-<version>-<sha8>.qcow2
```

A companion sidecar JSON file is uploaded alongside the qcow2 for operator audit. If a volume with the same filename already exists on the storage pool, the upload is skipped and the existing CID is returned.

The returned `stemcell_cid` is the PVE volume identifier: `<storage>:import/<filename>` (e.g. `nfs-pool:import/bosh-stemcell-ubuntu-jammy-1.438-a1b2c3d4.qcow2`).

`cloud_properties` must include `name` and `version`; both are required to build the deterministic filename. `stemcell_storage` must be a shared storage pool accessible from all cluster nodes.

---

### `delete_stemcell`

**Type:** Required v2

**Args:**

- `args[0]` (String): `stemcell_cid` — CID returned by `create_stemcell`

**Returns:** `null`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Parses the stemcell CID as `<storage>:import/<filename>` and deletes both the qcow2 volume and its companion sidecar JSON. If either volume is absent the call still succeeds (idempotent). Legacy integer-only CIDs (from template-based deployments) are rejected with an error.

Because PVE copies qcow2 data into each VM's root disk at create time (block-copy semantics), running VMs have no dependency on the stemcell volume after creation. The stemcell can be deleted at any time without affecting running VMs.

---

## VM

### `create_vm`

**Type:** Required v2

**Args:**

- `args[0]` (String): `agent_id` — ID the Director has selected for the BOSH agent
- `args[1]` (String): `stemcell_cid` — CID of the stemcell to import (`<storage>:import/<filename>` format)
- `args[2]` (Hash): `cloud_properties` — resource pool properties from the manifest (e.g., `cpu`, `memory`, `ephemeral_disk_size`)
- `args[3]` (Hash): `networks` — NetworkSpec map; each key is a network name, each value has `type`, `ip`, `netmask`, `gateway`, `dns`, and `cloud_properties`
- `args[4]` (Array of String): `disk_cids` — persistent disks likely to be attached (for placement optimization)
- `args[5]` (Hash): `environment` — resource pool env merged with BOSH-appended properties

**Returns:** `Array` — `[vm_cid, networks_with_mac]`

**Errors:**

- `Bosh::Clouds::VMCreationFailed` if VM creation fails (CPI must clean up partial resources)
- `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Creates a new VM with the stemcell disk imported in a single PVE `POST /nodes/<node>/qemu` call. The stemcell CID is passed directly as the `import-from=` value on `scsi0`. No clone step occurs.

The `stemcell_cid` must be in `<storage>:import/<filename>` format. Integer CIDs from previous template-based deployments are rejected.

A VMID is allocated from the range `[vmid_range_start, 5999]` (default: `[100, 5999]`). After the import task completes, the CPI configures NICs, attaches any pre-existing persistent disks, writes agent settings, and starts the VM. The returned `networks_with_mac` hash augments the input networks map with MAC addresses assigned by PVE.

`cloud_properties.tags` (map of `key: value`) is applied to the PVE tags field on the new VM as sanitized `<key>--<value>` entries. The BOSH-managed `director--`, `deployment--`, and `job--` triple is not known at create time and is added later by `set_vm_metadata`. See [Custom Tags](configuration.md#custom-tags).

---

### `delete_vm`

**Type:** Required v2

**Args:**

- `args[0]` (String): `vm_cid` — CID returned by `create_vm`

**Returns:** `null`

**Errors:** `Bosh::Clouds::CloudError` if deletion is not certain (to prevent orphaned VMs)

**Notes:** Stops the VM if running, then destroys it. If persistent disks are attached, the CPI detaches them before destroying the VM. If the VM does not exist, the call succeeds without error.

---

### `has_vm`

**Type:** Required v2

**Args:**

- `args[0]` (String): `vm_cid`

**Returns:** `Boolean`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Used by the BOSH cloudcheck consistency tool to detect orphaned or missing VMs.

---

### `reboot_vm`

**Type:** Required v2

**Args:**

- `args[0]` (String): `vm_cid`

**Returns:** `null`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Reboots the VM using the strategy set by `pve.reboot_mode` (default `soft`).

- **soft** (default): sends a graceful ACPI reboot via the PVE API and waits up to `pve.reboot_timeout` seconds (default 60) for the guest to respond. If the guest does not shut down in time, or the reboot call fails for any reason other than a 404, the CPI falls back to a hard reset automatically. A 404 response means the VM was not found and raises `Bosh::Clouds::CloudError`.
- **hard**: issues an immediate hard reset (power cycle) with no grace period. This was the only behavior before the `reboot_mode` option was introduced.

If the VM is stopped when `reboot_vm` is called, the CPI starts it so the VM ends up running, matching the BOSH expectation. The BOSH wire contract is unchanged: `reboot_vm` accepts only `vm_cid`.

---

### `set_vm_metadata`

**Type:** Required v2

**Args:**

- `args[0]` (String): `vm_cid`
- `args[1]` (Hash): `metadata` — arbitrary key-value pairs; the CPI must not require specific keys

**Returns:** `null`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Stores metadata as PVE VM tags and/or description. The Director passes standard keys such as `director`, `deployment`, `instance_group`, `job`, `id`, `name`, `index`, and `created_at`. Do not override or filter these.

The handler reads the VM's existing PVE tags, strips entries with the reserved prefixes `director--`, `deployment--`, and `job--`, rebuilds the triple from the incoming metadata, and merges the result with any operator-supplied custom tags already on the VM. Custom tags from `create_vm` therefore survive director re-syncs without manual reconciliation.

---

### `calculate_vm_cloud_properties`

**Type:** Optional v2

**Args:**

- `args[0]` (Hash): `desired_instance_size`
  - `cpu` (Integer): virtual core count
  - `ram` (Integer): RAM in MiB
  - `ephemeral_disk_size` (Integer): ephemeral disk size in MB

**Returns:** `Hash` — `cloud_properties` suitable for use in a BOSH VM type

**Errors:** `Bosh::Clouds::CloudError` if requested resources exceed cluster capacity

**Notes:** Queries PVE cluster node capabilities and returns the minimum PVE cloud_properties (cores, sockets, memory, target node) that satisfy the requested size. May oversize. Used by the BOSH CLI `interpolate` and `env` commands.

---

## Disk

### `create_disk`

**Type:** Required v2

**Args:**

- `args[0]` (Integer): `size` — disk size in MiB
- `args[1]` (Hash): `cloud_properties` — from the deployment manifest disk pool
- `args[2]` (String): `vm_cid` — VM where the disk will likely be attached (for placement optimization)

**Returns:** `String` — `disk_cid`

**Errors:** `Bosh::Clouds::CloudError` on storage or PVE API failure

**Notes:** Allocates a disk on `disk_storage`. The disk CID encodes the storage pool and disk identifier. Disks use VMIDs in the 9000–9999 range by convention.

`cloud_properties.tags` (map of `key: value`) is applied to the PVE tags field on the VM identified by `vm_cid`. PVE has no native disk-volume tag field — tags ride on the hosting VM. When `vm_cid` is empty (Director is creating an unattached disk), the tags are deferred and applied on the next `set_disk_metadata` call. See [Custom Tags](configuration.md#custom-tags).

---

### `delete_disk`

**Type:** Required v2

**Args:**

- `args[0]` (String): `disk_cid`

**Returns:** `null`

**Errors:** `Bosh::Clouds::CloudError` if deletion is not certain (to prevent orphaned disks)

**Notes:** The disk must be detached before deletion. The Director ensures `detach_disk` is called first.

---

### `has_disk`

**Type:** Required v2

**Args:**

- `args[0]` (String): `disk_cid`

**Returns:** `Boolean`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Used by cloudcheck to detect orphaned or missing persistent disks.

---

### `attach_disk`

**Type:** Required v2

**Args:**

- `args[0]` (String): `vm_cid`
- `args[1]` (String): `disk_cid`

**Returns:** `Hash` — disk hints, e.g. `{"path": "/dev/sdd"}`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Attaches the disk to the VM's PVE config and awaits the PVE task. Returns the kernel device path assigned to the disk. This is a v2 change: v1 returned void and updated the registry instead.

---

### `detach_disk`

**Type:** Required v2

**Args:**

- `args[0]` (String): `vm_cid`
- `args[1]` (String): `disk_cid`

**Returns:** `null`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Removes the disk from the VM's PVE config and awaits the PVE task. In v2 with `api_version: 2` stemcells, the Director sends disk-detach notification to the agent directly; the CPI does not touch the registry.

---

### `get_disks`

**Type:** Required v2

**Args:**

- `args[0]` (String): `vm_cid`

**Returns:** `Array of String` — disk CIDs currently attached to the VM

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Used by cloudcheck to reconcile disk attachment state.

---

### `snapshot_disk`

**Type:** Required v2

**Args:**

- `args[0]` (String): `disk_cid`
- `args[1]` (Hash): `metadata` — key-value snapshot annotations

**Returns:** `String` — `snapshot_cid`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Creates a PVE snapshot of the disk and returns an identifier encoding the disk CID and snapshot name.

---

### `delete_snapshot`

**Type:** Required v2

**Args:**

- `args[0]` (String): `snapshot_cid`

**Returns:** `null`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Deletes the PVE disk snapshot identified by `snapshot_cid`.

---

### `resize_disk`

**Type:** Required v2

**Args:**

- `args[0]` (String): `disk_cid`
- `args[1]` (Integer): `new_size` — target size in MiB

**Returns:** `null`

**Errors:**

- `Bosh::Clouds::NotSupported` when `new_size` is less than the current disk size (shrink rejected; Director falls back to create-new + copy-data)
- `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** The Director calls this only when `director.enable_cpi_resize_disk: true`. The disk must be detached before resize.

---

### `set_disk_metadata`

**Type:** Required v2

**Args:**

- `args[0]` (String): `disk_cid`
- `args[1]` (Hash): `metadata` — key-value pairs; reserved keys include `instance_id`, `instance_index`, `attached_at`, `instance_group`, `deployment`, `director`

**Returns:** `null`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** PVE has no native disk metadata. This CPI stashes metadata as a JSON block in the description of the VM that currently has the disk attached. If the disk is detached at call time, the CPI logs a warning and returns success without storing the metadata.

If `metadata` contains a `tags` sub-object (map of `key: value`), the entries are extracted from the regular `bosh_disk_metadata` payload, written to the hosting VM's PVE tags field as sanitized `<key>--<value>` entries, and recorded separately in the description sentinel under `bosh_disk_tags[<disk_cid>]`. Existing tag entries whose key prefix collides with a new tag are replaced; other entries are preserved. See [Custom Tags](configuration.md#custom-tags).

---

### `update_disk`

**Type:** Extension (PVE-specific)

**Args:**

- `args[0]` (String): `disk_cid`
- `args[1]` (Hash): `update_spec` — optional fields:
  - `size` (Integer): new size in MiB
  - `iothread` (Boolean): enable per-disk I/O thread
  - `cache` (String): PVE cache mode (`none`, `writeback`, `writethrough`, etc.)
  - `discard` (String): discard/TRIM mode (`ignore` or `on`)
  - `ssd` (Boolean): expose disk as SSD
  - `backup` (Boolean): include in backups
  - `iops_rd` (Integer): read IOPS limit
  - `iops_wr` (Integer): write IOPS limit

**Returns:** `null`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Not part of the canonical BOSH CPI v2 specification. Provides full PVE disk option updates beyond what `resize_disk` covers. If `size` is provided, the resize follows the same shrink-rejection rule as `resize_disk`.

---

## Network

### `create_network`

**Type:** Optional v2

**Args:**

- `args[0]` (Hash): `network_spec`
  - `type` (String): network type — required
  - `cloud_properties` (Hash): IaaS-specific config — required
  - `range` (String): CIDR range — optional
  - `gateway` (String): gateway IP — optional
  - `netmask_bits` (Integer): prefix length — optional

**Returns:** `Array` — `[network_id, address_properties, cloud_properties]`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Creates a named network resource in PVE. Many deployments do not use dynamic network creation; networks are typically pre-configured on the PVE host.

---

### `delete_network`

**Type:** Optional v2

**Args:**

- `args[0]` (String): `network_id` — from `create_network`

**Returns:** `null`

**Errors:** `Bosh::Clouds::CloudError` on PVE API failure

**Notes:** Removes the named PVE network resource.

---

## Method Summary

| # | Method | Group | Returns | v1→v2 Change |
|---|---|---|---|---|
| 1 | `info` | Info | Hash | Required; must return `api_version: 2` |
| 2 | `create_stemcell` | Stemcell | stemcell\_cid | No change |
| 3 | `delete_stemcell` | Stemcell | null | No change |
| 4 | `create_vm` | VM | [vm\_cid, networks] | Returns array (was bare string) |
| 5 | `delete_vm` | VM | null | Registry skip in v2 |
| 6 | `has_vm` | VM | Boolean | No change |
| 7 | `reboot_vm` | VM | null | No change |
| 8 | `set_vm_metadata` | VM | null | No change |
| 9 | `calculate_vm_cloud_properties` | VM | Hash | New in v2 |
| 10 | `create_disk` | Disk | disk\_cid | No change |
| 11 | `delete_disk` | Disk | null | No change |
| 12 | `has_disk` | Disk | Boolean | No change |
| 13 | `attach_disk` | Disk | disk\_hints Hash | Returns hints (was void) |
| 14 | `detach_disk` | Disk | null | Registry skip in v2 |
| 15 | `get_disks` | Disk | Array of String | No change |
| 16 | `snapshot_disk` | Disk | snapshot\_cid | No change |
| 17 | `delete_snapshot` | Disk | null | No change |
| 18 | `resize_disk` | Disk | null | No change |
| 19 | `set_disk_metadata` | Disk | null | No change |
| 20 | `update_disk` | Disk | null | Extension (PVE-specific) |
| 21 | `create_network` | Network | [id, addrs, props] | New in v2 (optional) |
| 22 | `delete_network` | Network | null | New in v2 (optional) |
| — | `configure_networks` | REMOVED | — | Removed in v2 |
