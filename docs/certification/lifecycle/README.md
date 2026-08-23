# CPI Lifecycle Harness

The CPI lifecycle harness (`scripts/lifecycle`) exercises every CPI method a BOSH Director calls during a deploy, redeploy, and teardown cycle, end to end against a live Proxmox VE cluster. We run it with `./scripts/test integration lifecycle`, which adds one pass per configured disk storage pool, network mode, and snapshot-detach mode; the [certification hub](../index.md) documents the setup and invocation.

## Latest run

**PASSED** on 2026-08-23: 4 passes run, 0 failed, in 15m07s. Full report: [runs/2026-08-23-162046.md](runs/2026-08-23-162046.md).

| Item | Value |
|---|---|
| CPI release | v0.4.0-16-gb195a9b-dirty |
| Proxmox VE | 9.2.4 |
| Stemcell | bosh-stemcell-1.364-openstack-kvm-ubuntu-noble.tgz |
| Config | ci/integration.yml |

## Run history

| Date | Result | Passes | Failed | Wall clock | Report |
|---|---|---|---|---|---|
| 2026-08-23 | PASSED | 4 | 0 | 15m07s | [runs/2026-08-23-162046.md](runs/2026-08-23-162046.md) |
| 2026-08-21 | PASSED | 4 | 0 | 18m14s | [runs/2026-08-21.md](runs/2026-08-21.md) |
