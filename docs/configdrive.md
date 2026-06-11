# ConfigDrive

When `pve.agent_mode` is `cloudinit`, the CPI delivers BOSH agent settings to a newly created VM via an OpenStack ConfigDrive ISO attached as a CD-ROM. This is the only cloud-init bootstrap path.

## How it works

The CPI builds an ISO 9660 + Rock Ridge volume labeled `config-2`. The ISO contains two layout trees:

- **Primary (OpenStack):** `/openstack/latest/user_data` carries the raw BOSH settings JSON. `/openstack/latest/meta_data.json` is written alongside it as a minimal JSON stub. This is the path that BOSH openstack-kvm stemcell agents read first.

- **Fallback (EC2):** `/ec2/latest/user-data` and `/ec2/latest/meta-data.json` duplicate the content for stemcells whose datasource is configured for EC2.

ISO size is fixed at 10 MiB. The CPI uploads the ISO to the PVE storage pool, then attaches it to the VM as a CD-ROM on `scsi30` (see [SCSI slot usage](#scsi-slot-usage)).

Stock OpenStack stemcells from bosh.io (for example `bosh-openstack-kvm-ubuntu-noble-go_agent`) recognise the primary layout on first boot without any `#cloud-config` parsing, `runcmd`, or `systemctl` step.

## Why no PVE-native sub-mode

An earlier `cloudinit_mode: native` setting attempted to deliver settings via PVE's `cicustom` + snippets mechanism. PVE's storage upload REST endpoint only accepts content types `iso`, `vztmpl`, and `import` — `snippets` requires SSH/filesystem placement to `/var/lib/vz/snippets/`. The CPI therefore drops the native sub-mode. ConfigDrive-via-CD-ROM works on stock OpenStack stemcells without any cloud-init service running in the guest.

## SCSI slot usage

PVE exposes `scsi0` through `scsi30` (31 slots). The CPI reserves slots as follows:

| Slot          | Reserved for                                                  |
|---------------|---------------------------------------------------------------|
| `scsi0`       | System disk (cloned from stemcell template).                  |
| `scsi1`–`scsi28` | Ephemeral and persistent disks (attached by `create_vm` and `attach_disk`). |
| `scsi29`      | Headroom — unallocated to leave space for future use.         |
| `scsi30`      | ConfigDrive CD-ROM.                                           |

`create_vm` rejects deployments that supply more than 28 persistent `disk_cids` at creation time, so the headroom and ConfigDrive slots stay free.

## See also

- [Configuration reference](configuration.md)
- [Architecture overview](architecture.md)
