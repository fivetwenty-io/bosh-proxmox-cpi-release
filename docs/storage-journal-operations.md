# Audit and recover storage allocations

Keep the allocation journal on durable storage and back it up with the Director that uses it. Each namespace has one writer authority. Before restoring or replacing that writer, stop its CPI workers and prevent them from restarting against the namespace. A second directory containing a copied journal is not an independent authority.

The commands below use the CPI binary and its effective configuration. Run them as the journal's execution user. The `cleanup` action deletes verified allocation-owned resources. The other commands inspect PVE without changing its resources. Commands that change journal state save the supporting audit before completing the transition.

## Enroll a new namespace

[Provision the private directory](storage-journal-provisioning.md), then inspect existing allocation provenance.

```sh
cpi storage-journal audit-enrollment --config /path/to/cpi.json
```

Initialization requires a complete historical absence audit. It also requires you to confirm that the previous writer is fenced and all its remote tasks have settled, including submissions whose responses were lost. For a new namespace, confirm that no previous writer or task exists. An empty storage listing cannot establish task settlement.

```sh
cpi storage-journal initialize \
  --config /path/to/cpi.json \
  --authority-id production-director \
  --previous-writer-fenced \
  --remote-tasks-settled
```

The flags record operator attestations. Supply them only after establishing the stated conditions. Initialization refuses existing enrollment or conflicting provenance. Restore retained history when resources already belong to the namespace.

The audit lists the content of every image-capable storage on every node. If it cannot finish one of those listings, `complete` stays false, since an unread storage could hold volumes the namespace already owns. A storage whose definition is disabled is the one exception. PVE refuses to list a disabled storage and refuses to allocate on it, so the audit skips it, reports it under `skipped_disabled_storages`, and stays complete. The audit never inspected the content of a storage it skipped, so run it again after re-enabling one and before relying on absence there. A disabled storage that a retained record does name is still audited, and its listing failure still keeps the audit incomplete.

## Inspect retained allocations

```sh
cpi storage-journal audit --config /path/to/cpi.json
```

The audit reports cluster continuity, generation-index health, retained record summaries, exact recorded CIDs, observed provenance, and incomplete access. A readable record does not establish a healthy index. Resolve access failures before relying on absence, including access to stores removed from current placement policy.

For a proposed deployment request, `pve-cid storage-plan` explains current placement without reserving capacity or claiming allocation ownership. Use the [configuration examples](../manifests/examples/multi-storage-placement/README.md) to prepare its sanitized request file.

## Restore authority or repair its index

Restore the complete private journal, including enrollment, index, records, and audit evidence. Preserve ownership and permissions. After fencing the old writer, audit the restored copy and rotate its authority.

```sh
cpi storage-journal recover-authority \
  --config /path/to/cpi.json \
  --authority-id replacement-director \
  --previous-writer-fenced
```

A cluster-identity change cannot rebind live or unresolved allocations. Changing an API hostname does not create a new cluster identity; the CPI verifies the cluster root CA.

If the generation index is corrupt or missing, reconstruct it from retained records only after settling all previous remote tasks.

```sh
cpi storage-journal recover-index \
  --config /path/to/cpi.json \
  --authority-id replacement-director \
  --previous-writer-fenced \
  --remote-tasks-settled
```

Index repair cannot discard an indexed generation whose record is missing. The following command addresses an index-first crash only when independent evidence confirms that no allocation mutation was submitted. A missing file alone does not prove that condition.

```sh
cpi storage-journal resolve-missing-vm \
  --config /path/to/cpi.json \
  --agent-id AGENT_ID \
  --allocation-id ALLOCATION_UUID \
  --authority-id replacement-director \
  --previous-writer-fenced \
  --index-first-crash-confirmed
```

The command retains the reconstruction evidence and a cleaned tombstone for that exact generation. It refuses matching remote resources or agent provenance.

## Record adoption or completed cleanup

Use the exact CID from the audit when a completed allocation needs explicit adoption. The command checks current ownership and settled mutation evidence before recording the decision. It does not insert the CID into the Director database.

```sh
cpi storage-journal adopt \
  --config /path/to/cpi.json \
  --allocation-id ALLOCATION_UUID \
  --expected-cid EXACT_RECORDED_CID \
  --decision-id incident-1234-adoption
```

Use a nonsecret audit reference for `decision-id`. To finalize cleanup after resources have been removed, record a separate decision.

```sh
cpi storage-journal finalize-cleanup \
  --config /path/to/cpi.json \
  --allocation-id ALLOCATION_UUID \
  --decision-id incident-1234-cleanup
```

Cleanup finalization does not delete resources. It requires settled mutations and a complete audit proving that the allocation's resources and provenance are absent. A remaining root volume, ISO, renamed disk, or uncertain task prevents finalization. Keep tombstones and verification history; age does not authorize purging them.

## Delete an allocation through its retained authority

Use `cleanup` when you intend to remove an allocation's resources. This command performs physical deletion, so select the full allocation UUID from the audit and record the reason in a nonsecret decision reference.

