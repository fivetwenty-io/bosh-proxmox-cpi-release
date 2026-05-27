# CF Lab Runbook

Operating notes for the cf-deployment lab that sits on top of this CPI.

## Topology

```
Cloudflare Tunnel  ─►  HAProxy 192.168.1.50  ─►  gorouter (192.168.1.28)  ─►  api/cc (192.168.1.25:9024)
                                                                          ─►  diego-cells
```

DNS: `*.cf.wayne.pve.lab.fivetwenty.io` routes through the `fivetwenty-traefik`
Cloudflare Tunnel to HAProxy 192.168.1.50 to the lab's gorouter VM. Both the
api FQDN (`api.cf.wayne.pve.lab.fivetwenty.io`) and pushed-app FQDNs flow
through the same chain.

## NATS-storm remediation bundle

cf-deployment on this director suffered from a recreate-loop pattern: a
single flapping job on the api VM (policy-server-asg-syncer or routing-api)
would convince the health-monitor scan-and-fix plugin that the instance was
dead, HM would request a recreate, the recreate would drain nginx on
`api:9024` for up to 30 seconds, gorouter would prune the api route on the
first dial failure and serve `no_endpoints` (503/502) to cf-cli, and
route_registrar would re-register ~10 seconds later — only to repeat the
cycle a few minutes later when the next flap came.

The remediation has two halves: stop the false-positive recreates, then
absorb the residual once-per-deploy drain at the gorouter.

### Stop the churn (director side)

Applied automatically by `scripts/bosh create-env`:

- `manifests/bosh/nats-tuning.yml` — widens NATS ping window to 30 s and
  extends `bosh-nats-sync` poll cadence from 10 s to 60 s. The latter cuts
  auth.json/SIGHUP frequency 6× during deploy churn.

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

## Trade-offs

- `empty_pool_timeout: 5s` adds up to 5 s tail latency during a *real* CC
  outage. Callers with short timeouts (3 s `curl`, aggressive health
  probes) will see slow failures instead of fast `no_endpoints`. There is
  no stock knob to distinguish "transient drain" from "real outage".

- Disabling the HM resurrector lab-wide means a genuine hardware fault
  (PVE host hiccup, disk full, kernel panic) no longer self-heals. Single
  host, operator-present workflow — acceptable here, not in production.

- The 60 s `bosh-nats-sync` poll cadence means a brand-new agent waits up
  to 60 s for its CN to land in `auth.json` before its first NATS
  connection. In a CF deploy creating one VM per ~2 min, that delay is
  invisible.

## Rollback

Each ops file is independently removable. To revert a single change drop
its `-o` flag from `scripts/bosh` or `scripts/cf` and redeploy. The
director-side knobs (HM, NATS) take effect on the next `bosh create-env`;
the cf-side knobs take effect on the next `cf` deploy of the affected
instance group.

## Operator commands

- `scripts/cf deploy` — full deploy, resurrection-gated.
- `scripts/cf bosh-deploy` — bosh-deploy step only; does **not** toggle
  resurrection (so it is safe to invoke from inside another script that
  already manages it).
- `scripts/cf unstick-agent <instance>` — bypass NATS and restart the
  bosh-agent on a wedged VM via PVE QGA.
- `scripts/cf update-resurrection on|off` — manual toggle when needed.
- `scripts/cf teardown` — delete the cf deployment.

## Provenance

Remediation bundle landed 2026-05-27. Each ops file's header documents
the specific behaviour it changes; together they form the lab's
operating profile for cf-deployment on this director.
