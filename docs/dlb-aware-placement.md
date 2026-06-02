# DLB-Aware Placement

This document covers opt-in integration between the BOSH PVE CPI and the PVE 9.2 Dynamic Load Balancer (DLB).

---

## Overview

PVE 9.2 introduced a Cluster Resource Scheduler (CRS) dynamic mode that, when enabled cluster-wide, continuously rebalances **HA-managed guests** across cluster nodes based on load. The relevant cluster settings are:

- `ha=dynamic` — enables the dynamic scheduling mode
- `ha-rebalance-on-start=1` — rebalances on VM start
- `ha-auto-rebalance=1` — enables ongoing automatic rebalance

The key constraint: CRS/DLB acts **only on HA-managed guests**. An ordinary VM that is not registered as a PVE HA resource is invisible to the DLB.

This CPI feature bridges the gap. When enabled, the CPI registers newly created BOSH VMs as PVE HA resources with `auto-rebalance=1` and `state=started`, making them eligible for DLB placement and ongoing rebalancing. The feature is **off by default** and requires explicit operator opt-in.

---

## Two Opt-In Mechanisms

### Master Flag — `pve.placement.dlb.enabled`

Setting `pve.placement.dlb.enabled: true` in the BOSH job properties marks **every VM** created by this CPI instance for DLB registration.

This flag works **alongside** your existing availability zone topology. VMs that belong to a configured AZ (via `pve.placement.az_map`) are still initially placed within that AZ's candidate node set; DLB then rebalances them within those allowed nodes afterward. The master flag does not require collapsing or removing your AZ configuration.

### Sentinel Availability Zone — `pve.placement.dlb.az_name`

The sentinel AZ provides per-workload opt-in without changing the global flag. Any VM whose `cloud_properties.availability_zone` matches the sentinel name (default: `"dlb"`) is treated as DLB-delegated:

- The CPI skips its own AZ-map lookup (the sentinel name is not expected to be in `az_map`).
- All online nodes become candidates for the initial placement.
- The VM is registered as a PVE HA resource with `auto-rebalance=1` so CRS/DLB places and rebalances it.

To disable the sentinel entirely, set `pve.placement.dlb.az_name: ""`. With the sentinel disabled, only the master flag can trigger DLB.

### Cloud-Config Example

The following cloud-config fragment shows a normal AZ (`z1`, scored by the CPI against a fixed node set) alongside the DLB sentinel AZ. Both can coexist in the same deployment.

```yaml
azs:
  - name: z1
    cloud_properties:
      availability_zone: z1   # must be in pve.placement.az_map

  - name: dlb-zone
    cloud_properties:
      availability_zone: dlb  # matches pve.placement.dlb.az_name default
```

In the CPI job properties:

```yaml
pve.placement.az_map:
  z1: [pve01, pve02]
# pve.placement.dlb.az_name defaults to "dlb" — no override needed
```

VMs in `z1` are scored and placed on `pve01` or `pve02`. VMs in `dlb-zone` are placed on any online node and rebalanced by the DLB.

---

## Configuration Reference

