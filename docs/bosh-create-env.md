# Deploying a BOSH Director to Proxmox VE with `bosh create-env`

This document describes the repeatable workflow for deploying a single-VM BOSH
Director on a Proxmox VE (PVE) host using this release and the upstream
[`cloudfoundry/bosh-deployment`](https://github.com/cloudfoundry/bosh-deployment)
ops library.

The workflow:

1. Build a local tarball of `bosh-pve-cpi`.

2. Clone `bosh-deployment`.

3. Fill in `manifests/vars.yml`.

4. Run `bosh create-env` with the supplied ops file.

5. Verify the Director.

## 1. Prerequisites

- A reachable Proxmox VE 9.x cluster, with credentials (`root@pam` password or an API token), at least one storage pool for VMs, one for persistent disks, and one for stemcell templates (often `local-lvm` and `local`).

- A Linux bridge on the PVE node (default `vmbr0`) with reachable network to the desired Director IP.

- Local tooling: `bosh` CLI v7+, Go 1.27+, `git`, `make`.

- A workstation that can reach both the PVE API (`https://<pve>:8006`) and the Director IP.

## 2. Build the release tarball

From the repository root:

```bash
make release VERSION=0.1.0
```

This produces `./bosh-pve-cpi-0.1.0.tgz` at the repo root. Capture the absolute path; it goes into `vars.yml` as `pve_cpi_release_path`.

For an untagged build, use `make dev-release` — output is `./bosh-pve-cpi-dev-<UTC-timestamp>.tgz`.

The release is named `bosh-pve-cpi`. If you build a final release later, the name and version are recorded under `releases/bosh-pve-cpi/`.

## 3. Clone `bosh-deployment`

```bash
mkdir -p ~/w/cloudfoundry
git clone https://github.com/cloudfoundry/bosh-deployment ~/w/cloudfoundry/bosh-deployment
```

Pass `~/w/cloudfoundry/bosh-deployment/bosh.yml` as the base manifest.

## 4. Prepare `vars.yml`

```bash
cp manifests/vars.yml.example manifests/vars.yml
$EDITOR manifests/vars.yml
```

Required fields:

- `pve_host`, `pve_user`, `pve_node`

- Exactly one of `pve_password` or `pve_api_token` (see [Authentication](#authentication) below)

- `pve_vm_storage`, `pve_disk_storage`, `pve_network_bridge`

- `pve_stemcell_storage` — a file-backed PVE storage (dir/nfs/cifs/glusterfs/cephfs) with `import` content enabled. The CPI uploads qcow2 stemcell images here and references them via `import-from=<storage>:import/<file>.qcow2`. Block-based storages (lvm, lvmthin, zfspool, rbd) cannot accept file uploads and are rejected. Defaults to `vm_storage`.

- `pve_iso_storage` — a file-backed PVE storage (dir/nfs/cifs) with `iso` content enabled, used to hold the per-VM ConfigDrive ISO. Block storages (lvm/lvmthin/zfspool) cannot hold ISO files. Defaults to `local`.

- `internal_cidr`, `internal_gw`, `internal_ip` (the Director IP)

- `director_cpu`, `director_memory`, `director_disk`

- `pve_cpi_release_path` (absolute path to the tarball from step 2)

- `stemcell_url`, `stemcell_sha1` (Ubuntu noble openstack-kvm). Known-good values:

  - `stemcell_url: https://bosh.io/d/stemcells/bosh-openstack-kvm-ubuntu-noble?v=1.364`

  - `stemcell_sha1: d6cc58bda0120fe47787a46775ff5bafc5718257`

  Latest published values: <https://bosh.io/stemcells/bosh-openstack-kvm-ubuntu-noble-go_agent>

When `pve.agent_mode = cloudinit` (the default), the CPI delivers settings via an OpenStack ConfigDrive ISO attached as a CD-ROM. The OpenStack stemcells linked above support this layout natively. See [ConfigDrive](configdrive.md).

Keep `vars.yml` out of git — it contains the PVE password.

### `pve_host` must be reachable from the Director VM

`pve_host` is written into the rendered `cpi.json` on the Director VM. The Director's CPI invocations resolve and dial this address from inside that VM, not from the workstation running `bosh create-env`.

Use an address the Director can resolve on its own networks:

- **Good**: the PVE node's LAN IP on the same bridge as the Director (e.g., `192.168.1.180`).

- **Good**: a hostname that resolves via the public DNS configured in the Director's network (`1.1.1.1` / `8.8.8.8` by default in this manifest).

- **Bad**: a Tailscale magicDNS hostname like `pve-0.<tailnet>.ts.net`. The Director VM is not on the tailnet; DNS lookups fail with `no such host` and every CPI call from the Director (stemcell upload, VM clone, deploy) errors out before it reaches PVE.

The Mac workstation may reach PVE over Tailscale fine — but `bosh create-env` runs the CPI binary locally on the workstation, while subsequent `bosh upload-stemcell`, `bosh deploy`, and related commands run the CPI on the Director. Both share the same `cpi.json`, so the address must work for both. The LAN IP is the safe default.

### Authentication

See [pve-api-permissions.md](pve-api-permissions.md) for token creation and the minimum-privilege `bosh@pve` user setup.

The ops file in `manifests/cpi.yml` wires both `pve_password` and `pve_api_token` into the CPI job. Both vars must exist in `vars.yml` so the BOSH var interpolator can resolve them, but only one should hold a real value — leave the other as an empty string. The CPI validates this XOR at startup (`one of password or api_token is required`; `password and api_token are mutually exclusive`).

Password auth (default):

```yaml
pve_user:      root@pam
pve_password:  "your-password"
pve_api_token: ""
```

API token auth:

```yaml
pve_user:      root@pam
pve_password:  ""
pve_api_token: root@pam!tokenid=00000000-0000-0000-0000-000000000000
```

Use an API token for non-interactive automation — it can be scoped and revoked independently of the root password.

## 5. Run `bosh create-env`

```bash
bosh create-env ~/w/cloudfoundry/bosh-deployment/bosh.yml \
  --state=manifests/state.json \
  --vars-store=manifests/creds.yml \
  -o manifests/bosh/bosh-release.yml \
  -o manifests/cpi.yml \
  -o ~/w/cloudfoundry/bosh-deployment/jumpbox-user.yml \
  -l manifests/vars.yml
```

`scripts/bosh create-env` wraps this exact invocation with full ops layering — prefer it over the raw command above.

The `bosh-release.yml` ops file pins the `cloudfoundry/bosh` release to **282.1.13**, overriding whatever version the local `bosh-deployment` checkout ships. Bump `version`, `url`, and `sha1` together to move to a newer release.

The `jumpbox-user.yml` ops file (optional but recommended) adds a `jumpbox` user with a generated SSH keypair stored in `creds.yml`. Without it, the Director VM has no operator-accessible login — BOSH stemcells ship with root SSH disabled, and the bosh-agent resets the `vcap` password on every boot, making direct SSH impossible.

What each flag does:

- `--state=manifests/state.json`

  Persists the Director VM identity (VMID, disk CIDs) so reruns update rather than recreate.

- `--vars-store=manifests/creds.yml`

  Persists generated credentials (NATS, mbus, blobstore, jumpbox) across runs. Required for idempotent updates.

- `-o manifests/bosh/bosh-release.yml`

  Pins the `cloudfoundry/bosh` release to a reproducible version (282.1.13) instead of the local `bosh-deployment` default.

- `-o manifests/cpi.yml`

  Layers the PVE CPI onto `bosh.yml`: stemcell URL, network bridge, instance-group job, and `cloud_provider` block.

- `-l manifests/vars.yml`

  Supplies the variables consumed by the ops file.

The first run takes several minutes: it uploads the stemcell template, clones it to a VM, attaches the persistent disk, installs Director jobs, and completes the mbus handshake.

## 6. Point the CLI at the new Director

```bash
DIRECTOR_IP=$(bosh int manifests/vars.yml --path /internal_ip)

bosh alias-env pve \
  -e "${DIRECTOR_IP}" \
  --ca-cert <(bosh int manifests/creds.yml --path /director_ssl/ca)

export BOSH_ENVIRONMENT=pve
export BOSH_CLIENT=admin
export BOSH_CLIENT_SECRET=$(bosh int manifests/creds.yml --path /admin_password)

bosh env
```

Stash the environment block in a sourceable file for repeated use:

```bash
cat > ~/.bosh-pve.env <<EOF
export BOSH_ENVIRONMENT=pve
export BOSH_CLIENT=admin
export BOSH_CLIENT_SECRET=$(bosh int $(pwd)/manifests/creds.yml --path /admin_password)
EOF
# source ~/.bosh-pve.env
```

## 7. SSH into the Director VM

If you deployed with `jumpbox-user.yml` (step 5), pull the private key from `creds.yml` and connect:

```bash
bosh int manifests/creds.yml --path /jumpbox_ssh/private_key > /tmp/jumpbox.key
chmod 600 /tmp/jumpbox.key
ssh -i /tmp/jumpbox.key jumpbox@$(bosh int manifests/vars.yml --path /internal_ip)
# inside the VM: sudo -i  (root shell)
```

Useful one-liners from the jumpbox session:

```bash
sudo monit summary                                  # state of director jobs
sudo journalctl -u monit -n 100                     # monit log tail
sudo cat /var/vcap/sys/log/director/director.log    # Director-side CPI calls
sudo cat /var/vcap/jobs/pve_cpi/config/cpi.json     # rendered CPI config
```

To SSH into an instance the Director deployed (not the Director itself), use `bosh ssh` once the Director is healthy:

```bash
bosh -d <deployment> ssh <instance>/<index>
```

## 8. Updating the Director

To redeploy after changing the release, ops file, or vars, run:

```bash
make release VERSION=0.1.0            # rebuild if release code changed
bosh create-env ~/w/cloudfoundry/bosh-deployment/bosh.yml \
  --state=manifests/state.json \
  --vars-store=manifests/creds.yml \
  -o manifests/cpi.yml \
  -l manifests/vars.yml
```

`bosh create-env` is idempotent: it reads `state.json`, diffs the manifest, and applies the delta. Keep `state.json` and `creds.yml` together — losing either forces a full redeploy.

When the only change is the rendered CPI config — for example, after editing `pve_host` or rebuilding the release tarball — `create-env` still drains and recreates the Director VM. It does not hot-reload `cpi.json` in place; expect brief downtime even on config-only updates.

## 9. Tearing down

```bash
bosh delete-env ~/w/bosh-deployment/bosh.yml \
  --state=manifests/state.json \
  --vars-store=manifests/creds.yml \
  -o manifests/cpi.yml \
  -l manifests/vars.yml
```

This destroys the Director VM, its persistent disk, and the stemcell template.

If `delete-env` fails partway, residual PVE resources may need manual cleanup before retrying. On the PVE host, run:

```bash
# Replace 105 with the current_vm_cid from state.json, 9000 with the
# synthetic disk VMID (visible as vm-9NNN-disk-0 in the disk_storage pool).
qm stop 105 --skiplock 1 2>/dev/null
qm destroy 105 --purge 1 2>/dev/null
lvremove -f <disk_storage>/vm-9000-disk-0 2>/dev/null
rm -f /var/lib/vz/template/iso/vm-105-config.iso
```

`qm destroy --purge` removes the VM and every disk listed in its config, including `unusedN` slots. Persistent disks live under a synthetic vmid (9000-29999); if they were never attached to the deleted VM, or were properly cleared via the SDK's two-PUT `DetachDisk`, they survive. Bare orphans on storage that no VM ever referenced must be removed explicitly with `lvremove` or `zfs destroy`.

## 10. Files in this workflow

| Path | Role | Commit? |
|---|---|---|
| `manifests/bosh/bosh-release.yml` | Ops file pinning the `bosh` release version | yes |
| `manifests/cpi.yml` | Ops file applied to `bosh.yml` | yes |
| `manifests/vars.yml.example` | Template for variables | yes |
| `manifests/vars.yml` | Real variables, includes secrets | **no** |
| `manifests/state.json` | VM/disk identity tracked across runs | **no** |
| `manifests/creds.yml` | Generated Director credentials | **no** |
| `./bosh-pve-cpi-*.tgz` | Built release tarball | **no** |

Add `manifests/vars.yml`, `manifests/state.json`, `manifests/creds.yml`, and `bosh-pve-cpi-*.tgz` to `.gitignore`.

## 11. Troubleshooting

- **CPI errors stream into `bosh create-env` output**

  The Director isn't up yet — `create-env` invokes the CPI binary locally over JSON-RPC. Look for the `cpi` task line; the JSON envelope contains the error.

- **PVE returns 401 / token errors**

  Verify `pve_user` includes the realm suffix (`root@pam`) and that the password or token is correct. Set exactly one of `pve_password` or `pve_api_token` in `vars.yml` and leave the other as `""`. See [Authentication](#authentication).

- **`create-env` fails with `Expected to find variables: pve_api_token` (or `pve_password`)**

  The ops file references both vars unconditionally. Define both in `vars.yml` — set the one you want to use and leave the other as `""`.

- **VM provisions but agent never reports**

  This is most often a network problem: the Director VM cannot reach the mbus endpoint at `((internal_ip)):6868`. Confirm the bridge, gateway, and DNS in `vars.yml` match the PVE network.

- **Stemcell upload is slow or times out**

  Set `pve_stemcell_storage` to a faster pool (`local` SSD) and ensure the PVE node has enough free space — stemcells are 1–2 GB.

- **Need a fresh state**

  Delete `manifests/state.json` and `manifests/creds.yml`, then rerun `create-env`. If the previous Director VM still exists, remove it from PVE manually first (see [Tearing down](#9-tearing-down) for the cleanup commands, including the synthetic-vmid persistent disk).

- **`Image is not in qcow2 format` during create_stemcell**

  Should not occur on current builds. The CPI auto-detects gzip-wrapped tar payloads (the BOSH stemcell carries `root.img` inside an inner gzipped tar) and extracts the qcow2 before upload. If you see this on an old release tarball, rebuild with `make dev-release`.

- **`parameter 'storage' not allowed for linked clones`**

  Triggered when `pve_vm_storage` differs from the stemcell template's storage and `full_clone` is unset. The CPI forces `full_clone=true` whenever `vm_storage` is set, but stale ops files that override this may still trigger the error.

- **`can't upload to storage type 'lvmthin'` on ConfigDrive ISO**

  `pve_iso_storage` must point at a file-backed storage (dir/nfs/cifs). Block storages (lvm/lvmthin/zfspool) cannot hold ISO content. Default is `local`.

- **`NUMA needs to be enabled for memory hotplug`**

  The CPI no longer requests memory hotplug. If you see this on an old build, rebuild with `make dev-release`.

- **`Logical Volume "vm-NNNN-disk-0" already exists in volume group`**

  Orphan persistent disk from a prior failed run. The CPI scans the disk storage for existing `vm-9NNN-disk-N` volumes when allocating a synthetic VMID, but stale orphans created before this fix may need manual removal:

  ```bash
  ssh root@<pve> 'lvremove -f <storage>/vm-9000-disk-0'
  ```

- **`Method 'POST /nodes/<node>/qemu/<vmid>/resize' not implemented`**

  PVE's `/resize` endpoint is PUT, not POST. Fixed in the CPI by calling raw `PutCtx` directly. Upstream SDK fix landed in `proxmox-apiclient-go` `qemu.ResizeDisk`. Rebuild if seen on an old release.

- **`Can't find property 'director.cpi_job'` during template render**

  `manifests/cpi.yml` must set `/instance_groups/name=bosh/properties/director/cpi_job` to `pve_cpi`. Without this property the Director's `director.yml.erb` template aborts. The ops file in this repo sets it by default.

- **`lvm name 'data:vm-...' contains illegal characters` during attach**

  Caused by a double-prefixed volid reaching PVE's disk-config parser. Resolved by decoding the `disk_cid` envelope with `pve.ParseEncodedDiskCID` (disk CIDs are `pvd-<base64url>`, or `pvz-<base64url(gzip)>` when compressed) and passing only the decoded bare `<storage>:<volname>` to `AttachDisk`. Rebuild the release if seen on an old build.

- **`no such logical volume <storage>/vm-9NNN-disk-0` during attach on redeploy**

  Re-deploying the Director destroyed its previous persistent disk. Root cause: PVE's `PUT /qemu/{vmid}/config` with `delete: scsiN` demotes the disk to an `unusedN` slot rather than fully clearing it. A subsequent `DELETE /qemu/{vmid}` then destroys every disk still referenced in the VM config — `unusedN` included — silently nuking the volume.

  The fix has two layers:

  - **SDK (`proxmox-apiclient-go` ≥ v3.1.2, published as `pve-apiclient-go` before v3.4.0)**: `qemu.DetachDisk` issues a second `delete: unusedN` PUT after the bus-slot detach so the volume is no longer reachable from the VM config.

  - **CPI `delete_vm` guard**: before issuing the destroy, the CPI reads the VM config and refuses to proceed if any `unusedN` entry references a volume on `pve_disk_storage`. This catches future SDK regressions or any bypass path.

  Rebuild the release and re-vendor the SDK if seen on an old build. If the LV is already gone, recover by deleting `manifests/state.json` and `manifests/creds.yml` and rerunning `create-env` (a fresh Director with a fresh persistent disk), or by manually re-creating the LV with `pvesm alloc <storage> <vmid> <name> <size>M --format raw` if the Director identity must be preserved.

## 12. Reference

- BOSH Director docs: <https://bosh.io/docs/bosh-components/>
- `bosh create-env` reference: <https://bosh.io/docs/cli-envs/>
- Upstream ops library: <https://github.com/cloudfoundry/bosh-deployment>
- Release source: this repository, see `README.md` for configuration properties.
- Persistent disks (shared vs local PVE storages, cloud_properties): [`persistent-disks.md`](./persistent-disks.md).
