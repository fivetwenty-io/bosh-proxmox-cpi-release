# Multi-storage lab topology

This page records the reference lab that certifies multi-storage placement. It lists the storage targets both clusters register, the capacity basis behind their quotas, and the way we reach and administer them. It also records the readiness checks that precede any CPI run, the order in which we remove temporary backing aliases, and the guest-agent stemcell fixture the harness depends on.

## Storage targets

Each export has its own ZFS dataset beneath `tank/nfs/labs/pve-cpi/multi-storage`. Its export path is the dataset name prefixed with `/`. Both clusters register the following storage IDs:

| Storage ID | Dataset suffix | Quota | Content |
| --- | --- | --- | --- |
| `nfs-ephemeral-1` | `ephemeral-1` | 512 GiB | images, ISO, import |
| `nfs-ephemeral-2` | `ephemeral-2` | 512 GiB | images, ISO, import |
| `nfs-ephemeral-3` | `ephemeral-3` | 512 GiB | images, ISO, import |
| `nfs-ephemeral-4` | `ephemeral-4` | 512 GiB | images, ISO, import |
| `nfs-persistent-1` | `persistent-1` | 2 TiB | images |

The multi-storage parent has a 4 TiB quota. Its CPI lab ancestor has a 4.5 TiB quota, and `tank/nfs` has a 5 TiB quota. The saved `pve-cpi` pmx definition sets `nfs_quota_gb` to `4608`, so NFS reconciliation preserves the CPI lab limit. The datasets use LZ4 compression, 64 KiB records, and standard synchronous-write behavior. Their capacity limits do not reserve space. All exports share the physical `tank` pool and ancestor quotas; they are separate placement targets, not independent hardware failure domains.

## Capacity basis

We corrected the initial smoke-test quotas after inspecting the `wayneeseguin` and `nabramovitz` labs through `pmx`. VM configurations provided disk sizes, and storage status provided physical usage. Templates are excluded from the configured totals below.

| Reference lab | CF disks | All configured guest disks | Physical usage on local-lvm-data |
| --- | --- | --- | --- |
| `wayneeseguin` | 226 GiB | 2,528 GiB | About 240 GiB |
| `nabramovitz` | 234 GiB | 1,874 GiB | About 223 GiB |

Each reference contains 15 CF VMs. The wider totals include management services, artifact storage, and other deployed workloads. The `wayneeseguin` guests were stopped when inspected; `nabramovitz` included running guests. These are observed lab footprints, not identical deployment specifications.

The larger reference has 1,243 GiB of root disks and 1,285 GiB of additional data disks. The revised targets provide 2 TiB for roots and ephemeral disks, plus 2 TiB for persistent disks. This budget accommodates one comparable OCFP environment across the two CPI labs, with room for compilation, rolling replacements, and growth. It does not assume two complete copies of the larger environment or unlimited snapshot retention. Low physical usage from thin provisioning is not the sizing baseline.

## Access and operation

NFS access is limited to `10.254.0.10` through `10.254.0.12` and `10.255.0.10` through `10.255.0.12`. The clusters use server addresses `10.254.0.1` and `10.255.0.1`, respectively. Mounts use NFS 4.1 with hard retry behavior, and the default disk format is qcow2.

Use `pmx` for inspection and administration. Storage definitions were created through `pmx pve storage create`; custom ZFS datasets and exports were created through `pmx -c lab ssh sm-0`. The fixed `pmx lab nfs attach` command manages the existing images, backup, and ISO exports, rather than these additional targets.

## Storage readiness

All five stores were active and shared on all six nodes. After resizing, every node reported four 512 GiB ephemeral targets and one 2 TiB persistent target. We allocated thirty temporary 1 MiB volumes, listed them at the expected size through the other cluster, deleted them, and confirmed that they were gone. Existing storage definitions remain unchanged.

These checks establish storage readiness, but readiness alone does not certify deployment behavior or the full release matrix. The five original shares provide the requested singleton persistent target, and we register a supplemental encrypted persistent target for the plural-persistent cases. Because both clusters share the exports, certification must use nonoverlapping VMID ranges.

## Certification order for temporary backing aliases

When constraint fixtures register alternate storage IDs for the same backing export, remove those temporary aliases after the constraint tests and before continuing lifecycle or recovery audits. First verify the exact owned definitions and complete guest-reference inventory, then remove only the PVE storage metadata through `pmx` and retain raw before/after readbacks. Do not remove the backing exports or shared data. Compare configuration state without the cluster-wide revision digest; apply only the established unordered-membership rules to membership fields.

Keep failed assertions and incomplete attempts unchanged. Cleanup does not turn a failed test into a pass. Run the corrected assertion separately and combine only independently completed results in the final coverage record.

## Isolated guest-agent fixture

The existing Noble 1.484 stemcell lacks QEMU Guest Agent, which the harness needs to observe `/` and `/var/vcap/data`. We copied its import image through `pmx` and customized a separate copy on the bastion. The original import image and cached templates were not modified. Offline installation added `qemu-guest-agent` version `1:8.2.2+ds-0ubuntu1.18` and its dependencies. Guest inspection confirmed the installed package, systemd unit, and udev activation rule; `qemu-img check` found no image errors.

| Image | SHA-256 |
| --- | --- |
| Original Noble 1.484 source | `938a5e4c9dc1793336731684b38d6e78bcd46ce82e10a077afa2be1f39b65ccc` |
| Isolated QGA fixture | `47b48ce04ebac935227a69380d47920c34d71f9df1e69edbd7815a771c7ba9bd` |

The candidate created the fixture under the unique name `bosh-openstack-kvm-ubuntu-noble-storage-cert-qga`, version `1.484.cert20260909`. The primary cached template is VM 30880 on `lab-pve-cpi-0`; the secondary is VM 30381 on `lab-pve-cpi-az2-0`. Its path-identity CID uses `:heavy:nfs-images:import/bosh-stemcell-bosh-openstack-kvm-ubuntu-noble-storage-cert-qga-1.484.cert20260909-47b48ce0.qcow2`. These are owned validation fixtures and must remain tracked through cleanup.

The guest fixture uses 2048 MiB of RAM, an 8192 MiB root disk, and a 4096 MiB ephemeral disk. The larger ephemeral disk allows space for agent swap and `/var/vcap/data`. `agent_mode: cloudinit` selects the CPI's OpenStack configdrive implementation. The passing base case verified guest-agent responsiveness and actual filesystem mappings. Certification agent IDs now use UUIDs because BOSH uses the agent ID as the guest hostname; the previous scenario-prefixed IDs exceeded the hostname limit.
