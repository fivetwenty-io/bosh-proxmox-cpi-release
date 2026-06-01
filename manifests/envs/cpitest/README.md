# Env bundle: `cpitest` — isolated SDN test network

An isolated, CPI-owned network for CF (and other) deploy testing on a private
`172.x` range backed by a Proxmox SDN vnet. Selecting this env keeps the
Director and the deployment off any shared office/lab LAN, which eliminates the
duplicate-IP / ARP-ambiguity flapping described in
[`docs/troubleshooting.md`](../../../docs/troubleshooting.md) ("duplicate IP on
a shared LAN") and [`docs/networks.md`](../../../docs/networks.md) ("Isolated
test network (SDN)").

## Why

When the deployment subnet overlaps a physical LAN, an address BOSH assigns to
a VM can collide with a printer, laptop, or appliance already using it. Two
MACs answer ARP, the Director's ARP cache flaps, and mbus packets are
periodically delivered to the wrong host — which RSTs them. Agents then loop
`connection reset by peer` → reconnect, and large deploys fail random instances
with `Timed out sending 'get_state'`. A dedicated network BOSH fully owns
removes the collision at the source.

## Files

- `vars.yml` — the whole network spec (vnet, zone, CIDR, gateway, reserved
  bands, static IPs, Director/HAProxy IPs). **Edit this one file to reshape the
  network.** Overrides the matching keys in `manifests/bosh/vars.yml`.
- `cf-cloud-config.yml` — ops layer that points the CF cloud-config subnet at
  the values in `vars.yml`. Auto-layered by `scripts/cf` under this env.

The Director network needs no ops file: it is driven entirely by
`internal_cidr` / `internal_gw` / `internal_ip` / `pve_network_bridge`, which
`vars.yml` overrides.

## Usage

```bash
# 1. Create the SDN zone + vnet + subnet on the PVE host (idempotent).
./scripts/bosh net-up

# 2. Deploy the Director onto the isolated network.
BOSH_PVE_ENV=cpitest ./scripts/bosh create-env
BOSH_PVE_ENV=cpitest ./scripts/bosh alias-env

# 3. Upload the cloud-config and deploy CF onto the same network.
BOSH_PVE_ENV=cpitest ./scripts/cf deploy

# Inspect / tear down the SDN objects.
./scripts/bosh net-status
./scripts/bosh net-down       # after delete-env + cf teardown
```

`net-up` / `net-down` / `net-status` read the same `vars.yml`, so the SDN
objects and the BOSH manifests always agree.

## Customizing

To run a different layout (different range, vnet name, or reserved/static
bands), copy this directory and edit the copy:

```bash
cp -r manifests/envs/cpitest manifests/envs/myenv
$EDITOR manifests/envs/myenv/vars.yml
BOSH_PVE_ENV=myenv ./scripts/bosh net-up
```

Keep these invariants when editing `vars.yml`:

1. `pve_network_bridge` **equals** `cpitest_sdn_vnet` (the vnet name is the
   bridge VMs attach to).
2. `internal_cidr` **equals** `cpitest_sdn_subnet`.
3. `internal_gw` **equals** `cpitest_sdn_gateway` and is the subnet gateway.
4. Every `cpitest_reserved` / `cpitest_static` entry, plus `internal_ip` and
   `haproxy_private_ip`, lies inside `internal_cidr`.

Proxmox limits: zone name ≤ 8 chars; vnet name ≤ 8 chars and starts with a
letter.
