# Configuration

The CPI is configured via properties in a BOSH deployment manifest. The job template renders the manifest properties into a JSON document that the binary reads with the `--config` flag. The properties below match `jobs/pve_cpi/spec`.

| Property | Description | Default | Required |
|---|---|---|---|
| `pve.host` | PVE host (IP or FQDN) | - | yes |
| `pve.port` | PVE API port | `8006` | no |
| `pve.user` | PVE username (e.g. `root@pam` or `bosh@pve`) | - | yes |
| `pve.password` | PVE password. Mutually exclusive with `api_token`. Must be credhub-managed in production via `((pve_password))`. | `""` | one of password or api_token |
| `pve.api_token` | PVE API token (`<user>!<token-id>=<uuid>`). Mutually exclusive with `password`. Must be credhub-managed in production via `((pve_api_token))`. | `""` | one of password or api_token |
| `pve.realm` | Authentication realm | `pam` | no |
| `pve.node` | Default node for placement and bridge operations | - | yes |
| `pve.vm_storage` | Storage pool for VM root disks | - | yes |
| `pve.disk_storage` | Storage pool for persistent disks | - | yes |
| `pve.stemcell_storage` | Storage pool for stemcell qcow2 images. Must be a file-based PVE storage (`dir`, `nfs`, `cifs`, `glusterfs`, `cephfs`) — block-based storages (`lvm`, `lvmthin`, `zfspool`, `rbd`) cannot accept qcow2 uploads. Must also be shared across cluster nodes when the cluster has more than one node. Defaults to `vm_storage`; in that case `vm_storage` must satisfy the same constraints. | `""` (falls back to `vm_storage`) | no |
| `pve.iso_storage` | Storage pool (`dir`, `nfs`, or `cifs` with `iso` content enabled) used for per-VM ConfigDrive ISOs in `cloudinit` agent mode. Block storages (`lvm`, `lvmthin`, `zfspool`) cannot hold ISO files. The default `local` value places ISOs on node-local storage and is readable by any user with PVE node access — see [ConfigDrive ISO storage](operations.md#configdrive-iso-storage) for the dedicated-pool recommendation. | `local` | no |
| `pve.network_bridge` | Default Linux bridge for `create_vm` NIC attachment. Required regardless of `network_mode`. | `vmbr0` | no |
| `pve.network_mode` | Network creation mode for managed networks. `sdn` — PVE SDN vnet lifecycle. `bridge` — Linux bridge lifecycle. `auto` — SDN when `cloud_properties.zone` or `pve.sdn_zone` is set; bridge otherwise. See [Network configuration](networks.md). | `auto` | no |
| `pve.sdn_zone` | Default PVE SDN zone for vnet placement. When empty, the zone must be supplied per-call in `cloud_properties.zone`. See [Network configuration](networks.md). | `""` | no |
| `pve.sdn_zone_type` | Zone type the CPI uses when creating a zone (`simple`, `vlan`, `qinq`, `vxlan`, `evpn`). Only relevant when `sdn_auto_manage_zone` is `true`. | `simple` | no |
| `pve.sdn_auto_manage_zone` | When `true`, the CPI may create SDN zones on `create_network` and delete them on `delete_network` when all safety conditions are met. See [Network configuration](networks.md). | `false` | no |
| `pve.verify_ssl` | Verify the PVE API TLS certificate | `true` | no |
| `pve.ca_cert` | Optional PEM-encoded CA certificate bundle for verifying the Proxmox VE API TLS certificate. When empty (default), the system trust pool is used — behavior is byte-identical to prior releases. When set, the PEM is parsed and the resulting cert pool is used for PVE API HTTPS verification. Ignored when `verify_ssl` is `false`. Symmetric to `registry.ca_cert`. | `""` | no |
| `pve.agent_mode` | Agent bootstrap mode (`cloudinit`, `registry`, `noagent`) | `cloudinit` | no |
| `pve.vm_disk_format` | Disk image format (`qcow2`, `raw`, `vmdk`) | `qcow2` | no |
| `pve.hotplug` | PVE hotplug flags applied to every new VM. Comma-separated subset of `network,disk,cpu,memory,usb,cloudinit`, or `0` to disable hotplug entirely. Per-VM override via `cloud_properties.hotplug`. | `network,disk,cpu,memory` | no |
| `pve.numa` | Enable NUMA (`numa=1`) on every new VM. Required at create time for live memory hotplug to allocate DIMM slots; without it, memory hot-add silently no-ops. Per-VM override via `cloud_properties.numa`. | `true` | no |
| `pve.reboot_mode` | `reboot_vm` strategy: `soft` (graceful ACPI reboot, hard-reset fallback) or `hard` (immediate reset). | `soft` | no |
| `pve.reboot_timeout` | Seconds to wait for graceful shutdown before hard-reset fallback (soft mode only). Range 1–3600. | `60` | no |
| `pve.log_level` | Structured log level (`debug`, `info`, `warn`, `error`) | `info` | no |
| `pve.vmid_range_start` | First VMID used for VM allocation. VMs use `[vmid_range_start, vmid_range_end]`. Persistent disks use `[9000, 9999]`. | `100` | no |
| `pve.vmid_range_end` | Inclusive upper bound of the VM VMID range. Must be greater than `vmid_range_start` and at most `9999`. The allocator scans this range from a randomized start so concurrent CPI invocations rarely pick the same VMID; a retry-on-conflict loop backstops the rare collision. | `5999` | no |
| `pve.clone_mode` | Clone type used when `create_vm` clones a stemcell template. `auto` (default): linked clone for snapshot-capable backends (`dir`, `nfs`, `cifs`, `zfspool`, `lvmthin`, `rbd`, `cephfs`); full clone for `lvm`-thick (linked clone not supported). `linked`: force linked clone; returns an error on `lvm`-thick. `full`: force full clone on all backends. One of `auto`\|`linked`\|`full`. | `auto` | no |
| `pve.stemcell_template_vmid_range_start` | Starting VMID for stemcell template VM allocation. When unset (`0`), the CPI derives the start as `vmid_range_end + 1`; with the default VM range (`vmid_range_end = 5999`) this yields `6000`. Must not overlap the VM range or the persistent-disk range `9000–9999`. | `0` (derived) | no |
| `pve.stemcell_template_vmid_range_end` | Inclusive upper bound of the template VMID range. When unset (`0`), defaults to `8999`. Must be greater than `stemcell_template_vmid_range_start` and at most `8999`. Must not overlap the persistent-disk range. | `0` (derived) | no |
| `pve.stemcell_template_pool` | Optional PVE resource pool to assign to newly created template VMs. When empty (default), templates are not assigned to any pool. An invalid pool name causes `create_stemcell` to return an error. | `""` | no |
| `pve.stemcell_template_node` | Optional PVE node on which template VMs are created. When empty (default), falls back to `pve.node`. When using local `stemcell_storage`, this must equal the node where that storage is mounted; pointing to a different node with local storage causes the template import to fail because the uploaded qcow2 is not visible from the other node. | `""` | no |
| `pve.vm_prefix` | Optional prefix prepended to every CPI-provisioned VM's PVE name. With `cpi`, names take the form `cpi-<deployment>-<job>-<index>`. Empty means the prefix is omitted. The prefix is cluster-wide — every VM created by this CPI deployment carries it. | `""` | no |
| `pve.create_env_deployment` | Synthetic deployment name used for VMs created by `bosh create-env`. bosh-init does not pass a deployment in env, so a stable placeholder is required for the `<deployment>` segment of the VM name. | `create-env` | no |
| `pve.allow_disk_ops_with_snapshots` | When `true`, bypasses the snapshot pre-flight guard in `attach_disk`, `detach_disk`, and `resize_disk`. Use only for emergency disk recovery — snapshot state becomes inconsistent after the operation. | `false` | no |
| `pve.require_snapshot_check_pass` | Controls behavior when the snapshot pre-flight check itself cannot reach PVE. `false` (default) logs a warning and proceeds (fail-open); `true` aborts the disk operation if the snapshot list cannot be fetched (fail-closed). | `false` | no |
| `pve.stemcell_staging_dir` | Optional absolute path. When set, all stemcell file reads and writes for director-supplied paths are scoped to this directory using Go's `os.Root` API, preventing access to files outside the declared root. When unset (default), behavior is unchanged from prior releases. Must be an absolute path to an existing directory on the CPI host. Defense-in-depth against unexpected stemcell paths. | `""` | no |
| `pve.fetch_credential_defaults` | Ordered list of URL-prefix-to-auth mappings used when fetching a light stemcell whose `cloud_properties.image_url` carries no per-stemcell `image_url_auth`. The entry with the longest matching `url_prefix` wins. Each entry requires a `url_prefix` string and an `auth` object with a required `type` field; supported types are `basic`, `bearer`, `s3`, `oci`, and `blobstore`. When unset or empty, light-stemcell fetches without per-stemcell credentials are unauthenticated. See [Light stemcells](light-stemcells.md) for full auth-object schemas. | `[]` | no |
| `agent.mbus` | URL the BOSH agent should bind/listen on inside the VM. Required for `bosh create-env`: bosh-init does not pass it via the per-call env argument, only via CPI config. When empty, the CPI derives `nats://<blobstore-host>:4222` from the blobstore endpoint if one is configured (loopback hosts rejected). | `""` | yes for `create-env` |
| `agent.blobstore` | Optional default blobstore settings for the agent's `settings.json` (mirrors `agent.blobstore` in `bosh.yml`). | `{}` | no |
| `registry.endpoint` | BOSH registry URL. Must use `https://` unless `registry.allow_insecure` is `true`. | `""` | yes when `agent_mode = registry` |
| `registry.user` | Registry HTTP basic-auth username | `""` | yes when `agent_mode = registry` |
| `registry.password` | Registry HTTP basic-auth password | `""` | yes when `agent_mode = registry` |
| `registry.allow_insecure` | When `true`, permits a plaintext `http://` registry endpoint. Default `false` rejects any non-`https` scheme so registry credentials never travel in cleartext. Set `true` only for lab and test deployments. | `false` | no |
| `registry.ca_cert` | Optional PEM-encoded CA certificate (or chain) appended to the host system trust pool when verifying the registry's TLS certificate. Use when the registry presents a certificate signed by a private CA. Empty means use the system pool unmodified. | `""` | no |
| `registry.allowed_hosts` | Optional list of host patterns that restrict the registry HTTP client to only contact matching hosts. Each entry is an exact host (e.g. `registry.example.com`) or a single-level wildcard prefix (e.g. `*.example.com`). When non-empty, the client rejects any request whose resolved host does not match at least one entry. Empty (default) disables allow-list filtering; the configuredHost invariant and disabled redirects still apply regardless. Defense-in-depth against SSRF via host mutation. | `[]` | no |

## Stemcell Storage

`stemcell_storage` must be a **file-based** PVE storage pool. The CPI uploads the qcow2 image via the PVE upload API, which only accepts file-based storages (`dir`, `nfs`, `cifs`, `glusterfs`, `cephfs`). Block-based storages (`lvm`, `lvmthin`, `zfspool`, `rbd`) reject uploads with `can't upload to storage type '<type>'` and are unusable for stemcells regardless of cluster topology.

For multi-node clusters, `stemcell_storage` must additionally be shared. The `create_stemcell` call enforces this: if the storage is local-only and the cluster has more than one node, the call fails immediately with a descriptive error. Single-node clusters may use local file-based storage (e.g. the default `local` dir at `/var/lib/vz`); the shared check is skipped when the cluster reports exactly one node.

Recommended shared backends: NFS, CIFS, CephFS, GlusterFS, or any other PVE storage type configured with `shared=1` in `/etc/pve/storage.cfg`.

The storage pool must have the `import` content type enabled. See [Proxmox VE Settings](pve-settings.md) for the steps to enable it.

## Stemcell Template Cloning

`create_stemcell` builds one frozen PVE template VM per stemcell and returns a `template:<vmid>` CID. `create_vm` then clones that template instead of running a full qcow2 block-copy per VM. On linked-clone–capable storage backends this reduces VM creation time from roughly four minutes to seconds.

The five properties in the table above (`clone_mode`, `stemcell_template_vmid_range_start`, `stemcell_template_vmid_range_end`, `stemcell_template_pool`, `stemcell_template_node`) are all optional. Zero configuration is required; the defaults produce the correct behavior for most deployments.

### Clone type by storage backend

| Storage backend | Default clone type | Notes |
|---|---|---|
| `dir`, `nfs`, `cifs`, `cephfs` | Linked (CoW) | Fastest; backed by qcow2 snapshots |
| `zfspool`, `lvmthin`, `rbd` | Linked (CoW) | Fastest; backed by ZFS/LVM-thin/RBD snapshots |
| `lvm` (thick) | Full | `lvm`-thick does not support linked clones |

Set `clone_mode: full` to force full clones everywhere, or `clone_mode: linked` to force linked clones and get an explicit error on `lvm`-thick rather than a silent fallback.

### Template VMID range

Template VMIDs default to `[vmid_range_end + 1, 8999]`. With the default VM range (`vmid_range_end = 5999`) this is `[6000, 8999]`. If you raise `vmid_range_end`, the template range start shifts up automatically; no explicit template range configuration is needed unless you want to override it.

Override example:

```yaml
pve:
  vmid_range_start: 100
  vmid_range_end: 5999
  stemcell_template_vmid_range_start: 6000
  stemcell_template_vmid_range_end: 7999
```

The template range must not overlap `[vmid_range_start, vmid_range_end]` or the persistent-disk range `[9000, 9999]`. The validator rejects overlapping configurations at CPI startup.

### Cross-node and multi-node considerations

Template VMs are created on `stemcell_template_node` (or `pve.node` if unset). For shared storage backends (NFS, CIFS, CephFS, GlusterFS, RBD), any cluster node can clone the template — no additional configuration is needed.

For local storage backends (`dir`, `zfspool`, `lvmthin`, `lvm`) on multi-node clusters, the template and the VM being cloned must be on the same node. Options:

- Pin `stemcell_template_node` and set `cloud_properties.target_node` in your BOSH VM types to the same node.
- Use shared storage for `stemcell_storage` (recommended for production multi-node clusters).

The CPI does not auto-migrate templates between nodes. If a clone lands on the wrong node, the workaround is to manually live-migrate the resulting VM in the PVE UI after `create_vm` completes.

### Back-compatibility

Stemcells uploaded before this feature was introduced continue to work without operator action. When `create_vm` receives a pre-upgrade CID (a `<storage>:import/<file>` or `light:...` form), it looks for a matching template by content hash. If a template is found the fast clone path runs; if not the original slow `import-from=` path runs. No re-upload is required.

## Authentication

Exactly one of `pve.password` or `pve.api_token` must be set. API tokens are preferred for production deployments; they support per-token revocation and per-token privilege separation in PVE 9.

See [pve-api-permissions.md](pve-api-permissions.md) for token creation and the minimum-privilege `bosh@pve` user setup.

## SDN Network Management

When the Director's cloud-config marks a network as `managed: true`, the CPI calls `create_network` and `delete_network` to provision and remove the network resource. The CPI supports two backends: PVE SDN vnets and Linux bridges on a node.

### Configuration Properties

| Property | Description | Default |
|---|---|---|
| `pve.network_mode` | `sdn` — PVE SDN vnet lifecycle. `bridge` — Linux bridge lifecycle. `auto` — SDN when a zone is resolvable, bridge otherwise. | `auto` |
| `pve.sdn_zone` | Default SDN zone name for vnet placement. Overridable per-call via `cloud_properties.zone`. | `""` (per-call) |
| `pve.sdn_zone_type` | Zone type used when the CPI creates a zone. One of: `simple`, `vlan`, `qinq`, `vxlan`, `evpn`. | `simple` |
| `pve.sdn_auto_manage_zone` | When `true`, the CPI may create and delete SDN zones. See zone lifecycle notes below. | `false` |

### Prerequisites — SDN Mode

1. PVE SDN must be enabled at the datacenter level. The **Datacenter > SDN** menu appears in PVE 7.2+ and requires `libpve-network-perl` installed on all cluster nodes.

2. At least one SDN zone must exist before `create_network` is called, unless `sdn_auto_manage_zone: true` is set to let the CPI create it. The zone name must match `cloud_properties.zone` or `pve.sdn_zone`.

3. The PVE API token or user must hold the `SDN.Allocate` privilege on `/sdn`.

### Manifest Example — SDN Mode

```yaml
properties:
  pve:
    host: pve.example.com
    user: root@pam
    api_token: root@pam!bosh=<token>
    node: pve1
    vm_storage: local-lvm
    disk_storage: local-lvm
    network_bridge: vmbr0
    network_mode: sdn
    sdn_zone: boshzone
    sdn_zone_type: simple
    sdn_auto_manage_zone: false
```

Cloud-config managed network:

```yaml
networks:
- name: bosh-net
  type: manual
  managed: true
  cloud_properties:
    zone: boshzone
    vnet: boshvn
  subnets:
  - range: 10.200.0.0/24
    gateway: 10.200.0.1
```

### Manifest Example — Bridge Mode

```yaml
properties:
  pve:
    network_mode: bridge
    network_bridge: vmbr0
```

Cloud-config managed network:

```yaml
networks:
- name: bosh-bridge
  type: manual
  managed: true
  cloud_properties:
    bridge: vmbr1
  subnets:
  - range: 10.201.0.0/24
    gateway: 10.201.0.1
```

### Notes

- Most deployments pre-configure networks and do not set `managed: true`. The `create_network` and `delete_network` handlers run only when the Director's cloud-config marks a network as managed.

- `pve.network_bridge` remains required for `create_vm` NIC attachment regardless of `network_mode`. It is the default bridge VMs attach to at boot.

- SDN changes are staged by the PVE API and committed by the CPI via a `PUT /cluster/sdn` apply call after each create or delete operation. This is PVE's two-phase commit model. On error, the CPI issues a rollback to clear any staged-but-unapplied changes.

- Zone auto-deletion (`sdn_auto_manage_zone: true`) is opt-in and disabled by default. When enabled, `delete_network` removes the zone only when all three conditions hold: `sdn_auto_manage_zone` is `true`, the zone name does not match `pve.sdn_zone` (the operator-pinned default zone is never auto-deleted), and the zone has zero remaining vnets after the vnet is removed. Leave `sdn_auto_manage_zone: false` unless the CPI should own the full zone lifecycle.

## MBus fallback

When `agent.mbus` is empty but a blobstore endpoint is configured, the CPI derives `nats://<blobstore-host>:4222` from the blobstore endpoint host and uses that as the agent's NATS URL. Explicit `agent.mbus` values always win. Loopback hosts (`127.0.0.1`, `localhost`, `::1`, `0.0.0.0`) are rejected — the MBus stays empty so the misconfiguration fails loudly instead of being silently misrouted to a non-routable URL.

This convention matches the typical BOSH topology where NATS and the DAV blobstore are colocated on the director (or on the create-env machine during initial bootstrap, when the director does not yet exist to advertise an MBus URL).

## Example

```yaml
properties:
  pve:
    host: pve.example.com
    port: 8006
    user: root
    realm: pam
    password: ((pve_password))
    node: pve1
    vm_storage: local-lvm
    disk_storage: local-lvm
    stemcell_storage: nfs-shared
    network_bridge: vmbr0
    verify_ssl: true
    agent_mode: cloudinit
    vm_disk_format: qcow2
    log_level: info
```

In the example above, `nfs-shared` is a PVE NFS storage pool with the `import` content type enabled and `shared=1`. Both `vm_storage` and `stemcell_storage` must be accessible from all cluster nodes when operating a multi-node cluster.

## Custom Tags

Operators may attach arbitrary tags to VMs and persistent disks via the `tags` cloud-property on `vm_types` and `disk_types`. Tags surface in the PVE UI for filtering, cost-allocation, ownership tracking, and ad-hoc grouping.

The `tags` cloud-property is a map of `key: value` pairs. Each pair is sanitized and emitted as a `<key>--<value>` entry in the PVE tags field (PVE allows only `[A-Za-z0-9-]` in tag values; other bytes are replaced with `-`). Multiple entries are joined with `;`.

Example cloud-config snippet:

```yaml
vm_types:
- name: tagged
  cloud_properties:
    cpu: 2
    memory: 1024
    tags:
      env: prod
      owner: platform-team

disk_types:
- name: small
  disk_size: 1024
  cloud_properties:
    tags:
      tier: bronze
```

Notes:

- Tags are sanitized: a key like `bad key` becomes `bad-key`, a value like `with spaces` becomes `with-spaces`.

- The combined tag string is capped at 255 bytes; entries past the cap are dropped at a `;` boundary so partial entries are never emitted.

- The CPI reserves three tag-key prefixes for its own use: `director--`, `deployment--`, and `job--`. These are rebuilt from BOSH-supplied metadata on every `set_vm_metadata` call. Custom tags survive those re-syncs.

- PVE has no native disk-volume tag field. Tags supplied on a `disk_type` are written to the tags field of the VM the disk is attached to and recorded in the VM description sentinel under `bosh_disk_tags`. Disk tags only become visible once the disk is attached to a VM; if `create_disk` is called without a `vm_cid` hint, the tags are deferred and applied on the next `set_disk_metadata` call.
