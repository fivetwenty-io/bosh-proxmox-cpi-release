# Smoke-Testing the Director with an `emptyvm` Deployment

After `bosh create-env` brings up a Director (see [bosh-create-env.md](bosh-create-env.md)), verify that the Director can drive the PVE CPI end-to-end: upload a stemcell, render a cloud-config, create a VM, attach a persistent disk, and complete the agent handshake.

The `emptyvm` deployment is the minimum smoke test. It boots a single stemcell VM with no jobs — just a bare bosh-agent. If it converges, the full CPI surface (create_stemcell, create_vm, create_disk, attach_disk, agent settings delivery, network plumbing) is healthy. If it fails, the trace shows exactly which CPI method is broken.

This document covers:

1. The two manifests in `manifests/`: `cloud-config.yml` and `emptyvm.yml`.

2. Targeting the Director with the BOSH CLI.

3. The upload-stemcell, update-cloud-config, and deploy sequence.

4. Verification.

5. Common pitfalls discovered while bringing this CPI up.

## 1. Manifests

Two files in `manifests/`, both driven by `vars.yml`:

### `manifests/cloud-config.yml`

Minimal cloud-config: one AZ, two `vm_types` (`default` for compilation workers, `small` for instances), one `disk_type`, one manual network on the same subnet as the Director, and a compilation pool.

Key fields:

- **`azs`** — single AZ named `z1`. PVE has no AZ concept; the label is bookkeeping only.

- **`vm_types[*].cloud_properties`** — passed to `create_vm` as `cloud_properties`. The CPI honors `cores`, `memory` (MiB), `disk` (MiB — used for the post-clone `scsi0` resize), and `network_bridge`.

- **`disk_types[*]`** — `disk_size` is in MiB. Persistent disks are placed on `pve_disk_storage`.

