# BOSH Proxmox CPI Documentation

This site documents the BOSH Proxmox CPI, a Cloud Provider Interface (CPI) that manages virtual machines on PVE infrastructure within the BOSH ecosystem.

## Overview

This CPI enables BOSH to provision and manage resources on PVE 9.x. It implements the BOSH CPI v2 specification with full support for all required methods. The binary is written in Go and consumes the `github.com/fivetwenty-io/proxmox-apiclient-go/v3` SDK.

## Table of Contents

- [An Operator's Introduction](intro-overview/index.md): This one-hour operator walkthrough explains how the CPI is put together, how it works, and how it is configured. It includes ten prose chapters and a matching [Slidev deck](presentations/intro-overview/README.md).

- [An Architecture, From First Principles](architecture/index.md): This thirteen-chapter narrative derives the design from fundamentals by presenting the problem, then the principle, and then the feature. It serves architects, engineering managers, and new team members, and it has a matching [Slidev deck](presentations/architecture/README.md). `make docs-architecture-html` compiles the whole narrative into a single-page HTML edition.

- [Design Decisions](design-decisions.md): This operator-facing record explains the context, options, chosen behavior, and migration notes for each stemcell CID, storage, network, and multi-cluster design decision.

- [Multiple NFS Shares Design](designs/multi-storage-placement-design.md) explains the vSphere research, independent role sets, versioned strategies, and recovery contract.

- [Multiple NFS Shares Implementation Plan](plans/multi-storage-placement-plan.md) describes the required code changes, dependencies, acceptance tests, recovery behavior, and release gates for the feature. Follow the [implementation progress record](plans/multi-storage-placement-progress.md) for current validation results.

- [Multi-storage Placement Implementation and Validation Report](multi-storage-placement-implementation-and-validation-report.md) explains the implemented behavior, lab campaigns, Director results, remediations, cleanup evidence, and remaining release gates.

- [Parker Prefix, Pool, and Tags Implementation Plan](plans/parker-prefix-pool-and-tags-plan.md) gives parker and mover VMs an operator-visible identity through a configurable name prefix, a resource pool, and a `prefix--` tag, and records the configuration, permission, and release consequences of each.

- [Configure Multiple NFS Shares](multi-storage-placement.md) explains storage sets, strategy selection, role boundaries, and capacity domains for the implementation under development.

- [Provision the Storage Journal](storage-journal-provisioning.md) explains durable paths, ownership, and Director mounts.

- [Audit and Recover Storage Allocations](storage-journal-operations.md) covers enrollment, authority restoration, index repair, adoption, and explicit cleanup.

- [Certify Candidate Artifacts](certification/candidate-artifacts.md) explains how to test the exact candidate release and retain its checksum in certification reports.

- [Verify Multi-storage Placement](certification/multi-storage-placement.md) explains the live lifecycle assertions and the evidence they produce.

- [Multiple NFS Shares: Proposed Design](designs/multi-storage-placement-design.md): vSphere datastore research, independent disk-role storage sets, placement strategy options, and a staged implementation and certification plan. Proposed functionality, not current configuration.

- [Multiple NFS Shares Implementation Plan](plans/multi-storage-placement-plan.md) describes the required code changes, dependencies, acceptance tests, recovery behavior, and release gates for the proposed feature.

- [Known Limitations](limitations.md): The consolidated list of what the CPI does not do, covering disk, storage, networking, topology, and permissions constraints, one sentence per item with a link to the page that owns the detail.

- [Development Guide](development.md): Instructions for setting up a development environment, running tests, and building releases.

- [Architecture](architecture.md): High-level overview of the CPI’s design and components. For the reasoning behind that design, read the [architecture narrative](architecture/index.md).

- [CPI Methods](cpi_methods.md): Detailed documentation of implemented CPI methods and their PVE interactions.

- [Configuration](configuration.md): Comprehensive guide to configuration options.

- [Network Management](networks.md): SDN versus bridge routing, vnet naming, zone auto-management, and network cloud_properties.

- [Persistent Disks](persistent-disks.md): Storage backend classification, the storage type matrix, and disk cloud_properties for storage and node selection.

- [Persistent Disk Lifecycle Strategy](persistent-disk-strategy.md): Free-floating versus parked detachment strategies, the `scripts/disk-audit` tool, parker VM provisioning and teardown, and provenance sentinel details.

- [ConfigDrive](configdrive.md): How the CPI delivers BOSH agent settings via an OpenStack ConfigDrive ISO, plus the SCSI slot reservation map.

- [Light Stemcells](light-stemcells.md): Pre-uploaded and CPI-fetch light-stemcell modes, storage requirements, and node pinning.

- [Deploying a Director with `bosh create-env`](bosh-create-env.md): Step-by-step workflow to bring up a BOSH Director on PVE, including network reachability gotchas and SSH access.

- [Smoke-testing with `emptyvm`](emptyvm.md): Minimal post-deploy deployment that exercises the full CPI surface (create_stemcell, create_vm, create_disk, attach_disk, agent handshake).

- [CPI Certification](certification/index.md): This hub covers every path we use to certify the CPI against the BOSH CPI v2 contract, including the local lifecycle harness, the BOSH Acceptance Tests, the BOSH Director Upgrade Test, and the upstream Concourse pipeline.

- [Lifecycle results](certification/lifecycle/README.md): The committed record of CPI lifecycle harness runs, with the latest verdict, per-pass timings, and per-run reports.

- [End-to-end results](certification/e2e/README.md): The committed record of full-stack end-to-end runs, from director and CloudFoundry bootstrap through `cf push` and `cf ssh`, with the latest verdict and per-run reports.

- [Running BATS](certification/bats.md): How we run the upstream BOSH Acceptance Tests against a PVE lab with `./scripts/bats`, including configuration, the tag exclusion policy, and report generation.

- [BATS results](certification/bats/README.md): The committed record of BATS runs, with the latest verdict, environment tuple, and per-run reports.

- [BOSH Director Upgrade Test](certification/upgrade.md): How we run the upstream certification suite's director-upgrade scenario with `./scripts/certify`, upgrading a live director and its managed deployment across CPI releases.

- [Director upgrade results](certification/upgrade/README.md): The committed record of director-upgrade runs, with the latest verdict, the version matrix, and per-run reports.

- [PVE API Permissions](pve-api-permissions.md): Creating the API token and a minimum-privilege `bosh@pve` user with a custom `BoshOperator` role, plus the per-method privilege inventory.

- [PVE Per-Storage Lockfile Behaviour](pve-storage-locking.md): How PVE's per-storage lockfile serialises every storage mutation, why bursty BOSH deploys hit it, and how the CPI retries to absorb the contention.

- [DLB-Aware Placement](dlb-aware-placement.md): This guide covers opt-in integration with the PVE 9.2 Dynamic Load Balancer, including node scoring, availability-zone pinning, and HA node-affinity rules.

- [HA and Resurrection](ha-and-resurrection.md): Ownership matrix for BOSH-resurrector-owned versus PVE-HA-owned recovery, the double-healing race, and the CPI's warning guard rail.

- [Multi-Cluster Deployments](multi-cluster.md): cpi-config walkthrough for multiple PVE clusters, AZ-to-CPI binding, disjoint VMID banding, and shared-storage safety rules.

- [PVE Transient Transport Faults](pve-transient-transport.md): How pvedaemon worker recycling produces HTTP 596 and auth-ticket EOFs under burst load, and how the CPI absorbs them.

- [PVE Host Tuning](pve-host-tuning.md): Operator-side knobs (`pvedaemon` / `pveproxy` worker counts, storage layout) for sustained concurrent CPI workloads.

- [Troubleshooting](troubleshooting.md): This symptom-first runbook covers authentication, storage, networking, and agent failures. It links to the detailed documentation for each failure class.

- [Operations Runbook](operations.md): This runbook covers day-2 operations and diagnostics, including log access, PVE-side inspection commands, pre- and post-deploy health checks, orphan and lock recovery, and bug reports.

- [Examples](examples.md): Sample BOSH deployment manifests and usage scenarios.

- [Changelog](../CHANGELOG.md): Operator-visible change by release, plus the work already merged for the next one.

- [Source](https://github.com/fivetwenty-io/bosh-proxmox-cpi-release): CPI source code, including package-level Go documentation in `src/pve_cpi`.
