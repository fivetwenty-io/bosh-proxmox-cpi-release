# Network Management

This CPI supports two network backends for BOSH managed networks: PVE SDN vnets and Linux bridges. When a BOSH cloud-config marks a network as `managed: true`, the Director calls `create_network` to provision the resource and `delete_network` to remove it. All other networks (pre-configured bridges, static VLANs) operate with `managed: false` and are never touched by the network lifecycle handlers.

## SDN vs Bridge Routing

The handler selects a backend based on three inputs: the `network_mode` config property, the `zone` field in `cloud_properties`, and the CPI config `sdn_zone`. The routing logic runs in this order:

1. If `network_mode` is `"sdn"` → SDN path (unconditional; error if zone unresolvable).
2. If `network_mode` is `"bridge"` → bridge path (unconditional; error if bridge unresolvable).
3. If `network_mode` is `"auto"` (the default):
   - If `cloud_properties.zone` is set OR `config.sdn_zone` is set OR `cloud_properties.vnet` is set → SDN path.
   - Otherwise → bridge path (requires `cloud_properties.bridge` or `config.network_bridge`).
4. If neither path is resolvable → `cpierrors.Cloud` returned to the Director.

### Routing decision table

| `network_mode` | `cloud_properties.zone` | `config.sdn_zone` | `cloud_properties.bridge` / `config.network_bridge` | Outcome |
|---|---|---|---|---|
| `sdn` | any | any | any | SDN path |
| `bridge` | any | any | any | Bridge path |
| `auto` | set | any | any | SDN path |
| `auto` | empty | set | any | SDN path |
| `auto` | empty | empty | set | Bridge path |
| `auto` | empty | empty | empty | Error: no routing info |

For `delete_network`, the handler probes the SDN backend first (GET vnet by name). If the vnet exists, it takes the SDN delete path. If PVE reports the vnet absent (`ErrSDNNotFound`), it falls back to the bridge delete path. Any other probe error is returned to the Director.

## cloud_properties schema

These keys are read from the per-network `cloud_properties` block in the BOSH cloud-config.

| Key | Type | Required | Meaning |
|---|---|---|---|
| `zone` | string | SDN path only, when `config.sdn_zone` is empty | PVE SDN zone name. Takes precedence over `config.sdn_zone`. |
| `zone_type` | string | no | Zone type to use when the CPI creates the zone (requires `sdn_auto_manage_zone: true`). One of: `simple`, `vlan`, `qinq`, `vxlan`, `evpn`. Falls back to `config.sdn_zone_type` (default `simple`). |
| `vnet` | string | SDN path | PVE SDN vnet name. Must match `[a-z0-9]+` and be at most 8 characters (PVE constraint). |
| `bridge` | string | Bridge path only, when `config.network_bridge` is empty | Linux bridge interface name on the target node (e.g. `vmbr1`). |
| `node` | string | Bridge path only, when `config.node` is empty | PVE node where the bridge is created or deleted. |

### Vnet naming rules

PVE enforces a strict naming constraint on vnet identifiers: the name must match `[a-z0-9]+` (lowercase alphanumeric only, no hyphens or underscores) and must be at most 8 characters long. The CPI validates this constraint before calling the SDN API and returns an error to the Director if the name is invalid. Choose short, lowercase names such as `boshvn`, `cf1net`, or `prodnet`.

## Zone auto-management

By default (`sdn_auto_manage_zone: false`), the CPI manages only vnets and subnets within an existing zone. The operator creates and deletes zones through the PVE UI or API. The CPI returns an error if `create_network` is called with a zone that does not exist in PVE.

When `sdn_auto_manage_zone: true`, the CPI may create and delete zones autonomously:

**Auto-create:** If the zone named in `cloud_properties.zone` or `config.sdn_zone` does not exist in PVE at `create_network` time, the CPI creates it using the zone type from `cloud_properties.zone_type` or `config.sdn_zone_type` (default `simple`). The CPI does not invent zone names — a name must always be supplied.

**Auto-delete:** At `delete_network` time, the CPI removes the parent zone only when **all three** conditions hold:

1. `sdn_auto_manage_zone` is `true`.
2. The zone name does not match `config.sdn_zone` (the operator-pinned zone is never auto-deleted; it may be shared across multiple managed networks).
3. The zone has zero remaining vnets after the vnet is removed (confirmed by listing vnets filtered by zone before deleting).

If any condition fails, the zone is left in place. A failure to list vnets for the zone-empty check is treated as a reason to skip deletion, not as an error.

**PVE constraint:** The SDN zone create API (`POST /cluster/sdn/zones`) does not accept description, notes, or comment fields. CPI-owned zones are not annotated in PVE; tracking which zones belong to the CPI is done through the stateless config rule (condition 2 above).

## Manifest examples

### Example 1 — SDN with managed zone

The CPI manages the zone lifecycle. The zone `boshzone` is created on first `create_network` call and deleted when the last vnet in it is removed.

BOSH manifest CPI properties:

```yaml
properties:
  pve:
    host: pve.example.com
    user: root@pam
    api_token: root@pam!bosh=((pve_api_token))
    node: pve1
    vm_storage: local-lvm
    disk_storage: local-lvm
    network_bridge: vmbr0
    network_mode: sdn
    sdn_zone: boshzone
    sdn_zone_type: simple
    sdn_auto_manage_zone: true
```