| Property | Type | Default | Effect |
|---|---|---|---|
| `pve.placement.dlb.enabled` | bool | `false` | Master flag — register all VMs for DLB. Works alongside AZ topology. |
| `pve.placement.dlb.az_name` | string | `"dlb"` | Sentinel AZ name. VM with this AZ value is DLB-delegated even when `dlb.enabled` is false. Set to `""` to disable the sentinel. |
| `pve.placement.dlb.manage_cluster_crs` | bool | `false` | When true, the CPI writes the cluster CRS setting automatically. When false (default), the CPI reads and warns. See [Required Cluster Setup](#required-cluster-setup). |
| `pve.placement.dlb.require_shared_storage` | bool | `true` | When true (default), VMs on local storage are silently skipped for DLB registration. Set false only when all VMs are guaranteed to use shared storage. |
| `pve.placement.anti_affinity.strict` | bool | `false` | When true, PVE HA negative-affinity rules are set to strict (hard) mode. See [Anti-Affinity Interaction](#interaction-with-placement-scorer-and-anti-affinity). |

---

## Required Cluster Setup

DLB rebalancing only runs when the PVE cluster is in CRS dynamic mode. This is a **one-time admin step** outside the scope of the CPI by default.

Run the following on a cluster node (or via a management host with `pvesh`):

```bash
pvesh set /cluster/options \
  --crs ha=dynamic,ha-rebalance-on-start=1,ha-auto-rebalance=1
```

### `manage_cluster_crs` Knob

**Default behavior (`manage_cluster_crs: false`):** The CPI reads `/cluster/options` each time a DLB-eligible VM is created. If the cluster is not in dynamic mode, the CPI logs a warning with the corrective `pvesh` command and continues. DLB rebalancing will be inactive until the operator applies the setting.

**Opt-in automation (`manage_cluster_crs: true`):** The CPI calls `UpdateOptions` to set `crs=ha=dynamic,ha-rebalance-on-start=1,ha-auto-rebalance=1` automatically. Use this with caution: writing the cluster CRS option is a **cluster-wide change** that affects every HA-managed guest, not only BOSH VMs.

---

## Safety Prerequisites

Read these carefully before enabling DLB. Each item describes a condition that can cause production failures if ignored.

### BOSH Resurrector Must Be Disabled

**The BOSH resurrector must be turned off for any deployment that uses DLB or HA-rule registration.** Both PVE HA and the BOSH resurrector independently detect that a VM is stopped and restart it. When both are active simultaneously:

- A VM that stops triggers both PVE HA and the BOSH resurrector.
- PVE HA restarts the guest on a different node; the BOSH Director sees the original VM ID as unresponsive and issues a `create_vm`, producing a duplicate.
- The original HA-managed guest and the Director-spawned replacement can conflict on IP, VMID, or agent credentials, leaving orphaned HA resources that block future operations.

The CPI has no way to detect the resurrector's state — this is entirely the operator's responsibility. Disable the resurrector via:

```yaml
# In bosh-deployment / manifest resurrector property
resurrector_enabled: false
```

Or through the BOSH CLI:

```bash
bosh update-resurrection off
```

### Shared Storage Required

Live migration — the mechanism the DLB uses to rebalance VMs — requires the VM's root disk to reside on storage that is accessible from all cluster nodes (rbd, nfs, cifs, glusterfs, or cephfs).

VMs on node-local storage (dir, lvm, lvmthin, zfspool) cannot be live-migrated. With `pve.placement.dlb.require_shared_storage: true` (the default), the CPI checks both the VM root pool (`pve.vm_storage`) and the persistent disk pool (`pve.disk_storage`); if either is node-local, the CPI silently skips DLB registration for that VM, logging a debug entry. This prevents PVE from attempting an impossible migration.

If the storage type cannot be determined from the PVE API at create time, the CPI fails open (proceeds with registration) and logs a debug entry.

### Multi-Node Cluster Required

PVE HA requires quorum. The recommended minimum is three nodes. On a single-node cluster, the CPI detects the node count and skips all DLB registration silently — the feature is entirely inert.

### SDN Networking Preserves IP and MAC Across Migration

When PVE live-migrates a VM, the guest retains its IP address and MAC. Your network configuration must support this:

- Use PVE SDN (configured as described in the CPI SDN networking documentation) or another network fabric that does not tie MAC-to-port entries to a specific physical host.
- Static IP assignments in BOSH cloud-config must remain valid cluster-wide, not only on the VM's initial node.

### Director and Bootstrap VMs Must Not Be DLB-Managed

The BOSH Director VM (created via `bosh create-env`) must never be registered with DLB or PVE HA. If the Director is live-migrated mid-deploy, in-flight CPI RPC connections are dropped and the deployment fails. The standard approach is to use a separate `cpi.json` for the `create-env` step that does not include any `placement.dlb` properties — the Go config accessor returns safe OFF defaults when the `placement.dlb` block is absent.

### Large VMs and Migration Pause

VMs with large working-set memory (approximately 16 GiB or more of dirty pages) incur a significant migration pause. During that pause the guest is suspended briefly. If the pause exceeds the BOSH agent heartbeat timeout, the Director may declare the VM unhealthy and trigger recovery. Size VMs appropriately and, for latency-sensitive workloads, consider excluding them from DLB by not using the DLB sentinel AZ and leaving `dlb.enabled: false`.

---

## Interaction with Placement Scorer and Anti-Affinity

### Normal AZ Topology with Master Flag

When `pve.placement.dlb.enabled: true` and the VM also belongs to a configured AZ (via `pve.placement.az_map`), the CPI still restricts initial placement to that AZ's candidate node set. The node scorer runs normally within that set. After the VM is created, DLB is permitted to rebalance it, but only within nodes that PVE HA considers valid — effectively the same node set that the AZ map defines.

### Sentinel AZ

When a VM's `availability_zone` matches the sentinel name and the sentinel name is not in `az_map`, the CPI uses all online nodes as candidates. The scorer runs across the full cluster and picks an initial landing node. DLB then rebalances freely across the cluster.

### Anti-Affinity Interaction

The CPI's existing anti-affinity feature (`pve.placement.anti_affinity.*`) spreads same-instance-group VMs across nodes using PVE HA negative-affinity rules. DLB may override that spreading under load: CRS/DLB sees resource pressure and migrates a VM onto a node that already hosts a sibling, because the DLB's balancing objective can conflict with anti-affinity constraints.

To reduce this divergence:

- Use `pve.placement.anti_affinity.use_ha_rules: true` alongside DLB. This encodes anti-affinity as PVE-level negative HA resource-affinity rules, giving the scheduler a formal constraint rather than only a scored preference.
- Optionally set `pve.placement.anti_affinity.strict: true` to make those rules hard constraints. **Warning:** strict mode on a cluster with two or three nodes can prevent PVE from evacuating a faulting node when no compliant destination exists. Only use strict mode on clusters large enough to always have a node that satisfies every active constraint simultaneously.

### Delete Cleanup

On `delete_vm`, the CPI removes the VM from any PVE HA resource and prunes associated affinity rules. This cleanup runs whenever `anti_affinity.use_ha_rules` is enabled **or** `placement.dlb` is configured (master flag or non-empty sentinel name). It is best-effort and never blocks VM deletion.

---

## Single-Node and Pre-9.2 Behavior

On a **single-node cluster**, the CPI detects that there is only one node, logs a debug entry, and skips DLB registration. The VM is created normally. No errors are returned and `create_vm` is not affected.

On a **PVE version older than 9.2**, the CPI parses the node version string, logs a debug entry, and skips DLB registration. Again, VM creation proceeds normally. HA affinity rules (anti-affinity via `use_ha_rules`) continue to work on older PVE versions; only the DLB-specific `auto-rebalance` registration is skipped.

In both cases, the feature is silently inert. Operators can enable the DLB properties in their manifests and deploy to mixed-version or single-node clusters without errors; the DLB path does nothing until the prerequisites are met.
