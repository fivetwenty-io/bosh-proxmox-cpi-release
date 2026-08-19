# BOSH Acceptance Tests

The [BOSH Acceptance Tests](https://github.com/cloudfoundry/bosh-acceptance-tests) (BATS) exercise a live BOSH director end to end through the CPI under test: deploys, recreates, persistent disks, cloud-check resolutions, manual networking with static IP changes, and the stemcell's agent and supervision contract. We run the suite against a Proxmox VE lab with `./scripts/bats`; [Running BATS](../bats.md) documents the setup and invocation.

## Latest run

**PASSED** on 2026-08-18: 47 examples, 0 failures, 11 skipped, in 2h22m20s. Full report: [runs/2026-08-18.md](runs/2026-08-18.md).

| Item | Value |
|---|---|
| CPI release | v0.1.1-dirty |
| BATS revision | 31057b3 |
| BOSH director | 282.1.13 |
| Stemcell | bosh-proxmox-kvm-ubuntu-noble-go_agent-light/1.383 |
| Proxmox VE | 9.2.4 |

## Run history

| Date | Result | Examples | Failures | Wall clock | Report |
|---|---|---|---|---|---|
| 2026-08-18 | PASSED | 47 | 0 | 2h22m20s | [runs/2026-08-18.md](runs/2026-08-18.md) |
