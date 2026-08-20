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

### Added

- BOSH Director Upgrade Test runner: `./scripts/certify`, which stands a Director up on the previous CPI release, deploys the upstream [certification release](https://github.com/cloudfoundry/bosh-cpi-certification) under it, upgrades the Director onto the latest release over the same state, and recreates the deployment, asserting that disk CIDs and the disk attachment survive the version change. Ships with the [BOSH Director Upgrade Test](docs/certification/upgrade.md) guide and a committed [run record](docs/certification/upgrade/README.md).

- Parked-disk lifecycle coverage in the integration harness: `scripts/lifecycle` drives a detached disk through both `detached_disk_strategy` settings against live PVE and verifies the parker VM, its protection flag, its provenance sentinel, and every refusal path out-of-band; `scripts/test integration` grows `tier1.parked_disk` (band and toggle) and `tier1.stemcell_replicate_local` for multi-node labs with node-local stemcell storage.

### Changed

- `pve.detached_disk_strategy` now defaults to `parked`: a detached persistent disk is held on a protected parker VM instead of floating as an unattached storage volume, so PVE shows who owns it and `protection=1` blocks an accidental delete. The previous free-floating behavior is still available by setting the property to `free`. Three consequences for existing deployments that never set the property: detach and attach each spend more PVE API calls, and `attach_disk` and `delete_disk` now resolve the volume's holder on every call, which costs one cluster listing plus a config read per VM in the cluster; each PVE node accrues one protected `bosh-parker-<n>` VM once a disk is parked there, which no CPI call ever deletes (`scripts/disk-audit` reports empty parkers along with the two commands that remove one); and the parker VMID band (`90000`–`90999` by default) must not overlap the VM, persistent-disk, or stemcell-template bands. A config that sets neither the strategy nor a parker band and whose other bands already reach into that window keeps the previous free-floating behavior for that load and says so in a warning, rather than failing to load, so the upgrade cannot take a running deployment down. Disks already detached stay free-floating and are picked up normally by the next `attach_disk` or `delete_disk`; `manifests/cpi-config.yml` shows the per-entry parker bands a shared-storage multi-cluster layout needs.

- **Before rolling back to a release older than this one, drain the parkers.** A release without the parked strategy has no unpark step, so it would attach a volume the parker still references and leave two VM configs pointing at one volume. Set `pve.detached_disk_strategy: free` and set `pve.parked_disk_vmid_range_start: 90000` / `_end: 90999` explicitly (a manifest that never named them has nothing to keep), which leaves the unpark probes running. Nothing drains a parker on its own: each disk comes off when the Director next attaches or deletes it, so redeploy the instance groups that own them and `bosh delete-disk` any orphans. Run `scripts/disk-audit` until it reports no parked disks, and roll back after that.

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

[Unreleased]: https://github.com/fivetwenty-io/bosh-pve-cpi-release/compare/v0.1.2...HEAD
[0.1.2]: https://github.com/fivetwenty-io/bosh-pve-cpi-release/compare/b7ead4a1d2763f88a34baad9746f798cda8e68ef...v0.1.2
[0.1.1]: https://github.com/fivetwenty-io/bosh-pve-cpi-release/compare/v0.1.0...b7ead4a1d2763f88a34baad9746f798cda8e68ef
[0.1.0]: https://github.com/fivetwenty-io/bosh-pve-cpi-release/compare/v0.0.1...v0.1.0
[0.0.1]: https://github.com/fivetwenty-io/bosh-pve-cpi-release/releases/tag/v0.0.1
