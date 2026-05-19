# Examples

## Basic Deployment Manifest

```yaml
instance_groups:
- name: example-vm
  jobs:
  - name: pve_cpi
    release: bosh-pve-cpi
    properties:
      pve:
        host: pve.example.com
        user: root@pam
        password: secret
        node: pve1
        stemcell_storage: local
        vm_storage: local-lvm
        disk_storage: local-lvm
        network_bridge: vmbr0
  vm_type: small
  stemcell: ubuntu-jammy
  networks:
  - name: default
```
This deploys a VM on PVE with specified storage and network settings.

