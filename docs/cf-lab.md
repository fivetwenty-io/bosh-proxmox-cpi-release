# CF Lab Runbook

Operating notes for the cf-deployment lab running on this CPI.

## Topology

```
Cloudflare Tunnel  ─►  HAProxy 192.168.1.50  ─►  gorouter (192.168.1.28)  ─►  api/cc (192.168.1.25:9024)
                                                                          ─►  diego-cells
```

DNS: `*.cf.wayne.pve.lab.fivetwenty.io` routes through the `fivetwenty-traefik`
Cloudflare Tunnel to HAProxy 192.168.1.50 to the lab's gorouter VM. Both the
api FQDN (`api.cf.wayne.pve.lab.fivetwenty.io`) and pushed-app FQDNs flow
through the same chain.

## Running CF across two clusters

The `pve-cpi` env bundle gives us two Proxmox clusters, with the Director and most of CF on the first. `manifests/cf/multi-cpi-azs.yml` binds `z1` and `z2` to the `pve-az1` entry and `z3` to the `pve-az2` entry, but stock cf-deployment puts every instance group in `z1` and `z2`, so nothing would ever dispatch through the second entry. The layer `manifests/envs/pve-cpi/cf-az2.yml` moves three groups to `z3`, which puts one CF deployment on both clusters.

| Group | Why it moves |
|---|---|
| `doppler` | A stateless singleton with no persistent disk |
| `tcp-router` | A stateless singleton with no persistent disk |
| `diego-cell` | The heaviest group. `manifests/cf/ops-resize-cf.yml` sizes each cell at 4 cores and 16 GiB, and there are two. Moving them also takes load off the first cluster |

All three are diskless on purpose. A group with a persistent disk stays in `z1` and `z2`, because moving one across entries later recreates it with an empty disk and leaves the old disk orphaned on the original cluster. The warning in `manifests/cf/multi-cpi-azs.yml` and [the multi-cluster guide](multi-cluster.md) explains that trap.

To run it, we work through these steps.

1. Apply the cpi-config and re-apply the cloud-config.

   ```bash
   BOSH_PVE_ENV=pve-cpi ./scripts/bosh update-cpi-config
   BOSH_PVE_ENV=pve-cpi ./scripts/cf cloud-config
   ```

   The first command should finish with the light stemcell registered with both entries. The second layers `manifests/envs/pve-cpi/cf-cloud-config.yml`, which adds the second cluster's subnet for `z3`, and then `manifests/cf/multi-cpi-azs.yml`, because the Director reports an active cpi-config. Without the cpi-config, a `z3` instance would draw an address from the second cluster's range but be created on the first cluster.

2. Check the memory on the first cluster before deploying.

   The first cluster is the one that still holds the Director and every group that stays in `z1` and `z2`. We read the free memory of its nodes from the PVE web UI, or with `pvesh get /cluster/resources --type node` on one of its nodes. We then add up the memory of the vm_types for the groups that stay behind, using `manifests/cf/ops-resize-cf.yml` and `manifests/cf/cloud-config.yml`, plus any other VMs the cluster hosts. The free memory has to cover that total with room to spare. The three moved groups no longer count against it, so the check is about the groups that remain, and about anything else sharing the cluster.

   The compile workers count too. The cloud-config puts compilation in `z1`, so on a Director that has no compiled packages for the stemcell yet, the first deploy runs four `compilation` VMs of 12 GiB each on the first cluster before it creates any instance VM. That 48 GiB peak can be larger than the steady state. Each worker also needs a single node with 12 GiB free, so we check the per-node figures as well as the total. If the first cluster can't hold that peak, we move compilation to the second cluster. We add a `replace` operation to `manifests/envs/pve-cpi/cf-cloud-config.yml` that sets `/compilation/az` to `z3`, and then we re-run `./scripts/cf cloud-config`.

3. Deploy with the layer.

   ```bash
   BOSH_PVE_ENV=pve-cpi ./scripts/cf deploy -o manifests/envs/pve-cpi/cf-az2.yml
   ```

   The extra `-o` flag passes through to `bosh deploy`, after the lab's own ops files. On a Director that has no CF releases compiled for the lab stemcell, the deploy stops and asks us to run `./scripts/cf precompile-releases` first. We can instead set `CF_REQUIRE_PRECOMPILE=0`. The deploy then falls back to cf-deployment's `use-compiled-releases.yml`, and the Director compiles each package whose compiled build targets a different stemcell version.

