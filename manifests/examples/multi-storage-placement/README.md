# Configure multi-storage placement

These examples use three ephemeral exports and two persistent exports. The CPI chooses one export for each volume. Register the named stores in PVE, enable `images` content on disk stores, and make the shared ISO and image stores reachable from the selected nodes. The base template target remains `vm_storage`, inside the ephemeral set.

Copy a configuration and replace its host, node, credentials, storage IDs, and namespace. The JSON files use the CPI binary configuration format. For a BOSH job, place their properties under `properties.pve`; keep the durable journal directory mounted as described in [journal provisioning](../../../docs/storage-journal-provisioning.md).

| File | Configuration |
| --- | --- |
| [separate-sets.json](separate-sets.json) | Uses independent persistent and ephemeral sets. |
| [singleton-persistent.json](singleton-persistent.json) | Pins persistent disks to one export while retaining the ephemeral pool. |
| [alternate-strategies.json](alternate-strategies.json) | Defines all three strategies on the same members for each disk role. |
| [cloud-config.json](cloud-config.json) | Supplies VM and disk types that select the alternate strategy sets. Merge these types into the existing cloud config. |
| [regex-membership.json](regex-membership.json) | Selects shared NFS members with anchored patterns. |
| [root-split.json](root-split.json) | Gives root disks a separate global boundary and strategy. |
| [shared-capacity-domain.json](shared-capacity-domain.json) | Treats the persistent exports as consumers of one underlying capacity budget. |
| [cpi-config.json](cpi-config.json) | Defines two cluster contexts with separate namespaces and VMID bands. |

The cluster contexts inherit the journal directory and ISO-following behavior from the installed job. They cannot override those process settings. Each context supplies its complete storage-set map.

The root and a dedicated ephemeral disk share a target by default. A root override also moves guest ephemeral space when that space comes from the root disk. Set `ephemeral_disk_size_mb` when a VM needs a separate ephemeral volume.

Inspect a persistent request with the read-only planner after replacing the configuration values:

```sh
pve-cid storage-plan \
  --config separate-sets.json \
  --request disk-request.json \
  --json
```

This command reports an observation, without reserving capacity or creating resources. The configuration examples are loaded by the production Go validator in the offline test suite. They do not certify export availability, permissions, or a live deployment.