```sh
cpi storage-journal cleanup \
  --config /path/to/cpi.json \
  --allocation-id ALLOCATION_UUID \
  --decision-id incident-1234-approved-removal
```

The command requires live cluster continuity, a complete historical audit, and settled mutation evidence. Disk cleanup verifies the retained UUID and token, uses the recorded CID when available, and applies the managed disk lifecycle checks. Detach a disk from an ordinary VM through the managed lifecycle before requesting cleanup; free and parked disks can be cleaned directly. VM cleanup preserves independently owned persistent disks and removes only verified VM-owned resources. An uncertain allocation or asynchronous mutation blocks cleanup unless one of the narrowly defined proofs below applies. An empty resource listing does not establish task settlement.

A synchronous VM configuration update can remain `planned` when PVE accepted it but the CPI rejected its readback. For this case, explicit cleanup can retain the pending history and remove independently verified owned resources. First fence the previous writer and independently establish that all its remote tasks have settled, including submissions whose responses were lost. Record that investigation under the decision reference, then run:

```bash
cpi storage-journal cleanup \
  --config /path/to/cpi.json \
  --authority-id operator-writer --previous-writer-fenced \
  --remote-tasks-settled \
  --allocation-id FULL-ALLOCATION-UUID \
  --decision-id incident-1234-config-cleanup
```

This path accepts only a pending, task-less, uncharged configuration step on a recorded VM or disk parker. It checks every unresolved task on its source node and requires an empty active-task list on every cluster node. The API identity needs `Sys.Audit` on those nodes, in addition to complete storage audit permissions. It also requires a complete conflict-free resource audit and independent ownership proof: the full current VM marker and recorded volumes, or the disk's full allocation UUID, namespace, and token. An unmarked clone, absent artifact, running or failed task, missing task record, incomplete visibility, or uncertain allocation remains blocked.

