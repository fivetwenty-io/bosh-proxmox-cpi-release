# Scheduled Acceptance Workflow

`.github/workflows/acceptance.yml` runs the director-upgrade certification (`scripts/certify`) and the BOSH Acceptance Tests (`scripts/bats`) unattended every Saturday at 02:30 UTC, on our self-hosted runner fleet, against the `cpitest` environment (an SDN slice of the `pmx` nested lab). It is the automated counterpart to the local certification paths in [the certification index](index.md): the same scripts, the same lab, no operator at the keyboard.

We can also dispatch it by hand:

```sh
gh workflow run acceptance.yml --repo fivetwenty-io/bosh-pve-cpi-release
gh workflow run acceptance.yml --repo fivetwenty-io/bosh-pve-cpi-release -f skip_bats=true
```

`skip_bats=true` runs certify alone, which is the cheaper first probe after any change to the workflow, the CI image, or the lab. The workflow's `concurrency: group: lab` serializes it with every other lab-touching workflow, so a manual dispatch queues behind a running one rather than colliding with it.

## One-time setup

The workflow reads its lab access from repository configuration, all of which exists as of 2026-08-21:

- Repository secret `LAB_BOSH_VARS_YML` holds the content of the gitignored `manifests/bosh/vars.yml`. Rotate it with `gh secret set LAB_BOSH_VARS_YML --body-file manifests/bosh/vars.yml`.

- Repository secret `LAB_PVE_SSH_KEY` holds a dedicated ed25519 private key for the pvesh-over-ssh paths. Its public half sits in `/root/.ssh/authorized_keys` on `lab-pmx-0`, restricted with `from="10.115.0.10"` (the runner's SNAT address).

- Repository variable `LAB_BATS_TARGET_NODE` names the PVE node the suites target, currently `lab-pmx-0`.

- The runner host directory `/home/runner/gha-lab-state` (mode 0700, owner `runner`) persists director state between runs; the workflow bind-mounts it into the job container.

The lab side of this setup (the `host.fw` rules on `lab-pmx-0`, the runner fleet, and the cross-lab network path) lives in the lab repository's `docs/runbooks/gha-runner-runbook.md`, under "The scheduled acceptance workflow and the pmx lab".

## How run reports land

Required status checks protect `main`, and the job's `GITHUB_TOKEN` cannot bypass them, so the workflow commits its run reports (under `docs/certification/`) to a per-run branch named `certification-reports-<run id>`, opens a PR, and arms auto-merge.

One wrinkle makes the last mile work: GitHub suppresses workflow triggers from events a job's `GITHUB_TOKEN` creates, so the branch push and the PR raise no `push` or `pull_request` events, and the required checks would never start. `workflow_dispatch` events are exempt from that suppression, so after arming auto-merge the job dispatches `ci.yml`, `security.yml`, and `codeql.yml` on the report branch itself. Their check runs land on the branch head, the required checks report, and auto-merge fires.

Two consequences worth knowing:

- The repository does not delete branches on merge, so `certification-reports-*` branches accumulate and need occasional pruning.

- If a report PR sits open with no checks, the dispatch step failed or was skipped; dispatching those three workflows on the report branch by hand (`gh workflow run <wf> --ref <branch>`) unblocks it.

## Director state on the runner host

`creds.yml` and `state.json` are `bosh create-env` products. The workflow restores them from `/home/runner/gha-lab-state` before the run and persists them back after, because without them a standing director from the previous run is invisible: `create-env` would build a second director and orphan the first. The directory holds lab credentials, so it never leaves the runner host.

## Troubleshooting

Dispatch `lab-probe.yml` first. It re-proves both halves of the network path (PVE API by HTTP status, director by TCP connect) without needing any secret, and separates "the lab is unreachable" from "the workflow is broken".

Failures we have already seen, with their signatures:

- The job cannot pull the CI image: the `packages: read` permission or the ghcr digest pin in `acceptance.yml` is stale. `ci-image.yml` prints the new digest after a Dockerfile change; pin it.

- `qemu-img: command not found` during certify: the CI image lost `qemu-utils`, which the light-stemcell qcow2 derivation needs.

- `net:up` times out reaching the PVE API or ssh: check the two `host.fw` ACCEPT rules for `10.115.0.10` on `lab-pmx-0` (ports 8006 and 22).

- ssh fails with `Permission denied (publickey,password)` even though the key step ran: OpenSSH expands `~` from the passwd entry, not `$HOME`, and container jobs run with `HOME=/github/home` while the passwd home is `/root`. The workflow derives the ssh directory with `getent` for this reason; keep that pattern in any new ssh-using step.

## Certification record

The first fully unattended-shape run (certify plus BATS, dispatched from `main`) passed on 2026-08-21 as run 32499183044, after a certify-only run passed the same day. The committed reports live under `docs/certification/upgrade/` and `docs/certification/bats/`.