4. Confirm the placement.

   `bosh -d cf vms` should list `doppler`, `tcp-router`, and `diego-cell` in `z3` with addresses from the second cluster's range, and every other group in `z1` or `z2`. On the second cluster, the PVE node list should show those VMs, and the first cluster should show none of them.

The run succeeds when `bosh -d cf vms` shows every instance running and the three moved groups sitting in `z3`.

## NATS-storm remediation bundle

cf-deployment on this director suffered from a recreate-loop pattern: a
single flapping job on the api VM (`policy-server-asg-syncer` or `routing-api`)
would convince the health-monitor scan-and-fix plugin that the instance was
dead, HM would request a recreate, the recreate would drain nginx on
`api:9024` for up to 30 seconds, gorouter would prune the api route on the
first dial failure and serve `no_endpoints` (503/502) to cf-cli, and
`route_registrar` would re-register ~10 seconds later — only to repeat the
cycle a few minutes later when the next flap came.

The remediation has two halves: stop the false-positive recreates, and then
absorb the residual once-per-deploy drain at the gorouter.

### Stop the churn (director side)

Applied automatically by `scripts/bosh create-env`:

- `manifests/bosh/nats-tuning.yml` — widens the NATS ping window to 30 s ×
  3 (~120 s) and extends `bosh-nats-sync` `poll_user_sync` to 120 s. The
  latter halves the auth.json/SIGHUP reload frequency during deploy churn,
  which is what drops agent mbus connections (see "Deploy hang" below).

- `manifests/bosh/hm-tuning.yml` — disables `hm.resurrector_enabled` and
  widens `agent_timeout` to 180 s + `analyze_agents` to 120 s. In a
  single-PVE-host lab a genuinely dead VM stays dead until the operator
  recreates it; that is the expected workflow. Trade-off accepted.

Applied automatically by `scripts/cf bosh-deploy`:

- cf-deployment stock `operations/disable-dynamic-asgs.yml` — removes the
  flapping `policy-server-asg-syncer` job. Lab has no use for dynamic ASGs.

### Absorb the drain (cf-deployment side)

Applied automatically by `scripts/cf bosh-deploy`:

- `manifests/cf/gorouter-tuning.yml` — sets `router.empty_pool_timeout` to
  5 s. During an nginx_cc drain the gorouter now blocks the request for up
  to 5 s waiting for route_registrar to re-register, instead of returning
  `no_endpoints` immediately.

- `manifests/cf/route-registrar-tuning.yml` — tightens the api-route
  `registration_interval` from 10 s to 5 s so the re-registration lands
  inside gorouter's `empty_pool_timeout` window.

### Resurrection toggle

`scripts/cf deploy` wraps the full deploy with `bosh update-resurrection
off` (before) and `bosh update-resurrection on` (after, in a `finally`
block). This protects against the small remaining window where the
director-side per-deployment resurrection flag — which `bosh recreate` and
scan-and-fix honour — could still interfere with the deploy even though
the HM resurrector itself is off.

If `scripts/cf deploy` is killed with SIGKILL, resurrection stays off.
Check `bosh resurrection` at the start of any subsequent deploy. The
integration harness asserts on this.

## Deploy hang: mbus auth-reload RPC loss (root cause)

Separate from the recreate-loop above, deploys would intermittently fail
two ways from one cause — proven 2026-05-29 by packet capture and the
director's own NATS log:

- transient `Timed out sending 'get_state' to instance: <name>` errors, and
- a deploy task that **hangs indefinitely holding the deployment lock**
  (one run sat at `executing pre-stop` for 9 h).

Mechanism: `bosh-nats-sync` regenerates `/var/vcap/data/nats/auth.json`
every `poll_user_sync` seconds and SIGHUPs nats-server when the managed-VM
set changes (log: `Reloaded: authorization users`). `nats.cfg` uses
`verify_and_map: true`, so each agent is a cert-mapped NATS user in that
file; on reload, nats-server RSTs the agents whose entry changed. The agent
reconnects ~2 s later (new source port), but core NATS does not buffer for
an absent subscriber, so any in-flight RPC is lost. A one-shot `get_state`
lost in the gap times out at 45 s (retryable). A long-running pre-stop or
drain `get_task` *poll* lost in the gap never recovers — the director waits
forever. During a deploy the VM set churns nearly every poll, so reloads
fire ~once a minute; on an idle director nothing changes and connections
are stable for hours.