After a managed VM cleanup deletion task succeeds, the CPI polls read-only volume absence for up to 90 seconds, bounded by any shorter caller deadline. PVE can remove an empty ISO directory and recreate it during storage activation. NFS directory-cache defaults can delay visibility for up to 60 seconds; see the [NFS mount documentation](https://man7.org/linux/man-pages/man5/nfs.5.html). The longer window allows this convergence without repeating a deletion. It still requires a successful, complete listing and independent visibility proof. If absence remains unproven, the allocation retains its pending deletion evidence and requires explicit reconciliation.

The same command can finalize an already completed disk deletion whose success readback failed. This task-backed path requires exactly one recorded submitted `imgdel` operation. Its UPID must identify the recorded node and the volume's embedded VMID and storage, and independent task observation must report `stopped` and `OK`. The target must match earlier observed ownership. Complete historical absence and a fresh exact backing and storage-membership check must then prove that no owned artifact remains. The command records `cleaned` without issuing another remote mutation or rewriting the original planned and submitted steps. A missing UPID, unrelated task, changed target, remaining artifact, or incomplete visibility still blocks this path.

For a persistent allocation whose synchronous response was lost, the attested command accepts its original `create_persistent_volume` intent only when the birth filename contains the exact namespace and full allocation UUID, its target and untouched charges match the frozen plan, and the intent retains the exact-name absence observation made before submission. A complete historical audit must prove either owned, detached storage or complete artifact absence. The command preserves the unknown step and never retries allocation. If cleanup itself loses a deletion response, a later invocation can finalize the exact full-UUID target only after fresh task settlement, unchanged backing, and complete absence; it does not submit another delete. Known deletion UPIDs still require successful completion.

For an ephemeral allocation with a lost synchronous response, cleanup requires the full allocation UUID in the birth filename and the recorded absence check from before submission. The original target and charges must match the frozen plan. While the VM exists, its full allocation marker, the unattached volume, the exact size, and all guest references must be verified independently. The command never substitutes the older numeric ephemeral filename for UUID provenance. A preexisting name blocks allocation before the CPI submits a request; matching size does not establish ownership.

A root creation or clone whose response was lost needs the original request/response receipt as well as its recovered UPID. Retain that receipt at the forwarding boundary before interrupting the CPI. Do not reconstruct it from a task list or a matching template ID. Supply the private receipt file with the exact original step and recovered task identifiers:

```bash
cpi storage-journal cleanup \
  --config /path/to/cpi.json \
  --authority-id operator-writer --previous-writer-fenced \
  --remote-tasks-settled \
  --allocation-id FULL-ALLOCATION-UUID \
  --decision-id incident-1234-root-cleanup \
  --recovered-task-step ORIGINAL-STEP-ID \
  --recovered-task-upid ORIGINAL-UPID \
  --recovered-task-evidence /private/evidence/original-response.json
```

The receipt records its schema version, namespace, allocation UUID, exact original step, request method and path, selected request identity fields, response status, and UPID. Creation receipts must include the full allocation marker and destination VMID. Clone receipts also identify the source endpoint, destination node, clone mode, and any required storage and format settings. Upload receipts identify the intended ISO filename and content type. The CLI rejects symlinks, nonregular files, public permissions, oversized files, and trailing JSON. These are retained operator records, not signed PVE attestations. The command records them with its decision evidence and still obtains fresh task and ownership observations from PVE.

Root allocation tasks may finish with an error and still leave an owned VM. Only the exact pending root task may use terminal failure as settlement evidence. The task must have stopped, and every cluster node must report an empty active-task list. Cleanup then requires either the full VM marker and exact root and auxiliary volume readbacks or complete VM and artifact absence. A marked VM whose root was never created can also be removed, but only after its configuration has no disk references and every current and historical backing is checked for files at its VMID. A failed deletion or ISO upload does not gain this exception. Durably observed steps do not require expired task logs to be fetched again.

An interrupted cleanup retains its original unknown steps. A later invocation may proceed after a lost stop, destruction, volume deletion, or HA purge response only when it freshly observes the required effect. It does not repeat a request whose outcome remains unknown. A remaining VM after an unknown destruction or a remaining volume after an unknown deletion blocks that path. HA recovery also verifies that other resources remain in shared rules with their existing policy.

If destruction succeeded before an unattached ephemeral volume or ISO could be removed, the earlier cleanup admission must already contain its ownership proof. Recovery combines that retained identity with fresh guest absence, unchanged backing, exact content, and a complete reference scan. It rejects unrelated files at the VMID. Cleanup preserves the original submission history and records the final disposition separately.

`--remote-tasks-settled` also attests that accepted synchronous requests have finished. An empty PVE task list cannot prove this. Retain that investigation under the decision reference before invoking cleanup.

The attested cleanup command can also close a VM generation whose only mutation intent is a planned shared-pool creation, before any VM or storage write. The frozen plan must identify a valid shared pool, not a pool-based lock; mutation charges, additional steps, and ambiguous attempt history block this path. Writer fencing, fresh task settlement, a complete historical audit, authoritative VM absence, and a scan for files at the planned VMID are required. Cleanup preserves the original planned step and the shared pool, records `cleaned`, and releases only the unused allocation generation. Outstanding reservations in the frozen plan are preserved as history; they are not evidence of an acquired resource.

After a successful VM-owned volume deletion task, normal cleanup waits up to 30 seconds for a complete absence observation, checking every 500 milliseconds. These checks read storage membership and permissions; they never repeat the deletion. Cancellation or an unproven deadline leaves the step submitted for explicit reconciliation. Logs distinguish still-present content, unavailable or malformed listings, and incomplete visibility using bounded reason codes without backend response text.

For a VM's already deleted agent ISO, the command accepts one submitted `vm.delete.volume` step whose successful `imgdel` task identifies the exact node and storage. The ISO must match an earlier observed VM-owned volume and the frozen ISO plan. Deletion from another node is allowed only on the same shared backing. Complete historical absence, current backing and node access, and fresh exact volume absence must all be verified. Cleanup then runs the existing VM and infrastructure disposition checks before recording `cleaned`. It preserves the submitted step and does not authorize ordinary lifecycle replay or another ISO deletion.

The same attested cleanup command also supports one submitted agent ISO upload whose task completed successfully but whose readback failed. It requires the exact frozen ISO target and outstanding charge, the current full VM ownership marker, unchanged backing, and exact ISO listing and detail sizes. The recorded `imgcopy` worker may run on another node in the independently observed cluster. PVE can copy the upload to its destination over SCP. Cleanup queries that exact worker node and UPID and requires `stopped` and `OK`. It also scans the worker’s task history for the exact start second and task type, through a complete bounded set of pages, and requires one matching successful task with a valid recorded start and end time. Missing, duplicate, conflicting, or unavailable task history blocks cleanup. Because the task has no file identity, the listed inode change time must fall within that recorded interval, including its endpoints. This timestamp remains corroboration, not independent ownership proof; clock differences or later inode changes can still block cleanup. Successful admission permits deletion of that exact ISO and the independently owned VM resources. It does not permit upload replay or mark the original submitted step observed.

Cleanup failures include a bounded stage code. For example, `cleanup_pending_mutation_settlement` means task or fencing evidence could not be established; `cleanup_historical_audit` means complete resource visibility failed; and `cleanup_vm_ha` means HA membership or purge readback could not be verified. These codes omit raw backend responses. Inspect the retained audit and task evidence before another invocation.

The command saves the attestations and task evidence without marking the old configuration step observed. Its permission to proceed exists only during this cleanup invocation. A new failed cleanup step blocks further writes; another invocation must establish fresh evidence. Allocation replay, adoption, and ordinary lifecycle operations do not inherit this permission.

Normal VM deletion may retain ephemeral storage under the configured retention policy. The journal records this as `vm_deleted_retained`. The same agent can create a new VM generation, while the retained artifacts remain auditable and prevent cluster rebinding. Later cleanup keeps the old generation closed, including when its cleanup response is uncertain. The `keep_failed_vm` policy leaves a failed allocation unresolved and does not use this retained-deletion state.
