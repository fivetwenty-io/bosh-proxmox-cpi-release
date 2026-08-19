# Running BATS

The [BOSH Acceptance Tests](https://github.com/cloudfoundry/bosh-acceptance-tests) (BATS) are the CloudFoundry community's acceptance suite for BOSH directors and the CPIs beneath them. The suite drives a live director end to end through real deploys: VM create and recreate, persistent disk attach, resize, and orphan reattach, cloud-check resolutions, manual networking with static IP changes, canary failure semantics, and the stemcell's agent and supervision contract. A CPI that passes BATS has demonstrated the behavior a BOSH director depends on, on real infrastructure, through the same code paths production uses.

We run BATS against a Proxmox VE lab with `./scripts/bats`, the same way `./scripts/e2e` runs the rest of the live test surface: one command, the shared `ci/integration.yml` config, the same env bundles, and machine and prose artifacts at the end.

## How the runner works

`./scripts/bats run` performs these phases, each reported as PASS or FAIL steps in the summary table:

1. Preflight

   Verifies the tools (`ruby`, `bundle`, `bosh`, `git`, `ssh-keygen`), the config files, the `bosh-acceptance-tests` checkout (cloned and refreshed under `.deps/`, or `BATS_DIR` for a local checkout), and that the director is reachable and idle. BATS refuses to start examples while the director is processing tasks, so an occupied director would hang the suite rather than fail it.

2. Prepare

   Copies our cloud-config template (`manifests/bats/cloud_config_pve.yml.erb`) into the checkout as `templates/cloud_config_pve.yml.erb`, appends `rspec_junit_formatter` to the checkout's Gemfile, patches the checkout's ssh helper so Net::SSH offers only the run's ephemeral key rather than every identity in the workstation's OpenSSH config and ssh-agent, runs `bundle install`, resolves the stemcell, generates an ephemeral ed25519 keypair, and renders `bat.yml` from the active env bundle plus the `bats:` config section.

3. rspec

   Runs `bundle exec rspec spec/system` in the checkout with the PVE tag exclusions, the four `BAT_*` variables, and the director credentials resolved through `bosh int` from the gitignored creds file. The full console stream is captured to the run directory with private key material scrubbed.

4. Finalize

   Restores the director's cloud config (BATS replaces it with its own generated one), then writes the run report under `docs/bats/runs/` and regenerates the `docs/bats/README.md` summary.

## Prerequisites

- Ruby 3.3 or newer with bundler

  Upstream BATS tests Ruby 3.3, 3.4, and 4.0 in CI. On macOS, `brew install ruby` is sufficient. When ruby or bundler is absent, the runner records a skip and exits 2 rather than failing.

- The `bosh` CLI, `git`, `uv`, and `ssh-keygen` on PATH

- A running BOSH director from this repo's harness

  `./scripts/e2e bootstrap` stands one up. The director must be idle for the duration of the run, and BATS deploys, deletes, and cloud-checks freely against it, so use a lab director, never a production one.

- Runtime-config addons scoped away from the `bat` deployment

  The `os`-tagged service configuration specs compare monit's full process list against the test job's own pid, so any runtime-config addon that adds a monitored process to BATS VMs (bosh-dns, for one) fails them. Every addon in the director's runtime configs needs `exclude: {deployments: [bat]}`; the `director:addons` preflight step verifies this and names any addon that would ride along.

- `ci/integration.yml` with a `bats:` section

  See [Configuration](#configuration) below.

## Configuration

The `bats:` section of `ci/integration.yml` declares the network shape BATS deploys into. The cidr, gateway, and bridge default to the active env bundle's values (`internal_cidr`, `internal_gw`, and `pve_network_bridge` from `manifests/envs/<env>/vars.yml`), so most setups only declare the address bands:

| Key | Required | Purpose |
|---|---|---|
| `network_cidr` | no | Subnet range; defaults to the env bundle's `internal_cidr` |
| `network_gateway` | no | Gateway; defaults to the env bundle's `internal_gw` |
| `network_bridge` | no | PVE bridge or SDN vnet; defaults to `pve_network_bridge` |
| `network_reserved` | yes | Address bands BATS VMs must never take (the Director, infra slots, and anything else on the subnet) |
| `network_static` | yes | Static band BATS assigns addresses from |
| `static_ip` | yes | Primary static IP, inside the static band |
| `second_static_ip` | yes | Redeploy target for the static IP change test; same band, different address |
| `cpi_id` | when multi-CPI | cpi-config entry name for the BATS AZ; a director with a cpi-config applied rejects any AZ that does not name one |
| `vm_cores` | no | Cores for the BATS `default` vm_type (default 2) |
| `vm_memory_mib` | no | Memory in MiB (default 2048) |
| `vm_disk_mib` | no | Root disk in MiB (default 8192) |
| `exclude_tags` | no | Extra rspec tag exclusions on top of the built-in set |
| `rspec_timeout_s` | no | Wall-clock ceiling for the rspec run (default 14400) |

`ci/integration.yml.example` carries a commented example block sized for the `cpitest` env.

## Invocation

```bash
# The full suite with the built-in PVE tag exclusions
make bats

# Equivalent, with explicit env selection
./scripts/bats run --env pve-cpi

# A faster core-only pass while iterating
./scripts/bats run --env pve-cpi --only core --fail-fast

# Keep failed state on the director for post-mortem
./scripts/bats run --env pve-cpi --keep

# Re-print the last run's summary
./scripts/bats report
```

Useful flags:

- `--stemcell light|PATH`

  `light` (the default) builds or reuses the light stemcell the env already uses, exactly as `scripts/bosh upload-stemcell` does. A path runs BATS against that stemcell tarball instead.

- `--exclude TAG` and `--only TAG`

  Repeatable rspec tag filters layered on top of the built-in exclusions. `--no-default-excludes` drops the built-in set entirely.

- `--dry-run`

  Prints every command without executing anything. The primary validation path when no lab is reachable.

## Tag policy

BATS uses rspec tags to mark capabilities that not every IaaS has, and every provider runs the suite with the exclusions that match its platform; the upstream README shows the same pattern for vSphere and Warden. Our built-in exclusion set, with the reason for each:

| Tag | Reason |
|---|---|
| `vip_networking` | PVE has no floating IP or VIP allocation concept for the CPI to bind |
| `root_partition` | Needs an IaaS flavor with no ephemeral disk; PVE vm_types size the root disk explicitly |
| `raw_ephemeral_storage`, `raw_instance_storage` | PVE VMs have no raw instance-store devices |
| `ipv6`, `ipv6_manual_networking`, `ipv6_prefix_allocation`, `dual_stack` | The lab network is IPv4 only, and IPv6 prefix delegation is modeled only by the upstream AWS template |
| `nic_groups` | The lab env provides a single vnet, one NIC per VM |
| `multiple_manual_networks` | The lab env provides a single manual network |

Everything else runs: the `core` lifecycle set, `ssh`, `manual_networking`, `changing_static_ip`, `reboot`, the persistent disk and cloud-check suites, and the `os` supervision contract. Each run report reproduces the exclusion table so the covered surface is always explicit.

## Results and reports

Machine artifacts land in the gitignored `.e2e-results/<timestamp>/` directory:

- `junit.xml`

  Spec-level results from `rspec_junit_formatter`, one testcase per example.

- `junit-steps.xml` and `results.json`

  Harness step results in the same shape `scripts/e2e` writes, plus the `latest.json` pointer that `./scripts/bats report` reads.

- `console.log`

  The complete rspec stream, with private key blocks scrubbed.

- `bat.yml`, `bats_ssh`, and `bats_ssh.pub`

  The rendered BAT deployment spec and the run's ephemeral keypair. These contain the ephemeral private key, which is why the whole directory stays gitignored.

The committed record lives under `docs/bats/`:

- `docs/bats/runs/<date>.md`

  A generated per-run report: verdict, wall clock, per-phase and per-step timing tables, environment tuple (CPI release, BATS revision, director version, stemcell, PVE version), the exclusion table with reasons, and a per-example results table with durations.

- `docs/bats/README.md`

  A generated summary regenerated on every run: the latest verdict plus a history table linking every recorded run.

Both are plain Markdown intended to be committed, so the repository carries a public, reviewable record of what was run, against what, and with what result.

## The cloud-config template

BATS loads `templates/cloud_config_<cpi>.yml.erb` from inside its own checkout, selected by the `cpi:` key of `bat.yml`. No PVE template exists upstream yet, so the canonical copy lives in this repo at `manifests/bats/cloud_config_pve.yml.erb` and the runner copies it into the checkout before every run (the checkout refresh resets tracked files, so the copy is reapplied each time). Once local runs are established, we plan to contribute the template and a Proxmox VE setup example to the upstream `bosh-acceptance-tests` README; the template file is written to be submitted as is.

## Troubleshooting

- `director:idle` fails

  Something is running tasks on the director. Wait for it to finish, or find the task with `BOSH_PVE_ENV=<env> ./scripts/bosh tasks`.

- rspec cannot ssh to instances

  `bosh ssh` and the direct ssh checks both reach VMs by their lab addresses. Confirm that the workstation routes to the env's subnet (the same reachability `scripts/e2e` needs) and that the deployment's static IPs are inside the configured static band.

- A failed run left a `bat` deployment behind

  With `--keep` this is intentional. Clean up with `BOSH_PVE_ENV=<env> ./scripts/bosh -d bat delete-deployment --force`, then `./scripts/bosh clean-up --all` if orphaned disks remain.

- The director's cloud config looks wrong after a run

  The finalize phase restores it with `scripts/bosh ucc`; if the run was killed before finalize, run `BOSH_PVE_ENV=<env> ./scripts/bosh ucc` by hand.

- Many ssh-based examples fail with `Net::SSH::Disconnect: Too many authentication failures`

  BATS's direct ssh connections use Net::SSH, which reads the workstation's `~/.ssh/config` by default: `IdentityFile` entries there are offered before the run's ephemeral key (`keys_only` does not exclude them, since it filters agent identities only), and any reachable ssh-agent contributes every loaded identity even when `SSH_AUTH_SOCK` is hidden. Enough extra identities trip sshd's `MaxAuthTries` and the server disconnects. The runner patches the checkout's ssh helper with `keys: []`, `keys_only`, and `use_agent: false` so the ephemeral key is the only identity offered, while config parsing stays on so directives such as `ProxyJump` keep working. Seeing this error means rspec was launched outside the runner against an unpatched checkout.

- The pid-file example fails with `actual batlight pid (...) different from pid monitored by monit` listing several pids

  A runtime-config addon deployed extra monit-monitored processes onto the BATS VM, and the spec's single-pid comparison sees the whole list. Scope every addon with `exclude: {deployments: [bat]}` (this repo's `manifests/runtime-configs/pve-guest-agent.yml` carries the exclusion; directors bootstrapped elsewhere need the same edit to their `dns` and other runtime configs), then rerun. The `director:addons` preflight step catches this before rspec starts.

- rspec crashes late in the run with `LoadError` on a standard library file

  A package manager upgraded or removed the running ruby mid-run, deleting the standard library out from under the rspec process. Do not upgrade ruby (for example with `brew upgrade`) while a run is in flight; rerun on the new ruby.

- Bundler fails to install gems

  Upstream ships no Gemfile.lock, so gem versions float. Confirm ruby is 3.3 or newer (`ruby --version`); native extensions (`ed25519`, `bcrypt_pbkdf`) need a working compiler toolchain.

## Exit codes

- 0

  Every step passed.

- 1

  A step failed, including rspec example failures.

- 2

  Ruby or bundler is not installed (soft skip), or a usage or config error.

- 124

  A step hit its timeout ceiling.

- 130

  Interrupted.