Cloud-config managed network:

```yaml
networks:
- name: bosh-sdn-net
  type: manual
  managed: true
  subnets:
  - range: 10.200.0.0/24
    gateway: 10.200.0.1
    cloud_properties:
      zone: boshzone
      vnet: boshvn
```

The CPI will:

1. Check whether zone `boshzone` exists; create it (type `simple`) if absent.
2. Create vnet `boshvn` in zone `boshzone` (idempotent on conflict).
3. Create subnet `10.200.0.0/24` with gateway `10.200.0.1` on the vnet.
4. Apply the SDN configuration (`PUT /cluster/sdn`).
5. Return network CID `boshvn`, address properties, and `cloud_properties` containing `zone`, `vnet`, and `bridge` (all set to `boshvn`, since PVE simple-zone vnets are realized as a Linux bridge of the same name).

### Example 2 — Bridge fallback

No SDN required. The CPI creates a Linux bridge on the target node.

BOSH manifest CPI properties:

```yaml
properties:
  pve:
    host: pve.example.com
    user: root@pam
    api_token: root@pam!bosh=((pve_api_token))
    node: pve1
    vm_storage: local-lvm
    disk_storage: local-lvm
    network_bridge: vmbr0
    network_mode: bridge
```

Cloud-config managed network:

```yaml
networks:
- name: bosh-bridge-net
  type: manual
  managed: true
  subnets:
  - range: 10.201.0.0/24
    gateway: 10.201.0.1
    cloud_properties:
      bridge: vmbr1
      node: pve1
```

The CPI will:

1. Call `POST /nodes/pve1/network` to create bridge `vmbr1` (idempotent on 409 conflict).
2. Call `PUT /nodes/pve1/network` to reload the node network config.
3. Return network CID `vmbr1`, address properties, and `cloud_properties` containing `bridge` and `node`.

## Isolated test network (SDN)

For deploy testing — especially CloudFoundry, where dozens of VMs are placed at
once — never share an L2 segment with unmanaged devices. If the deployment
subnet overlaps a physical office or lab LAN, an address BOSH assigns to a VM
can collide with a device already using it; two MACs then answer ARP, the
Director's ARP cache flaps, mbus packets are misdelivered, and agents loop
`connection reset by peer` → reconnect, failing random instances with
`Timed out sending 'get_state'`. See
[Troubleshooting — duplicate IP on a shared LAN](troubleshooting.md#agent-never-comes-up).

This repo ships a turnkey isolated network as a PVE SDN **simple** zone + vnet +
subnet on a private `172.x` range. Selecting it moves both the Director and the
deployment onto a network BOSH fully owns, so no foreign device can claim an
address.

```bash
# 1. Create the SDN zone + vnet + subnet on the PVE host (idempotent).
./scripts/bosh net-up

# 2. Deploy the Director onto the isolated network.
BOSH_PVE_ENV=cpitest ./scripts/bosh create-env
BOSH_PVE_ENV=cpitest ./scripts/bosh alias-env

# 3. Upload the cloud-config and deploy CF onto the same network.
BOSH_PVE_ENV=cpitest ./scripts/cf deploy

# Inspect / tear down.
./scripts/bosh net-status
./scripts/bosh net-down        # after delete-env + cf teardown
```

`net-up` creates a simple zone (a plain local bridge — an isolated L2 segment
plus a gateway), one vnet whose name becomes the Linux bridge VMs attach to, and
a subnet with `snat` enabled so VMs reach the internet via the PVE host's uplink
while staying off the shared LAN. It commits with `pvesh set /cluster/sdn` and is
idempotent.

Everything is operator-configurable in a single file —
[`manifests/envs/cpitest/vars.yml`](../manifests/envs/cpitest/vars.yml) — which
defines the vnet name, zone, CIDR, gateway, reserved bands, and static IPs. The
same `vars.yml` drives both the SDN objects (`net-up`) and the BOSH manifests
(Director network + CF cloud-config), so they always agree. To run a different
layout, copy `manifests/envs/cpitest/` to `manifests/envs/<name>/`, edit the
copy, and drive it with `BOSH_PVE_ENV=<name>`. Keep these invariants:
`pve_network_bridge` == `cpitest_sdn_vnet`, `internal_cidr` ==
`cpitest_sdn_subnet`, `internal_gw` == `cpitest_sdn_gateway`, and every
reserved/static/host IP inside `internal_cidr`. Proxmox limits zone and vnet
names to 8 characters; vnet names must start with a letter. See
[`manifests/envs/cpitest/README.md`](../manifests/envs/cpitest/README.md).

## Cross-references

- [Configuration Reference](configuration.md) — full property table including `network_mode`, `sdn_zone`, `sdn_zone_type`, and `sdn_auto_manage_zone`.
- [Operations Guide](operations.md) — operational guidance for SDN setup, troubleshooting apply failures, and bridge cleanup.
- [Troubleshooting](troubleshooting.md#agent-never-comes-up) — duplicate-IP / ARP-ambiguity agent flapping and the isolated-network fix.
