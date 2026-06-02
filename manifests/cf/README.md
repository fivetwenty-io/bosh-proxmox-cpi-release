# Cloud Foundry on bosh-pve

Deploys `cf-deployment` against the bosh-pve director using the PVE CPI.

## Layout

- `cloud-config.yml` — vm_types, disk_types, vm_extensions, single `default` network, 3 AZ aliases all bound to the same PVE node.
- `vars.yml` — `system_domain` + `haproxy_private_ip`. Combine with `manifests/bosh/vars.yml` (PVE credentials, network range).
- `deploy.sh` — chains `update-runtime-config` (dns), `update-cloud-config`, `upload-stemcell`, `bosh deploy`.

## DNS — Cloudflare via flarectl

Wildcard A record pointing at the in-deployment HAProxy:

```bash
export CF_API_EMAIL=<your-cloudflare-email>
export CF_API_KEY=<your-cloudflare-global-api-key>

flarectl dns create \
  --zone fivetwenty.io \
  --type A \
  --name '*.cf.wayne.pve.lab' \
  --content 192.168.1.50 \
  --ttl 120

flarectl dns create \
  --zone fivetwenty.io \
  --type A \
  --name 'cf.wayne.pve.lab' \
  --content 192.168.1.50 \
  --ttl 120
```

Both `cf.wayne.pve.lab.fivetwenty.io` and `*.cf.wayne.pve.lab.fivetwenty.io` must resolve to the HAProxy IP. The system domain (`api.cf.wayne.pve.lab.fivetwenty.io`, `login.cf.wayne.pve.lab.fivetwenty.io`, etc.) is served via the wildcard; the apex record is for the system_domain itself.

### `cf ssh` — dedicated DNS-only record

`cf ssh` connects to `ssh.<system_domain>:2222` (the `app_ssh_endpoint` published by `/v2/info`). HAProxy already fronts the diego ssh-proxy on `2222` — `use-haproxy.yml` wires the `ssh_proxy` link, so no extra HAProxy config or static scheduler IP is needed.

The problem is the *path* to `2222`. When the wildcard `*.cf.wayne.pve.lab` is a Cloudflare-**proxied** record (orange cloud), it carries only HTTP/HTTPS — Cloudflare drops `2222`, so `cf ssh` hangs and times out after 120s even though HTTP works.

Add a dedicated **DNS-only** (grey-cloud) `A` record for the ssh host so it bypasses the proxied wildcard and points straight at HAProxy. The IP is private (RFC1918), so this only resolves usefully for clients on the lab network / Tailscale — which is exactly who runs `cf ssh`.

```bash
flarectl dns create \
  --zone fivetwenty.io \
  --type A \
  --name 'ssh.cf.wayne.pve.lab' \
  --content 172.31.0.50 \
  --ttl 120 \
  --proxy=false        # DNS-only — must NOT be Cloudflare-proxied
```

Set `--content` to the env's `haproxy_private_ip` (`172.31.0.50` for the `cpitest` env, `192.168.1.50` for the shared-LAN default). Verify:

```bash
dig +short ssh.cf.wayne.pve.lab.fivetwenty.io      # -> haproxy_private_ip, not a Cloudflare edge IP
nc -z 172.31.0.50 2222                             # HAProxy -> diego ssh-proxy
cf ssh <app> -c 'hostname'
```

## Deploy

```bash
cd ~/w/proxmox/bosh-pve-cpi-release
bosh -e pve env                  # confirm login
./manifests/cf/deploy.sh         # full pipeline
```

First run: 45-90 minutes. The deploy command is idempotent; rerun safely.

## Post-deploy smoke

```bash
# Admin password from credhub on the director
bosh -e pve int <(bosh -e pve -d cf manifest) --path /instance_groups/name=uaa/jobs/name=uaa/properties/uaa/scim/users/0/password || true

# Easier: credhub
credhub login
credhub get -n /pve/cf/cf_admin_password

# Target + login
cf api https://api.cf.wayne.pve.lab.fivetwenty.io --skip-ssl-validation
cf auth admin <password-from-credhub>
cf create-org demo && cf target -o demo
cf create-space dev && cf target -s dev
```

The HAProxy TLS cert is self-signed by CredHub (variable `haproxy_ssl`). Use `--skip-ssl-validation` until you swap in a real cert.

## Notes

- 3 AZ aliases all map to PVE node `pve`. cf-deployment's instance counts spread across z1/z2/z3 but VMs all land on the same node — fake HA, fine for a lab.
- HAProxy is the single entry point on `192.168.1.50` (static IP, declared in `cloud-config.yml` under `subnets[0].static`).
- Default ops files: `use-compiled-releases.yml`, `use-haproxy.yml`. Append more via `./manifests/cf/deploy.sh -o /path/to/extra-ops.yml`.
