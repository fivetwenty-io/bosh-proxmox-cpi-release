# Verify multi-storage placement in the lifecycle harness

The lifecycle harness can verify the base configuration against a disposable PVE lab. It checks actual VM and disk locations through the PVE API and obtains set membership from the production `pve-cid storage-plan` command. It does not reproduce the selection algorithm in Python.

Prepare at least two PVE nodes that can reach three independent ephemeral NFS members and two independent persistent members. Bind the global ephemeral and persistent sets separately, and provide the infrastructure storage required by the selected stemcell and agent mode. The CPI and `pve-cid` binaries must be built from the same candidate and installed together. Initialize the durable journal through the [journal operations procedure](../storage-journal-operations.md) before running the harness. The [lab topology page](multi-storage-lab.md) records the storage targets, capacity basis, and guest-agent fixture our reference lab uses.

Use the existing lifecycle environment variables for credentials, the stemcell, and a disposable test IP. Enable the placement checks with the following command. Replace the paths with the intended lab files.

```sh
CPI_CONFIG=/absolute/path/lab-cpi.json \
CPI_BIN=/absolute/path/candidate/bin/cpi \
STEMCELL_PATH=/absolute/path/stemcell.tgz \
MULTI_STORAGE_PLACEMENT_TEST=on \
VM_EPHEMERAL_MIB=1024 \
STORAGE_PLACEMENT_REPORT=/absolute/path/evidence/dedicated.json \
./scripts/lifecycle
```

The preflight rejects insufficient topology, duplicate backings, incomplete node reachability, and an infeasible plan. After creation, it verifies that root belongs to E, a dedicated ephemeral disk shares root’s member, and the persistent disk belongs to P. The ordinary lifecycle sequence then exercises attachment, snapshots, resizing, detachment, and deletion.

The fault checks submit an unknown set, competing selectors, and a scalar outside the persistent boundary. Each must fail for the expected policy reason without changing the journal’s allocation records. These checks do not alter exports, quotas, or cluster permissions.

Run again with `VM_EPHEMERAL_MIB=0` and a different report path to verify that root uses E without adding a dedicated ephemeral disk. Repeat both modes after setting the global P and E strategies to each of `spread`, `weighted_free_space`, and `least_utilized`, with version 1. Keep the set membership and namespace stable between those strategy runs. Each lifecycle run uses a new agent ID by default.

The report records the candidate binary checksum, production planning observations, actual volids, and completed checks. It deliberately does not mark the release matrix complete. Preserve it with the [candidate archive identity](candidate-artifacts.md) and the remaining evidence required by the [implementation plan](../plans/multi-storage-placement-plan.md). Singleton policies, shared capacity domains, injected backend failures, interrupted operations, and upgrade/downgrade cases require their corresponding fixtures and results; a passing base lifecycle run supplies no evidence for an unexecuted case.

Use the [parameterized scenario runner](multi-storage-scenarios.md) for policy combinations, membership removal, declared backend faults, and isolated recovery cases. Its report keeps missing capabilities separate from passing evidence.
