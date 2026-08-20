# BOSH Director Upgrade Test

The [BOSH CPI certification](https://github.com/cloudfoundry/bosh-cpi-certification) suite's Director Upgrade Test stands a Director up on one CPI release, deploys the upstream certification release under it, upgrades the Director onto a newer CPI release over the same state, and recreates the deployment through it. We run it against a Proxmox VE lab with `./scripts/certify`; [BOSH Director Upgrade Test](../upgrade.md) documents the setup and invocation.

## Latest run

**PASSED** on 2026-08-19: 0.1.2 to bosh-pve-cpi-dev-20260819230911.tgz, 36 steps passed, 0 failed, in 16m24s. Full report: [runs/2026-08-19.md](runs/2026-08-19.md).

| Item | Value |
|---|---|
| CPI release before | 0.1.2 |
| CPI release after | bosh-pve-cpi-dev-20260819230911.tgz |
| bosh release | held |
| Stemcell | bosh-proxmox-kvm-ubuntu-noble-go_agent-light/1.383 |
| BOSH director | 282.1.13 |
| Certification suite revision | 48f3611 |
| Proxmox VE | 9.2.4 |

## Run history

| Date | Result | CPI upgrade | Steps failed | Wall clock | Report |
|---|---|---|---|---|---|
| 2026-08-19 | PASSED | 0.1.2 to bosh-pve-cpi-dev-20260819230911.tgz | 0 | 16m24s | [runs/2026-08-19.md](runs/2026-08-19.md) |
