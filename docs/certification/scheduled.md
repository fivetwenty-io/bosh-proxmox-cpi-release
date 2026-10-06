# Scheduled Certification Workflow

`.github/workflows/certification.yml` runs the director-upgrade certification (`scripts/certify`) and the BOSH Acceptance Tests (`scripts/bats`) unattended every Saturday at 02:30 UTC, on our self-hosted runner fleet, against the `cpitest` environment (an SDN slice of the `pmx` nested lab). It is the automated counterpart to the local certification paths in [the certification index](index.md): the same scripts, the same lab, no operator at the keyboard.

We can also dispatch it by hand:

```sh
gh workflow run certification.yml --repo fivetwenty-io/bosh-proxmox-cpi-release
gh workflow run certification.yml --repo fivetwenty-io/bosh-proxmox-cpi-release -f skip_bats=true
```

`skip_bats=true` runs certify alone, which is the cheaper first probe after any change to the workflow, the CI image, or the lab. The workflow's `concurrency: group: lab` serializes it with every other lab-touching workflow, so a manual dispatch queues behind a running one rather than colliding with it.

For an unpublished CPI build, follow [candidate artifact certification](candidate-artifacts.md).

## One-time setup

The workflow reads its lab access from repository configuration, all of which exists as of 2026-08-21:

