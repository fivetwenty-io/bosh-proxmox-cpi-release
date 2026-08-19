# BOSH PVE CPI Documentation

Documentation for the BOSH PVE CPI — a Cloud Provider Interface (CPI) for managing virtual machines on PVE infrastructure within the BOSH ecosystem.

## Overview

This CPI enables BOSH to provision and manage resources on PVE 9.x. It implements the BOSH CPI v2 specification with full support for all required methods. The binary is written in Go and consumes the `github.com/fivetwenty-io/proxmox-apiclient-go/v3` SDK.

## Table of Contents

- [An Operator's Introduction](intro-overview/index.md): The one-hour operator walkthrough — how the CPI is put together, how it works, and how it is configured, in ten prose chapters with a matching [Slidev deck](presentations/intro-overview/README.md).

- [An Architecture, From First Principles](architecture/index.md): The thirteen-chapter narrative that derives the design from fundamentals — problem first, principle next, feature last — for architects, engineering managers, and new team members, with a matching [Slidev deck](presentations/architecture/README.md). `make docs-architecture-html` compiles the whole narrative into a single-page HTML edition.

- [Design Decisions](design-decisions.md): The operator-facing record of the stemcell CID, storage, network, and multi-cluster design decisions — context, options considered, chosen behavior, and migration notes for each.

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

- [Running BATS](bats.md): How we run the upstream BOSH Acceptance Tests against a PVE lab with `./scripts/bats`, including configuration, the tag exclusion policy, and report generation.

- [BATS results](bats/README.md): The committed record of BATS runs, with the latest verdict, environment tuple, and per-run reports.

- [PVE API Permissions](pve-api-permissions.md): Creating the API token and a minimum-privilege `bosh@pve` user with a custom `BoshOperator` role, plus the per-method privilege inventory.

- [PVE Per-Storage Lockfile Behaviour](pve-storage-locking.md): How PVE's per-storage lockfile serialises every storage mutation, why bursty BOSH deploys hit it, and how the CPI retries to absorb the contention.

- [DLB-Aware Placement](dlb-aware-placement.md): Opt-in integration with the PVE 9.2 Dynamic Load Balancer — node scoring, availability-zone pinning, and HA node-affinity rules.

- [HA and Resurrection](ha-and-resurrection.md): Ownership matrix for BOSH-resurrector-owned versus PVE-HA-owned recovery, the double-healing race, and the CPI's warning guard rail.

- [Multi-Cluster Deployments](multi-cluster.md): cpi-config walkthrough for multiple PVE clusters, AZ-to-CPI binding, disjoint VMID banding, and shared-storage safety rules.

- [PVE Transient Transport Faults](pve-transient-transport.md): How pvedaemon worker recycling produces HTTP 596 and auth-ticket EOFs under burst load, and how the CPI absorbs them.

- [PVE Host Tuning](pve-host-tuning.md): Operator-side knobs (`pvedaemon` / `pveproxy` worker counts, storage layout) for sustained concurrent CPI workloads.

- [Troubleshooting](troubleshooting.md): Symptom-first runbook for authentication, storage, networking, and agent failures; cross-references the deep-detail docs for each failure class.

- [Operations Runbook](operations.md): Day-2 operations and diagnostics — log access, PVE-side inspection commands, pre- and post-deploy health checks, orphan and lock recovery, and how to file a bug report.

- [Examples](examples.md): Sample BOSH deployment manifests and usage scenarios.

- [Changelog](../CHANGELOG.md): Operator-visible change by release, plus the work already merged for the next one.

- [API Reference](https://pkg.go.dev/github.com/fivetwenty-io/bosh-pve-cpi): Package-level Go documentation on pkg.go.dev.
