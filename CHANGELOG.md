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

- BOSH Acceptance Tests (BATS) runner: `./scripts/bats`, a PVE cloud-config template, generated per-run reports, the [Running BATS](docs/bats.md) guide, and the committed [run record](docs/bats/README.md).

- Go toolchain blob drift gate, so a packaged Go blob that disagrees with `go.mod` fails the build instead of a release.

### Changed

- Migrated to `github.com/fivetwenty-io/proxmox-apiclient-go/v3` v3.8.5 (the SDK was renamed from `pve-apiclient-go`), and refreshed transitive dependencies.

- Bumped the packaged Go toolchain blob to 1.26.6 to match the `go.mod` requirement.

- Hardened the GitHub Actions workflows, widened security-scanner coverage, and moved the Go module and build caches onto mounted host volumes on the self-hosted runner.

### Fixed

- BATS deployments no longer pull in the QEMU guest-agent addon, which the acceptance manifests do not expect.

- The BATS runner pins its SSH identities without disabling the operator's OpenSSH configuration.

## [0.1.1] - 2026-08-18

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

[Unreleased]: https://github.com/fivetwenty-io/bosh-pve-cpi-release/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/fivetwenty-io/bosh-pve-cpi-release/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/fivetwenty-io/bosh-pve-cpi-release/compare/v0.0.1...v0.1.0
[0.0.1]: https://github.com/fivetwenty-io/bosh-pve-cpi-release/releases/tag/v0.0.1
