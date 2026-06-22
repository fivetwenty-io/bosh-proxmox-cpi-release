# Examples

## Minimal `bosh create-env` CPI block

The `cloud_provider` block in your `bosh.yml` (or its ops-file overlay) wires up the PVE CPI for Director bootstrapping. The CPI job name is `pve_cpi` from release `bosh-pve-cpi`.

```yaml
cloud_provider:
  template:
    name: pve_cpi
    release: bosh-pve-cpi

  properties:
    pve:
      host: pve.example.com
      port: 8006
      user: root@pam
      # Use api_token or password, not both.
      api_token: "root@pam!bosh-cpi=<uuid>"
      node: pve01
      vm_storage: local-lvm
      disk_storage: local-lvm
      stemcell_storage: local
      iso_storage: local
      network_bridge: vmbr0
      verify_ssl: true
      agent_mode: cloudinit

    agent:
      mbus: "https://mbus:((mbus_bootstrap_password))@0.0.0.0:6868"
```

The `agent.mbus` value shown is the `bosh-deployment` default for create-env bootstrapping. When using the upstream [cloudfoundry/bosh-deployment](https://github.com/cloudfoundry/bosh-deployment) base manifest, supply the PVE CPI properties via the `manifests/bosh/cpi.yml` ops file in this repo; it sets `cloud_provider.properties.pve` from vars so you do not have to hand-edit the base manifest.

## Minimum required properties

| Property | Required | Notes |
|---|---|---|
| `pve.host` | Yes | PVE API hostname or IP reachable from the Director VM |
| `pve.user` | Yes | PVE username, e.g. `root@pam` |
| `pve.password` or `pve.api_token` | Yes (one of) | Mutually exclusive; prefer `api_token` |
| `pve.node` | Yes | Default PVE node name |
| `pve.vm_storage` | Yes | Storage pool for VM root disks |
| `pve.disk_storage` | Yes | Storage pool for persistent disks |
| `pve.stemcell_storage` | No | File-backed pool for stemcell qcow2 images; falls back to `vm_storage` when unset |
| `pve.iso_storage` | Yes | File-backed storage pool (`iso` content enabled) for ConfigDrive ISOs; block storages are rejected |
| `pve.network_bridge` | No | Default bridge for NICs that set no `bridge` cloud property; defaults to `vmbr0` |
| `pve.agent_mode` | No | Bootstrap mode: `cloudinit`, `noagent`, or `auto`; defaults to `cloudinit`. `auto` always selects ConfigDrive regardless of stemcell API version. |

## Stemcell

Use `ubuntu-noble` stemcells. The openstack-kvm flavor boots on PVE because PVE runs QEMU/KVM.

```yaml
stemcells:
- alias: default
  os: ubuntu-noble
  version: latest
```

Upload via:

```bash
bosh upload-stemcell \
  --sha1 "$(bosh int manifests/bosh/vars.yml --path /stemcell_sha1)" \
  "$(bosh int manifests/bosh/vars.yml --path /stemcell_url)"
```

## Complete `bosh create-env` invocation

```bash
bosh create-env ~/w/cloudfoundry/bosh-deployment/bosh.yml \
  --state=manifests/bosh/state.json \
  --vars-store=manifests/bosh/creds.yml \
  -o  manifests/bosh/cpi.yml \
  -o  ~/w/cloudfoundry/bosh-deployment/uaa.yml \
  -o  ~/w/cloudfoundry/bosh-deployment/credhub.yml \
  -l  manifests/bosh/vars.yml \
  -v  director_name=my-director
```

See [bosh-create-env.md](bosh-create-env.md) for the full vars file schema and bring-up sequence.
