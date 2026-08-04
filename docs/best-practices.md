# Best Practices

How the BOSH PVE CPI aligns with Proxmox VE best practices, section by section. Every entry states the practice, what the CPI actually does about it, and a status: **Meets** (the CPI implements the practice as the sensible default or as its only behavior), **Exceeds** (the CPI goes beyond PVE's own default or beyond what a CPI is typically expected to do), or **Configurable** (the practice is available but requires an operator to set a property — the recommended setting is given).

Property names and defaults are cross-referenced to [Configuration reference](configuration.md); day-2 operational detail lives in [Operations runbook](operations.md), [Network configuration](networks.md), [PVE API permissions](pve-api-permissions.md), and [Troubleshooting](troubleshooting.md).

## 1. VM hardware

**VirtIO SCSI single with a dedicated I/O thread per disk.**

**Best practice.** A shared SCSI controller serializes every disk's I/O behind one QEMU main-loop thread; giving each disk its own controller and I/O thread removes that contention.

**CPI behavior.** `pve.disk_performance.virtio_scsi_single` and `pve.disk_performance.iothread` both default to `true`, matching the modern PVE creation default. Both bake in at create/attach time only, so an existing VM's controller is never rewritten retroactively.

**Status.** Meets.

**VirtIO network model.**

**Best practice.** VirtIO NICs give the best network throughput and the lowest CPU overhead of any PVE-emulated model; emulated hardware models (e1000, rtl8139) exist only for guests that cannot load VirtIO drivers.

**CPI behavior.** Every NIC resolves to model `virtio` unless an override names something else: `cloud_properties.network_model` at the call or `vm_type` profile layer, or `cloud_properties.network_defaults.model` / a per-NIC `networks[].cloud_properties.model` (`network_defaults` wins over the per-NIC value). There is no global `pve.network_model` property; the default requires no configuration.

**Status.** Meets.

**CPU model guidance.**

**Best practice.** On a homogeneous cluster, `host` is the best-performing model: the guest sees the physical CPU's full feature set with no emulation mask. On clusters mixing CPU generations, a `host`-typed guest can crash when live-migrated to a differently-generationed node, so a portable named model is required there; the API-level `kvm64` fallback is the worst of both worlds (lacks AES-NI, taxing every TLS handshake a BOSH workload makes).

**CPI behavior.** `pve.cpu_type` defaults to `host`. `cloud_properties.cpu_type` overrides it per instance group. On mixed-generation clusters that rely on live migration, set `x86-64-v2-AES` (PVE's own creation-wizard default since 8.0 — keeps AES-NI, live-migrates across CPU generations from roughly 2010 onward) or, for older hardware, the cluster's lowest-common-denominator named model. The sentinel value `pve-default` (at either layer) writes no `cpu` key at all, restoring PVE's API-level `kvm64` fallback for clusters that need it.

**Status.** Meets — homogeneous clusters get maximum performance by default. Mixed-generation clusters must override with a portable named model wherever live migration is possible.

**A minimum of two cores per VM.**

**Best practice.** A single-vCPU guest serializes every kernel thread, interrupt handler, and application thread behind one thread of execution; two cores is the sensible floor for any VM regardless of how light its workload is.

**CPI behavior.** When neither `cloud_properties.cores` nor the vSphere-style `cloud_properties.cpu` names a vCPU count, the CPI defaults to 2 cores. An explicit `cores: 1` is honored as given — the floor fills absence, it does not override intent.

**Status.** Meets.

**Machine type left to PVE.**

**Best practice.** PVE's default `i440fx` machine type is the right answer for Linux server guests; `q35` matters when a guest needs PCIe passthrough (`hostpciN` with `pcie=1`) or other PCIe-native topology.

**CPI behavior.** The CPI never writes a `machine` key — VMs get PVE's `i440fx` default. Operators who pass through PCIe devices can set `cloud_properties.pve_config.machine` (allowlisted) to `q35` on the affected instance groups.

**Status.** Configurable — set `pve_config.machine: q35` only alongside PCIe passthrough; otherwise leave unset.

**Tablet disabled on headless VMs.**

**Best practice.** The emulated USB tablet exists to reconcile absolute mouse coordinates for a graphical console session; a BOSH VM never has one, and the device costs roughly 2-3% CPU per VM at scale for nothing.

**CPI behavior.** The CPI writes `tablet: 0` unconditionally on every VM it creates, on both the import and clone paths, and on the stemcell templates it builds (so even a hand-made clone from a CPI template inherits tablet-off). There is no opt-out knob — every BOSH VM is headless by construction, so there is nothing to reconcile.

**Status.** Exceeds — PVE's own default leaves the tablet on; the CPI turns it off unconditionally rather than leaving that tax to the operator to discover.

**Serial console for cloud guests.**

**Best practice.** Cloud-image guests log their early boot and the BOSH agent's own output to the serial console; without a serial device that output has nowhere to go, which is exactly the wrong moment to lose visibility into a wedged VM.

**CPI behavior.** The CPI writes `serial0: socket` on every VM it creates, on both the import and clone paths — PVE's own default is no serial device at all. `cloud_properties.pve_config.serial0` is allowlisted for operators who need to override it.

**Status.** Exceeds.

**NUMA and hot-add-only hotplug.**

**Best practice.** Memory hot-add requires `numa=1` at create time to allocate DIMM slots; without it, a later memory hot-add silently no-ops. PVE's own hotplug support for CPU and memory is add-only in both directions — there is no hot-remove — so the hotplug set only needs to cover what PVE can actually do.

**CPI behavior.** `pve.numa` defaults `true`; `pve.hotplug` defaults `network,disk,cpu,memory`. Both are per-VM overridable (`cloud_properties.numa`, `cloud_properties.hotplug`), and enabling `cloud_properties.memory_hotplug` forces NUMA on regardless of any other layer, since memory hotplug cannot function without it.

**Status.** Meets.

**Ballooning disabled by default.**

**Best practice.** BOSH sizes VMs deterministically from the manifest and the agent plans job memory against that size, so auto-ballooning — which reclaims guest memory beneath those assumptions — invites OOM kills that look like application failures. Disable the balloon device unless the cluster deliberately overcommits memory.

**CPI behavior.** The CPI writes `balloon: 0` on every VM and template it creates, disabling the balloon device. `pve.balloon` (globally) or `cloud_properties.balloon` (per instance group) accepts a positive MiB floor to enable PVE auto-ballooning, and the sentinel `pve-default` leaves no `balloon` key on the VM — clearing the template-inherited value on clones — restoring PVE's own default (device enabled, balloon = memory) for clusters that want it.

**Status.** Meets.

**Explicit boot order.**

**Best practice.** An unset boot order leaves PVE to guess which device to boot from; on a multi-disk VM that guess is not guaranteed to land on the root disk.

**CPI behavior.** Every VM the CPI creates carries an explicit `boot: order=<root-disk-key>` naming the actual root disk device (`virtio0` by default, or `scsi0` when `pve.root_disk_bus: scsi` is set) — written directly on the import path, and inherited on the clone path from a cache template the CPI stamped with the same order. Never left to PVE's own device-order heuristic.

**Status.** Meets.

**Guest agent enabled at create, before first boot.**

**Best practice.** The QEMU guest agent channel must exist in the VM config before the guest's first boot, or the in-guest agent package has nothing to attach to and IP/status reporting through PVE never comes up.

**CPI behavior.** The CPI writes `agent: enabled=1` before the VM is started — in the create payload on the import path, and as a pre-start config patch on the clone path — never as a post-boot reconfiguration.

**Status.** Meets.

## 2. Storage

**SSD emulation and discard on TRIM-capable backends, resolved automatically.**

**Best practice.** Without `discard=on`, a thin-provisioned or copy-on-write pool never reclaims guest-deleted blocks — it grows monotonically even as the guest filesystem reports space as free. Forcing `discard`/`ssd` onto a backend that cannot honor them (thick LVM, CephFS, GlusterFS) is either rejected by PVE or silently ineffective.

**CPI behavior.** `pve.disk_performance.discard` and `.ssd` are tri-state (`true`\|`false`\|unset), defaulting to unset — resolved per disk at bake time against the disk's actual storage pool: on for TRIM-capable backends (lvmthin, zfspool, rbd, or a qcow2 image on a file-backed pool), omitted everywhere else. `ssd` additionally only ever reaches the SCSI bus.

**Status.** Meets.

**Create-time-only flags never retrofitted.**

**Best practice.** Silently rewriting a live VM's structural disk options on a later attach or config sync is a correctness hazard — an operator who changed a global default should not find every existing disk's behavior has quietly shifted underneath them.

**CPI behavior.** `cache`, `iothread`, `ssd`, and `aio` are baked once, at create or attach time, into the disk's recorded structural options and never rewritten retroactively. `pve.disk_perf_invariant_mode` (default `enforce`) governs what happens when a later re-attach's resolved value diverges from the recorded one: reject in `enforce`, warn in `warn`, ignore in `off`. `discard` is deliberately excluded from invariant tracking — it can change on a live device without a structural reconfiguration.

**Status.** Meets.

**Thin-pool utilization ceilings alongside absolute headroom.**

**Best practice.** Copy-on-write pools degrade noticeably from around 50% utilization and badly by 80%; Ceph enforces its own nearfull/backfill-full/full thresholds at 85/90/95%. An absolute-bytes headroom filter alone does not capture that proportional risk curve.

**CPI behavior.** `pve.storage.max_utilization_pct` (default `0`, disabled) adds a proportional ceiling gating `create_vm` placement, `create_disk`, and `resize_disk`; `snapshot_disk` is always warn-only regardless of `max_utilization_mode`, since snapshot growth cannot be estimated ahead of time. It composes with, rather than replaces, the separate absolute-bytes `placement.reserve_storage_headroom` filter. See [Operations — Storage capacity](operations.md#storage-capacity-utilization-bands-and-the-cpi-ceiling-gate).

**Status.** Configurable — recommended: `80`.

**Format selection per storage backend.**

**Best practice.** Block-backed storages (LVM, LVM-thin, ZFS pool) reject the qcow2 format outright and only accept raw; file-backed storages (dir, NFS, CIFS) support qcow2's copy-on-write and snapshot capabilities that raw lacks.

**CPI behavior.** `pve.vm_disk_format` defaults to `qcow2`, which is correct for file-backed pools. When no call- or profile-level layer expresses an explicit format preference, `create_disk` omits the format parameter entirely on block-backed storage so PVE selects the correct default for that storage type itself, rather than forcing qcow2 where PVE would reject it. (`create_vm`'s root disk is a separate path: its format is always carried in the import-from string.)

**Status.** Meets.

**Linked-clone placement rules.**

**Best practice.** A linked clone's copy-on-write overlay always lands on the source template's own storage pool — PVE does not honor a `Storage` override parameter for linked clones — so when the template's storage differs from the operator's intended `vm_storage`, a linked clone silently misplaces every root disk onto the wrong pool.

**CPI behavior.** `clone_mode: auto` (the default) downgrades to a full clone whenever `vm_storage` differs from the template's own storage, so the root disk lands where `vm_storage` points; `clone_mode: linked` forced explicitly against mismatched storages fails `create_vm` with a clear error before any clone is submitted, rather than silently misplacing the disk.

**Status.** Meets.

**Disk-op deferral while the holding guest is locked.**

**Best practice.** Deleting a disk out from under a guest that is mid-backup, mid-clone, mid-migrate, mid-snapshot, mid-rollback, or still being created can corrupt the in-flight operation or leave PVE's storage layer inconsistent; PVE's own API takes only a storage-level lock for the delete task itself, not a guest-level interlock.

**CPI behavior.** `pve.disk_delete_state_guard` (default `on`) resolves the guest currently holding the disk being deleted, reads its config lock, and defers the delete with a retriable error when that lock is `backup`, `clone`, `migrate`, `snapshot`, `rollback`, or `create`. The guard is fail-open on permanent resolution failures — a disk attached to no VM passes straight through.

**Status.** Meets.

**Root disk resized before first boot.**

**Best practice.** A template's root disk is built once at a fixed size; if the actual instance-group ask is larger and the disk is not grown before the guest boots, the BOSH agent's own bootstrap fails when it runs out of root filesystem space partway through.

**CPI behavior.** `create_vm` reads the template's actual root-disk size and grows the root disk to the resolved instance-group size before the VM is started — never as a post-boot operation. An exact match is a no-op, and a requested size smaller than the template fails with a clear error, since a root disk cannot shrink.

**Status.** Meets.

**One export, one storage ID — avoid duplicate physical backings.**

**Best practice.** Registering the same physical export or path under two different PVE storage IDs looks harmless in `storage.cfg` but silently defeats identity-sensitive logic: linked-clone downgrade decisions, VMID-collision scanning, and placement all key off storage ID, not off the underlying bytes.

**CPI behavior.** On its first storage lookup, the CPI resolves the backing identity (server+export for NFS/CIFS, path for dir-style plugins) of every storage ID in the cluster's `/storage` index — not just the ones it is configured to use — and warns once per storage cache, in practice once per PVE client, so a process serving a second cpi-config context warns again for that context, when two or more IDs resolve to the same physical backing: `storage_info: two or more storage IDs share one physical backing`. The check is per-CPI-entry only; it cannot see a second cpi-config entry's storage IDs, so it is not a signal about deliberate multi-cluster storage sharing (see [Multi-cluster deployments — Shared-storage rules](multi-cluster.md#shared-storage-rules)) — only about accidentally registering one export twice within a single cluster's `storage.cfg`.

**Status.** Meets — the check is a startup warning, not a hard failure; consolidate to a single storage ID per physical export when you see it.

**Multi-cluster shared storage: disjoint VMID bands, `destroy_unreferenced_disks` stays `false`.**

**Best practice.** Two independent PVE clusters pointed at the same shared export (a common shape for `:light:` stemcell distribution, see [Light Stemcells](light-stemcells.md)) have no cross-cluster coordination: PVE's own `pmxcfs` is per-cluster, and nothing outside the CPI stops both clusters from allocating the same VMID or destroying a volume the other cluster still owns.

**CPI behavior.** Each `type: pve` cpi-config entry is independently configured, so cross-cluster safety is an operator convention the CPI validates only within one entry: give every entry that shares storage with another entry disjoint VMID bands across all four ranges (VM, persistent disk, stemcell-template cache, and parker VM when `detached_disk_strategy: parked`), and leave `pve.destroy_unreferenced_disks` at its default `false` on any storage a second cluster can reach — PVE's `DestroyUnreferencedDisks` flag frees every volume matching the destroyed VM's VMID regardless of which cluster's config actually references it. `:light:` stemcells (operator-managed, preuploaded qcow2, never deleted by the CPI) are the one artifact designed to be shared this way: upload once, every cluster clones from its own independently-built cache template. See [Multi-cluster deployments](multi-cluster.md) for the full worked cpi-config example, banding table, and shared-storage rules.

**Status.** Meets — enforced within one cpi-config entry (its own four VMID bands are validated mutually disjoint at load); disjoint banding *across* entries and `destroy_unreferenced_disks: false` on shared storage are both operator responsibilities the CPI cannot see or enforce across process boundaries.

## 3. Stemcells and images

**Never-booted templates.**

**Best practice.** A stemcell template that has ever booted carries a live machine identity (machine-id, DHCP client-id) baked into its filesystem; every clone made from it would then share that identity unless something regenerates it, causing DHCP and systemd machine-id collisions across the fleet.

**CPI behavior.** The CPI converts every stemcell to a PVE template (`template=1`) immediately after import and never starts it — PVE templates cannot be started directly, only cloned. Each clone is a distinct guest that boots fresh and generates its own machine identity on its own first boot.

**Status.** Meets.

**Content-addressed dedup via SHA tags.**

**Best practice.** Re-uploading and re-converting an identical stemcell image on every deploy wastes storage and time; deduplicating by content hash lets repeat deploys of the same stemcell reuse the existing template.

**CPI behavior.** Every stemcell template whose SHA-256 digest is known is tagged `bosh-stemcell-sha-<sha8>`. Before uploading, `create_stemcell` dedups on the deterministic qcow2 filename (which itself carries the sha8); the sha tag is what dedups the per-cluster cache template, confirmed against the full SHA-256 recorded in the template's provenance.

**Status.** Meets.

**Server-side download with checksum verification.**

**Best practice.** Streaming a stemcell image through the CPI process (download, then re-upload to PVE) doubles the transfer and adds a failure mode the platform's own download path does not have.

**CPI behavior.** When `cloud_properties.source_url` is set, `create_stemcell` uses PVE's `download-url` storage API so PVE streams the image directly into storage rather than through the CPI process, and verifies the result against `cloud_properties.sha256`, which is required on this path — a `source_url` with no valid `sha256` fails `create_stemcell` outright rather than importing an unverified image.

**Status.** Meets.

**Template refcounting against linked clones.**

**Best practice.** Deleting a template that still backs a live linked clone destroys the base volume the clone's overlay depends on, corrupting every VM cloned from it.

**CPI behavior.** The opt-in orphan-prune pass (`pve.stemcell.prune_orphans`) skips any template still referenced by a linked clone rather than destroying it, logging the skip by name. Provenance recording and director scoping are unconditional — no property controls them — since every CPI call carries the calling director's identity in its request context.

**Status.** Configurable — enable `pve.stemcell.prune_orphans` to activate orphan detection and pruning; without it, stale templates accumulate and must be cleaned up by hand.

**Template replication keyed on `vm_storage`, not the stemcell pool.**

**Best practice.** A cache template can only be cloned onto a node that can actually read its disk. What decides that is where the template's *root disk* lands — `pve.vm_storage` — not where the stemcell qcow2 sits. Treating the two as the same pool skips replicas in exactly the split configuration that needs them: a shared NFS qcow2 pool with node-local `vm_storage`.

**CPI behavior.** `pve.stemcell_replicate_local` (default `false`) gates replication, and when it is on, `create_stemcell` builds per-node template replicas whenever `vm_storage` classifies as node-local — regardless of the qcow2 pool's shared-ness, and for every stemcell kind including a pre-uploaded `:light:`. A shared qcow2 pool only suppresses the per-node *file* copy; the per-node template is still built. Classification reads PVE's live storage index rather than the raw `shared` flag, so an NFS pool without an explicit `shared: 1` entry is not misclassified. Replication failures are warn-only, and `delete_stemcell` sweeps replicas by sha8 tag.

**Status.** Configurable — leave `stemcell_replicate_local: false` when `vm_storage` is shared; enable it when `vm_storage` is node-local on a multi-node cluster, which otherwise fails at `create_stemcell` time.

**Cross-node clone pre-flight.**

**Best practice.** PVE cannot clone across nodes when either side of the operation sits on node-local storage. Discovering that from PVE's own error mid-clone leaves a partially created VM to roll back.

**CPI behavior.** When the cache template is on a different node than the VM's target, `create_vm` checks *both* sides before submitting the clone: the template's own storage must be shared, and so must the destination `vm_storage`. Either one failing is a clear, non-retriable rejection naming the pool, the two nodes, and the three ways out — enable `stemcell_replicate_local` so every node gets its own replica, pin `cloud_properties.node` to the template's node, or move the pool to shared storage.

**Status.** Meets — the check is unconditional and needs no configuration.

## 4. Cloud-init / config drive

**Config-drive ISO on SCSI, hot-swappable, no IDE anywhere.**

**Best practice.** An IDE CD-ROM device cannot be swapped while the guest is running without a guest-side rescan, and IDE itself is a legacy bus with no place in a virtio-first VM.

**CPI behavior.** The BOSH agent's settings ISO always attaches as `media=cdrom` on `scsi30` — reserved by convention specifically so it never collides with the persistent-disk slot range (`scsi1`-`scsi28`, with `scsi29` held as headroom) or the root disk slot. No IDE device is ever configured by the CPI.

**Status.** Meets.

**Regenerated atomically on every settings change.**

**Best practice.** Mutating a config-drive ISO in place risks the guest reading a half-written image if a read and a rebuild race.

**CPI behavior.** Every settings change builds a complete new ISO off-VM in a temporary file, uploads it as a whole file, and only then attaches or re-attaches it — the VM is never pointed at a partially written image. A stale orphan ISO from an earlier failed attempt is removed before the new upload to avoid PVE's own 409-on-name-collision.

**Status.** Meets.

**No first-boot package upgrades to race automation.**

**Best practice.** A cloud-init `package_upgrade: true` directive that fires during first boot competes with the BOSH agent's own bootstrap for network and package-manager locks, and can change installed package versions out from under a job's own dependency assumptions.

**CPI behavior.** The config drive carries only a ConfigDrive v2 `settings.json` payload for the BOSH agent — not a cloud-init user-data script — so there is no package-management directive of any kind for the CPI to omit or race.

**Status.** Meets.

**Shared ISO storage for migration-capable VMs.**

**Best practice.** A config-drive ISO on node-local storage blocks live migration and HA recovery of that VM, since the file does not exist on any other node — silently defeating DLB rebalancing, HA AZ pinning, and HA anti-affinity, all of which depend on migration actually working.

**CPI behavior.** `pve.iso_storage` defaults to `local` (node-local). `pve.require_shared_iso_for_ha` (default `false`) escalates the CPI's migration-safety warning to a hard `create_vm` error whenever the VM is being HA-registered and the resolved ISO pool is not shared; `pve.iso_storage_follow_vm_storage` (default `true`) already offers a path off `local`, resolving the ISO pool to `vm_storage` whenever that pool is shared and advertises `iso` content. Because BOSH renders the `local` spec default for `iso_storage` unconditionally, the flag treats the literal value `local` as the "unset" signal — set `iso_storage` to any other value to pin a pool the flag will never override, or set the flag `false` to use `iso_storage` as configured.

**Status.** Configurable — recommended: point `iso_storage` at a shared pool whenever DLB, HA AZ pinning, or HA anti-affinity is in use, or rely on the default `iso_storage_follow_vm_storage: true` when `vm_storage` is itself shared and advertises `iso` content; set `require_shared_iso_for_ha: true` to make the gap fail closed.

**Credential-at-rest warning for local ISO storage.**

**Best practice.** A config-drive ISO on node-local storage is readable by anyone with filesystem access to that storage pool on that node — including whatever bootstrap credentials the settings payload carries.

**CPI behavior.** When `iso_storage` resolves to the default `local`, the CPI logs a Warn once per process naming the exposure and recommending a dedicated pool.

**Status.** Meets.

## 5. Networking and SDN

**Every SDN mutation followed by an apply, and the apply task polled — including rollback paths.**

**Best practice.** An SDN zone, vnet, or subnet create/delete that is never followed by `PUT /cluster/sdn` stays pending in PVE's configuration directory indefinitely, invisible until an operator happens to check.

**CPI behavior.** Every SDN mutation path — create, delete, advertised-route injection, and rollback after a partial create failure — calls `applySDN` and awaits the resulting task (for async zone types) before returning.

**Status.** Meets.

**Pending-state visibility in every probe.**

**Best practice.** Reading only committed SDN state hides a change that is staged but not yet applied cluster-wide, giving a false picture of what the fabric will actually look like once realization finishes.

**CPI behavior.** Every SDN read the CPI performs for reconciliation or idempotency checks (zone, vnet, subnet listings) requests `pending=1`, so staged-but-unapplied changes are visible to the same code paths that decide whether something already exists.

**Status.** Meets.

**Per-node bridge convergence gate, on by default.**

**Best practice.** SDN state propagates to each node over inter-node SSH; one broken node can leave a change silently pending cluster-wide while the apply task itself reports success, so a VM placed on that node fails to attach to a bridge that does not yet exist there.

**CPI behavior.** `pve.network_resolve_retries` defaults to `30` (~30 s at the CPI's 1 s poll cadence): `create_network` polls for cluster-wide vnet convergence, and `create_vm` confirms the target node's bridge is actually present before attaching a NIC to it — converting the silent race into a retriable error naming node and bridge.

**Status.** Meets.

**VNI band segregation with zone-control exclusion.**

**Best practice.** VXLAN Network Identifiers are fabric-global — an EVPN zone's own control-plane VNI (`vrf-vxlan`) and an auto-allocated vnet tag share one identifier space, and PVE does not reject the collision; it silently cross-talks or blackholes traffic on the affected VRF.

**CPI behavior.** `pve.sdn_vni_range_start`/`_end` (default 5000-5999 for vxlan/evpn zones, 2000-2999 when `sdn_zone_type` is `vlan` or `qinq`, so the band fits the 4094 VLAN ID cap) bounds auto-allocation, and the allocator excludes every zone-level `vrf-vxlan` value it can list from the candidate set, falling open with a single Warn if the zone listing itself fails.

**Status.** Meets — the exclusion is fail-open by design, so we recommend keeping any operator-managed control VNI below 5000 (e.g. 4999) as a convention independent of the exclusion succeeding.

**MTU inheritance on vnet NICs, with jumbo-frame and IPv6 caveats.**

**Best practice.** VXLAN encapsulation costs roughly 50 bytes of overhead per frame; a guest NIC set to the underlay's full MTU on an overlay segment produces oversized frames that PVE silently drops rather than fragments.

**CPI behavior.** Unless a per-NIC or `network_defaults` `mtu` is set explicitly, every virtio NIC the CPI attaches to an SDN vnet carries `mtu=1`, meaning "inherit the bridge MTU" — so a guest never emits a frame larger than what the overlay can actually carry. PVE derives the overlay MTU from the underlay automatically (a 1500-byte underlay yields 1450); `pve.sdn_zone_mtu` overrides that derivation for jumbo-frame or IPv6 underlays, where the automatic figure assumes IPv4 encapsulation overhead and over-estimates on an IPv6 underlay.

**Status.** Meets, Configurable for non-standard underlays — recommended: `sdn_zone_mtu: 8950` for a 9000-byte jumbo underlay, `sdn_zone_mtu: 1430` for an IPv6 underlay.

**PVE-generated MACs honoring the datacenter `mac_prefix`.**

**Best practice.** A CPI that generates its own MAC addresses independently of the platform's own OUI convention risks colliding with MACs the platform itself would have generated, and bypasses whatever `mac_prefix` an operator set at the datacenter level for fleet identification.

**CPI behavior.** The CPI never sets an explicit `macaddr` on any NIC it creates; every MAC is generated by PVE itself, which honors the datacenter-level `mac_prefix` setting when it does so. The CPI only reads the assigned MAC back from PVE's config after the VM starts.

**Status.** Meets.

**SDN vnet-per-VLAN remains the recommended isolation model; trunked-bridge VLAN tagging is first-class for operator-managed fabrics.**

**Best practice.** VLAN correctness depends on every tagging point in the path agreeing — the physical switch's trunk configuration, the bridge's VLAN-aware flag, and the tag applied to each NIC. A platform that owns the tagging end-to-end (one SDN vnet per VLAN; VMs join by bridge selection alone) removes the class of misconfiguration where a per-NIC setting disagrees with the switch, so it remains the recommended default for CPI-managed isolation. Where the trunk is fabric the network team already owns and controls end-to-end, having BOSH tag frames per NIC directly — rather than provisioning an additional vnet layer over an already-trunked bridge — is the leaner, equally correct choice for that operator-managed shape.

**CPI behavior.** Two independent mechanisms exist, and choosing one does not disable the other. A PVE SDN zone of type `vlan` maps each vnet to one discrete VLAN ID (`cloud_properties.vnet_tag`, capped at 4094, default auto-allocation band 2000–2999) — `create_network` creates the zone turnkey with `pve.network_bridge` as underlay, and VMs join by bridge selection alone, with no `tag=` in any NIC config. Independently, `create_vm`'s per-NIC `cloud_properties.vlan` (1–4094) writes `tag=<n>` directly onto a NIC attached to any operator-managed bridge — Pattern A, no `create_network` call and no SDN zone involved at all — see [Networks — Per-NIC cloud_properties reference](networks.md#per-nic-cloud_properties-reference). Per-NIC `firewall`/`security_groups`/`allowed_address_pairs` (ipfilter seeding) and `pve.ensure_no_ip_conflicts`'s `(bridge, vlan)` L2-domain grouping both apply identically to a trunked-bridge NIC as to an SDN vnet NIC — a trunked-bridge network is not a lesser-protected path for either mechanism, and two networks sharing one trunk bridge but differing `vlan` values are correctly treated as separate L2 domains rather than flagged as a false IP conflict.

**Status.** Meets — pick per fabric ownership: SDN vnet-per-VLAN when the CPI should own the tag end-to-end, trunked-bridge `cloud_properties.vlan` when the network team already owns and trunks the fabric. Caveat: the CPI validates only the tag range (1–4094) and the virtio-model constraint on `mtu`; it cannot verify the physical switch ports are actually trunked to match, or that the bridge is actually `bridge-vlan-aware` — a mismatch on either side fails silently as unreachable traffic, not as a `create_vm` error.

**EVPN zones operator-owned; advertised routes provenance-tagged and refcount-cleaned.**

**Best practice.** An EVPN fabric's BGP controller, route reflectors, and exit nodes are cluster-wide infrastructure that should not be created or destroyed by a per-deployment CPI call; and injected routes that are never cleaned up on VM deletion accumulate as fabric debris.

**CPI behavior.** The CPI never creates or deletes EVPN zones — `create_network` against an absent EVPN zone fails fast with instructions to create it in PVE first. `cloud_properties.advertised_routes` injects routes into an existing EVPN zone's fabric, stamping a `advrt-<vnet>-<hash>` provenance tag per route on the VM; `delete_vm` removes each recorded subnet unless another live VM carries the same tag (paired routers sharing a route) — fail-open throughout, so a cleanup failure never blocks the delete. Injection at `create_vm` is the opposite: a failed route injection rolls back and fails the call.

**Status.** Meets.

**Two-layer IP-conflict defense.**

**Best practice.** A statically assigned IP that collides with an address already claimed by another guest on the same segment causes ARP flapping and mbus misdelivery — a hard-to-diagnose failure mode that is much cheaper to catch before `create_vm` than after.

**CPI behavior.** `pve.ensure_no_ip_conflicts` (default `true`) always checks static `ipconfig{N}` assignments recorded in existing VM configs before provisioning. An opt-in second layer, the guest-agent IP probe, additionally queries running guests' live network state for a same-segment collision that a purely static config scan cannot see — PVE offers no ARP or lease-table API, so a DHCP-assigned collision on either layer remains outside what the CPI can detect.

**Status.** Meets for the static layer; Configurable for the guest-agent probe layer.

## 6. Cluster, placement, and HA

**Cluster-scan node resolution on every operation.**

**Best practice.** Caching a VM's node from create time and reusing it on every later operation breaks the moment HA failover, DLB, or an operator-initiated migration moves the VM — the cached node no longer holds it.

**CPI behavior.** Every operation that needs a VM's current node (reboot, snapshot, disk operations, delete) resolves it fresh via a cluster-wide scan rather than trusting a cached value, so it stays correct across HA failover and migration.

**Status.** Meets.

**Memory never overcommitted at placement.**

**Best practice.** Scoring a node's memory fitness as one weighted factor among several still allows placement onto a node that does not actually have enough free memory for the VM being created, if other factors score highly enough to compensate.

**CPI behavior.** Placement scoring (`pve.placement.enabled`, default `true`) applies a hard reject — not merely a scoring penalty — to any node whose free memory falls below the VM's requested memory, before the weighted-sum scoring (memory, storage, CPU, guest count, anti-affinity) ever runs.

**Status.** Meets.

**Maintenance-node exclusion on by default.**

**Best practice.** A node an operator has explicitly put into HA maintenance state is being drained for a reason; placing a new VM there works against that intent.

**CPI behavior.** `pve.placement.exclude_maintenance_nodes` defaults `true`: any node PVE's HA subsystem reports in a `maintenance`, `error`, `fence`, or `recovery` state is excluded from candidate scoring, as is any node carrying a tag listed in `pve.placement.maintenance_node_tags`.

**Status.** Meets.

**Ceph treated as locality-free.**

**Best practice.** Node-pinning a disk on cluster-visible shared storage such as Ceph RBD or NFS constrains placement for no reason — the storage is reachable from every node regardless of which one hosts the guest.

**CPI behavior.** `create_disk` treats shared storage as cluster-visible and imposes no node constraint from the storage type alone; only local (node-pinned) storage requires the disk and its VM to land on the same node, and fault-domain co-location (below) applies only when a disk is explicitly tagged with an availability zone.

**Status.** Meets.

**Disk fault-domain pinning only for local backends.**

**Best practice.** A shared-storage disk needs no fault-domain constraint at all; a local-storage disk absolutely does, since the VM must land on the exact node holding the data.

**CPI behavior.** `create_disk`'s optional `availability_zone` cloud property, when set on a shared-storage disk, constrains `create_vm` placement to that AZ so the VM and its pre-existing shared disks stay co-located by policy; without an AZ label, shared-storage disks impose no placement constraint at all, matching their actual locality-free nature.

**Status.** Meets.

**HA deregistration before destroy, with purge.**

**Best practice.** Destroying a VM while it is still a registered HA resource (or still a member of a CPI-managed anti-affinity rule) leaves stale HA state referencing a VMID that no longer exists.

**CPI behavior.** `delete_vm` removes the VM from any anti-affinity or DLB-related HA rule and deregisters its HA resource, and separately removes any node-affinity pin rule, before the destructive `purge=1` destroy call — every step best-effort and logged, never blocking the delete on an HA-cleanup failure.

**Status.** Meets.

**Hard Stop, never a guest-initiated shutdown, with a time-boxed graceful reboot.**

**Best practice.** `delete_vm` should never wait on a guest to voluntarily shut itself down — a wedged or unresponsive guest would hang the delete indefinitely — while `reboot_vm` should still try a graceful path first, bounded by a timeout, so a healthy guest gets a clean reboot.

**CPI behavior.** `delete_vm` always issues a hard `QEMU().Stop`, never a guest-shutdown request. `reboot_vm`'s default `soft` mode issues a graceful ACPI reboot bounded by `pve.reboot_timeout` (default 60 s), falling back to a hard reset on timeout or error; `hard` mode issues the reset immediately.

**Status.** Meets.

**Strict AZ pin semantics, with the ≥2-node guidance.**

**Best practice.** BOSH's AZ concept is a placement contract, not a preference — a strict pin should mean the VM stays in that AZ or does not run, never a silent fallback elsewhere. The tradeoff: a strictly pinned VM whose entire AZ goes down cannot restart anywhere, and a strict pin can also block routine node maintenance if there is nowhere else legal for the VM to go.

**CPI behavior.** `pve.placement.pin_az_strict` defaults `true`, creating a hard PVE HA node-affinity rule rather than a soft preference. The CPI logs a Warn naming the AZ whenever a strictly pinned AZ resolves to fewer than three nodes — one node has no failover target within the AZ at all, and two leaves no headroom for a node in maintenance.

**Status.** Meets — recommended: map every strict AZ to at least three nodes.

**Shared-storage guards covering VM, disk, and ISO pools.**

**Best practice.** A migration-dependent feature (DLB, HA AZ pinning, HA anti-affinity) is only as reliable as the least-shared storage pool the VM actually depends on — a VM with shared root and persistent-disk pools but a node-local config-drive ISO still fails migration and HA recovery.

**CPI behavior.** The DLB shared-storage guard checks `vm_storage`, `disk_storage`, and the resolved `iso_storage` pool together, skipping DLB registration under the strict guard when any of the three is non-shared.

**Status.** Meets.

**Resurrection authority left to BOSH.**

**Best practice.** A CPI that lets PVE's own HA subsystem restart a failed guest independently of the BOSH Director creates two competing authorities over the same VM's lifecycle.

**CPI behavior.** Every VM the CPI creates carries `onboot: 0` — PVE never auto-starts it on node boot or recovery outside of an explicit HA resource restart the CPI itself registered. Instance-level resurrection is BOSH's resurrector, not PVE's own restart-on-failure policy.

**Status.** Meets.

## 7. API usage

**Task polling with hard timeouts that stay retriable.**

**Best practice.** A CPI request that blocks forever on a stuck PVE task wedges a BOSH Director worker slot indefinitely; a polling timeout must still resolve as a retriable error so the Director can re-drive rather than hang.

**CPI behavior.** Task awaits carry a hard maximum wait (300 s default, 600 s for the stemcell upload/create path) and, on timeout, return a retriable error rather than blocking past the bound.

**Status.** Meets.

**Adaptive, progress-aware task polling.**

**Best practice.** A fixed poll interval either wastes API calls against a slow-moving task or adds needless latency to a fast one; deriving the next poll interval from the task's own reported progress tightens both.

**CPI behavior.** `pve.task_poll_adaptive` (default `false`) derives each poll interval from the task's reported progress, clamped between 1 s and 10 s, instead of a fixed cadence.

**Status.** Configurable — recommended: enable on clusters running long-running stemcell uploads or clones under sustained load.

**Randomized band-scan VMID allocation with purpose-segregated bands.**

**Best practice.** PVE's own `/cluster/nextid` endpoint is a race between concurrent callers; a fixed scan start point compounds the collision rate under concurrent `create_vm` calls, and mixing VMs, persistent disks, and templates in one ID space makes it impossible to reason about capacity per purpose.

**CPI behavior.** `NextVMID` scans its configured range from a randomized start point with a retry-on-conflict loop backstopping the rare collision. VMs (`100`-`8999` by default), persistent disks (`9000`-`29999`), and stemcell templates (`30000`-`30999`) each allocate from a dedicated, non-overlapping band — see [Configuration — VMID ranges](configuration.md#vmid-ranges).

**Status.** Meets.

**Three-tier retry taxonomy plus quorum-aware pacing.**

**Best practice.** A transient network blip, a PVE server-side worker-pool exhaustion signal, and a per-storage lock timeout are three distinct failure shapes with three distinct correct retry paces; treating them all with one generic backoff either retries too fast against a condition that needs minutes, or too slow against one that clears in seconds.

**CPI behavior.** The CPI classifies errors into transport-transient, PVE pushback (server-side rate limiting or worker-pool exhaustion), and storage-lock timeout, each on its own retry curve. Cluster quorum loss is classified separately (`not quorate`/`no quorum`) and routed onto the storage-lock curve (2 s to 30 s, 10 attempts) rather than the faster transport curve, since quorum loss is a minutes-scale condition, not a momentary blip.

**Status.** Meets.

**Cross-process cluster lock.**

**Best practice.** Two CPI processes (or two Directors sharing a cluster) racing the same VMID-allocation or template-build window can collide in ways a single process's own retry logic cannot see.

**CPI behavior.** `pve.cluster_lock_mode` (default `off`) offers a `pool`-based advisory lock using sentinel PVE resource pools (`bosh-lock-*`) to serialize cross-process critical sections when enabled.

**Status.** Configurable — recommended: `pool` on any cluster where more than one CPI process (or Director) can run against the same PVE cluster concurrently.

**Guest-config-lock detection with actionable recovery.**

**Best practice.** A generic 5xx response for "the VM's config is locked" leaves an operator guessing; naming the lock type and the exact recovery command turns an opaque retry loop into a one-line fix.

**CPI behavior.** `IsVMConfigLocked` detects PVE's `VM is locked (<type>)` shape and wraps the error with the lock type and the `qm unlock <vmid>` recovery command. Under a literal `root@pam` password identity, `delete_vm` and rollback cleanup retry once with `skiplock=true` automatically; under any other identity (including an API token, even one owned by `root@pam`), no retry is attempted — PVE's `skiplock` check accepts only the literal `root@pam` password identity — and the original lock error surfaces with the lock type and recovery command attached. See [Troubleshooting — VM is locked](troubleshooting.md#vm-is-locked).

**Status.** Meets.

**Idempotent SDN conflict handling.**

**Best practice.** A `create_network`/`delete_network` retry after a partial prior success should converge to the same end state, not fail on "already exists" or "not found."

**CPI behavior.** Vnet, subnet, and bridge creation treat a 409 conflict as success (the entity already exists from a prior attempt); zone creation is guarded by an existence probe instead, so a genuine concurrent zone-create race still surfaces as an error. `ErrSDNNotFound` during deletion is swallowed throughout the delete path, making repeated or concurrent `delete_network` calls idempotent.

**Status.** Meets.

## 8. Security

**Token-based auth, a non-root pve-realm service user, and least privilege with per-endpoint mapping and optional pool scoping.**

**Best practice.** Authenticating as `root@pam` with a password gives a CPI (and anything that can read its credential) full datacenter administration; a dedicated realm user with a scoped role and a privilege-separated token bounds the blast radius to exactly what the CPI's own API calls need.

**CPI behavior.** The CPI authenticates via `pve.api_token` (preferred) or `pve.password`, never requiring `root@pam`. [PVE API permissions](pve-api-permissions.md) documents a minimum-privilege `bosh@pve` custom-role setup mapped to the exact privilege each CPI call needs, plus an optional `pve.vm_pool`-scoped variant that bounds VM-mutation ACLs to a resource pool instead of cluster-wide `/vms`.

**Status.** Meets, Configurable for the pool-scoped variant — recommended on any cluster shared with VMs the CPI does not manage.

**TLS CA-chain verification by default, no fingerprint pinning.**

**Best practice.** Full CA-chain verification against a proper certificate authority is the durable trust model; certificate-fingerprint pinning is brittle across routine certificate rotation and offers no advantage the CA chain does not already provide.

**CPI behavior.** `pve.verify_ssl` defaults `true`; `pve.ca_cert` supplies a custom CA bundle when the PVE cluster's certificate is not in the system trust pool. There is no fingerprint-pinning mechanism to misuse instead.

**Status.** Meets.

**Credential scrubbing on every log sink.**

**Best practice.** A CPI that logs its own request/response traffic risks leaking API tokens, passwords, or presigned URL signatures into a log file an operator did not expect to be sensitive.

**CPI behavior.** Every log sink runs through a shared redaction layer that scrubs credentials before a line is emitted — including presigned-URL signature query parameters and the userinfo segment of any URL (`scheme://user:pass@host`, masked through the last `@` so passwords containing `@` are covered in full).

**Status.** Meets.

**Protection flags on parked disks, with fail-closed destroy guards.**

**Best practice.** A detached persistent disk parked on a placeholder VM (the `detached_disk_strategy: parked` model) must survive an accidental destroy of that placeholder just as reliably as an attached disk survives an accidental `delete_vm`.

**CPI behavior.** Every parker VM is created with `protection: 1`, and the parked-disk destroy path fails closed rather than silently proceeding when protection cannot be confirmed. See [Persistent disk lifecycle strategy](persistent-disk-strategy.md).

**Status.** Meets.

**Foreign-disk detach-and-refuse on delete.**

**Best practice.** `delete_vm` destroying a persistent disk that happens to still be attached to the VM being deleted — because BOSH's own detach call raced or failed — is a data-loss bug; the correct behavior is to detach and preserve the disk, not destroy it along with the VM.

**CPI behavior.** Before destroying a VM, `delete_vm` identifies any attached disk whose volid encodes a different owning VMID than the one being deleted (a foreign disk) and detaches it, leaving the volume intact on storage, rather than letting the VM's own destroy call take it along. The delete fails closed if a foreign disk cannot be detached.

**Status.** Meets.

**Datacenter-firewall enforcement probe.**

**Best practice.** PVE's datacenter firewall master switch defaults off; an operator who configures `security_groups` or `allowed_address_pairs` and watches every API call succeed has no signal that zero packets are actually being filtered until an incident proves it.

**CPI behavior.** Whenever a VM is created with `pve.vm_firewall`, `security_groups`, or `allowed_address_pairs` in play, `create_vm` probes `GET /cluster/firewall/options` once per PVE cluster per process — so a Director running several cpi-config entries probes each cluster — and logs a Warn naming the gap when the datacenter master switch is off. The probe is diagnostic only — a probe failure (e.g. a token missing `Sys.Audit`) logs and proceeds rather than blocking the VM.

**Status.** Meets.

## 9. Backups and snapshots

**Disk-op deferral during backup, clone, and migrate locks — on by default.**

**Best practice.** A `delete_disk` that races a running vzdump/PBS backup, clone, or migrate targeting the same guest can corrupt the in-flight operation.

**CPI behavior.** `pve.disk_delete_state_guard` defaults `on` and defers `delete_disk` with a retriable error whenever the attached guest holds a `backup`, `clone`, `migrate`, `snapshot`, `rollback`, or `create` lock — see [Disk-op deferral while the holding guest is locked](#2-storage) above.

**Status.** Meets.

**Disk-only snapshots, no RAM state.**

**Best practice.** Including guest memory state (`vmstate`) in a routine snapshot roughly doubles snapshot size and time for a capability BOSH's own recreate-and-resurrect model never uses.

**CPI behavior.** `snapshot_disk` calls PVE's snapshot API without a `vmstate` option, so every snapshot is disk-only by default — matching what PVE itself would do without an explicit opt-in the CPI never makes.

**Status.** Meets.

**CPI power-cycle effect on incremental-backup bitmaps, disclosed with scheduling guidance.**

**Best practice.** Any operation that restarts a guest's QEMU process — not only an explicit reboot — drops Proxmox Backup Server's incremental dirty-bitmap for that guest, forcing the next backup to re-read the disk in full; scheduling backup windows during a rolling deploy compounds that cost across every instance recreated mid-window.

**CPI behavior.** `delete_vm`, a hard reboot (or the soft-reboot fallback to one), and any Director recreate — including a stemcell update, which the Director always implements as delete-then-create — all restart the guest's QEMU process. See [Operations — Backups (PBS) interplay](operations.md#backups-pbs-interplay) for the full effect, the `dirty-bitmap status: created new` task-log marker, and using the CPI's own provenance tags and VMID bands to schedule and scope backup jobs around it.

**Status.** Meets — this is a disclosed platform interaction, not a CPI-side mitigation; no amount of CPI logic can prevent PVE from dropping the bitmap on a QEMU process restart, so the correct response is scheduling, not code.

**Snapshot naming and lifecycle coupling disclosed.**

**Best practice.** PVE has no per-disk snapshot primitive — a snapshot always targets the whole VM — and an operator relying on `snapshot_disk` should know that up front, along with how the CPI names the snapshots it creates.

**CPI behavior.** `snapshot_disk` locates the VM currently holding the target disk and snapshots that VM as a whole; the generated name is `bosh-<unix-timestamp>-<8-hex>`. `snapshot_disk` refuses to target a parker VM outright (a PVE snapshot would otherwise entangle every parked disk from every deployment sharing that parker into one snapshot).

**Status.** Meets — this is a PVE-level limitation the CPI discloses rather than works around; no per-disk snapshot API exists for PVE to expose.

## 10. Operational limits honored

**8-character vnet names validated before the API call.**

**Best practice.** PVE enforces a strict 1-8 lowercase-alphanumeric constraint on SDN vnet identifiers; discovering that constraint via a rejected API call after other resources have already been created leaves partial state to clean up.

**CPI behavior.** `create_network` validates the vnet name against PVE's exact constraint (`[a-z0-9]{1,8}`) before issuing any SDN mutation, returning a clear error to the Director rather than a PVE-side rejection mid-sequence.

**Status.** Meets.

**PVE integer-boolean decoding.**

**Best practice.** PVE's API renders boolean fields as the JSON integers `1`/`0`, not JSON's own `true`/`false`; decoding such a field into a Go `*bool` via the standard library silently drops every occurrence instead of erroring, which is a much harder bug to notice than a decode failure would be.

**CPI behavior.** Every PVE boolean-shaped field the CPI decodes (template membership, sparse-provisioning flags, and others) goes through a dedicated `pveBool` type whose `UnmarshalJSON` understands PVE's integer convention, rather than the standard library's own bool decoding.

**Status.** Meets.

**Pool-membership single-pool rule.**

**Best practice.** A PVE VM can be a member of exactly one resource pool at a time; configuring the CPI to assign the same VM into two conflicting pool roles is a misconfiguration that should be caught before it ever reaches PVE.

**CPI behavior.** `pve.vm_pool` is validated at config load to differ from both `pve.stemcell_template_pool` and the reserved `bosh-lock-*` sentinel-pool namespace, so a manifest cannot configure a VM for two conflicting pool memberships. Both properties default to a distinct, non-empty pool name (`bosh` for VMs, `bosh-templates` for stemcell templates) that the CPI creates on demand, so this separation holds even for a manifest that sets neither property explicitly.

**Status.** Meets.

**Resource pool layout: segregated defaults, create-if-missing, and opt-in reaping.**

**Best practice.** Grouping workload VMs and stemcell templates under the same resource pool defeats the pool as an ACL boundary — a grant scoped to that pool for VM operations would also reach the shared templates every deployment clones from. A pool layout should keep the two separate by default and give an operator a low-effort way to scope pools further per deployment or per business unit without hand-editing every manifest.

**CPI behavior.** `pve.vm_pool` (default `bosh`) and `pve.stemcell_template_pool` (default `bosh-templates`) are separate, create-if-missing pools: the CPI creates whichever one it needs the first time it is needed, tagging it with a `managed by bosh-pve-cpi` provenance comment, so no operator setup step is required before the first deploy. `pve.vm_pool_template` renders a per-deployment or per-business-unit pool name from `{prefix}`, `{director}`, `{deployment}`, and `{instance_group}` when neither a call-level nor a `vm_type` `cloud_properties.pool` override is set, letting an operator running several deployments (or several teams, "blocs") against one PVE cluster get a distinct pool per deployment without setting `cloud_properties.pool` in every manifest. Recommended: leave the two defaults as-is unless a shared-cluster ACL design calls for narrower scoping (see [PVE API permissions — shared-cluster variant](pve-api-permissions.md#7-shared-cluster-variant-scoping-vm-mutation-to-a-resource-pool)), and reach for `pve.vm_pool_template` before hand-rolling a per-deployment `cloud_properties.pool` in every vm_type.

**Status.** Meets, Configurable for the template layer and the opt-in reaper.

**Opt-in empty-pool reaping trade-off.**

**Best practice.** Pools created for short-lived or scaled-down deployments can accumulate indefinitely once their last VM is destroyed; automatically deleting them needs to weigh the operator's PVE UI tidiness against the risk of ever touching a pool it did not create.

**CPI behavior.** `pve.pool_reap_empty` (default `false`) trades pool tidiness for one extra `delete_vm`-time lookup: when enabled, `delete_vm` confirms the pool carries the CPI's own provenance comment, then attempts the delete and relies on PVE itself to refuse it if the pool is not actually empty — a not-empty or already-gone refusal is tolerated as an expected race rather than an error. Leaving it `false` is the safer default for a cluster where pools are also managed by other tooling; enable it on a cluster where every `managed by bosh-pve-cpi` pool is understood to be CPI-owned end to end.

**Status.** Configurable — recommended on any cluster where CPI-managed pools would otherwise accumulate unbounded.

**Migration note.** `pve.vm_pool` and `pve.stemcell_template_pool` used to default to no pool assignment at all; they now default to `bosh` and `bosh-templates` respectively, created on demand. A deployment upgrading without touching either property starts assigning VMs and templates into these auto-created pools and needs `Pool.Allocate` on the CPI's PVE token (see [PVE API permissions](pve-api-permissions.md)) — `Permissions.Modify` is not required, and `Pool.Audit` is a separate, opt-in requirement of `pool_reap_empty`/`cluster_lock_mode: pool`, not of this default path. Set `pve.vm_pool: ""` and/or `pve.stemcell_template_pool: ""` explicitly to keep the previous no-pool behavior. See [Configuration — Migration / Upgrade Notes](configuration.md#migration--upgrade-notes) for the full detail.

**Node-scoped API semantics.**

**Best practice.** Most of PVE's QEMU and storage endpoints are scoped to a specific node in the URL path; calling them against the wrong node (a stale or cached node value) either 404s or silently operates on the wrong guest.

**CPI behavior.** The CPI resolves a VM's current node via a cluster-wide scan immediately before every node-scoped call, rather than caching a node value across operations — see [Cluster-scan node resolution](#6-cluster-placement-and-ha) above.

**Status.** Meets.