- **`networks[*]`** — one `manual` subnet covering the same network the Director sits on (the env bundle's cloud config supplies the CIDR). `reserved` excludes the gateway and the Director IP so the next free address in that subnet goes to the smoke-test VM.

- **`compilation`** — minimal one-AZ pool. Cloud-config validation requires it even when no release will compile.

### `manifests/emptyvm.yml`

One-instance deployment with no jobs and no releases:

```yaml
name: emptyvm

releases: []

stemcells:
- alias:   default
  os:      ubuntu-noble
  version: latest

instance_groups:
- name:      emptyvm
  instances: 1
  jobs:      []
  vm_type:   small
  stemcell:  default
  azs:       [ z1 ]
  networks:
  - name: default

update:
  canaries:          1
  max_in_flight:     1
  canary_watch_time: 5000-30000
  update_watch_time: 5000-30000
  serial:            true
```

The `stemcells[*].os` value must match the stemcell uploaded in step 3 below. The defaults in `vars.yml` use `ubuntu-noble`. If you swap to `ubuntu-jammy`, edit both the URL and SHA in `vars.yml` and the `os:` field here.

## 2. Target the Director

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

`bosh env` should print the Director name and UUID. If it errors with a TLS error or `connection refused`, see Troubleshooting in [bosh-create-env.md](bosh-create-env.md).

## 3. Upload stemcell, cloud-config, and deploy

```bash
# 3a. Stemcell — same URL/SHA the Director itself was built on
bosh upload-stemcell \
  --sha1 $(bosh int manifests/vars.yml --path /stemcell_sha1) \
  $(bosh int manifests/vars.yml --path /stemcell_url)

# 3b. Cloud config — defines vm_types, networks, az, compilation
bosh update-cloud-config manifests/cloud-config.yml -l manifests/vars.yml

# 3c. Deploy
bosh -d emptyvm deploy manifests/emptyvm.yml -l manifests/vars.yml
```

Watch the task output. A clean run takes roughly one to two minutes and proceeds through:

1. `create_stemcell` — uploads the qcow2 image directly to `pve_stemcell_storage` (file-based PVE storage with `import` content enabled).

2. `create_vm` — clones the template to a new VMID, applies cpu/memory/network/scsihw, resizes `scsi0` to the requested disk size, generates an agent UUID, builds the `settings.json` payload.

3. `create_disk` + `attach_disk` — allocates a synthetic vmid in `[9000,29999]`, creates `vm-<vmid>-disk-0` on `pve_disk_storage`, attaches it to the new VM as `scsiN`.

4. Agent configuration — uploads the ConfigDrive ISO to the effective ISO pool (`pve_vm_storage` by default, since `pve.iso_storage_follow_vm_storage` defaults true and treats `iso_storage: local` as unset), attaches as `scsi30`, starts the VM.

5. Agent handshake — the bosh-agent boots from the stemcell, reads `settings.json` via the OpenStack ConfigDrive datasource, binds the mbus endpoint on `:6868`, registers with the Director.

## 4. Verify

```bash
bosh -d emptyvm vms
bosh -d emptyvm instances --details

# Confirm the VM responds to bosh ssh
bosh -d emptyvm ssh emptyvm/0 -c 'hostname; uptime; lsblk'
```

Expected output: one running VM with the agent reporting, `lsblk` showing the root disk (`/dev/sda`), the carved ephemeral partition, and the persistent disk (`/dev/sdb` or `/dev/sdc` depending on bus order).

On the PVE host, the new VM is visible with `qm list` and the persistent disk with `lvs` (lvmthin) or `zfs list` (zfspool). The VM's vmid is in the standard `[100,4999]` range; the persistent disk lives under `vm-9NNN-disk-0`.

## 5. Teardown

```bash
bosh -d emptyvm delete-deployment
```

This removes the VM and persistent disk in one step. If `delete-deployment` errors with a leftover resource, see the cleanup commands in [bosh-create-env.md](bosh-create-env.md#9-tearing-down).

## 6. Common pitfalls

These all surfaced while bringing the CPI up against a real PVE cluster. Each is now handled in the CPI, but the symptoms are worth recognizing.

### Director cannot resolve `pve_host`

```text
dial tcp: lookup pve-0.<tailnet>.ts.net on 127.0.0.53:53: no such host
```

The Director's DNS (1.1.1.1 / 8.8.8.8 by default) cannot resolve tailnet hostnames. Use the PVE LAN IP in `vars.yml`. See [pve_host must be reachable from the Director VM](bosh-create-env.md#pve_host-must-be-reachable-from-the-director-vm).

### ISO upload rejected

```text
can't upload to storage type 'lvmthin'
```

`pve_iso_storage` must be a file-backed storage (dir/nfs/cifs). Block storages cannot hold ISO files. By default the CPI follows `pve_vm_storage` (`pve.iso_storage_follow_vm_storage`, default true) and falls back to `pve_iso_storage` — spec default `local` — with a warning when `vm_storage` is block-backed or non-shared. Set `pve_iso_storage` to an explicit file-backed pool in `vars.yml` to pin it.

### Persistent disk name collision

```text
lvcreate 'data/vm-9000-disk-0' error: Logical Volume "vm-9000-disk-0" already exists
```

Orphan disk from a prior failed run. The CPI now lists existing `vm-9NNN-disk-N` volumes on the disk storage and unions them into the "used" VMID set before allocating, so 9000 is never recycled while its LV still exists. If you hit this on an older release, manually remove the orphan:

```bash
ssh root@<pve> 'lvremove -f <storage>/vm-9000-disk-0'
```

### Double-prefixed disk_cid attached to a new VM

```text
lvm name 'data:vm-9000-disk-0' contains illegal characters
```

Earlier CPI builds passed the full `disk_cid` (`<storage>:<volid>`) to PVE's disk-config endpoint, causing PVE to interpret `data:vm-9000-disk-0` as an LV name. Fixed by parsing `disk_cid` with `pve.ParseDiskCID` before the attach.

### Agent never reports ready

The bosh-agent will sit forever in two specific bad states, both producing the same outward symptom (`Waiting for the agent on VM '<vmid>' to be ready...` hangs):

- **Ephemeral disk poll**: an agent config that names a non-existent device (e.g., `Ephemeral: "/dev/sdb"`) makes the `DevicePathResolver` poll forever. The CPI leaves `Ephemeral` empty and lets the stemcell's `CreatePartitionIfNoEphemeralDisk=true` carve the ephemeral partition out of the root disk.

- **Disk too small**: the stemcell base disk is 5 GiB; the agent cannot carve a usable ephemeral partition without growing it first. The CPI resizes `scsi0` to `cloud_properties.disk` (MiB) after clone, via a PUT on `/nodes/<node>/qemu/<vmid>/resize`.

### Missing `director.cpi_job`

```text
Can't find property 'director.cpi_job'
```

Set by `manifests/cpi.yml` against `/instance_groups/name=bosh/properties/director/cpi_job: pve_cpi`. Without it, `director.yml.erb` aborts during template rendering. The ops file in this repo includes this op; if you fork it, preserve it.

## 7. Reference

- [bosh-create-env.md](bosh-create-env.md): bring up the Director.

- [cpi_methods.md](cpi_methods.md): which PVE endpoints each CPI method calls.

- [troubleshooting.md](troubleshooting.md): general CPI troubleshooting.

- BOSH cloud-config docs: <https://bosh.io/docs/cloud-config/>

- BOSH deployment manifest docs: <https://bosh.io/docs/manifest-v2/>