- Repository secret `LAB_BOSH_VARS_YML` holds the content of the gitignored `manifests/bosh/vars.yml`. Rotate it with `gh secret set LAB_BOSH_VARS_YML < manifests/bosh/vars.yml`. Besides every variable the Director manifest, the cloud config, and the cpi-config read, it must carry `certification_director_ip`, the certification Director's address (see [The certification Director's address](#the-certification-directors-address) below). Certify's preflight names any variable the secret is missing before it touches the lab.

- Repository secret `LAB_PVE_SSH_KEY` holds a dedicated ed25519 private key for the pvesh-over-ssh paths. Its public half sits in `/root/.ssh/authorized_keys` on `lab-pmx-0`, restricted with `from="10.115.0.10"` (the runner's SNAT address).

- Repository variable `LAB_BATS_TARGET_NODE` names the PVE node the suites target, currently `lab-pmx-0`.

- Repository variable `BOSH_PROTECTED_DIRECTOR_CIDS` names the main Director's VM, which on PVE is the bare VMID, with several VMs comma separated. Set it with `gh variable set BOSH_PROTECTED_DIRECTOR_CIDS --body 3808` for a main Director in VM 3808. The value must hold only positive whole numbers and commas, with optional spaces around each number, so a value such as `vm-3808` or `3808;3809` stops the job. The job refuses to run any Director command while the variable is empty or malformed, and it refuses any slot whose state names one of these VMs. The runner's checkout holds no main Director state, so this variable is the only thing in CI that names the main Director's VM. Every `create-env` that rebuilds the main Director gives it a new VMID, so we set the variable again after each rebuild.

- The runner host directory `/home/runner/gha-lab-state-certification` (mode 0700, owner `runner`) on the runner VM that carries the `bosh-lab` label is the certification Director's slot. The workflow bind-mounts it into the job container as `BOSH_STATE_DIR`. The directory must start empty, and `/home/runner/gha-lab-state` or any file from it must never be copied, moved, or linked into it. The workflow's slot step stops when it finds a `state.json` or `creds.yml` there before any run has written `slot.yml`.

The lab side of this setup (the `host.fw` rules on `lab-pmx-0`, the runner fleet, and the cross-lab network path) lives in the lab repository's `docs/runbooks/gha-runner-runbook.md`, under "The scheduled acceptance workflow and the pmx lab".

## How run reports land

Required status checks protect `main`, and the job's `GITHUB_TOKEN` cannot bypass them, so the workflow commits its run reports (under `docs/certification/`) to a per-run branch named `certification-reports-<run id>`, opens a PR, and arms auto-merge.

One wrinkle makes the last mile work. The PR raises `pull_request` runs of `ci.yml`, `security.yml`, and `codeql.yml`, but GitHub holds them for approval, because their actor is `github-actions[bot]`, which is not a collaborator, and the repository requires approval for every external contributor's runs as a guard against fork PRs on the shared self-hosted runner. Nobody is at the keyboard on a Saturday morning, so unapproved runs would expire as failures and the PR would never see its required checks. The job approves them itself through the workflow-run approve endpoint, which its `actions: write` permission covers. Once approved they run like any PR's checks, report on the PR's own rollup, and auto-merge fires.

Three consequences worth knowing:

- The repository does not delete branches on merge, so `certification-reports-*` branches accumulate and need occasional pruning.

- The report step fails when the three runs never appear or an approval is refused, so a red certification run with its reports already on a branch means the PR needs a hand. Dispatch the three check workflows on the report branch (`gh workflow run <wf> --ref <branch>`), confirm their verdicts, and land the PR with `gh pr merge <n> --squash --admin`. A dispatched run's check runs attach to the commit but not to the PR's rollup, which is why the admin merge is needed.

- If approvals are refused repeatedly, the job's token is not accepted by the approve endpoint. The fixes are opening the PR with a collaborator's token (a GitHub App or fine-grained PAT), so no approval is needed, or relaxing the approval policy to first-time contributors only, which weakens the fork guard on the runner.

## Director state on the runner host

The workflow builds, upgrades, and tears down its own Director, the certification Director, and never the main one. Its `creds.yml` and `state.json` are `bosh create-env` products that live in the slot directory `/home/runner/gha-lab-state-certification`, which the job mounts and names with `BOSH_STATE_DIR`. Every `./scripts/bosh`, `./scripts/certify`, and `./scripts/bats` call in the job reads and writes them there, so the state survives from one run to the next without a copy step. Losing them would make a standing certification Director invisible, and `create-env` would then build a second one on the same address and orphan the first. The directory holds lab credentials, so it never leaves the runner host.

The main Director's state directory, `/home/runner/gha-lab-state`, is not mounted into the job at all. On top of that, the slot guard described under [Destructive by design](upgrade.md#destructive-by-design) runs before every Director command. It refuses the default slot and any checkout's `manifests/bosh/`, it refuses a state that names a VM in `BOSH_PROTECTED_DIRECTOR_CIDS`, and it refuses a state that the slot's own `create-env` didn't write, which `slot-owner.json` in the slot records. The job sets `BOSH_REFUSE_DEFAULT_SLOT=1`, so a lost `BOSH_STATE_DIR` stops `./scripts/bosh`, `./scripts/certify`, and the harnesses alike.

At the start of each run the workflow renders `slot.yml` into the slot from the committed template `ci/certification-slot.yml` and the vars secret. It gives the certification Director its own `internal_ip`, the alias `pve-certification`, the name `certification`, and the VM name segment `create-env-certification`.

## The certification Director's address

The certification Director sits on the same cpitest network as the main Director (172.31.0.0/24), so it needs an address that nothing else claims. These are the addresses the tracked files claim on that network.

| Address | Claimed by | Evidence |
|---|---|---|
| 172.31.0.1 | Gateway | `internal_gw` and `cpitest_sdn_gateway` in `manifests/envs/cpitest/vars.yml` |
| 172.31.0.2 | Kept for routing by convention | `manifests/envs/cpitest/cc-reserved.yml` |
| 172.31.0.10 | Main Director | `internal_ip` in `manifests/envs/cpitest/vars.yml`, and `docs/artifacts.md` |
| 172.31.0.11 | Artifacts VM | `manifests/envs/cpitest/artifacts.yml`, `scripts/_artifacts.py`, and `docs/artifacts.md` |
| 172.31.0.1 to 172.31.0.19 | Reserved infra band | `cpitest_reserved` in `manifests/envs/cpitest/vars.yml`, applied to the bosh cloud config by `cc-reserved.yml` |
| 172.31.0.20 to 172.31.0.199 | The main Director's dynamic placement, apart from .50 | Everything in 172.31.0.0/24 outside `cpitest_reserved` and `cpitest_static` |
| 172.31.0.150 to 172.31.0.189 | The certification Director's dynamic placement, and every address the scheduled BATS run uses | `cpitest_reserved` in `ci/certification-slot.yml`, and `bats:` in `ci/integration.scheduled.yml` |
| 172.31.0.50 | HAProxy static IP | `haproxy_private_ip` and `cpitest_static` in `manifests/envs/cpitest/vars.yml` |
| 172.31.0.190 to 172.31.0.199 | Static band for local BATS runs against the main Director, with .190 and .191 as its two static IPs | `bats:` in `ci/integration.yml.example` |
| 172.31.0.200 to 172.31.0.254 | Reserved infra band | `cpitest_reserved` in `manifests/envs/cpitest/vars.yml` |

Pick the certification Director's address from the reserved band below .20, skipping the addresses above, so that no deployment's dynamic placement can ever take it. The tracked files can't prove that nothing in the lab already answers on an address, so check yours before the first run, for example by pinging it from the PVE node and looking for it in the cpitest vnet's ARP table. The address goes into the secret as `certification_director_ip` and into no committed file.

### How the two Directors share the dynamic range

Each Director keeps its own record of which addresses its deployments hold, and neither one can see the other's, so we split the range between them instead. BOSH gives a new VM the lowest free address in its dynamic range. The main Director's range starts at .20, so its deployments fill the network from the bottom up, and we put the certification Director at the top.

The certification slot's `slot.yml` carries its own `cpitest_reserved`, which `ci/certification-slot.yml` sets. It keeps every band the main Director reserves and adds .20 to .149 and .190 to .199, so the certification Director's deployments can only land in .150 to .189. `./scripts/bosh ucc` layers `slot.yml` after the env's vars, so the longer list applies to the certification Director alone, and the main Director's cloud config doesn't change. A test in `scripts/_slot_test.py` renders both cloud configs and fails if either one drifts, and it also fails if the template drops an entry the env's `cpitest_reserved` has.

The scheduled BATS run deploys through the certification Director, so its addresses come from the same band. BATS uploads a cloud config of its own, and `ci/integration.scheduled.yml` gives it dynamic addresses from .150 to .179 and its static band from .180 to .189, with everything else reserved. That keeps it clear of .190 to .199, which local BATS runs against the main Director use.

The split holds only while the main Director's deployments hold 129 dynamic addresses or fewer, because the 130th would take .150. If the main Director ever grows that large, we have to move the certification band or give the certification Director a network of its own.

## Troubleshooting

Dispatch `lab-probe.yml` first. It re-proves both halves of the network path (PVE API by HTTP status, director by TCP connect) without needing any secret, and separates "the lab is unreachable" from "the workflow is broken".

Failures we have already seen, with their signatures:

- The job cannot pull the CI image: the `packages: read` permission or the ghcr digest pin in `certification.yml` is stale. `ci-image.yml` prints the new digest after a Dockerfile change; pin it.

- `qemu-img: command not found` during certify: the CI image lost `qemu-utils`, which the light-stemcell qcow2 derivation needs.

- `net:up` times out reaching the PVE API or ssh: check the two `host.fw` ACCEPT rules for `10.115.0.10` on `lab-pmx-0` (ports 8006 and 22).

- ssh fails with `Permission denied (publickey,password)` even though the key step ran: OpenSSH expands `~` from the passwd entry, not `$HOME`, and container jobs run with `HOME=/github/home` while the passwd home is `/root`. The workflow derives the ssh directory with `getent` for this reason; keep that pattern in any new ssh-using step.

## Certification record

The first fully unattended-shape run (certify plus BATS, dispatched from `main`) passed on 2026-08-21 as run 32499183044, after a certify-only run passed the same day. The committed reports live under `docs/certification/upgrade/` and `docs/certification/bats/`.
