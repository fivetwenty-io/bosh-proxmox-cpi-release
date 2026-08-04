# HA and Resurrection

Two independent systems can restart a BOSH-managed VM that has stopped responding: PVE HA at the cluster level, and the BOSH resurrector at the Director level. Run both against the same VM and they race each other, producing a duplicate guest. This document defines who owns recovery by default, describes the race in detail, and covers the guard rail the CPI provides plus how to safely opt into PVE HA ownership instead.

## Ownership model

| | Recovery owner | VM registered in PVE `ha-manager`? | What you do |
|---|---|---|---|
| **Default (zero-config)** | BOSH resurrector | No | Nothing — leave the resurrector on (the Director default) |
| **Opt-in (any HA-registration feature active)** | PVE HA | Yes | Disable the resurrector for that deployment: `bosh update-resurrection off` |

With a stock configuration — no `placement.anti_affinity.use_ha_rules`, no `placement.pin_az_via_ha_rules`, no `placement.dlb`, and no `cloud_properties.pci_passthroughs` — the CPI never registers a VM as a PVE HA resource. Zero HA API calls are made. The BOSH resurrector is the sole recovery mechanism, and this is the correct, zero-configuration default: leave resurrection on.

Three configuration knobs opt a VM into PVE HA:

- `placement.dlb` (Dynamic Load Balancer rebalancing) — see [DLB-Aware Placement](dlb-aware-placement.md)
- `placement.anti_affinity.enabled` with `use_ha_rules: true` (PVE-enforced negative affinity between instance-group members)
- `placement.pin_az_via_ha_rules` with a non-empty `az_map` (a hard or preferred node-affinity pin keeping a VM's AZ durable across HA failover)

Any one of these registers the VM in `ha-manager`. The moment that happens, **you must disable the BOSH resurrector for that deployment**, via `bosh update-resurrection off` or `resurrector_enabled: false` in the Director manifest.

### Two prerequisites that are easy to miss

**`pin_az_via_ha_rules` also needs `cloud_properties.availability_zone` on every `vm_type`.** BOSH does not pass the cloud-config AZ *name* to `create_vm` — an AZ block selects which CPI runs and which subnet the VM gets, and nothing more. The CPI therefore learns a VM's AZ only from `cloud_properties.availability_zone`, and it must name an AZ that appears in `placement.az_map`. Setting `pin_az_via_ha_rules` and `az_map` without it yields an entirely inert feature: every VM is created with no HA registration and no pin. The CPI warns once per unpinned VM:

> `create_vm: HA node-affinity pin is enabled but this VM was not pinned — no availability_zone resolved for the placed node; BOSH does not pass the cloud-config AZ name to the CPI, so each vm_type needs cloud_properties.availability_zone set to an AZ named in placement.az_map`

A minimal `vm_type`:

```yaml
vm_types:
- name: default
  cloud_properties:
    cores: 2
    memory: 4096
    availability_zone: z1   # must be a key in pve.placement.az_map
```

**PVE HA recovers a guest only when it can establish that the failed node is gone.** Relocation waits on fencing confirmation. A node that keeps running with corosync stopped is never confirmed dead, so the cluster resource manager parks the service in the `fence` state and refuses to move it — the correct split-brain-safe refusal, but it means the guest stays down until an operator intervenes. Opting into PVE HA ownership therefore assumes a cluster that can actually fence: quorum plus a watchdog (`softdog` by default, or hardware/IPMI). Without that, disabling the BOSH resurrector removes the recovery mechanism that would have worked and replaces it with one that will not.

That refusal window is also the widest opening for the double-healing race described below: throughout it the guest is still running on the partitioned node while the cluster considers it lost, so a live resurrector would build its replacement against an original that was never actually down. [Observed timings](#observed-timings) measures a real instance — roughly six minutes.

**Shared storage is a third prerequisite.** A guest whose root disk (`pve.vm_storage`) or ConfigDrive ISO (`pve.iso_storage`) lives on node-local storage cannot start on any other node, so HA relocation always fails. Point both at a pool every node mounts before enabling any HA feature; the CPI's `checkISOStorageForHA` check and the DLB shared-storage guard exist to catch this, but they warn rather than block.

**One exception, and it is automatic.** A VM that declares `cloud_properties.pci_passthroughs` is always registered with a strict, single-node HA pin, with no config knob to opt out — PCI passthrough is incompatible with live migration, so the VM can never move off its assigned node regardless. This pin is deliberately excluded from the HA-registration warning below: a PCI-pinned VM is not expected to migrate, so the double-healing race this document otherwise addresses does not apply to it in the same way.

## The double-healing race

When a VM registered under PVE HA stops responding, both systems act independently and roughly concurrently:

1. The VM stops (crash, OOM, hypervisor issue).
2. PVE HA's cluster resource manager detects the failure and begins relocating the guest to another node.
3. The BOSH Director, seeing the original VM's agent go unresponsive on its heartbeat timeout, independently issues its own `create_vm` to replace it.
4. Both recoveries can complete: the PVE-HA-relocated original guest, and the Director-spawned replacement. They now conflict — on IP address, on VMID, or on agent credentials — and the resulting orphaned HA resources can block future operations against either VM.

Both systems act on similar timescales (each roughly a one-minute detection window, independently tuned — PVE HA's own failure-detection interval on one side, the Director's agent-heartbeat timeout on the other). Nothing in the CPI coordinates the two; they are entirely separate control loops with no shared state.

## The CPI's guard rail: a once-per-process warning

The CPI cannot read or set the BOSH resurrector's state — there is no PVE-API-only mechanism for that, and the CPI has no shell access to the Director. What it can do is warn whenever it is about to make PVE HA a co-owner of recovery. `create_vm` logs this once per CPI process, the first time any HA-registration feature fires:

> `create_vm: this VM is registered as a PVE HA resource; the BOSH resurrector must be disabled for HA-managed deployments, or PVE HA and the resurrector can both try to recover the same failed guest independently, producing a duplicate VM that conflicts on IP, VMID, or agent credentials -- run` `bosh update-resurrection off` `(or set` `resurrector_enabled: false` `in the Director manifest) for any deployment using` `placement.dlb, placement.anti_affinity.use_ha_rules, or placement.pin_az_via_ha_rules; see docs/ha-and-resurrection.md`

The warning fires once per process, not once per VM — later HA-registered `create_vm` calls in the same CPI process do not repeat it, even under a different combination of features. It always fires when any HA-registration feature is active, because the CPI has no way to know whether the resurrector has actually been disabled; it only knows that HA registration is about to happen.

## `has_vm` semantics on a dead node

`has_vm` answers from a cluster-wide resource scan (`GET /cluster/resources`), not from PVE HA status. A VM sitting on a node that has gone unresponsive is still listed in that scan — with a `status: unknown` — for as long as the cluster retains quorum, so `has_vm` reports `true` even though the guest is currently unreachable.

If the cluster loses quorum entirely, the scan itself fails, and that failure propagates as a retriable transport error rather than being interpreted as "the VM is gone." This fail-toward-retry direction is deliberate on both sides of the ownership split: it is what PVE HA recovery needs (a VM that still exists but is temporarily unreachable must never be treated as deleted), and it is what `bosh cloud-check` needs (a transient scan failure must not make the Director conclude a VM has disappeared and offer to recreate it).

## The small-node-set AZ pin wedge

`pve.placement.pin_az_strict` defaults to `true` once `pin_az_via_ha_rules` is enabled: the resulting node-affinity pin is a hard guarantee, and PVE HA will not relocate the VM off its assigned AZ node set even if every node in that set is simultaneously down. This is a deliberate trade-off — durability of AZ locality over availability — but it carries a real wedge risk on a small node set. The CPI warns when a strict pin targets an AZ with two or fewer nodes:

> `create_vm: strict AZ node-affinity pin targets a small node set; if every pinned node is simultaneously down or drained for maintenance, PVE HA has nowhere in the AZ left to fail over to and the VM stays down until a pinned node returns (map the AZ to >= 3 nodes to avoid this wedge risk, or set` `pve.placement.pin_az_strict=false` `to allow HA to relocate off-AZ instead)`

Two concrete triggers: every node in a one- or two-node AZ going down simultaneously, or one node down while the other is drained for maintenance — either leaves PVE HA with no legal placement target in that AZ, and the VM stays down until a pinned node returns. Mitigate by mapping each `az_map` entry to at least three nodes, or by setting `pin_az_strict: false` so HA can relocate off-AZ instead of wedging.

## DLB caveats

The Dynamic Load Balancer (`placement.dlb`) is a third HA-registering feature and carries its own prerequisites beyond the resurrector conflict above — see [DLB-Aware Placement](dlb-aware-placement.md) for the full guard ladder. The parts most relevant to HA ownership:

- **Shared storage required.** Live migration — the mechanism both DLB rebalancing and PVE HA recovery depend on — requires the VM's root disk, persistent disks, and its ConfigDrive ISO (which lives at `scsi30` for the VM's entire life, not just at boot) to all sit on storage reachable from every cluster node. `pve.iso_storage_follow_vm_storage` (see [Design Decisions — D7](design-decisions.md#d7--iso_storage-default-follow-vm_storage)) helps here by pointing the ISO pool at an already-shared `vm_storage` without a separate manifest edit. If any of the three pools resolves to node-local storage, the CPI silently skips DLB registration for that VM.
- **Multi-node cluster required.** PVE HA requires quorum, with three nodes as the recommended minimum. On a single-node cluster, the CPI detects the node count and skips all DLB registration silently — the feature is entirely inert there, and no resurrector conflict can arise.
- **The BOSH Director and bootstrap VMs must never be DLB- or HA-managed.** A `bosh create-env` Director VM that gets live-migrated mid-deploy loses its in-flight CPI RPC connections, and the deployment fails. Use a separate `cpi.json` for the `create-env` step that omits `placement.dlb` entirely — the config accessors return safe off defaults when the block is absent.

## Observed timings

Measured 2026-08-04 on a three-node PVE 9.2.4 cluster (`lab-pve-cpi-az2`), one BOSH instance with its root disk and its `scsi30` ConfigDrive ISO on shared NFS, polled once per second from a surviving node's API. Two kill mechanisms, because they produce materially different outcomes:

| Failure | Node offline | HA acts | VM running again | Recovered where |
|---|---|---|---|---|
| Abrupt crash (`sysrq-b`), node reboots in ~26 s | +8 s | restart in place | **+152 s** | same node |
| Sustained partition (corosync stopped), node stays up | +2 s | service parked in `fence` | **+245 s** | different node |

Neither run ever produced a second guest: exactly one VMID and one VM name throughout, and with the resurrector off the Director issued no scan-and-fix.

Three things are worth carrying forward from these numbers.

**A fast reboot never reaches HA failover.** The crashed node returned well inside the fence timeout, so the cluster resource manager never fenced it and never relocated the guest. Recovery was the returning node's own LRM restarting the VM after it reacquired the agent lock — and that lock wait, not the 26-second reboot, is most of the 152 seconds.

**Relocation needs a node that can actually be fenced.** In the partition run the node kept running with corosync stopped, so no fencing confirmation ever reached the CRM. The CRM correctly parked the service in the `fence` state and refused to move it — the textbook split-brain-safe refusal — and relocation only proceeded once we restored corosync by hand. The 245 seconds therefore measures operator-assisted recovery, not an unattended failover, and should not be quoted as one. The general point stands regardless of the mechanism: PVE HA recovers a guest only when it can establish that the failed node is gone.

**That refusal window is the widest opening for the double-healing race.** For roughly six minutes the guest was still running on the partitioned node while the cluster considered it lost. A live resurrector, working from the Director's own agent-heartbeat timeout, would have built its replacement squarely inside that window — against an original that was never actually down.

## See also

- [Design Decisions — D11](design-decisions.md#d11--ha-versus-the-bosh-resurrector-warn-document-and-race-test) — why we chose warn-and-document over attempting resurrector detection.
- [DLB-Aware Placement](dlb-aware-placement.md) — the full DLB guard ladder, CRS cluster-option interplay, and migration prerequisites.
- [ConfigDrive — migration and HA interaction](configdrive.md#migration-and-ha-interaction) — why the ConfigDrive ISO pool matters for migration and HA recovery.
