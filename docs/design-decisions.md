# Design Decisions

This is the operator-facing record of the design decisions behind the pre-release stemcell, storage, network, and multi-cluster hardening pass. For each decision we describe the problem, the options we weighed, what we shipped, what changes for you as an operator, and how to migrate.

**This is a pre-release cutover.** None of the changes below preserve backward compatibility with CIDs, template layouts, or config defaults from earlier builds. If you have an existing deployment, delete it and redeploy fresh with the new CPI — there is no decode path for old stemcell CIDs, no compatibility shim for the old defaults, and no partial-upgrade path. Treat every environment as greenfield.

## Summary

| # | Decision | Choice |
|---|---|---|
| D1 | Stemcell CID architecture | Path-identity CIDs (`:light:`/`:heavy:`) naming the qcow2 file directly; the template VM becomes a lazily-reused per-cluster cache, not the identity |
| D2 | VM-creation strategy | `pve.stemcell_strategy: template \| import`, default `template`, with a per-stemcell override |
| D3 | Stemcell refcounting | Per-director reference sets keyed by BOSH director UUID, stored in the cache template's provenance |
| D4 | Disk CIDs | Kept the `pvd-`/`pvz-` envelope; shipped a first-class `pve-cid` operator tool; fixed a sentinel gap |
| D5 | `network_mode` default | `bridge` (plain Linux bridges, zero SDN prerequisites); SDN is a one-line opt-in |
| D6 | Resource pools | Kept default-on (`bosh`/`bosh-templates`); added a startup preflight that fails fast on a missing grant |
| D7 | `iso_storage` default | Follows `vm_storage` when eligible, instead of always defaulting to node-local `local` |
| D8 | Copy-vs-move / storage identity | Full backing-identity normalization: two storage IDs pointing at the same export are recognized as the same storage at every point the CPI compares two storage identities |
| D9 | Cross-cluster delete safety | `pve.destroy_unreferenced_disks` now defaults to `false` |
| D10 | qcow2 deletion policy | `:light:` files are never deleted by the CPI; `:heavy:` files are deleted only at the last director reference within a cluster |
| D11 | HA versus the BOSH resurrector | An active, once-per-process warning plus a dedicated ownership doc; no change to `has_vm` behavior |
| D12 | Per-request cloud-property overrides | BOSH's nested cpi-config context flattened into a closed `pve_*` override registry, with every overridable field in the client-bundle cache key |
| D13 | Stable disk identity and ownership transfer | A drive `serial=` token becomes the disk's stable identity; ownership transfers by `move_disk` reassignment; the envelope volid becomes a birth record with legacy fallback forever |

---

## D1 — Stemcell CID architecture: path identity, template as cache

**Context.** The previous design made a PVE template VM's own identifier the stemcell's CID. That tied a stemcell's identity to a specific, per-node PVE object, which caused a cluster of related problems: a stemcell couldn't be shared across two BOSH directors or two PVE clusters without ambiguity about which template "was" the stemcell, dedup logic and delete logic could disagree about what templates existed, and there was no natural place to record which directors still depended on a given template.

**Options considered.**

1. Keep the template-VMID CID and bolt refcounting onto it.
2. An opaque, content-hash-only CID with a separate lookup table mapping hash to file.
3. A path-identity CID that names the qcow2 file directly, with an explicit kind discriminator, where the template VM becomes a lazily built, per-cluster cache.

**Chosen: option 3.** A stemcell CID is now one of:

```
:light:<storage>:import/<file>
:heavy:<storage>:import/<file>
```

`:light:` marks an operator-managed qcow2 — the operator placed the file (typically via `cloud_properties.image_id`) and the CPI never deletes it. `:heavy:` marks a CPI-uploaded or CPI-downloaded qcow2 (tarball upload, `image_url` fetch, or server-download) — the CPI owns its lifecycle and deletes it under the policy in D10.

The leading colon is deliberate: no PVE storage identifier can begin with `:`, so a `:light:`/`:heavy:` CID can never be confused with a raw `<storage>:<path>` volume ID — including a storage pool literally named `light` or `heavy`.

### CID family reference

The CPI emits and accepts six CID shapes in total:

