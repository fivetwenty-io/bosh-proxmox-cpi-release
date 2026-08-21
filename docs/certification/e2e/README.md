# End-to-End Runs

The end-to-end harness (`./scripts/e2e`) bootstraps a BOSH director and CloudFoundry from nothing against a Proxmox VE lab, then exercises the CPI lifecycle, a real `cf push`, and a real `cf ssh`. Each run below is one committed proof document; the [certification hub](../index.md) places these runs alongside the other certification paths.

## Latest run

**PASSED** on 2026-06-02: 41 steps passed, 0 failed, in 24m50s. Full report: [runs/2026-06-02.md](runs/2026-06-02.md).

| Item | Value |
|---|---|
| Command scope | all |
| CPI release | unknown |
| Proxmox VE | unknown |
| Stemcell | unknown |
| Env bundle | cpitest |

## Run history

| Date | Result | Scope | Steps failed | Wall clock | Report |
|---|---|---|---|---|---|
| 2026-06-02 | PASSED | all | 0 | 24m50s | [runs/2026-06-02.md](runs/2026-06-02.md) |
