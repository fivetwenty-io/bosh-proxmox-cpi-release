# BOSH Director Upgrade Test

The [BOSH CPI certification](https://github.com/cloudfoundry/bosh-cpi-certification) suite's Director Upgrade Test stands a Director up on one CPI release, deploys the upstream certification release under it, upgrades the Director onto a newer CPI release over the same state, and recreates the deployment through it. We run it against a Proxmox VE lab with `./scripts/certify`; [BOSH Director Upgrade Test](../upgrade.md) documents the setup and invocation.

## Latest run

**PASSED** on 2026-08-24: 0.3.0 to 0.4.0, 37 steps passed, 0 failed, in 1h00m11s. Full report: [runs/2026-08-24-142623.md](runs/2026-08-24-142623.md).

| Item | Value |
|---|---|
| CPI release before | 0.3.0 |
| CPI release after | 0.4.0 |
| bosh release | held |
| Stemcell | bosh-proxmox-kvm-ubuntu-noble-go_agent-light/1.383 |
| BOSH director | unknown |
| Certification suite revision | 48f3611 |
| Proxmox VE | 9.2.4 |

## Run history

| Date | Result | CPI upgrade | Steps failed | Wall clock | Report |
|---|---|---|---|---|---|
| 2026-08-24 | PASSED | 0.3.0 to 0.4.0 | 0 | 1h00m11s | [runs/2026-08-24-142623.md](runs/2026-08-24-142623.md) |
| 2026-08-22 | PASSED | 0.3.0 to 0.4.0 | 0 | 1h00m09s | [runs/2026-08-22-024655.md](runs/2026-08-22-024655.md) |
| 2026-08-21 | PASSED | 0.2.0 to 0.3.0 | 0 | 59m57s | [runs/2026-08-21-154407.md](runs/2026-08-21-154407.md) |
| 2026-08-21 | PASSED | 0.2.0 to 0.3.0 | 0 | 57m58s | [runs/2026-08-21.md](runs/2026-08-21.md) |
| 2026-08-20 | PASSED | 0.2.0 to dev (bosh-pve-cpi-dev-20260821012107.tgz) | 0 | 56m19s | [runs/2026-08-20.md](runs/2026-08-20.md) |
| 2026-08-19 | PASSED | 0.1.2 to bosh-pve-cpi-dev-20260819230911.tgz | 0 | 16m24s | [runs/2026-08-19.md](runs/2026-08-19.md) |