| Family | Format | Meaning |
|---|---|---|
| Stemcell (operator-managed) | `:light:<storage>:import/<file>` | Operator-managed qcow2; the CPI never deletes the file |
| Stemcell (CPI-managed) | `:heavy:<storage>:import/<file>` | CPI-uploaded qcow2; deleted at the last director reference within a cluster |
| Persistent disk | `pvd-<base64url(json)>` | Disk envelope — see [D4](#d4--disk-cids-keep-the-envelope-ship-a-tool) |
| Persistent disk (compressed) | `pvz-<base64url(gzip(json))>` | Compressed disk envelope, used when the plain form would exceed the Director's CID column width |
| VM | `<vmid>` | Bare integer |
| Snapshot | `<vmid>:<name>` | VMID plus PVE snapshot name |

### The refs-anchor rule

Every `create_stemcell` call builds (or reuses) the per-cluster cache template, tagged `bosh-stemcell-cache`, regardless of which `stemcell_strategy` you have configured. This is deliberate, not an oversight: the cache template's provenance JSON is also where the set of referencing director UUIDs lives (D3). If the template were skipped for `strategy: import`, two import-strategy directors registering the same `:heavy:` stemcell would have no shared place to record their references, and the first director to delete the stemcell would remove the qcow2 out from under the second. So `stemcell_strategy` only governs how `create_vm` materializes a VM's root disk (clone from the cache versus import the qcow2 directly); it never determines whether the cache/anchor template gets built.

**Operator-visible consequences.** Stemcell CIDs recorded in the Director database change shape — the template VMID is never part of the CID and is never exposed to BOSH. `pve-cid decode <cid>` (see D4) understands the new grammar. A stemcell uploaded once as `:light:` on storage shared by multiple clusters can be consumed by every cluster from that one file.

**Migration.** Pre-cutover stemcells, templates, and any deployment referencing them must be deleted before redeploying. There is no decode path for the old template-VMID or bare-integer CID forms — a `delete_stemcell` call against one is a hard parse error.

---

## D2 — VM-creation strategy: `template` default, `import` opt-in

**Context.** Cloning from a cached template is fast (copy-on-write) but ties every clone to that template's lifecycle. Some deployments need each VM's root disk to be a fully independent copy that never shares a base volume with anything else — a storage-backend constraint, or a compliance requirement.

**Options considered.** Ship only the clone-from-cache path; ship only the direct-import path; ship both, with a global default and a per-VM override.

**Chosen: both, with `template` as the default.** `pve.stemcell_strategy` accepts `template` (clone the per-cluster cache — the fast path, and the default) or `import` (import-from the stemcell qcow2 directly — slower, but the resulting VM shares no base volume with any other clone). A per-stemcell `cloud_properties.stemcell_strategy` overrides the global default.

**Implementation note.** `create_vm` never builds a template itself — that stays `create_stemcell`'s job. If `stemcell_strategy: template` is in effect and the cluster-scoped cache-template lookup finds no match for the stemcell's content hash (a manually deleted cache, or an unextractable hash from the CID's filename), `create_vm` logs a warning and falls back to `strategy: import` for that one VM rather than failing or attempting to rebuild the cache inline. The same fallback also covers a cache template that disappears mid-flight — if the clone call itself fails because the source template was deleted between lookup and clone, `create_vm` logs a warning and imports directly rather than failing the VM.

**Operator-visible consequences.** No change for the common case — clone-from-cache remains the default and behaves as before. Set `stemcell_strategy: import` (globally or per stemcell) when you need import semantics.

**Migration.** None beyond the general cutover — this knob's default direction is unchanged from before the pass.

---

## D3 — Stemcell refcounting: per-director reference sets

**Context.** Two BOSH directors (for example, a management director and an environment director) commonly share one PVE cluster and can both depend on the same stemcell. Without per-director tracking, either director's `delete_stemcell` could destroy a cache template the other director still needs. A related bug (now fixed) cleared the reference record *before* attempting the destroy — if the destroy then failed, the template was left both ref-less and undeletable.

**Options considered.** No refcounting at all (first delete wins, unsafe for the two-director pattern); a CID-keyed reference count; a set of BOSH director UUIDs stored on the template itself, keyed off the director UUID BOSH already sends on every JSON-RPC request context.

**Chosen: the director-UUID set.** Every cache template's provenance JSON carries `director_refs`, a set (not a counter — duplicate registrations from the same director are idempotent) of the director UUIDs currently depending on it. A request that arrives with no director UUID in its context is recorded under a single reserved "unknown director" entry rather than being dropped, so an unattributed `create_stemcell` still holds a reference. `create_stemcell` registers the calling director's UUID on every return path, fresh build or dedup hit alike. `delete_stemcell` removes the calling director's UUID; the template (and, for `:heavy:`, the qcow2) is destroyed only when that removal empties the set.

**The trapdoor fix.** When a delete empties the reference set, the CPI destroys the template *first* — the provenance is never separately rewritten to "empty" beforehand. If the destroy fails (for example, PVE reports the template still backs a linked clone), the template's provenance is untouched and a `bosh-destroy-pending` tag marks it so a retried `delete_stemcell` resumes the destroy directly instead of re-deriving "was this the last reference".

**Operator-visible consequences.** Two directors sharing a cluster can each run `delete_stemcell` independently without racing each other's dependency — the template and qcow2 survive until the last director releases its reference. A destroy blocked by a linked clone returns a clear, non-retriable error naming the template and instructing you to delete or migrate the dependent VM first; retrying afterward resumes the destroy rather than re-running reference logic.

**Migration.** Director UUIDs are stamped unconditionally into every new template's tags and provenance from the first post-cutover `create_stemcell`. Pre-cutover templates carry no such data and must not survive into the redeployed environment.

---

## D4 — Disk CIDs: keep the envelope, ship a tool

**Context.** The stemcell CID grammar needed a redesign (D1); the persistent-disk CID envelope did not. The `pvd-`/`pvz-` base64url envelope had already proven itself against BOSH Director REST-routing quirks and the classic MySQL Director's `varchar(255)` CID column limit. Redesigning it alongside the stemcell grammar would have been unnecessary churn.

**Options considered.** Keep the envelope unchanged; move to a human-readable disk CID grammar (explicitly out of scope — see [Out of scope](#out-of-scope) below); keep the envelope and add first-class tooling around it.

**Chosen: keep the envelope, delete every legacy decode branch, ship `pve-cid`.** The envelope format is unchanged. Every garbage-tolerant legacy decode arm — a `|`-separator branch, colon-escape special cases for storage names that happened to look like other CID shapes — is deleted; an unrecognized prefix is now a hard parse error rather than a silently-swallowed pass-through. `pve-cid` (`src/pve_cpi/cmd/pve-cid`) is a new, read-only Go CLI that ships in the release, installed on the Director VM at `/var/vcap/packages/pve_cpi/bin/pve-cid` (not on `PATH` by default):

| Subcommand | Purpose |
|---|---|
| `pve-cid decode <cid>` | Decode any CID family offline — no PVE API calls |
| `pve-cid encode --volid ...` | Build a `pvd-`/`pvz-` envelope offline |
| `pve-cid locate <cid\|volid>` | Online: find the holder VM for a disk CID, or the cache templates and director references for a stemcell CID |
| `pve-cid stemcells [--orphans]` | Online: per-cluster stemcell inventory — qcow2 files against cache templates against director references, the "what is safe to delete" view |

`pve-cid` never mutates PVE; every online subcommand is read-only.

**A residual gap closed alongside the tool.** VM disks attached inline during `create_vm` (as opposed to attached later via `attach_disk`) were not recording their CID sentinel the same way `attach_disk` does. `get_disks` on such a VM could return incorrect data and cloud-check membership comparisons could disagree. `create_vm`'s disk-attach path now records the sentinel identically to `attach_disk`.

**A second gap closed: over-length CIDs.** A `pvd-` CID exceeding the Director's 255-character column, with `disk_cid_compression` off, previously only emitted a warning — the volume was created, the Director then received a CID too long to store, and the disk was effectively orphaned. `create_disk` now fails fast in that case with a remediation error naming `disk_cid_compression: true`, and rolls back the volume it created.

**Operator-visible consequences.** `pve-cid decode <cid>` replaces manual base64/gzip decoding for support and debugging. An over-255-character disk CID with compression disabled now fails at `create_disk` time with an actionable message, instead of silently producing a Director-side orphan.

**Migration.** Delete any deployment carrying disk CIDs in the removed legacy grammars before upgrading; the new build will not parse them.

---

## D5 — `network_mode` default: `bridge`

**Context.** The prior default resolved toward the SDN-managed path. Feedback from meeting discussions was clear that many deployments — particularly ones on operator-managed physical fabrics — want the simplest possible zero-config shape: point the CPI at a pre-existing Linux bridge, done. Requiring SDN concepts (zones, vnets) for that shape was unnecessary friction.

**Options considered.** Keep the SDN-first default; keep the prior `auto` heuristic (SDN if a zone is configured, bridge otherwise); switch the default to `bridge` outright, with SDN and `auto` remaining fully supported as explicit opt-ins.

**Chosen: `bridge` as the default.** A plain, pre-existing Linux bridge (`managed: false`, `cloud_properties: {bridge: vmbrX}`) needs no SDN prerequisites and no CPI-side provisioning. SDN remains fully supported — set `network_mode: sdn` once, or name a zone/vnet per network in `cloud_properties`.

**Two capabilities were added alongside the default flip**, closing gaps the prior bridge path had:

- **Per-NIC VLAN tagging** — `cloud_properties.vlan` (or `network_defaults.vlan`), an 802.1Q tag from 1–4094, for operator-managed trunk fabrics.
- **Per-NIC MTU** — `cloud_properties.mtu` (or `network_defaults.mtu`), 576–65520 (or `1` to inherit the bridge's MTU), virtio-only.

Both were already anticipated in the pre-existing per-NIC design; the bridge-mode default flip is what made them worth completing.

**Operator-visible consequences.** A zero-config deployment now provisions plain bridge NICs, not SDN vnets. Environments that relied on the previous default meaning SDN must set `network_mode: sdn` explicitly.

**Migration.** Any manifest that depended on the implicit SDN default needs `network_mode: sdn` added explicitly before redeploying; `network_mode` governs `create_network`/`delete_network` dispatch and is not a live-editable property.

---

## D6 — Resource pools: default-on, with a startup preflight

**Context.** Resource pools (`bosh` for VMs, `bosh-templates` for stemcell cache templates) were already default-on as an ACL scoping mechanism, but nothing verified pool access before the CPI began serving requests. A missing `Pool.Allocate`/`Pool.Audit` grant only surfaced mid-deploy, on the first real `create_vm` or `create_stemcell`.

**Options considered.** Make pools opt-in only (loses the ACL-scoping benefit by default); make pools mandatory with hard startup validation (breaking for operators who deliberately opt out); keep default-on and add a cheap, read-only startup probe.

**Chosen: default-on, unchanged, plus a startup preflight.** At boot, the CPI issues `GET /pools/{poolid}` (the cheapest side-effect-free call that exercises the `Pool.Audit` grant) against every configured pool. A pool that does not exist yet is not a failure — pools are created lazily on first use. Only a classified permission error (HTTP 401/403) fails the preflight, with a message naming the exact grant to add. Every other failure (pool genuinely absent, network fault, transient PVE error) is logged and treated as non-blocking, so a startup-time hiccup never prevents the CPI from booting.

**Operator-visible consequences.** A genuinely missing grant now surfaces as a boot-time failure with a copy-pasteable fix, rather than failing the first real deploy partway through. Opt out of pool assignment entirely with `pve.vm_pool: ""` and `pve.stemcell_template_pool: ""` — the preflight is then skipped, since there is nothing to probe.

**Migration.** None — this is additive. It only fails startup for a configuration that was already broken.

---

## D7 — `iso_storage` default: follow `vm_storage`

**Context.** `pve.iso_storage` defaulted to `local` (node-local) — fine for a single-node lab, but the ConfigDrive ISO attached at `scsi30` lives on that storage for the VM's *entire life*, not only at boot. The moment any HA-registering feature (DLB, AZ node-affinity pinning, anti-affinity HA rules) is active, a node-local ISO pool defeats live migration and HA recovery — the CPI already warns about this, and can be configured to hard-fail on it.

**Options considered.** Keep `local` as the default; require `iso_storage` to be set explicitly (a breaking change with no default); default to following `vm_storage` when it is eligible.

**Chosen: follow `vm_storage` by default.** `pve.iso_storage_follow_vm_storage` defaults `true`: at CPI process startup, before any `create_vm` call, the CPI resolves the ISO pool to `vm_storage` instead of the spec's `local` default — provided `vm_storage` advertises PVE content type `iso` and is shared. An `iso_storage` pinned to any value *other than* the spec default `local` always wins over this behavior. Because BOSH renders the `local` default into the job config whether or not the operator set it, the CPI cannot distinguish "unset" from "explicitly `local`", so it treats the literal value `local` as the unset signal. Setting `iso_storage_follow_vm_storage: false` restores the old always-`local` default.

**Operator-visible consequences.** HA/DLB-enabled deployments on a shared `vm_storage` pool that also serves `iso` content no longer need a separate `iso_storage` manifest edit to avoid the migration-safety warning. Genuinely single-node deployments cannot pin the node-local pool by writing `iso_storage: local` — that value reads as "unset" and gets `vm_storage`-following. Either set `iso_storage_follow_vm_storage: false`, or name the node-local pool under a distinct storage ID. When `vm_storage` is block-backed or non-shared the CPI falls back to `iso_storage` unchanged and logs a warning, so the practical outcome on a single-node lvmthin lab is still `local` — by fail-open, not by precedence.

**Migration.** None required.

---

## D8 — Copy-vs-move and storage identity: full backing normalization

**Context.** Several storage-comparison decisions in the CPI — whether a clone can stay a fast linked clone, whether two storage pools count as "the same storage" for placement or attach co-location, whether DLB/HA migration-safety checks see shared storage — compared PVE storage pool IDs by string equality. That breaks the moment an operator registers the same physical NFS export or directory under two different storage IDs (the "two names, one export" trap): the CPI would see two "different" pools and could silently downgrade a clone to a full copy, or produce a spurious co-location failure, even though both IDs are the same bytes on disk.

**Options considered.** Fix only the clone-target bug narrowly; fix the clone path plus the storage-mismatch check; normalize backing identity at every storage-comparison decision point in the codebase.

**Chosen: full normalization.** `StorageInfo.BackingKey()` returns a normalized physical identity for a storage entry: server-plus-export (host lowercased, export path cleaned) for NFS/CIFS, cleaned path for `dir`, and — deliberately — the bare storage ID itself for every other type (`lvm`, `lvmthin`, `zfspool`, `rbd`, `cephfs`, `glusterfs`, `pbs`, `btrfs`). Those other types never normalize to a shared key, even if they might wrap the same underlying device, because the CPI has no reliable way to prove it and a wrong merge is worse than none. Two storage IDs sharing a `BackingKey` are now treated as "the same storage" at both places the CPI compares two storage identities: clone-mode auto/linked selection, including the clone target-validation fix (validated against the *template's* storage, not the destination's), and the shared-storage classification behind the DLB and ConfigDrive migration-safety checks. The two remaining places a duplicate registration could have bitten — disk fault-domain placement and attach-disk co-location — turned out to need no fix: neither compares two storage IDs. Fault-domain classifies each disk's own pool independently, and attach-disk compares node names. Both are covered by regression tests against the "two IDs, one export" fixture. On its first storage lookup, the CPI warns when two storage IDs in the cluster's `/storage` index share a `BackingKey`.

**Operator-visible consequences.** `clone_mode: auto` now correctly stays a fast linked clone when the template's and the destination VM's storage IDs differ in name but share a physical backing, instead of silently downgrading to a full copy. A duplicate-registration mistake (the same export under two names) now surfaces as a warning on the CPI's first storage lookup instead of producing inconsistent behavior you'd have to debug from symptoms.

**Migration.** None required — this only widens what the CPI recognizes as "the same storage." Genuinely distinct backings behave exactly as before.

---

## D9 — Cross-cluster delete safety: `destroy_unreferenced_disks` defaults `false`

**Context.** This is the highest-severity hazard found in the multi-cluster research pass. PVE's `DeleteQemu` accepts a `DestroyUnreferencedDisks` flag that instructs PVE to free every volume that is *not referenced in the destroyed VM's config* **and** whose VMID *matches the VM being destroyed* — a storage-wide scan by VMID, not a config-scoped one. This flag was previously passed unconditionally as `true` on every non-retain `delete_vm`. On storage shared between two independent PVE clusters (two BOSH-Proxmox availability zones pointed at the same NFS or directory export) with overlapping VMID ranges, deleting VM 105 in cluster A would free cluster B's live `vm-105-disk-*` volumes — data destruction with no error, no warning, nothing to see it coming.

**Options considered.** Leave the flag unconditionally `true` (the status quo — unsafe the moment storage is shared); default it `false` with no way to opt back in (loses a real cleanup benefit for genuinely single-cluster storage); default `false`, with an explicit opt-in flag.

**Chosen: default `false`, opt-in via `pve.destroy_unreferenced_disks`.** The new config flag gates `DestroyUnreferencedDisks` at every site that sets it: the synchronous `delete_vm` path, the fast-path delete, the fast-path straggler sweep, template teardown in `delete_stemcell`, the freeze-failure template cleanup in `create_stemcell`, and the rollback purge in `create_vm`. Enable it only when the storage pools this CPI is configured against are **not** shared with any other independent PVE cluster or non-CPI tooling that allocates VMIDs in the same range.

**A companion fix.** The VMID-allocation-time storage scan (`WithStorageScan`, which prevents a *new* allocation from colliding with a foreign cluster's existing volumes) previously covered only the VM-storage and disk-storage pools. It now also covers parker-VM allocation (the `detached_disk_strategy: parked` band) and the ISO-storage pool, closing two allocation-time gaps that mirrored the same underlying hazard.

**Operator-visible consequences.** Single-cluster deployments that relied on the automatic orphan-volume sweep at delete time must now either set `pve.destroy_unreferenced_disks: true` explicitly, or rely on `scripts/disk-audit` for the class of orphan volumes that are no longer swept automatically. Multi-cluster and shared-storage deployments are protected by default with no configuration required.

**Migration.** If you run a genuinely single-cluster deployment and want the previous automatic-sweep behavior back, set `pve.destroy_unreferenced_disks: true` explicitly after confirming your storage pools are not shared with anything else that allocates VMIDs. See [Multi-Cluster Deployments](multi-cluster.md) for the full shared-storage guidance.

---

## D10 — qcow2 deletion policy: `:light:` never, `:heavy:` at last cluster reference

**Context.** With path-identity CIDs (D1) and per-director reference sets (D3) in place, we needed to define exactly when a stemcell's backing qcow2 file gets deleted — and, specifically, how a stemcell shared across multiple PVE clusters (the upload-once, use-everywhere flow) should behave when one cluster's director calls `delete_stemcell`.

**Options considered.** Delete the file at the first `delete_stemcell` call regardless of kind (breaks cross-cluster sharing outright); never delete a CPI-uploaded file at all (leaks storage indefinitely); split by kind — `:light:` files are never the CPI's to delete, `:heavy:` files are deleted only when the last director reference *within that cluster* is released, and a cluster missing its own copy of a shared `:heavy:` file self-heals by re-uploading on its next dedup miss.

**Chosen: the split above.** A related legacy behavior is also gone: the CPI used to delete the just-uploaded qcow2 immediately after freezing it into a template, to save space during the upload — that "post-freeze reclaim" no longer happens. The qcow2 now persists as the CID's own identity for as long as any director in its cluster references it.

**Operator-visible consequences.** A `:heavy:` stemcell's file lifetime tracks director references, not template-freeze timing. If an independent cluster is missing its own copy of a shared `:heavy:` stemcell's qcow2 (deleted, never uploaded there, or lost), it self-heals silently by re-uploading on its next `create_stemcell` dedup miss rather than erroring. `:light:` files are never touched by the CPI, at any reference count — cross-cluster sharing on shared storage is achieved entirely through `:light:` mode, by design.

**Migration.** Pre-cutover `:heavy:`-equivalent stemcells relied on post-freeze reclaim and had no director-reference tracking at all. Delete them and re-upload after the redeploy.

---

## D11 — HA versus the BOSH resurrector: warn, document, and race-test

**Context.** PVE HA and the BOSH resurrector can independently detect the same stopped guest and both try to recover it — producing a duplicate VM that conflicts on IP address, VMID, or agent credentials. The CPI has no API surface to read or control the resurrector's state, so this was previously documented in [DLB-Aware Placement](dlb-aware-placement.md) but entirely unenforced: an operator who enabled DLB or an HA-rules feature without separately remembering to disable the resurrector got no signal from the CPI at all.

**Options considered.** Have the CPI attempt to detect or control resurrector state (rejected — no PVE-API-only mechanism exists for this; the CPI has no shell access to the Director); document only, with no runtime signal (the status quo — easy to miss); an active, once-per-process warning plus a dedicated ownership-matrix document plus a live node-kill race test in the validation matrix.

**Chosen: the active warning plus documentation.** `create_vm` now logs one warning per CPI process the first time any HA-registration feature (`placement.dlb`, `placement.anti_affinity.use_ha_rules`, `placement.pin_az_via_ha_rules`) is active for a VM, naming `bosh update-resurrection off` as the fix. `has_vm` semantics are unchanged: reporting a VM as present based on cluster-wide visibility, even on a node that has gone unresponsive, remains the deliberate, fail-toward-retry behavior — see [HA and Resurrection](ha-and-resurrection.md) for the full ownership matrix and the reasoning.

**Operator-visible consequences.** The first HA-registered `create_vm` in a CPI process's lifetime produces a clear, actionable warning in the CPI log. See [HA and Resurrection](ha-and-resurrection.md) for the complete ownership model, the double-healing race in detail, and remediation.

**Migration.** None — this is warn-only and does not change `create_vm`'s or `has_vm`'s success/failure outcomes.

---

## D12 — Per-request cloud-property overrides from cpi-config

**Context.** BOSH's cpi-config feature lets one Director register several named CPI entries — two `type: pve` blocks pointing at distinct Proxmox clusters, say — and BOSH resolves the executable from the entry's `type`, so every entry dispatches into the *same* running instance of this CPI binary. The Director communicates which entry a request belongs to by merging that entry's `properties:` hash into the JSON-RPC request context. Nothing in the CPI read those keys back out, so every request ran against whichever `pve.*` config the process happened to be launched with — silently uploading stemcells and creating VMs on the wrong cluster while reporting success. This is the mechanism the entire multi-cluster story rests on; without it, a second cpi-config entry is decorative.

**Options considered.** Job-config only, requiring one CPI release deployment per cluster (rejected — it defeats the point of cpi-config and doubles the operational surface); per-request overrides read from flat `pve_*` context keys; per-request overrides that additionally fold BOSH's real nested context shape into that flat namespace.

**Chosen: per-request overrides with nested-shape flattening.** The Director does not deliver an entry's properties as flat keys. It merges the whole `properties:` hash verbatim, so they arrive nested — `context.pve = {host: …, vm_storage: …}` and `context.agent = {mbus: …}`. The CPI flattens each key of the nested `pve` map to `pve_<key>` (and `context.agent.mbus` to `pve_agent_mbus`) before applying it, with an explicit flat key always winning over a nested one.

The set of overridable fields is a closed registry, not "anything in the context". It covers connection and routing identity (host, port, user, password/token, realm, node), the four storage pools, the network bridge, `verify_ssl`, every VMID band (VM, disk, stemcell-template, and parker), `stemcell_replicate_local`, `vm_prefix`, `agent_mode`, `vm_disk_format`, the agent mbus, and the whole `placement` block. Everything else — disk-performance policy, hooks, OpenTelemetry, retry curves — is process-wide operating policy rather than a property of which cluster a request targets, and continues to come from the job config unconditionally.

`placement` is in the registry despite being policy rather than connection identity, because its `az_map` names PVE *nodes*, and node names mean nothing outside the one cluster that has them. Leaving it out made the three HA features (`placement.dlb`, `anti_affinity.use_ha_rules`, `pin_az_via_ha_rules`) unreachable from a cpi-config entry — unreachable, that is, in exactly the multi-cluster deployments that have more than one cluster to fail over within — and once `cloud_properties.availability_zone` was set, every `create_vm` on a non-job-level entry hard-failed against the job-level `az_map` that had never heard of that AZ. An entry's `placement` block replaces the job-level one wholesale rather than merging into it: each entry then states its own complete placement policy, and merging pointer-typed sub-blocks field by field would make "unset" and "explicitly false" indistinguishable, which is the ambiguity those pointer types exist to prevent. An entry that wants the job-level policy omits the key; an entry that wants none sends `null`. Unknown fields *inside* the block are rejected rather than dropped, so a typo like `pin_az_via_ha_rule` fails loudly instead of parsing into a block that pins nothing.

Two guardrails matter operationally. First, a key inside the registry carrying an uncoercible value, or a fully-merged effective config that fails the same validation the job config must pass at startup, is a hard error rather than a silent fall-back to the job config — falling back would reproduce the exact "request quietly runs against the wrong cluster" defect the mechanism exists to close. Unknown keys outside the registry are ignored, since most `pve.*` properties are legitimately job-level-only. Second, every overridable field participates in the per-request client-bundle cache key, so two requests resolving to the same effective config share one cached PVE client, boot agent, and backend resolver, while any difference in a keyed field — including a VMID band or `stemcell_replicate_local` — yields a distinct bundle rather than silently reusing another cluster's connection.

**Operator-visible consequences.** One deployed CPI release serves several PVE clusters, each cpi-config entry carrying its own credentials, storage pools, disjoint VMID bands, and its own `stemcell_replicate_local` setting. Because each entry builds its own client bundle, it also builds its own storage-info cache — which is why the duplicate-backing warning (D8) fires per entry rather than once per process. Entry-to-entry band disjointness remains an operator convention: the CPI validates its own four bands against each other within one effective config, but it cannot see a sibling entry's bands. See [Multi-Cluster Deployments](multi-cluster.md).

**Migration.** None for single-cluster deployments — a request with no `pve_*` context keys resolves to the job-level config unchanged. Multi-cluster operators should confirm every entry's bands are disjoint before applying a cpi-config with more than one `type: pve` entry.

---

## D13: Stable disk identity via drive serial, ownership transfer via reassignment

**Context.** Persistent-disk CIDs embed a PVE volid (`vm-<vmid>-disk-<n>`), but PVE's `move_disk` reassignment renames the volume to match its new owner, and BOSH CIDs are immutable once returned. Adopting reassignment as the ownership-transfer mechanism therefore requires a disk identity that survives the rename: the envelope's volid becomes a birth record, and a new stable ID becomes the identity the CPI resolves by. The open question was the identity's carrier on the PVE side, settled by a live spike rather than up front, with pre-agreed outcomes for both branches.

**Live spike results** (two-node PVE 9.2.4 cluster, 2026-08-20, throwaway VMs and volumes on lvmthin, dir, and NFS storages):

- Reassignment (`move_disk` with a target VM) completes in 0.8 to 1.5 seconds on all three storage families, and is metadata-only: an LV rename on lvmthin, a file move into the target's image directory on dir and NFS.

- Reassigning an attached slot carries the full drive option string, including `serial=` and performance options, and the landed slot is selectable via the target-disk argument.

- A running target VM accepts the reassignment and hotplugs the device live; the running QEMU immediately exposes the disk with its serial (verified through the QEMU monitor's device tree).

- A running source VM refuses direct reassignment ("Cannot move disk to another VM while the source VM is running - detach first"). Detaching to an `unused` entry and reassigning that entry works from a running VM, but `unused` entries carry no drive options, so the serial drops on this path and we re-apply it on the landed slot after the transfer.

- Editing a `serial=` on a running VM's attached disk updates the config silently while the live device keeps its original serial until the next full restart. A mid-life serial change is therefore an unbounded config-versus-guest divergence window: identity serials are written only at attach boundaries, never onto an already-attached disk.

- PVE strictly validates drive option schemas ("property is not defined in schema and the schema does not allow additional properties"), so `serial=` is the only in-drive carrier available; arbitrary marker keys are rejected outright.

- A disk referenced by a snapshot refuses reassignment immediately ("Can't move disk used by a snapshot to another VM"), before any task starts. Disks carrying snapshots keep the config-edit transfer path.

- Reassignment is same-node only, even on shared storage ("Both VMs need to be on the same node"). Cross-node disk mobility needs a different vehicle and is out of scope here.

**Chosen: drive serial as the carrier, reassignment as the transfer.** Both pre-agreed gate conditions passed (the carrier behaves, and a running target accepts reassignment), so the ownership-consistent design proceeds as planned. The serial wins over an owner-config marker because it rides the drive entry automatically through the parker-to-VM attach direction, which shrinks the unrecoverable crash window to the detach direction only, where write ordering covers it: the parker's provenance record is written before the holder's record is removed, so a scan always finds at least one carrier. A description-sentinel marker would survive no transfer automatically, and PVE's schema rejection rules out any other in-drive key. The serial is guest-visible by design and stable for the disk's life; because it is only ever written at attach boundaries, the guest only ever sees a serial appear on a freshly attached device, never a rename on a live one. PVE caps drive serials at 20 bytes, which bounds the token format.

**Operator-visible consequences.** CID wire format is unchanged; new disks gain a stable ID inside the existing envelope. Legacy CIDs resolve through a volid fallback that can never be removed, since director databases keep old CIDs forever. Disks under the `free` detachment strategy stay on the legacy config-edit path entirely.

**Follow-up recorded.** The attach path currently rebuilds drive option strings from the envelope's recorded options, and PVE rejects unknown keys, so any CPI-internal key that reaches the drive string is a live failure. The build verifies that CPI-internal option keys are filtered before the config write and adds a regression test.

## Out of scope

A few adjacent ideas were explicitly weighed and rejected for this pass:

- **Cross-cluster distributed refcounting** (a sidecar file or external coordination layer tracking stemcell references across independent PVE clusters) — rejected. Per-cluster reference sets (D3) are sufficient given `:light:`/`:heavy:` self-healing (D10); a distributed refcounting layer would add an external dependency for a problem the cutover already solves at the file level.
- **HA-aware `has_vm`** (consulting PVE HA status to distinguish "genuinely gone" from "HA is mid-recovery") — rejected. See [HA and Resurrection](ha-and-resurrection.md) for why fail-toward-retry is the correct default without it.
- **Multi-node `create_network` bridge provisioning** — the bridge-creation path remains single-node only; `managed: false` against a pre-provisioned bridge is the documented multi-node pattern (see [Networks](networks.md)).
- **A human-readable disk-CID grammar** — rejected in favor of keeping the proven `pvd-`/`pvz-` envelope and shipping `pve-cid` as the readability layer instead (D4).

## See also

- [Multi-Cluster Deployments](multi-cluster.md) — the cpi-config walkthrough, VMID banding, and shared-storage rules that put D9 and D10 into practice.
- [HA and Resurrection](ha-and-resurrection.md) — the full ownership matrix behind D11.
- [Operations Runbook](operations.md) — day-2 procedures, including stemcell lifecycle across directors and safe teardown ordering.
