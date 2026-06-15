# BOSH PVE CPI Documentation

Documentation for the BOSH PVE CPI — a Cloud Provider Interface (CPI) for managing virtual machines on PVE infrastructure within the BOSH ecosystem.

## Overview

This CPI enables BOSH to provision and manage resources on PVE 9.x. It implements the BOSH CPI v2 specification with full support for all required methods. The binary is written in Go and consumes the `github.com/fivetwenty-io/pve-apiclient-go/v3` SDK.

## Table of Contents

- [Development Guide](development.md): Instructions for setting up a development environment, running tests, and building releases.

- [Architecture](architecture.md): High-level overview of the CPI’s design and components.

- [CPI Methods](cpi_methods.md): Detailed documentation of implemented CPI methods and their PVE interactions.

- [Configuration](configuration.md): Comprehensive guide to configuration options.

- [Network Management](networks.md): SDN versus bridge routing, vnet naming, zone auto-management, and network cloud_properties.

- [Persistent Disks](persistent-disks.md): Storage backend classification, the storage type matrix, and disk cloud_properties for storage and node selection.

- [Persistent Disk Lifecycle Strategy](persistent-disk-strategy.md): Free-floating versus parked detachment strategies, the `scripts/disk-audit` tool, parker VM provisioning and teardown, and provenance sentinel details.

- [ConfigDrive](configdrive.md): How the CPI delivers BOSH agent settings via an OpenStack ConfigDrive ISO, plus the SCSI slot reservation map.

- [Light Stemcells](light-stemcells.md): Pre-uploaded and CPI-fetch light-stemcell modes, storage requirements, and node pinning.

- [Deploying a Director with `bosh create-env`](bosh-create-env.md): Step-by-step workflow to bring up a BOSH Director on PVE, including network reachability gotchas and SSH access.

- [Smoke-testing with `emptyvm`](emptyvm.md): Minimal post-deploy deployment that exercises the full CPI surface (create_stemcell, create_vm, create_disk, attach_disk, agent handshake).

- [PVE API Permissions](pve-api-permissions.md): Creating the API token and a minimum-privilege `bosh@pve` user with a custom `BoshOperator` role, plus the per-method privilege inventory.

- [PVE Per-Storage Lockfile Behaviour](pve-storage-locking.md): How PVE's per-storage lockfile serialises every storage mutation, why bursty BOSH deploys hit it, and how the CPI retries to absorb the contention.

- [DLB-Aware Placement](dlb-aware-placement.md): Opt-in integration with the PVE 9.2 Dynamic Load Balancer — node scoring, availability-zone pinning, and HA node-affinity rules.

- [PVE Transient Transport Faults](pve-transient-transport.md): How pvedaemon worker recycling produces HTTP 596 and auth-ticket EOFs under burst load, and how the CPI absorbs them.

- [PVE Host Tuning](pve-host-tuning.md): Operator-side knobs (`pvedaemon` / `pveproxy` worker counts, storage layout) for sustained concurrent CPI workloads.

- [Troubleshooting](troubleshooting.md): Symptom-first runbook for authentication, storage, networking, and agent failures; cross-references the deep-detail docs for each failure class.

- [Operations Runbook](operations.md): Day-2 operations and diagnostics — log access, PVE-side inspection commands, pre- and post-deploy health checks, orphan and lock recovery, and how to file a bug report.

- [Examples](examples.md): Sample BOSH deployment manifests and usage scenarios.

- [API Reference](https://pkg.go.dev/github.com/fivetwenty-io/bosh-pve-cpi): Package-level Go documentation on pkg.go.dev.
