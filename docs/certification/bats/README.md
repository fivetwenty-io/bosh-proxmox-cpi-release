# BOSH Acceptance Tests

The [BOSH Acceptance Tests](https://github.com/cloudfoundry/bosh-acceptance-tests) (BATS) exercise a live BOSH director end to end through the CPI under test: deploys, recreates, persistent disks, cloud-check resolutions, manual networking with static IP changes, and the stemcell's agent and supervision contract. We run the suite against a Proxmox VE lab with `./scripts/bats`; [Running BATS](../bats.md) documents the setup and invocation.

## Latest run

**PASSED** on 2026-08-24: 47 examples, 0 failures, 11 skipped, in 2h21m43s. Full report: [runs/2026-08-24-155302.md](runs/2026-08-24-155302.md).

| Item | Value |
|---|---|
| CPI release | v0.4.0-32-g962739b-dirty |
| BATS revision | 31057b3 |
| BOSH director | 282.1.13 |
| Stemcell | bosh-proxmox-kvm-ubuntu-noble-go_agent-light/1.383 |
| Proxmox VE | 9.2.4 |

## Run history

| Date | Result | Examples | Failures | Wall clock | Report |
|---|---|---|---|---|---|
| 2026-08-24 | PASSED | 47 | 0 | 2h21m43s | [runs/2026-08-24-155302.md](runs/2026-08-24-155302.md) |
| 2026-08-22 | PASSED | 47 | 0 | 2h21m39s | [runs/2026-08-22-041333.md](runs/2026-08-22-041333.md) |
| 2026-08-21 | PASSED | 47 | 0 | 2h21m51s | [runs/2026-08-21-171022.md](runs/2026-08-21-171022.md) |
| 2026-08-20 | PASSED | 47 | 0 | 2h21m51s | [runs/2026-08-20.md](runs/2026-08-20.md) |
| 2026-08-18 | PASSED | 47 | 0 | 2h22m20s | [runs/2026-08-18.md](runs/2026-08-18.md) |