Remediation:

- `poll_user_sync: 120` (in `nats-tuning.yml`) cuts reload frequency ~2×.
- `scripts/cf` wraps `bosh deploy` in a stall-watchdog: if the deploy emits
  no progress for `SCRIPTS_CF_DEPLOY_STALL_S` (default 900 s), it
  `bosh cancel-task`s the wedged task so the retry loop converges instead of
  hanging forever. This is the load-bearing fix — frequency tuning only
  lowers probability; it cannot eliminate the reconnect gap (the
  reload-drops-connections behavior is upstream; `write_deadline` is not
  exposed and is not the lever). `--skip-drain` does not help: the hang is
  at pre-stop, also a long-running `run_script`.

To inspect: the wedged VM has no QGA, but the director VM does — reach it
via `ssh root@<pve_host> qm guest exec <director_vmid> -- grep 'Reloaded'
/var/vcap/sys/log/nats/nats.log`. tcpdump on the VM tap (`tcp port 4222`)
shows the server RST + 2 s reconnect; payloads are TLS so read the close
reason from the director's plaintext nats.log, not the wire.

## Trade-offs

- `empty_pool_timeout: 5s` adds up to 5 s tail latency during a *real* CC
  outage. Callers with short timeouts (3 s `curl`, aggressive health
  probes) see slow failures instead of fast `no_endpoints`. There is
  no stock knob to distinguish "transient drain" from "real outage".

- Disabling the HM resurrector lab-wide means a genuine hardware fault
  (PVE host hiccup, disk full, kernel panic) no longer self-heals. Single
  host, operator-present workflow — acceptable here, not in production.

- The 120 s `bosh-nats-sync` `poll_user_sync` cadence means a brand-new
  agent waits up to 120 s for its CN to land in `auth.json` before its
  first NATS connection. That stays inside BOSH's create-vm agent-wait
  timeout, so onboarding still completes. Do not raise it so far that the
  "creating missing vms" phase times out.

## Rollback

Each ops file is independently removable. To revert a single change, drop
its `-o` flag from `scripts/bosh` or `scripts/cf` and redeploy. Director-side
knobs (HM, NATS) take effect on the next `bosh create-env`; cf-side knobs
take effect on the next `cf` deploy of the affected instance group.

## Operator commands

- `scripts/cf deploy` — full deploy, resurrection-gated.
- `scripts/cf bosh-deploy` — bosh-deploy step only; does **not** toggle
  resurrection (so it is safe to invoke from inside another script that
  already manages it). Runs each attempt under a stall-watchdog
  (`SCRIPTS_CF_DEPLOY_STALL_S`, default 900 s; 0 disables) that cancels a
  no-progress deploy task so the retry loop can recover. Retries up to
  `SCRIPTS_CF_DEPLOY_ATTEMPTS` (default 8).
- `scripts/cf unstick-agent <instance>` — bypass NATS and restart the
  bosh-agent on a wedged VM via PVE QGA. Probes `qm guest cmd <vmid>
  ping` first and waits up to `SCRIPTS_CF_QGA_WAIT` seconds (default 30)
  for the runtime-config addon's detached QGA install to settle.
  Note: the BOSH Noble stemcell ships neither `cloud-init` nor
  `qemu-guest-agent`, so QGA is only reachable once the addon's
  detached install on first boot completes. A bosh-agent that wedges
  before that point has no in-guest recovery channel — recreate the
  VM with `bosh recreate`.
- `scripts/cf update-resurrection on|off` — manual toggle when needed.
- `scripts/cf teardown` — delete the cf deployment.

## Provenance

The recreate-loop remediation bundle landed 2026-05-27. The mbus auth-reload
RPC-loss root cause was proven and remediated 2026-05-29 (`poll_user_sync`
120 s + the `scripts/cf` stall-watchdog). Each ops file's header documents
the specific behavior it changes; together they form the lab's operating
profile for cf-deployment on this director.
