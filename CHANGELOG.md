# Changelog

All notable changes to this project are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Entries describe operator-visible change: CPI behavior, properties, packaging, and the
documentation set. Refactors, test work, and CI plumbing appear only when they change
what an operator sees or how a release is produced. The `Unreleased` section collects
work as it lands; cutting a release renames it to the new version and dates it. See
[CONTRIBUTING](CONTRIBUTING.md#releasing-maintainers) for the release procedure.

## [Unreleased]

## [0.5.0] - 2026-08-23

### Added

- Single-shared-template stemcell topology. On a multi-node cluster whose `vm_storage` is shared (for example Ceph RBD), `create_stemcell` now accepts a node-local file pool for the qcow2 staging (`stemcell_storage`) under the default `template` strategy: one cache template is built, its disk lands on the shared `vm_storage`, and `create_vm` clones it to any node cross-node, so neither `stemcell_replicate_local` nor a shared file storage is required. The multi-node rejection still applies when `vm_storage` is node-local, when its shared-ness cannot be determined, or under `stemcell_strategy: import` (which reads the qcow2 from every VM's own node). A `stemcell_storage` that resolves to a block-only pool (rbd, lvm, lvmthin, zfspool) is now rejected up front with guidance to stage on a file-capable pool, instead of failing later with an opaque PVE upload error. The relaxation covers every `create_stemcell` mode the same way (tarball upload, `source_url` server-side fetch, `image_url` CPI-side fetch, and pre-uploaded `image_id`), a `stemcell_template_node` that cannot read the node-local staging pool is rejected with guidance instead of failing opaquely at template build, `delete_stemcell` sweeps stray per-node qcow2 copies that the replica list cannot name, and a `create_vm` import fallback on a node the staging pool cannot serve now returns a retriable error explaining the topology and the rebuild remedy instead of misleading re-upload guidance.

### Fixed

- Transport and API errors no longer reach the Director with the wrong retriability. Mid-lifecycle connection races (a server-closed idle connection, `net.ErrClosed`) now classify as transient in the CPI's own stemcell-fetch client, not only inside the SDK; a resolved non-429 4xx is a permanent verdict even when its body contains a pushback phrase such as "got timeout"; failed task exits under adaptive polling route through the same classifier as the default poller, so storage-lock and quorum contention in a task exit stays retriable; and a dozen terminal wraps that flattened an established retriable classification into a permanent cloud error now preserve it, so exhausted-but-transient storage faults surface as retriable to the Director. The JSON-RPC dispatcher's fallback consults the transient classifier before minting a permanent error from a raw transport failure.

- Every destructive branch now proves VM absence with authoritative per-node config probes. The `/cluster/resources` index can trail node-local state by minutes, and an index miss was read as "the VM is gone" by `delete_vm`, `has_vm`, `get_disks`, `set_vm_metadata`, `reboot_vm`, `delete_snapshot`, `attach_disk`, and `detach_disk`. On a miss, cluster membership is now enumerated from corosync-backed `/cluster/config/nodes` and every node's own config is probed; absence is concluded only when every node answers cleanly, and any unreachable node yields a retriable error instead. The `delete_vm` straggler sweep re-reads the authoritative config and re-tests its tags before destroying, so a reused VMID with a stale index row is never destroyed, and `delete_stemcell` refuses the branch that deletes the backing qcow2 unless its cluster-wide template sweep completed.

- Retried PVE mutations no longer replay an already-committed request destructively. A POST can commit server-side while its response drops in transit; each retried site now converges on the goal state, sweeps its own committed partial, or adopts the resource it created: `resize_disk` re-reads the size and treats at-target as success instead of re-submitting a landed grow; the stemcell upload re-awaits its own task after a drop and sweeps the committed partial before re-uploading; `reboot_vm` and the `create_vm` start convert a failed start into success when the VM is already running; `snapshot_disk` accepts a fully committed snapshot on replay; the disk-migrate mover is adopted only on an exact nonce match; and a destroy rejected by a config lock waits a bounded window for the lock to clear and re-issues once before tagging the VM for the reaper.

- Cleanup paths retry through the same storage-lock contention that triggered them. Rollback destroys, template destroys, orphan and ephemeral volume sweeps, replica sweeps, pool operations, the disk-migrate submit, and the unused-slot detach sweep were all single-shot, so one cfs-lock timeout permanently leaked the resource being reclaimed; each now rides the storage-lock retry curve with a role-scoped attempt budget, and resolved 500-text verdicts (pool already exists, VM already a pool member, config already gone) short-circuit as answers instead of spending the budget.

- Cluster-wide decision paths read authoritative per-node guest listings instead of the lagging index. The VMID allocator unions the per-node listings into its used-set, so a VMID a peer created moments ago can no longer be reallocated inside the index lag window; the `create_vm` rollback sweep refuses to destroy a VM whose identity it cannot prove; disk-holder scans, the advertised-route sole-owner refcount, template lookups, and the IP-conflict guards all enumerate per node, with absence proofs failing loudly on any unlistable member and advisory readers tolerating members the quorate cluster itself reports offline; `delete_stemcell` compares director refs through the create-env sentinel and feeds its orphan prune from the VMIDs this call destroyed; and anti-affinity rules drop a member only when a complete enumeration proves its guest gone.

- A lagging `/cluster/resources` index no longer hides a freshly built cache template. On a loaded cluster the index can trail a just-frozen template by minutes, and both consumers of the cluster-scoped sha-tag lookup mistook that lag for absence: `create_vm` fell through to the import fallback (which fails outright when the staging qcow2 lives on another node's local storage), and `delete_stemcell` took the no-template branch, deleting the staging qcow2 while orphaning the live template. The sha-tag lookup itself now reads authoritative `GET /nodes/<node>/qemu` listings, which serve node-local guest configs and do not lag, so neither consumer touches `/cluster/resources` at all; on top of that, `create_vm` probes the placement node and the template's home node (`stemcell_template_node`, or the configured `node`) inside its bounded re-check and clones the template it finds, and `delete_stemcell` sweeps every cluster node (create_stemcell can legitimately build on a non-default node) so the template and all its per-node replicas are found and destroyed.

- The `create_vm` rollback no longer destroys an attached persistent disk. When a create failed after the persistent disk was already attached (agent configure, VM start, or a post-start check, including the node-fallback retry), the rollback purged the VM with disk destruction enabled and PVE destroyed every referenced volume, including the persistent disk a `bosh recreate` was re-attaching. The rollback now detaches foreign persistent disks to safety first, exactly as `delete_vm` does, and when that protection cannot complete it preserves and tags the VM instead of purging it — an orphaned VM is recoverable, a purged persistent volume is not. The fast-path delete's straggler sweep gained the same protection: a `bosh-deleting` VM whose foreign-disk detach failed on the original delete is now deferred to the `delete_vm` retry instead of being destroyed with the disk still attached. Both paths also treat pmxcfs's "Configuration file ... does not exist" answer as the vanished-VM condition it is, rather than refusing to proceed.

- A connection dropped mid-request is retried instead of failing `create_vm`. Under a bursty deploy pveproxy can close an in-flight connection (the configdrive ISO upload was the live victim, surfacing as `Post ...: EOF` after one attempt), and that shape fell through every transient classifier: the upload was not retried and the error reached the Director as non-retriable. Mid-request drops (EOF, unexpected EOF, connection reset, broken pipe) now classify as transient transport faults, so the configdrive upload retries with a fresh body stream (clearing the target filename first, in case the dropped request actually committed the file; a clear that itself hits a retryable fault rides the same backoff curve instead of pressing into a guaranteed duplicate rejection), and an exhausted retry budget surfaces retriable.

- Ephemeral-disk allocation backs off in place on storage lock contention. Parallel `create_vm` calls allocating ephemeral volumes on one shared pool contend on the cluster storage lock, and the losers failed the whole call back to the Director, which re-ran clone, boot, and configure only to re-enter the same contention window. The allocation now retries in place on the same storage-lock backoff curve `create_disk` uses (with the default attempt budget), and when every attempt fails, a volume the failed request may still have committed is swept best-effort before the error surfaces, so the Director's redo does not leave an orphan behind.

- Storage lock contention on shared pools is retried instead of failing the deploy. PVE's cluster-wide storage lock surfaces as `cfs-lock 'storage-<pool>' error: got lock request timeout` when concurrent clone, create, or destroy tasks contend on one shared pool — routine during mass VM creation from a single template on RBD. That shape now classifies as a transient storage-lock timeout everywhere the existing file-lock timeout does, so `create_vm` retries the clone rather than surfacing a terminal error.

- Retriable errors now cross the wire as classes the Director recognizes, so its own retries engage. The dispatcher previously serialized `Bosh::Clouds::RetriableCloudError`, which the Director knows only as an abstract base class: it rejected the response as `Unknown CPI error`, discarded `ok_to_retry`, and failed the task on the spot, so a transient fault that outlived the CPI's internal retries (a mass-create burst exhausting the configdrive upload budget, for example) killed the deployment instead of triggering the Director's create retry. A retriable error escaping `create_vm` now serializes as `Bosh::Clouds::VMCreationFailed` with `ok_to_retry: true`, which the Director's create step retries up to `max_vm_create_tries` (default 5) with fresh CPI calls; retriable errors from other methods serialize as `Bosh::Clouds::CloudError` with the flag preserved, since the Director has no retry loop for them. The same translation maps the internal `DetachedDisk` type to the Director's `DiskNotAttached` and the internal stemcell-validation and snapshot-guard types to plain `CloudError`, so no CPI response can surface as `Unknown CPI error`.

- A destroy task that completes with `WARNINGS: N` no longer fails `delete_vm`. PVE ends `qmdestroy` with a warnings exit when the VM was removed but a disk could not be deleted under storage-lock contention ("Could not remove disk ..., check manually"); the plain task await treated any non-OK exit as failure, turning a completed delete into a CPI error. Both await paths now accept completed-with-warnings, matching the PVE UI's own reading; any leftover volume is left to `scripts/disk-audit`.

### Changed

- The bundled Proxmox SDK moves to proxmox-apiclient-go v3.9.1, which preserves the HTTP status chain through login failures, retries auto-login after a failed attempt, and surfaces server-closed-idle connection races as its typed connection error.

- `pve.api_idle_conn_timeout_sec` now defaults to 15 seconds when unset, instead of inheriting the SDK's 90-second idle window, which outlives pveproxy's server-side keep-alive and sets up a reused-dead-connection race; explicit values pass through, and `90` restores the previous behavior.

- The `create_vm` rollback cleanup budget grows from two minutes to ten. Everything the rollback does draws on one bounded context (the stop and destroy task awaits, foreign-disk protection with per-disk detaches, the bounded config-lock-clear wait, and the destroy retry budgets), and the old bound could expire mid-cleanup on a loaded node, orphaning the VM it existed to reclaim. Exhausting the bound still fails closed: the purge is refused and the VM is preserved and tagged.

- The packaged Go toolchain moves from 1.26.6 to 1.27.0: the `golang-1.26` BOSH package is now `golang-1.27` with the `go1.27.0.linux-amd64.tar.gz` blob, `src/pve_cpi/go.mod` requires `go 1.27.0`, and CI builds in the digest-pinned `golang:1.27` image.

## [0.4.0] - 2026-08-21

### Added

- Cross-node persistent-disk migration on attach, on by default. PVE's `move_disk` refuses to reassign a volume between VMs on different nodes, so a stable-ID disk stranded on node-local storage away from its VM used to fail `attach_disk` outright. Under `pve.disk_migration: on_attach` (the default; `""` means the same) the CPI isolates the disk onto a fresh single-purpose mover parker on the disk's node, offline-migrates the never-started mover to the VM's node, attaches from it with the usual same-node reassignment, and destroys the mover. Setting `pve.disk_migration: off` restores the hard refusal, which names the knob and the manual `qm migrate` escape; the setting is overridable per cpi-config entry as `pve_disk_migration`. Disks created before stable disk identities existed are still refused: the migration renames the volume, and a legacy CID is the volume name. See [Known Limitations](docs/limitations.md).

- Stemcell calls follow the storage's owning nodes. When the stemcell storage carries a `storage.cfg` nodes restriction that excludes the configured node, the upload, server-side download, and content-listing calls are addressed to the restriction set's first owning node instead of failing with "storage not available on node". Replication fan-outs now end with one aggregate summary line, and a stemcell `download-url` is fetched server-side by the PVE node when possible, with a bounded local-fetch fallback.

- Scheduled acceptance workflow. `.github/workflows/acceptance.yml` runs the director-upgrade certification and the BOSH Acceptance Tests unattended every Saturday against a live PVE lab, resolving the published releases to test, and lands the run reports on `main` through an auto-merged pull request. The lifecycle and e2e test tiers now write committed run reports under `docs/certification/` the same way the BATS and upgrade tiers do. See [Scheduled Acceptance Workflow](docs/certification/scheduled.md).

### Changed

- The vendored Proxmox API client SDK is v3.9.0.

### Fixed

- `pve.api_token` accepts the full Authorization-header form (`PVEAPIToken=user@realm!name=uuid`) as well as the bare `user@realm!name=uuid` form.

- Stemcell template VMs are created directly into their resource pool and tagged after creation, so reduced-ACL tokens whose `VM.Config` rights are scoped per option no longer fail `create_stemcell`.

- `attach_disk` returns a retriable error when a just-parked disk's parker VM is not yet visible in the cluster listing, instead of a terminal one.

- An explicit stemcell node pin outranks the owning-node retarget in the light-stemcell fetch path.

## [0.3.0] - 2026-08-21

### Added

- Stable disk identity. `create_disk` mints a `bpd-` plus 16-hex identity token that rides the drive's `serial=` attribute for the life of the disk, and attach resolves a disk by serial scan first, provenance sentinel second, and birth volid last, so a disk keeps its CID across the volume renames that parking and reassignment cause. Ownership moves by `move_disk` reassignment between the holder and the parker slot rather than by config-line surgery. CIDs minted before this release carry no token and resolve by volid forever. See [Stable disk identity and ownership transfer](docs/persistent-disk-strategy.md#stable-disk-identity-and-ownership-transfer).

- `create_disk` parks fresh disks under the parked strategy, fail-closed: a disk that cannot be parked at creation is not left floating.

- The disk CID records its create-time format, and block-native storage records `raw`, so later attaches rebuild the drive string from what was actually created rather than from current config.

- Operations on a parked disk whose parker anchor has vanished are refused with the repair path named, instead of proceeding against storage the parker no longer references.

- IPv6 dual-stack networks. Networks that share a `nic_group` plan onto one NIC with both `ip=` and `ip6=` addresses, and the BATS runner grows an opt-in dual-stack pass (`--only ipv6`).

- Offline wire-protocol conformance suite. `make test` now drives the compiled CPI binary against a refused endpoint and asserts exactly one well-formed response envelope per request, exit 0, retriable error typing with `ok_to_retry`, an `api_version` handshake matrix pinned to 2, and a stdout that carries nothing but envelopes, with no lab required.

- Consolidated [Known Limitations](docs/limitations.md) page stating every scope limit in one place.

- `update_disk` option updates are now durable. Every option change is recorded as a per-disk override map, and each attach builds the drive string by merging global `disk_performance` config, then the options recorded in the disk CID at `create_disk` time, then the recorded overrides — rightmost wins, and an empty-string value deletes the key. Previously a detach/attach cycle rebuilt the drive string from config and CID options alone, silently reverting every operator update. The record lives on the holder VM's description (`bosh_disk_opt_overlays` sentinel key) while attached and rides the parker's `bosh_parked_disks` provenance entry (new optional `opts` field) while parked; `update_disk` writes it fail-closed before touching the drive, and the invariant guard (`pve.disk_perf_invariant_mode`) treats recorded updates as part of the expected baseline rather than as divergence. A parked disk can now be updated too: the change is recorded (and `size` applied to the volume directly) and takes effect at the next attach, where the old behavior wrote options onto the parker's drive string and lost them at unpark. Two paths still drop the record, with a logged warning: a detach under `detached_disk_strategy: free` (no carrier exists for it), and `delete_vm`'s plain detach of a still-attached legacy foreign disk. See [Durable disk option updates](docs/persistent-disk-strategy.md#durable-disk-option-updates).

### Changed

- The gzip-compressed `pvz-` disk CID fallback now engages automatically whenever the plain `pvd-` envelope would exceed the 255-character bound of MySQL-backed Directors, instead of failing `create_disk` unless `pve.disk_cid_compression` was set. The stable-identity field made richly-annotated envelopes overflow in ordinary configurations, so the opt-in became a trap: a default-config `create_disk` under the parked strategy could fail outright. CIDs that fit stay `pvd-` and byte-identical, decode has always accepted both forms, and `pve.disk_cid_compression` is retained as an accepted no-op.

- `pve.vm_pool_template` now defaults to `bosh-{director}-{deployment}`, so every deployment gets its own auto-created resource pool (`bosh-<director>-<deployment>`; `bosh-create-env` for `bosh create-env` environments) instead of everything landing in the single static `bosh` pool. Each VM records how its pool was resolved in a `bosh_pool` sentinel on its PVE description, and `set_vm_metadata` re-renders the template from that record on every deploy, moving a template-placed VM whose pool has drifted; VMs placed by `cloud_properties.pool`, a `vm_type` profile, or the static `pve.vm_pool` are never moved. Existing VMs sitting in the static `bosh` pool are adopted into their deployment's pool on the next deploy. Upgrade notes: the CPI token needs `Pool.Allocate` and `Pool.Audit` at the `/pool` parent (per-deployment names are dynamic, so a per-poolid grant cannot cover them); reduced-ACL clusters whose token cannot create pools should set `pve.vm_pool_template: ""` to keep the single-pool model, and `create_vm` names that exact fix when pool creation is denied. A resolved pool name equal to `pve.stemcell_template_pool` is now rejected at `create_vm` time.

- `pve.pool_reap_empty` now defaults to `true`: a per-deployment pool is deleted when the synchronous `delete_vm` path destroys its last VM. Only pools carrying the CPI's `managed by bosh-pve-cpi` provenance comment are ever reaped, the static `pve.vm_pool` and `pve.stemcell_template_pool` are refused by name, and the fast-path delete still skips the reaper. Set the property to `false` explicitly to keep empty pools; the key is now always rendered into the CPI config, so an explicit `false` takes effect (previously the template could not express it).

- The parker VMID band now resolves under every `detached_disk_strategy`, at the job level and in every cpi-config entry alike: unset bounds fill with the built-in `90000`–`90999` whether the strategy is `parked`, `free`, or stood down by a band collision. Under `free` the band is read-only: nothing allocates a parker VMID in it, a band overlap is accepted rather than rejected at load, and disks parked while `parked` was in effect are recognized and unparked on their next `attach_disk` or `delete_disk` instead of being refused until an operator restores the band by hand. Switching to `free` no longer needs the band carried forward, and the load-time warning for `free` without a band is gone with the state it warned about. The stranded-parker refusal remains for a band moved away from VMIDs where parker VMs still live.

- The vendored Proxmox API client SDK is v3.8.6.

### Fixed

- A snapshot that blocks the detach reassignment defers the park instead of failing `detach_disk`; the disk parks on its next detach once the snapshot is gone.

- `create_vm` with `disk_cids` runs the full attach guard path, so a foreign holder or a stale parker is caught at VM creation the same way `attach_disk` catches it.

- Task-await log lines follow the UPID's effective node, so logs name the node that actually ran the task.

## [0.2.0] - 2026-08-19

### Added

- BOSH Director Upgrade Test runner: `./scripts/certify`, which stands a Director up on the previous CPI release, deploys the upstream [certification release](https://github.com/cloudfoundry/bosh-cpi-certification) under it, upgrades the Director onto the latest release over the same state, and recreates the deployment, asserting that disk CIDs and the disk attachment survive the version change. Ships with the [BOSH Director Upgrade Test](docs/certification/upgrade.md) guide and a committed [run record](docs/certification/upgrade/README.md).

- Parked-disk lifecycle coverage in the integration harness: `scripts/lifecycle` drives a detached disk through both `detached_disk_strategy` settings against live PVE and verifies the parker VM, its protection flag, its provenance sentinel, and every refusal path out-of-band; `scripts/test integration` grows `tier1.parked_disk` (band and toggle) and `tier1.stemcell_replicate_local` for multi-node labs with node-local stemcell storage.

### Changed

- `pve.detached_disk_strategy` now defaults to `parked`: a detached persistent disk is held on a protected parker VM instead of floating as an unattached storage volume, so PVE shows who owns it and `protection=1` blocks an accidental delete. The previous free-floating behavior is still available by setting the property to `free`. Three consequences for existing deployments that never set the property: detach and attach each spend more PVE API calls, and `attach_disk` and `delete_disk` now resolve the volume's holder on every call, which costs one cluster listing plus a config read per VM in the cluster; each PVE node accrues one protected `bosh-parker-<n>` VM once a disk is parked there, which no CPI call ever deletes (`scripts/disk-audit` reports empty parkers along with the two commands that remove one); and the parker VMID band (`90000`–`90999` by default) must not overlap the VM, persistent-disk, or stemcell-template bands. A config that sets neither the strategy nor a parker band and whose other bands already reach into that window keeps the previous free-floating behavior for that load and says so in a warning, rather than failing to load, so the upgrade cannot take a running deployment down. Disks already detached stay free-floating and are picked up normally by the next `attach_disk` or `delete_disk`; `manifests/cpi-config.yml` shows the per-entry parker bands a shared-storage multi-cluster layout needs.

- **Before rolling back to a release older than this one, drain the parkers.** A release without the parked strategy has no unpark step, so it would attach a volume the parker still references and leave two VM configs pointing at one volume. Set `pve.detached_disk_strategy: free`; on 0.2.0 itself also set `pve.parked_disk_vmid_range_start: 90000` / `_end: 90999` explicitly so the unpark probes keep running (releases after 0.2.0 resolve an unset band on their own). Nothing drains a parker on its own: each disk comes off when the Director next attaches or deletes it, so redeploy the instance groups that own them and `bosh delete-disk` any orphans. Run `scripts/disk-audit` until it reports no parked disks, and roll back after that.

- `delete_disk` now refuses to free a volume still held by a parker VM the configuration can no longer recognize, naming the property to set. Freeing it would leave the parker's slot referencing storage that no longer exists.

- `attach_disk` now refuses a volume that is already attached to a VM other than the target, naming the holder. PVE permits two VM configs referencing one volume and nothing downstream detects it, until whichever holder is destroyed takes the volume with it. When the holder turns out to be a parker VM outside the configured band, the message says which property to set to make the disk reachable again.

- `pve.detached_disk_strategy` is overridable per cpi-config entry as `pve_detached_disk_strategy`, so one cluster whose VMID topology has no room for a parker band can opt out while the rest of the deployment keeps the default.

- Parker VMs are adopted only by the director that created them. Each carries a `director--<uuid>` tag, and a park now skips parkers attributed to another director rather than filling them, which keeps two directors sharing one PVE cluster from entangling their volumes. Parkers with no attribution tag stay adoptable by anyone.

- Unparking a disk now clears the parker VM's `protection` flag for the length of the detach and restores it immediately after. PVE treats detaching a disk as a "remove disk" operation, which that flag blocks, so `attach_disk` and `delete_disk` could not retrieve a parked disk while it was set. The flag is restored even when the detach fails, and a failed restore is logged with the command that repairs it.

- Parking a disk resolves its current holder with a single cluster scan instead of two. The scan reads every VM config in the cluster, so this halves the dominant cost of `detach_disk` under the parked strategy.

- `snapshot_disk` and `delete_vm` recognize a parker VM by its `bosh-parker` tag rather than by the configured VMID band (`delete_vm` reads the tag from the cluster-resources row, falling back to the VM config when that row carries no tags at all). An operator who turns parking off without carrying the band forward would otherwise disarm both guards over exactly the parkers that change stranded: a PVE snapshot takes the whole VM, entangling every deployment's disks held on that parker, and the fast delete path issues a purge that bypasses `protection=1`. Neither guard costs an extra API call.

- Each cpi-config entry decides the parked default against its own VMID bands. An entry that widens `pve_vmid_range`, `pve_disk_vmid_range`, or `pve_stemcell_template_vmid_range` over the built-in parker band stands the default down for that entry with a warning, rather than failing every request routed to that cluster; an entry that narrows those bands back gets the default it would have had on its own. An entry that names `parked` itself, or that sets a parker bound of its own, is honored as written. The parker band ApplyDefaults fills in is no longer inherited as though the entry had typed it, so an entry that moves one bound gets the built-in fallback for the other instead of a value meant for a different cluster.

- The parker band's overlap check now applies only under the `parked` strategy. It exists to stop the CPI allocating a parker VMID another band will claim, and nothing allocates one under `free`, where the band is read-only and every parker classification also requires the `bosh-parker` tag. A deployment that opts out precisely because its VMID layout has no room for a parker band is no longer rejected for the overlap.

- Unparking a disk re-resolves its slot inside the protection window. `DetachDisk` names a slot, not a volume, so a slot looked up before the lock was a blind write by the time the detach ran: two overlapping unparks of one disk could detach a volume a concurrent park had just placed in the freed slot, silently un-parking a disk whose own `detach_disk` had already reported success. A volume found off the active bus but still named by an `unusedN` key is swept instead, and one already gone is an idempotent no-op.

- Parking and unparking one parker VM are serialized against each other by a per-VMID lock. Both write its `protection` flag, and PVE clears a detached disk in two requests: a park restoring `protection=1` between an unpark's two halves made PVE refuse the second one, leaving the volume referenced by an `unusedN` key no probe matches on while the unpark reported a retriable failure. The same lock stops a park claiming a slot an unpark is about to detach by name. The lock is the same `bosh-lock-<name>` sentinel-pool mechanism the anti-affinity and VMID locks use, and it is taken on every park and unpark rather than behind an opt-in, so the CPI's PVE identity needs `Pool.Allocate` on `bosh-lock-*`. Without it the window runs unserialized, with a warning, rather than failing the call; see [PVE API permissions](docs/pve-api-permissions.md).

- An unpark that detaches the volume but cannot clear the `unusedN` key PVE demotes it to now fails permanently, naming the parker and carrying the `qm unlink` sequence that clears the reference. The detach has already succeeded at that point, so a retriable failure would send the Director's retry past the holder scan (which matches active-bus slots only) and on to attaching a volume the parker still references. Two paths that previously reported such a sweep as clean, a config read that failed mid-sweep and a parker band overlapping the disk band, now report it as the unswept reference it is.

- LXC containers no longer break parking, unparking, or the holder scan. The cluster listing both use covers containers as well as VMs, and every read behind it goes through the QEMU config endpoint, which answers for a container with a pmxcfs "Configuration file ... does not exist" error rather than a 404. One container in the parker band used to fail every park and unpark on its node; one container anywhere in the cluster used to fail every `attach_disk`, `detach_disk`, and `delete_disk` of a persistent disk, retriably and forever, once parking became the default.

- A park that strands a reference now fails permanently on every exit, not just the one that ran out of slots. A slot lost to a concurrent park leaves the volume on an `unusedN` key; when the sweep that clears it fails, the park used to hand back a retriable error, or `ErrNoSlots`, either of which sends the volume to another parker or another attempt while the first parker still references it. Both references are invisible to the holder scan, and destroying either parker frees a live volume.

- A sweep that removes the `unusedN` key but cannot read the config back to confirm it now fails permanently and keeps the provenance entry. It used to report success on the grounds that a later read would catch a surviving reference; no later read happens, since an unpark of a volume already off the bus returns early and a park only sweeps a slot it lost in that same call.

- A full parker that still references the volume on an `unusedN` key is swept rather than skipped. Walking past it to the next parker is what puts one volume in two parkers' configs.

- Failures on the parker paths are classified rather than reported retriable by default. A missing `VM.Config.Disk` or `VM.Audit` grant never comes right on its own, and the Director was driving the park against it forever instead of surfacing the permission to grant. Config reads and mutating calls are classified separately: a read defaults to permanent, since its failure shapes are enumerable, while a mutating call defaults to retriable, since PVE reports plenty of transient conditions as prose no classifier recognizes and a park that failed permanently on one of those would leave the disk free-floating. `detach_disk` no longer flattens the result: a denied grant or an exhausted VMID band reaches the Director as the permanent condition it is, naming what to fix.

- Work inside a parker's protection window runs under a deadline derived from the window lock's own TTL, and every retry loop inside it is bounded. Bounding the loops alone was not enough, because their worst cases compose: the storage-lock curve alone would have slept past two minutes, and a protection write under PVE pushback past four, long enough for another park or unpark to steal the lock and enter the window while the first caller was still inside it. Releasing a lock whose claim has already lapsed no longer deletes the sentinel, which used to remove a stealer's claim and let a third caller in while they were mid-window.

- `set_disk_metadata` falls back to the VM config when a cluster-resources row carries no tags at all, as `delete_vm` already did. An empty tags field cannot tell an untagged VM from a PVE that does not populate the field, and treating a parker as an ordinary holder writes deployment metadata into the description carrying its provenance sentinel.

- The `detach_disk` retry path resolves the disk's holder once instead of twice. It asked the same cluster-wide sweep two questions, "is it parked?" and "does a real VM hold it?", that are two readings of one answer.

- `attach_disk` and `delete_disk` now resolve the volume's holder on every call, whatever the strategy says, and refuse when that holder is a parker the configuration can no longer recognize. The refusal reads the tags the holder scan already returned, so it costs no extra API call and has no failure mode of its own: an earlier shape read the holder's config a second time and treated an unreadable config as "not a parker", which sent `delete_disk` on to destroy a volume a parker's slot still referenced. One consequence to plan for: because the scan reads every VM's config, a node that is down fails both calls retriably cluster-wide until it returns. A cluster-endpoint fault that outlives the retry budget is now reported retriably too, so the Director re-drives instead of giving up on a corosync blip or a quorum loss.

- Parker tags are recognized whatever separator PVE stored them with (`;`, `,`, or a space, the three its tag format accepts). A tag string the CPI failed to tokenize was a guard that silently did not fire, which for `delete_vm` meant a `skiplock` purge over a VM holding up to 31 other deployments' disks.

- `scripts/disk-audit` counts `unusedN` references when deciding whether a parker is empty, and never calls a parker empty when its config could not be read. A parker whose only remaining reference was an unswept `unusedN` key was reported as empty and offered as a teardown candidate with `qm destroy --purge`, which frees exactly that volume. A stranded reference now draws its own warning naming the `qm unlink` sequence that clears it.

- Consolidated the certification documentation under `docs/certification/`: the certification overview, the BATS guide, and the BATS run record moved there, joined by the Director upgrade guide and its run record.

- The create-env harness and example manifests now pin work to the configured node, which multi-node clusters with node-local storage require. The light stemcell manifests the harness generates carry `cloud_properties.node` (the light preuploaded validation demands one on a multi-node cluster), and `manifests/bosh/cloud-config.yml` and `manifests/bosh/cpi.yml` pin the AZ and the Director's resource pool to `((pve_node))` — without the pin, placement can pick a node that holds neither the stemcell cache template nor the VM's persistent disk, and an operator-owned preuploaded qcow2 is nothing the CPI can replicate across nodes. Single-node clusters and shared-storage layouts are unaffected; shared-storage clusters may drop the pins.

## [0.1.2] - 2026-08-19

Supersedes the withdrawn 0.1.1. Upgrade to this release directly from 0.1.0.

### Added

- BOSH Acceptance Tests (BATS) runner: `./scripts/bats`, a PVE cloud-config template, generated per-run reports, the [Running BATS](docs/certification/bats.md) guide, and the committed [run record](docs/certification/bats/README.md).

- Go toolchain blob drift gate, so a packaged Go blob that disagrees with `go.mod` fails the build instead of a release.

### Changed

- Migrated to `github.com/fivetwenty-io/proxmox-apiclient-go/v3` v3.8.5 (the SDK was renamed from `pve-apiclient-go`), and refreshed transitive dependencies.

- Bumped the packaged Go toolchain blob to 1.26.6 to match the `go.mod` requirement, which is what made 0.1.1 undeployable.

- Hardened the GitHub Actions workflows, widened security-scanner coverage, and moved the Go module and build caches onto mounted host volumes on the self-hosted runner.

### Fixed

- BATS deployments no longer pull in the QEMU guest-agent addon, which the acceptance manifests do not expect.

- The BATS runner pins its SSH identities without disabling the operator's OpenSSH configuration.

## [0.1.1] - 2026-08-18 [YANKED]

Withdrawn on 2026-08-19. The published tarball packaged the Go 1.26.5 toolchain blob
while `go.mod` required 1.26.6, and the packaging script pins `GOTOOLCHAIN=local`, so
compiling the `pve_cpi` package on a Director failed with `go: go.mod requires go >=
1.26.6 (running go 1.26.5; GOTOOLCHAIN=local)`. No Director could deploy the release.
The GitHub release, its assets, and the `v0.1.1` tag have all been deleted; the commit
it pointed at (`b7ead4a`) stays reachable on `main`. Upgrade straight from 0.1.0 to
0.1.2, which packages a matching toolchain. The blob drift gate that now fails the build
on this mismatch landed after the tag was cut.

### Added

- Apache License 2.0 with a `NOTICE` file, a security disclosure policy in `SECURITY.md`, a contributing guide, and `CODEOWNERS`.

- Tag-driven release workflow: pushing `vX.Y.Z` on `main` gates on the full check suite, builds the release tarball, and publishes it with a sha256 checksum and a manifest snippet.

- [An Operator's Introduction](docs/intro-overview/index.md): a one-hour operator walkthrough in ten chapters, with a matching Slidev deck.

- Dependabot coverage for Go modules, GitHub Actions, and npm.

### Changed

- CI, security, and release workflows run on the self-hosted runner inside a digest-pinned `golang:1.26` container.

- Requires Go 1.26.6; Trivy pinned to 0.74.0 with the setup action pinned by commit SHA.

- Bumped `golang.org/x/net` to v0.56.0 and the grouped Go dependency updates.

### Fixed

- `create_vm` emits `instance_group` as a BOSH-managed VM tag, so cluster-side filtering matches the Director's view.

- The release job runs its scripts under bash rather than the container's default shell.

## [0.1.0] - 2026-08-04

First feature-complete release: full BOSH CPI v2 coverage against PVE 9.x, validated end
to end against a live cluster.

### Added

- Complete BOSH CPI v2 method set (`api_version: 2`) over the JSON-RPC stdin/stdout envelope, shipped as a single static Go binary in a BOSH release.

- Stemcells: template cloning instead of per-VM import, digest verification, cross-node replication, provenance metadata with orphaned-template collection, deduplication, and DNS-safe template names.

- Light stemcells in both pre-uploaded and CPI-fetch modes, with node pinning and storage requirements documented.

- Placement: availability-aware node scoring on reserved memory, same-instance-group anti-affinity spreading, disk fault-domain co-location, storage-pool selection, resource-pool assignment with create-if-missing and an opt-in reaper, and opt-in PVE 9.2 Dynamic Load Balancer awareness.

- Networking: `create_network` and `delete_network` via PVE SDN with Linux bridge fallback, layered `cloud_properties`, per-NIC overrides, per-VM firewall and security-group attachment, allowed-address-pairs VIP ipfilter, pre-create IP-conflict detection, and opt-in SDN eventual-consistency gates.

- Persistent disks: the `pvd-` CID envelope with legacy decode, per-disk performance options, creation-time performance invariants enforced on re-attach, an opt-in parked-disk detachment strategy with provenance sentinels, `get_disks` backed by attach-time recording, and the `scripts/disk-audit` tool.

- Agent bootstrap by ConfigDrive ISO with no external registry, agent-mode auto-selection, stemcell-driven disk sizing, and a boot-path agent integrity checksum assertion.

- Resilience: opt-in progress-aware adaptive task polling, per-method timeout envelopes with configurable retry and poll cadence, transport tuning for parallel deploys, opt-in fast-path delete with a self-healing straggler sweep, alternate-node retry on transient `create_vm` failures, and PVE pushback handling.

- Safety: `delete_vm` guarded against destroying attached persistent disks, `delete_disk` guarded against in-flight VMs, cross-process VMID and anti-affinity race tolerance, dispatch hooks with a rollback contract, and a keep-failed-VMs debug mode.

- Security: minimum-privilege `bosh@pve` role documentation and API-token support (preferred over password when both are set), SSRF and allow-list closure on stemcell fetch, log scrubbing of credentials and presigned signatures, strict config validation, and a frozen exec allow-list.

- Observability: opt-in `pve.otel.*` tracing, logs, and metrics with OTLP HTTP and gRPC exporters, per-request correlated logging, an opt-in redacted request/response trace at the dispatcher boundary, and duration metrics with outcome classification.

- Operations: soft and hard `reboot_vm` with graceful fallback, an `unstick-agent` subcommand, HA node-affinity pinning, multi-cluster (multi-CPI) support with disjoint VMID banding, and configurable VMID bands.

- Test and CI infrastructure: a tiered live-PVE integration harness, an end-to-end runner with per-phase timings, a JSON-RPC fuzz target, GitHub Actions check and security gates, and an 80% coverage gate.

- The documentation set: architecture, CPI methods, configuration, networks, persistent disks, permissions, best practices, troubleshooting, and the operations runbook.

### Changed

- VMs are named `{prefix}-{deployment}-{job}-{index}`.

- `create_vm` accepts `ram` as an alias for `memory` and `env.bosh.mbus` as a flat string.

- Task status polls target the node embedded in the UPID, and attach and detach target the VM's current node.

### Removed

- BOSH registry agent mode. Supported modes are `cloudinit`, `noagent`, and `auto`; `registry_*` properties now fail fast.

## [0.0.1] - 2026-05-21

### Added

- Initial PVE CPI spike: the JSON-RPC dispatcher, the first VM and disk methods, and the BOSH release skeleton.

[Unreleased]: https://github.com/fivetwenty-io/bosh-pve-cpi-release/compare/v0.5.0...HEAD
[0.5.0]: https://github.com/fivetwenty-io/bosh-pve-cpi-release/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/fivetwenty-io/bosh-pve-cpi-release/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/fivetwenty-io/bosh-pve-cpi-release/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/fivetwenty-io/bosh-pve-cpi-release/compare/v0.1.2...v0.2.0
[0.1.2]: https://github.com/fivetwenty-io/bosh-pve-cpi-release/compare/b7ead4a1d2763f88a34baad9746f798cda8e68ef...v0.1.2
[0.1.1]: https://github.com/fivetwenty-io/bosh-pve-cpi-release/compare/v0.1.0...b7ead4a1d2763f88a34baad9746f798cda8e68ef
[0.1.0]: https://github.com/fivetwenty-io/bosh-pve-cpi-release/compare/v0.0.1...v0.1.0
[0.0.1]: https://github.com/fivetwenty-io/bosh-pve-cpi-release/releases/tag/v0.0.1
