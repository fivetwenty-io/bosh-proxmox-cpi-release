---
layout: section
---

# Chapter 8
## How We Configure It

*Five properties are required; a few defaults protect us; everything else sleeps until asked — and upgrades change nothing uninvited.*

<!--
- Minutes 39–46. The reference documents 100+ properties; that number is designed to mislead. Five required, the rest inert — that ratio is the headline.
-->

---

## The five that matter

```yaml
properties:
  pve:
    host: pve.example.com          # which cluster
    user: bosh@pve                 # who we are
    api_token: ((pve_api_token))   # proof (or password)
    vm_storage: nfs-vms            # machine disks
    disk_storage: nfs-disks        # persistent disks
```

- Validated at the door — a bad config fails naming the field

<!--
- One namespace, pve.*, set in the Director manifest or the CPI config. In practice add node (default node) and network_bridge (defaults vmbr0).
- Whole document validated on every start, errors accumulated, malformed config rejected before any PVE connection — never twelve minutes into a deploy.
-->

---

## The credential is a sized liability

- Dedicated `bosh@pve` user + custom role — never root
- Every grant documented against the path it acts on
- Sharp edge: token needs **privilege separation off**
- Pool access checked at startup — missing grant fails loudly

<!--
- The role grants exactly what the CPI's operations use: VM management, storage allocation on configured pools, pool membership. Compromise = bounded loss.
- privsep trap: PVE tokens default to their own empty ACL; stemcell uploads fail confusingly until privsep=0. docs/pve-api-permissions.md has the exact commands in order.
- Startup preflight probes the pools and exits on 403 — fail-at-the-door, not mid-deploy.
-->

---

## Say it once, at the right altitude

```mermaid {scale: 0.75}
flowchart LR
    G["global defaults<br/>(pve.*)"] --> VT["vm_type profile"]
    VT --> DT["disk_type profile"]
    DT --> CP["per-call cloud_properties"]
    CP --> W["what actually happens"]
```

- The most specific voice wins
- Week to week: sizing, zones, an occasional storage override

<!--
- Globals: cluster truths (storage, bridge, VMID bands). Named vm_type/disk_type profiles: a workload class stated once. cloud_properties: this one VM — cores, memory, root_disk_size, availability_zone, target_node.
- Predictability is the point: overrides never surprise because specificity always wins.
-->

---

## Defaults that protect, defaults that sleep

- **Protective, on:** template cloning · placement scoring · duplicate-IP scan · parked disks · ballooning *off*
- **Sleeping, off:** telemetry, hooks, HA features, perf tuning…
- **Additive upgrades:** every switch arrives off; a default we conclude was wrong changes only with a changelog argument

<!--
- Ballooning off is the counterintuitive one worth saying aloud: BOSH sizes machines deterministically; a hypervisor quietly reclaiming memory produces OOM kills that masquerade as application bugs.
- The configuration reference's closing line: everything not required adds no privilege and no behavior until switched on.
- Additive upgrades is a maintained promise — new capabilities arrive as switches, every switch arrives off, and each feature's configuration tests prove the absent case valid. Upgrades become routine, not risk assessments.
-->
