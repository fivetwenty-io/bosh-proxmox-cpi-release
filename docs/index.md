# BOSH PVE CPI Documentation

Welcome to the documentation for the BOSH PVE CPI, a Cloud Provider Interface (CPI) for managing virtual machines on PVE infrastructure within the BOSH ecosystem.

## Overview

This CPI enables BOSH to provision and manage resources on PVE 9.x, implementing the BOSH CPI v2 specification with full support for all required methods. The binary is written in Go and consumes the `github.com/fivetwenty-io/pve-apiclient-go/v3` SDK.

## Table of Contents

- [Development Guide](development.md): Instructions for setting up a development environment, running tests, and building releases.

- [Architecture](architecture.md): High-level overview of the CPI’s design and components.

- [CPI Methods](cpi_methods.md): Detailed documentation of implemented CPI methods and their PVE interactions.

- [Configuration](configuration.md): Comprehensive guide to configuration options.

- [ConfigDrive](configdrive.md): How the CPI delivers BOSH agent settings via an OpenStack ConfigDrive ISO, plus the SCSI slot reservation map.

- [Deploying a Director with `bosh create-env`](bosh-create-env.md): Step-by-step workflow to bring up a BOSH Director on PVE, including network reachability gotchas and SSH access.

- [Smoke-testing with `emptyvm`](emptyvm.md): Minimal post-deploy deployment that exercises the full CPI surface (create_stemcell, create_vm, create_disk, attach_disk, agent handshake).

- [PVE Storage Locking](pve-storage-locking.md): How PVE's per-storage lockfile serialises every storage mutation, why bursty BOSH deploys hit it, and how the CPI retries to absorb the contention.

- [Troubleshooting](troubleshooting.md): Solutions to common issues.

- [Examples](examples.md): Sample BOSH deployment manifests and usage scenarios.

- [API Reference](https://pkg.go.dev/github.com/fivetwenty-io/bosh-pve-cpi): Package-level Go documentation on pkg.go.dev.
