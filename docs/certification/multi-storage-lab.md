# Multi-storage lab topology

## Current checkpoint on 2026-09-11 at 14:49 EDT

Candidate12 was built and compiled from the corrected source with release archive SHA-256 `c04a842f9dac222ba2f16fbc8e202b3c4817d433c012b208a707236eeca8033c`, CPI SHA-256 `3b4dffb727ca4b2bc34c8d76098b767215d22f971aca902de08e8ee336aadc28`, and pve-cid SHA-256 `91480a5f38c73704033754e842fc4f2fa873babdc52165f9d13b662ab0293241`. The bastion independently reconstructed the release through pmx, verified its full archive hash, and compiled it offline with the pinned Go 1.27.1 toolchain.

All fifteen fresh fault cases passed, including supported cleanup and fresh absence checks. The twenty-five-case policy, clone, and AZ2 package passed preparation and read-only validation. Its first two live cases passed, and the third is running. These results do not yet establish the complete release matrix; the remaining policy cases, four-VM batch, constraint and recovery cases, Director workflows, BATS, and rollout checks still require candidate12 evidence.

## Current checkpoint on 2026-09-11 at 13:30 EDT

Candidate11 completed all fifteen fault cases and the first twenty-four policy, clone, and AZ2 cases. Its final AZ2 weighted-free-space case failed after PVE completed the root clone for VM 8288. The CPI had already awaited and observed the guarded clone, then polled the same UPID a second time. That redundant poll converted a completed mutation into a reconciliation-required allocation when the completed task was briefly unavailable through the API.

The root creation path now relies on the managed mutation guard as its single task observer. A regression proves that import and clone creation wait exactly once; it fails against the prior code with two observations and passes with the correction. The scenario harness now records the current case before its create-VM diagnostic, preventing a planning or submission failure from retaining the preceding case identity. The full deterministic source checks, Python suite, vet, formatting, and static analysis pass. The handler package also passes under the race detector. The broader local race run reached only sandbox failures from tests that require loopback listeners.

The failed candidate11 case remains failed. Supported journal cleanup reconciled allocation `e7e4a3d4-8049-4c26-94a5-6a8d6e5df36b` and returned `state=cleaned`. Fresh `pmx` inspection confirms that VM 8288 is absent, and a fresh secondary-context audit is complete and conflict-free with the allocation recorded as cleaned. Candidate12 binaries and a source archive are prepared locally; they have no live acceptance credit. All sixty standalone cases, the separate batch, Director workflows, BATS, and rollout validation must run against the next verified candidate before the release matrix can pass.

## Current checkpoint on 2026-09-11 at 09:58 EDT

Candidate10 completed 26 named cases before its twelfth policy case failed during ISO cleanup. Supported journal cleanup subsequently succeeded, and fresh checks confirmed that the VM and all three volumes were absent. The original failure remains preserved.

The reviewed fix extends read-only deletion observation from 30 to 90 seconds without resubmitting deletion. Regression tests, the full source and race checks, and security scans passed, with 83.2% coverage. Candidate11's release archive and actual Linux binaries are built and verified.

Candidate11 requires all 60 fresh standalone cases, the separate batch, and Director, BATS, and rollout validation. Prior passes remain historical evidence. Director9 remains deployed, and the complete release matrix has not passed.

## Current checkpoint on 2026-09-11 at 03:08 EDT

**Candidate10 has completed twenty-five named cases.** All fifteen fresh fault cases remain passed. Policy controller `70631` has completed the first ten declared cases: all four spread combinations, all four least-utilized combinations, and the two weighted-free-space cases with a singleton persistent set. The eleventh case, `policy-weighted_free_space-pplural-esingleton`, is running and is not counted. The singleton-P/plural-E case has passed under all three strategies. Candidate10 has not yet completed the separate four-VM batch or started the twenty-case constraint and recovery sequence.

The Director10 updater, core/BATS package, and rollout package remain staged with verified inventories and have not been prepared or executed. Full standalone closure capture remains pending. It must follow all sixty fresh standalone cases and the batch, before backend teardown or access cleanup. The Director10 update and actual guest binary reproduction must follow completed teardown and access cleanup.

Candidate10’s archive and Linux binary identities remain unchanged. The full source gate, race tests, 83.2% coverage, all three security checks, and 677 integrated Python tests remain passed. A read-only repository audit verified 77 local documentation links and confirmed that the Git index was empty. Private and generated artifacts remain outside the intended commit scope.

Candidate9’s 56 named passes, its separate deterministic four-VM batch, and the completed recovery of its retained ISO allocation remain historical evidence. All earlier failures, artifacts, and checkpoints remain preserved. The complete release matrix remains **not passed**.

## Current checkpoint on 2026-09-11 at 02:50 EDT

**Candidate10 has completed twenty named cases.** All fifteen fresh fault cases remain passed, including supported cleanup and fresh absence verification. Policy controller `70631` has completed five cases through 02:50:18 EDT: the four spread combinations and `policy-least_utilized-psingleton-esingleton`. The sixth case, `policy-least_utilized-psingleton-eplural`, is running and is not counted. Candidate10 has not completed the separate four-VM batch.

The Director10 updater, core/BATS package, and rollout package are staged with verified inventories of 106, 98, and 110 files, respectively. Staging checks `54249`, `10409`, and `26248` passed. None of these packages has been prepared or executed. The twenty-case constraint and recovery package also remains staged and unexecuted. Its sequence creates both exact temporary metadata aliases, runs fourteen constraints, removes both aliases, and then runs six recovery cases.

The standalone closure workflow is implemented and independently reviewed. It validates the binding maps accepted by the current completion consumers and preserves seven exact mutable inputs in private snapshots. It retains historical JSON bytes without reinterpreting superseded binding maps. A read-only check of the actual fifteen-case fault closure passed against 2,149 bindings. Full closure capture must follow all sixty fresh standalone cases and the batch, before backend teardown or access cleanup. The Director10 update and actual guest binary reproduction must follow completed teardown and access cleanup.

Candidate10’s archive and Linux binary identities remain unchanged. The full source gate, race tests, 83.2% coverage, all three security checks, and 677 integrated Python tests remain passed. The supported cleanup of candidate9’s retained ISO allocation remains complete, with its original failed case and cleanup evidence preserved. Candidate9’s 56 named passes, its separate deterministic four-VM batch, and all earlier checkpoints remain historical evidence. The complete release matrix remains **not passed**.

## Current checkpoint on 2026-09-11 at 02:32 EDT

**All fifteen fresh candidate10 fault cases have completed.** Sequence `87603` exited successfully after every case passed its assertions, supported cleanup, fresh absence scan, and completion verification. Independent closure check `58828` passed for the final aggregate and sequence bindings. Candidate9’s 56 named passes and separate deterministic four-VM batch remain historical evidence.

Candidate10’s archive and Linux binary identities remain unchanged. The full source gate, race tests, 83.2% coverage, all three security checks, and 677 integrated Python tests remain passed. The supported cleanup of candidate9’s retained ISO allocation remains complete, and its original failed case and cleanup evidence remain unchanged.

Fresh policy, clone, AZ2, and batch preparation `72076` and validation `25585` passed. Live controller `70631` started at 02:32:30 EDT; no policy or batch pass is claimed here. The twenty-case constraint and recovery package remains staged and unexecuted. Its sequence creates both exact temporary metadata aliases, runs fourteen constraints, removes both aliases, and then runs six recovery cases.

The closure workflow is being completed to retain immutable standalone evidence before access cleanup changes bound SSH files. The Director10 update and its guest binary reproduction must follow all sixty fresh standalone cases, the batch, and teardown. Candidate10 core, BATS, and the three rollout cases remain pending. All earlier artifacts and checkpoints remain preserved, and the complete release matrix remains **not passed**.


## Current checkpoint on 2026-09-11 at 02:18 EDT

**Three fresh candidate10 fault cases have completed.** `vm_iso_response_loss`, `vm_iso_crash`, and `vm_ha_response_loss` passed their assertions, supported cleanup, fresh absence scan, and completion verification in sequence `87603`. The sequence is running `vm_ha_crash`; that case is not counted yet. Candidate9’s 56 named passes and separate deterministic four-VM batch remain historical evidence.

Candidate10’s archive and actual Linux binaries retain the identities recorded at 01:58 EDT. The full source gate, race tests, 83.2% coverage, and all three security checks remain passed, with 677 integrated Python tests. The pmx worktree was rechecked and remains clean. The separate candidate10 cleanup of candidate9’s retained ISO allocation remains complete, with the original failed case and cleanup preserved.

The fresh candidate10 twenty-case constraint and recovery package is frozen, reviewed, and staged. Its single sequence creates the two exact temporary metadata aliases, completes all fourteen constraints, removes both aliases with fresh settlement and ownership proof, and only then runs the six recovery cases. It will produce twenty new candidate10 rows without promoting historical passes. The fresh policy, clone, AZ2, and batch package is also staged. Both staged packages remain unprepared and unexecuted.

Final evidence preservation must occur before access cleanup changes bound SSH files. The closure workflow is being corrected to retain and validate snapshots before those intentional changes, while checking the final Director identity separately. Candidate10 Director core, BATS, and rollout successors remain pending. All earlier artifacts and checkpoints remain preserved, and the complete release matrix remains **not passed**.

## Current checkpoint on 2026-09-11 at 01:58 EDT

**Candidate 10 is built and verified, and the retained ISO allocation is cleaned and absent.** Candidate 9 retains its 56 completed named cases and separate deterministic four-VM batch. Candidate 10 has **zero completed acceptance cases**; resource recovery does not transfer those historical passes to the new candidate.

The ISO cleanup correction now accepts a verified upload worker on another cluster node and corroborates the ISO timestamp against the task's actual recorded start and end times. The reviewed source patch, test constant correction, and lint refactor were applied through their before/after guards. Their composed manifest is `ca0a93070c1540251d719f58c7506f476127e0a47bd5e31d8733133470bd0ddb`. Full `make check` session `28458` passed, including race tests and 83.2% coverage. Security session `63941` passed all three required checks. The integrated Python suite remains at 677 passing tests.

Release `0.0.0-storage-cert.20260911.10` passed build, archive verification, and native Linux compilation. Its archive SHA256 is `9d5ba43efe7d16e4f89e988ed4e69b9b6b6c69ee847eda6c6ed927f139908199`. The actual CPI SHA256 is `0f0962a35d37b35c5f61d0901515a65f055d1df8354d5be38f918325d1df2c8b`, and the diagnostic SHA256 is `f5c43fabd6cd766df14ce589d6923ff86907fd025f5f585b273d44b3be86de84`. The compiled receipt at `/tmp/candidate-v10/compiled-candidate-receipt.json` has SHA256 `3328fd0c71646b085b67065b3c872ed4bdb2f14b9d7f5b0f98390aba8fc523fb`. The build retains the candidate 9 baseline, all three reviewed patch components, and the installed PVE source evidence.

Supported cleanup session `10000` exited successfully for allocation `aac01aff-a08d-4fa7-9b75-076b1548d224`. Fresh settlement `6054` verified six idle nodes and both healthy namespaces without an allocation exception. Finish session `18849` completed at 01:57:57 EDT, proving VM `8073` and all three captured backing volumes absent. Its receipt is `/home/ubuntu/storage-cert-20260909/iso-cleanup-candidate10-r1-output/completion.json`, with `cleaned_and_absent:true` and `candidate10_acceptance_pass:false`. The candidate 9 ISO case and its failed R3 cleanup remain unchanged.

Fresh candidate 10 packages for all 15 fault cases and all 25 policy, clone, and AZ2 cases plus the separate batch are being prepared. The ISO fault cases will run first. Candidate 9 Director core, BATS, and rollout packages remain preserved and unexecuted; candidate 10 successors are still needed. All earlier failed attempts and checkpoints remain preserved. The complete release matrix remains **not passed**.

## Current checkpoint on 2026-09-11 at 01:27 EDT

**56 named live cases have completed on candidate 9.** These comprise 20 constraint and recovery cases, 25 policy, clone, and AZ2 cases, and 11 fault cases. The fault total combines two preserved R2 passes with nine fresh R3 passes, each completed through cleanup and fresh absence verification. The separate four-VM routing batch remains passed with its deterministic-key scope. Four fault rows remain incomplete.

Sequence `65084` stopped with exit code 1 during cleanup for `vm_iso_response_loss`. The fault assertion passed, but cleanup returned `cleanup_pending_mutation_settlement`, so the case is not counted as complete. Allocation `aac01aff-a08d-4fa7-9b75-076b1548d224` and its VM `8073` remain owned and retained. Settlement `36115` passed at 01:24:26 EDT, verifying all six nodes idle and both journal namespaces healthy while allowing that exact retained allocation. This is not an absence proof for the VM.

The cleanup code requires a submitted ISO upload worker's node to match the destination node. The actual worker ran on `lab-pve-cpi-0` for destination `lab-pve-cpi-1`. Inspection of the installed PVE `Status.pm` confirmed that it runs the upload worker locally and transfers the file through SCP. The retained source is `/tmp/pve-installed-storage-status-v9.pm`. The runtime correction is in progress; no patch has been applied. Candidate 9's archive and binaries are unchanged, and candidate 10 preparation is required after the correction.

The earlier quota export-identity correction and private command diagnostics are applied. All **677 integrated Python tests passed**, with output at `/tmp/multi-storage-python-20260911-0103.log`, and the Git whitespace check passed. The ISO case and cleanup evidence remain at `/tmp/fault-v9-iso-case` and `/tmp/fault-v9-iso-cleanup-failure`. All earlier failed attempts, completed results, journal evidence, and checkpoints remain preserved.

Director core workflows and BATS remain unexecuted. Rollout R4 is staged and checksum-verified, but it is unprepared and unexecuted. The fault matrix and complete release matrix remain **not passed**.


## Current checkpoint on 2026-09-11 at 01:01 EDT

**47 named live cases have passed on final candidate 9.** These comprise 20 constraint and recovery cases, 25 policy, clone, and AZ2 cases, and two fault cases. The separate four-VM batch also passed, including simultaneous placement across all four ephemeral shares, persistent disks on P1, and cleanup verification. Its deliberately selected request keys prove deterministic routing, not statistical fairness.

The export-outage and read-only-backend fault cases passed their assertions, restoration, cleanup, and fresh absence checks. The quota-exhaustion case then failed before applying its fault because the harness required an explicit anonymous UID in `exportfs` output. The host's effective export table confirms UID 65534, which the display omits as a default. Read-only inspection found no saved fault state and unchanged quota limits. A fresh scan verified both namespaces, all six idle nodes, and the restored backend. The original failure and its unsuccessful restoration attempt remain preserved. The remaining 13 fault cases will run separately after the harness correction.

The preflight diagnostic handoff correction is applied, and all **671 Python tests passed**. Candidate 9's runtime binaries are unchanged. The quota-display correction and private command diagnostics are under review. Rollout R3 is staged and checksum-verified, but its execution remains pending, along with Director workflows and BATS. The complete release matrix remains **not passed**.

## Current checkpoint on 2026-09-11 at 00:38 EDT

**45 named live cases have completed on final candidate 9.** The constraint and recovery aggregate contributes 20 passes, and all 25 policy, clone, and AZ2 cases have passed. Policy sequence `87812` completed successfully at 00:37:49 EDT. The separate four-VM routing batch also passed, including fresh verification that its resources were absent afterward.

The batch proved four simultaneous ephemeral placements and four persistent disks on P1. It used deliberately selected deterministic request keys; this result does not establish statistical fairness. The batch remains separate from the 45 named cases. At 00:38:27 EDT, the policy closure verifier passed all sequence and overlay bindings, including the completion receipt `/home/ubuntu/storage-cert-20260909/policy-sequence-v9-overlay-attestations/491281-b08803f6b8114c3c81d72b2d561ffccb.completed.json`.

Fault sequence `41889` started at 00:38:35 EDT through the same reviewed record-helper overlay, then stopped on its first case, `backend_export_outage`. The case failed its backing-identity preflight before the controller was invoked, any fault was applied, or an allocation was made. No retained resources were reported. The original failure evidence is preserved at `/tmp/fault-v9-first-failure`; backing inspection and remediation are underway. No fault case has passed yet, and the fault matrix remains incomplete.

The full Python suite remains at **671 passed tests**, and candidate 9's runtime identities are unchanged. All earlier failed attempts and historical checkpoints remain preserved. The 39 historical individual passes and the earlier four-VM batch retain their original candidate identities and remain separate from the final-candidate results.

The Director R3 package remains prepared and read-only validated, with no core workflow or BATS execution. Rollout R2 remains staged and checksum-verified, but it has not been prepared or executed. Fault cases, Director workflows, BATS, and the three rollout cases still require execution evidence. The complete release matrix remains **not passed**.


## Current checkpoint on 2026-09-11 at 00:08 EDT

**38 named live cases have completed on final candidate 9.** The completed constraint and recovery aggregate still contains 20 passes. All 18 primary policy cases have now completed their runs, fresh post-case settlement, and finish checks without failure.

The policy results cover all 12 strategy combinations of singleton and plural storage sets, the three root ISO cases, selectors and encryption, membership removal, and resumption after a strategy change. Policy sequence `87812` remains active. Its next case, `clone-auto-linked`, is running and is not included in the completed count. The other three clone cases, three AZ2 cases, and the separate four-VM routing batch remain pending. The policy aggregate is not yet complete.

The full Python suite remains at **671 passed tests**. Candidate 9's runtime identities are unchanged, and the `pmx` worktree remains clean. All earlier failed attempts and historical checkpoints remain preserved. The 39 historical individual passes and the earlier four-VM batch retain their original candidate identities and do not contribute to the 38 final-candidate cases.

The Director R3 package remains prepared and read-only validated, with no core workflow or BATS execution. Rollout R2 remains staged and checksum-verified, but it has not been prepared or executed. Fault cases, Director workflows, BATS, and the three rollout cases still require execution evidence alongside the remaining policy work. The complete release matrix remains **not passed**.


## Current checkpoint on 2026-09-10 at 23:35 EDT

**28 named live cases have completed on final candidate 9.** The completed constraint and recovery aggregate still contains 20 passes. The policy sequence has added 8 passes, covering all four singleton/plural combinations for `spread` and all four for `least_utilized`. Each of these policy cases completed its run, fresh post-case settlement, and finish checks.

The policy sequence started at 23:09:03 EDT through the reviewed record-helper overlay and remains active in session `87812`. Its first weighted case is running and is not included in the completed count. No case in the current policy sequence has failed. The policy aggregate and the separate four-VM routing batch are not yet complete.

The full Python suite remains at **671 passed tests**. Candidate 9's runtime identities are unchanged, and the `pmx` worktree was reverified clean. All earlier failed attempts and historical checkpoints remain preserved. The 39 historical individual passes and the earlier four-VM batch retain their original candidate identities and do not contribute to the 28 final-candidate cases.

The Director R3 package remains prepared and read-only validated, with no core workflow or BATS execution. Rollout R2 remains staged and checksum-verified, but it has not been prepared or executed. The remaining policy and clone cases, fault cases, routing batch, Director workflows, BATS, and three rollout cases still require execution evidence. The complete release matrix remains **not passed**.


On 2026-09-09, we added five NFS exports on physical host `sm-0` for the `pve-cpi` and `pve-cpi-az2` labs. The `lab` pmx context reaches this host through `pve-0.taile80fe.ts.net`. Each nested cluster has three nodes, and both clusters use the same five exports.


## Current checkpoint on 2026-09-10 at 23:09 EDT

All **20 final candidate 9 constraint and recovery cases have passed**, including their fresh settlement and completion checks. The aggregate combines 18 preserved passes with the two fresh `context_isolation` and `mixed_legacy_managed` passes. It covers 14 constraints and 6 recovery cases. The final report is `/home/ubuntu/storage-cert-20260909/recovery-two-v9-r4-20260910/final20-report.json`, and the sequence completion is `/home/ubuntu/storage-cert-20260909/recovery-two-sequence-v9-r4-20260910/completion.json`.

The earlier context-isolation attempt failed before allocation because its harness supplied an empty root storage-set override. The corrected harness omits the unset override. Both original isolated namespaces were independently verified empty, and the failed attempt remains unchanged. The earlier capacity and journal-audit failures, copied-authority cleanup evidence, and original journals also remain preserved. A cleanup or harness correction has not relabeled any failed attempt as a pass.

All **671 Python tests passed** with the applied context-isolation correction. Candidate 9's runtime archive and binary identities are unchanged. The 39 historical individual passes and the earlier four-VM routing batch retain their original candidate identities and remain separate from these 20 final-candidate results.

The Director R3 workflow package passed preparation and read-only validation without starting a core workflow or BATS. The rollout R2 successor is staged and checksum-verified. It preserves R1 and requires the completed 18+2 aggregate, policy and fault results, the routing batch, core workflows, and BATS before rollout. Its three runtime reproductions and final-phase verification remain mandatory.

Final-candidate policy and clone cases, fault cases, the routing batch, Director workflows, BATS, and the three rollout cases still require execution evidence. The complete release matrix remains **not passed**.

## Current checkpoint on 2026-09-10 at 22:34 EDT

All **14 final candidate 9 constraint cases have passed**, with assertions and fresh absence checks: 13 in the original run and the corrected post-root capacity case in its successor. The earlier failed case remains unchanged. These results are separate from the 39 historical individual passes and earlier routing batch, which retain their original candidate identities.

The first `journal_corrupt` attempt created and verified its owned 1 GiB disk, then its audit found the same allocation through `cert-ms-p1-alias` and `nfs-persistent-1`. The audit correctly refused before journal fault injection. Both constraint-only aliases have now been removed and independently verified absent. P1 deletion was not replayed. A finish-only check completed its evidence after accounting for the expected PVE configuration digest change. E4 removal passed its corrected postcheck. Only the storage definitions were removed; the backing exports and data were preserved.

The returned disk was deleted through the CPI using a fenced, exact copy of its journal authority. Fresh checks confirmed the copied record is deleted and the physical volume is absent. The original failed workspace and journal remain byte-for-byte intact for historical receipt bindings. The completion is `/home/ubuntu/storage-cert-20260909/recovery-returned-disk-cleanup-v9-output/completion.json`.

The six-case successor passed preparation and validation, then started at 22:33:19 EDT. The fresh `journal_corrupt` case passed its assertions, post-case settlement, and completion checks. This brings the final candidate 9 total to **15 completed cases: 14 constraints and 1 recovery**. Its receipt is `/home/ubuntu/storage-cert-20260909/recovery-six-v9-r3-20260910/cases/journal_corrupt/completion.json`. The next case, `journal_missing`, is running; no result is claimed for it. Its output is `/home/ubuntu/storage-cert-20260909/recovery-six-v9-r3-20260910`.

All 669 Python tests passed in the integrated run, including the applied bounded recovery-audit diagnostics. The retained log is `/tmp/multi-storage-python-20260910-2226.log`. The CPI runtime and candidate 9 binary identities are unchanged.

Director workflow certification, BATS, policy and fault cases, the routing batch, and rollout coverage still require final evidence. The complete release matrix remains **not passed**.

## Certification order for temporary backing aliases

When constraint fixtures register alternate storage IDs for the same backing export, remove those temporary aliases after the constraint tests and before continuing lifecycle or recovery audits. First verify the exact owned definitions and complete guest-reference inventory, then remove only the PVE storage metadata through `pmx` and retain raw before/after readbacks. Do not remove the backing exports or shared data. Compare configuration state without the cluster-wide revision digest; apply only the established unordered-membership rules to membership fields.

Keep failed assertions and incomplete attempts unchanged. Cleanup does not turn a failed test into a pass. Run the corrected assertion separately and combine only independently completed results in the final coverage record.

## Current checkpoint on 2026-09-10 at 22:08 EDT

Candidate 9 has passed the first 13 constraint cases, including stale capacity and acquired root capacity. Each has completed its assertions and fresh cleanup checks. Case 14 reached the expected capacity rejection, then its Python verifier rejected an omitted journal CID that the audit represented as an empty string. The corrected verifier and durable rollout evidence changes are applied; all 667 Python tests pass.

The supported CPI cleanup succeeded, and fresh checks confirmed that VM 8062 and its root volume are absent. The completion is `/home/ubuntu/storage-cert-20260909/post-root-v9-cleanup-r1-output/completion.json`. The failed case and the initial cleanup scan remain preserved. That scan failed before deletion because a packaged backend dependency used the wrong directory; the corrected successor passed its regression test and the live scan.

A fresh case 14 and the six recovery cases remain to run. The Director workflow preflight and configuration validation passed without starting a workflow. The complete release matrix remains **not passed**.

## Current checkpoint on 2026-09-10 at 21:25 EDT

The candidate 9 Director update and final verification passed. The Director now runs on VM 100008 and retains UUID `9c88e707-478d-4895-a708-001403623838`. Its logical persistent disk `84d35e43-a61a-41f4-65ff-d52d3cabad1a` remains on P2 at 64 GiB, and all 13 services are running. The final proof is `/home/ubuntu/storage-cert-director-20260909/create-env-v9-update-r1/verified-with-new-trust.json`.

Both actual guest executables were reproduced byte for byte from the pinned package and compiler. The guest CPI SHA-256 is `fa0f2f6ee1508caee0c3954b4eae621611fb19a76c5ddca2bb4d2f2417962783`, and the guest pve-cid SHA-256 is `978a777ad890315f53bea46512400e70aee771377f2f61efcbe29d767b3e7b8e`. These guest identities are recorded separately from the verified standalone candidate 9 executables.

The final candidate 9 constraint and recovery sequence started at 21:23:03 EDT. Its first case, `membership_regex_add_remove`, passed the assertion, fresh absence checks, and completion guard. This establishes **1 of 20 final candidate 9 cases** in that sequence. Its receipt is `/home/ubuntu/storage-cert-20260909/constraints-recovery-v9-20260910/cases/membership_regex_add_remove/completion.json`. The second case, `missing_storage_ids`, is in its pre-case settlement scan at this checkpoint; no result is claimed for it.

The **39 historical individual passes** and the earlier four-VM routing batch retain their original candidate identities. They are not counted as final candidate 9 certification. The final policy package has 25 prepared and validated cases plus a separate staged batch. The 15 fault cases are also prepared and validated. Both packages and their sequential controllers have been reviewed, and their live cases remain unrun.

The full Python suite passed with 662 tests, and the latest 33 focused rollout and TTY tests also passed. These counts describe separate checks and are not added together. Director workflow certification, BATS, the remaining standalone cases, the routing batch, and the rollout cases still require final evidence. The complete release matrix remains **not passed**. Earlier checkpoints and failed attempts remain preserved below.

## Current checkpoint on 2026-09-10 at 20:45 EDT

The retained R1 workload has been cleaned up. BOSH Task 10 deleted its deployment, and Task 11 deleted the owned 2 GiB orphan disk. Independent journal and pmx observations confirm that all three workload allocations are deleted and their VM and volume artifacts are absent. The baseline-only finish restored the original cloud configuration without repeating deletion. Its completion receipt is `/home/ubuntu/storage-cert-director-20260909/workload-v7-r1-cleanup-finish/completion.json`. The registered stemcell and the Director's 64 GiB persistent disk remain preserved.

The cleanup failures remain part of the evidence. Production parking uses a stopped dedicated VM with SCSI slots, so the guard now verifies that actual representation and its complete disk provenance. The Director truncates the orphan-deletion task result; the corrected harness instead checks the exact disk CID in authenticated completion events for the CLI-emitted task ID. A private import-path correction then allowed the baseline-only finish to complete. None of these failures caused a repeated deletion.

Candidate `0.0.0-storage-cert.20260910.9` is built and its packaged Linux executables are verified. The release archive SHA-256 is `e358a29e21060f4896e9cfba98144a9ad8e317f13535da84d9b69a0eac9cced0`. The standalone CPI SHA-256 is `5de04bd156b8fa532acc9b5f87e458f50f4b2859c75845b2d614c99b62da62a3`, and the pve-cid SHA-256 is `bb9f8075122f27c2f1a994a29f8d8f9013d12801f39dc3881157d8ce446911dc`. These are standalone build identities; any newly compiled Director binaries require their own verified reproduction.

The full Python suite passed after the cleanup and rollout corrections, and the current suite contains 662 tests. The first candidate 9 update preparation stopped on historical-symlink validation before creating its output or submitting an update. That issue is being inspected. The selected and original BOSH executable identities match; an executable-path mismatch has not been established.

The live tally remains **39 passing individual cases**, plus the separate four-VM routing batch. Those results retain their original candidate identities. The 25-case final policy package passed preparation and read-only validation. The separate batch is staged, and no final candidate 9 case is claimed here. The remaining final-candidate suites include policy and clone checks, constraints, recovery, faults, Director workflows, BATS, and three rollout cases. The complete release matrix remains **not passed**. The checkpoints below preserve the earlier evidence and status.

## Current checkpoint: 2026-09-10, 19:54 EDT

The live result remains **39 passing individual cases**, with the separate four-VM routing batch also passed. These historical results retain their original candidate identities. No v8 standalone recovery, continuation, or fault case has run, and the complete release matrix remains **not passed**.

The combined repository gate passed 638 Python tests, the full Go race suite, vet, staticcheck, and lint with zero findings. Go coverage was 83.2%. A subsequent security scan found two unchecked unsigned-to-signed charge conversions in VM cleanup admission. The applied correction rejects values above `MaxInt64` before either conversion. Boundary tests accept `MaxInt64` and reject larger values for both root and ephemeral charges; the original code fails all four overflow cases. The corrected isolated source passes the required govulncheck, gosec, and Trivy scans without new suppressions. All three source corrections are now applied, and the final combined gate is running.

The v8 R3 Linux candidate was compiled but is held because it predates that overflow correction. It has not been used for live cases. Candidate v9 will include the applied security, backend-observation, and Director orphan-cleanup corrections. Its build and verification remain outstanding.

The first settlement scan for the nine-case continuation rejected a backend mismatch before any case submission. That helper did not retain the differing raw observation, so the cause cannot be established. Two later probes matched the baseline exactly. The applied backend correction preserves raw observations before comparison and treats only storage `content` and `nodes` lists as unordered membership. Every other field remains exact. Its six focused tests pass; this does not establish the cause of the earlier mismatch.

BOSH delete-deployment Task 10 completed for the retained R1 workload, and its VMs are absent. BOSH retained one owned 2 GiB orphan disk, so cleanup is not complete. The original failed cleanup and its evidence are preserved. The supported BOSH orphan-disk cleanup correction is applied, and its separate continuation remains under review. No disk-deletion completion is claimed. The verified v7 Director, its persistent disk, and the registered stemcell remain preserved.

The stale-capacity rerun, two post-root constraints, six recovery cases, 15 fault cases, successful Director workflow certification, BATS, and upgrade/downgrade coverage remain outstanding. The pmx working tree is clean on `main`; commits `fee64cf` and `c8786d8` contain the completed inspection and NFS fixes. Earlier checkpoints below retain their historical status and evidence.

## Current checkpoint: 2026-09-10, 13:35 EDT

There are **39 passing individual live cases**: 28 earlier v6 lifecycle, strategy, supplemental-storage, and clone cases, plus 11 topology constraints. The separate four-VM routing batch also passed, including simultaneous coverage of all four ephemeral stores and verified cleanup. Its caller keys were deliberately selected; it does not prove equal distribution for arbitrary groups of four VMs. These results retain their original binary identities and are not relabeled as evidence for the next candidate.

The original constraint report passed its first 11 rows, then failed `stale_capacity_observation`; the two post-root capacity rows were not run. The stale-case failure came from a harness classifier that did not recognize the CPI's bounded error. The applied correction requires a feasible control, valid successful capacity reads delayed beyond the configured age, the exact rejection, no mutation, and unchanged state. A fresh live rerun remains pending. All six journal-recovery cases remain unrun. The original report and evidence remain in `/home/ubuntu/storage-cert-20260909/next-v6-20260910/`; both temporary aliases remain configured in PVE.

The v7 Director is verified at VM 100072 with UUID `9c88e707-478d-4895-a708-001403623838`; its persistent disk is preserved. Both guest executables were reproduced byte for byte from the pinned package and compiler. The CPI SHA-256 is `42ff5103940e08d92225593f7a8b00c3b5626d43c8a54d681a9c523d5306cf10`. Stemcell upload Task 4 passed. Workflow Task 6 completed the deployment, but the first certification row failed because copied fixture expectations did not match the deployed storage policy. That failure is preserved; it is not a passing Director workflow row.

The first retained-service proof then failed before storage evidence because its helper omitted topology preflight. The corrected read-only proof now passes for service VM 100137 and its exact persistent allocation. Supported BOSH cleanup of that retained deployment is prepared and reviewed but has no completion receipt at this checkpoint. The original Director, stemcell, journal, and failed-attempt evidence remain preserved. The fresh workload release `0.0.0-storage-cert.20260910.2` is verified and staged; its changed package fingerprint forces a new compilation when the next workflow runs.

The combined repository check passed **638 Python tests**, `go vet`, and staticcheck, then stopped on 42 lint findings. A separate 24-file refactor removes those findings without changing the ownership, task-settlement, or no-replay rules; isolated lint, vet, staticcheck, and the complete Go suite pass. The refactor still needs application and the combined final gate. The previously prepared v8 archive is held and cannot be used as the corrected candidate. The 43-file recovery change is applied, but live fault and recovery validation must use a newly verified candidate that includes the lint remediation.

The nine-case continuation and 15-case fault packages have reviewed preparation, sequencing, settlement, and evidence guards. Their source bindings must be refreshed for the corrected candidate before execution. BATS, successful Director workflows, backend and VM fault cases, and upgrade/downgrade coverage remain outstanding. The complete release matrix is **not passed**. The pmx NFS and inspection fixes are committed separately, and the pmx working tree is clean.

Earlier sections below preserve the preceding checkpoints and evidence. Their pending-status statements describe those earlier observations; this checkpoint gives the current state.


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

All five stores were active and shared on all six nodes. After resizing, every node reported four 512 GiB ephemeral targets and one 2 TiB persistent target. Thirty temporary 1 MiB volumes were allocated, listed with the expected size through the other cluster, deleted, and checked for absence. Existing storage definitions remain unchanged. Local evidence is under `/tmp/multi-storage-lab-20260909/`.

These checks establish storage readiness. CPI live validation has now begun, as recorded below; storage readiness alone does not certify deployment behavior or the full release matrix. The five original shares provide the requested singleton persistent target. A supplemental encrypted persistent target is now registered for the remaining plural-persistent cases, as recorded below. Because both clusters share the exports, certification must use nonoverlapping VMID ranges.

## Live CPI validation on 2026-09-09

We use the `ocfp-lab-pve-cpi` bastion to orchestrate the candidate through `ocfp --bloc ocfp-lab-pve-cpi ssh bastion`. The installed OCFP CLI is `v0.2.8/ad2eadc`, which passed bastion tests and SSH after the update. The certification workspace is `/home/ubuntu/storage-cert-20260909/`. Separate journal namespaces are enrolled for the two clusters. VM ranges are 8000–8199 in the primary cluster and 8200–8399 in the secondary cluster; disk-holder ranges are 20000–20999 and 21000–21999. Future parker ranges are 91000–91999 in the primary cluster and 92000–92999 in the secondary cluster. The rotation followed 51 pmx content listings, empty-holder checks, and complete audits; all 12 original configurations and all journal history were preserved. Existing workloads and Directors remain outside the certification workflow.

Three negative CPI checks passed. Requests naming an unknown set, competing selectors, or storage outside the persistent boundary were rejected, and each left both the physical inventory and journal hashes unchanged. The production VM diagnostic also returned a feasible observation-only plan after correcting its source of node CPU and memory counters. Its plan selects a full clone from the isolated fixture, with root, ephemeral, and ISO targets on an allowed ephemeral share. These results establish rejection and planning behavior.

The focused `policy-spread-psingleton-eplural` case passed its complete VM and persistent-disk lifecycle. VM 8100 used `nfs-ephemeral-3` for its root, ephemeral disk, and configdrive ISO. Guest inspection mapped `/` to `/dev/vda` and `/var/vcap/data` to `/dev/sda`. Persistent allocation `99034953-8cb4-4416-8f51-690cf2e6f488` stayed on `nfs-persistent-1` through attachment, growth from 1 GiB to 2 GiB, snapshot, detachment, and reattachment. Its allocation UUID, stable token, and NFS backing remained unchanged. Deletion completed, and the report verified actual absence of the VM and disk artifacts.

The earlier interrupted VM and disk allocations, including VM 8094, are cleaned or deleted. The Director bootstrap and subsequent v6 network correction passed, as recorded below. Explicit CPI cleanup settled the previously deleted persistent allocation `4a3d3ff6-e386-4e98-8cad-d3fc7af4d822` without another remote mutation. Audited cleanup removed VMs 8190, 8409, and 8009 and their owned artifacts. Normal CPI deletion removed VM 8007 and its root, ephemeral disk, and ISO. Independent pmx inspections confirmed absence, and all three failed preallocation records were cleaned.

Live testing also found that redundant storage-pool metadata could make a managed disk CID exceed the Director's length limit. The corrected encoding preserves the full allocation and volume identity, and focused regressions pass. The `pmx` custom-CA and raw-null-output corrections also have focused validation. Strict NFS membership and visibility checks now distinguish confirmed absence from unavailable or filtered observations. HA absence checks also use strict lists, and readback comparisons account for PVE-generated disk size and omitted empty descriptions. Later corrections address the remaining disk lifecycle absence checks and remove unnecessary retries for definitive missing-VM configuration responses.

The v5 source checkpoint passed the full `make check`, including 590 Python tests, the complete Go race suite, zero lint issues, and 83.2% statement coverage against an 80% requirement. `make security` passed govulncheck, gosec, and the configured Trivy HIGH/CRITICAL scan. The logs are `check-pool-boundary-final.log` and `security-pool-boundary-final.log` in the local evidence directory. The packaged v5 CPI has SHA-256 `a70e491bb75029f634fa9c65a1f8371f23199f0627c890f676978ac038a7f437`, recorded in `standalone-v5-identity.json`. This checkpoint includes the ISO-deletion convergence, metadata preservation, and pool-boundary corrections. The earlier 582-test, 83.0% gate remains recorded in `check-vm8099-final-v2.log` and `security-vm8099-final.log` for candidate `b28b7fc88a3d8e365f49c1ecb6b576583062e4dcd62bab05341db2e972465011`. These software gates do not establish completion of the live release matrix.

## Isolated guest-agent fixture

The existing Noble 1.484 stemcell lacks QEMU Guest Agent, which the harness needs to observe `/` and `/var/vcap/data`. We copied its import image through `pmx` and customized a separate copy on the bastion. The original import image and cached templates were not modified. Offline installation added `qemu-guest-agent` version `1:8.2.2+ds-0ubuntu1.18` and its dependencies. Guest inspection confirmed the installed package, systemd unit, and udev activation rule; `qemu-img check` found no image errors.

| Image | SHA-256 |
| --- | --- |
| Original Noble 1.484 source | `938a5e4c9dc1793336731684b38d6e78bcd46ce82e10a077afa2be1f39b65ccc` |
| Isolated QGA fixture | `47b48ce04ebac935227a69380d47920c34d71f9df1e69edbd7815a771c7ba9bd` |

The candidate created the fixture under the unique name `bosh-openstack-kvm-ubuntu-noble-storage-cert-qga`, version `1.484.cert20260909`. The primary cached template is VM 30880 on `lab-pve-cpi-0`; the secondary is VM 30381 on `lab-pve-cpi-az2-0`. Its path-identity CID uses `:heavy:nfs-images:import/bosh-stemcell-bosh-openstack-kvm-ubuntu-noble-storage-cert-qga-1.484.cert20260909-47b48ce0.qcow2`. These are owned validation fixtures and must remain tracked through cleanup.

The guest fixture uses 2048 MiB of RAM, an 8192 MiB root disk, and a 4096 MiB ephemeral disk. The larger ephemeral disk allows space for agent swap and `/var/vcap/data`. `agent_mode: cloudinit` selects the CPI's OpenStack configdrive implementation. The passing base case verified guest-agent responsiveness and actual filesystem mappings. Certification agent IDs now use UUIDs because BOSH uses the agent ID as the guest hostname; the previous scenario-prefixed IDs exceeded the hostname limit.

## Evidence and remaining coverage

Storage preparation evidence remains under `/tmp/multi-storage-lab-20260909/`. Current live evidence is under `/tmp/multi-storage-live-20260909/`, including `live-progress.json`, `qga-source-plan.json`, the stemcell creation and resource receipts, and `qga-fixture-evidence.md` with its raw `.tgz` archive. The negative-selector report is `evidence/negative-policy.json` beneath the bastion workspace. The persistent lifecycle and VM cleanup receipts are also retained beneath `bastion-evidence/` locally. The passing base-case report is `bastion-evidence/spread-singleton-p-plural-e-v7.json` locally and `evidence/spread-singleton-p-plural-e-v7.json` on the bastion. It identifies candidate SHA-256 `cd76344d5736042518e62ce053b06ff034927576ef37c3918513d6d038c0b631`. The historical ISO gate logs are `check-iso-recovery-final.log` and `security-iso-recovery-final.log`; `hostname-fixture-python.log` records the subsequent 571-test Python run.

The subsequent typed-diagnostic checkpoint also passed full `make check` with 590 Python tests, the complete Go race suite, zero lint issues, and 83.2% coverage. `make security` passed. Its logs are `check-absence-causes-final.log` and `security-absence-causes-final.log`. Standalone version `0.0.0-storage-cert-absence.20260909.1`, with CPI SHA-256 `b2c1f0d395d34db4d190e5b55ce06b336e3da5fc82a886da51daf349f087e8c8`, is staged. The combined v6 release is recorded below; these gates do not certify the incomplete live matrix.

The combined release `0.0.0-storage-cert.20260909.6` is built and deployed to the isolated Director. Its archive SHA-256 is `d28f3ff30d4765183c678b231fd8901746800bcd8df367a06f97d947b64263b2`, and its package fingerprint is `4bf30e299aeedcb40cb554e2051fe213499ba291715a699385db0e0748ae5889`. The isolated Linux packaging build produced CPI SHA-256 `36e3b6bed333815b889a4155e7772b62e03d0b8b31d4b65ddb64b7047f79baf3`. Artifact and manifest binding evidence is in `/tmp/multi-storage-director-20260909/v6-ready.json`; its pre-execution status records preparation, while subsequent bootstrap receipts record the completed update.

The v6 focused run completed all 17 cases successfully, superseding its eight-case progress snapshot at 20:59 UTC on 2026-09-09. Its report is `evidence/focused-v6-live-zlf42j09/report.json`. The secondary run passed all three singleton-P/plural-E strategies in `evidence/az2-v6-strategies-r_jksxcd/report.json`. The supplemental run passed all three plural-P/plural-E strategies and the encrypted-subset selector case in `evidence/supplemental-v6-la99cpyv/report.json`. Each report records CPI SHA-256 `36e3b6bed333815b889a4155e7772b62e03d0b8b31d4b65ddb64b7047f79baf3` and diagnostic SHA-256 `675cf73f76aad3ebfe0a24f254dea1423390d709883e36cb5388f029fa1b1003`. All 24 cases verified actual absence after cleanup and left no retained workload resources.

These runs select the packaged v6 executable through `--cpi-bin` without promoting it into the shared standalone `bin` directory. All three reports set `complete_release_matrix` to `false`. The final-candidate four-VM routing batch also passed in `evidence/spread-v6-live-e5774631/batch.json`. Four VMs ran simultaneously across `nfs-ephemeral-1` through `nfs-ephemeral-4`, with all persistent disks on `nfs-persistent-1`. Normal cleanup verified actual absence and left no active resources. The batch used deliberately selected caller keys, so it demonstrates deterministic routing across the four shares without establishing equal distribution for arbitrary groups of four VMs. Historical passes remain evidence for their recorded builds, and the cause of the earlier ISO-deletion interruptions remains unknown.

The secondary cluster completed all three singleton-P/plural-E strategy lifecycles, with actual absence verified after deletion. Its report is `evidence/az2-base-strategies-v1.json`. The primary four-VM batch originally stopped during a read-only verification with `PVEVerifyError`; the original report did not retain the exception details. A separate read-only recheck then verified all four simultaneously running VMs across `nfs-ephemeral-1` through `nfs-ephemeral-4`, with all persistent disks on `nfs-persistent-1`. Normal CPI cleanup subsequently passed for all four, including actual absence checks. The original failure remains recorded alongside `bastion-evidence/spread-batch-v1/readonly-probe.json` and `bastion-evidence/spread-batch-v1-completion/completion.json`. This demonstrates routing across all four ephemeral targets for deliberately selected requester keys; it does not establish equal distribution for arbitrary groups of four VMs.

The 17-case focused role run `focused-roles-gpNqW26l` passed its first case and failed its second during CPI `delete_vm`; the remaining 15 cases did not run. Explicit cleanup subsequently settled VM 8099 while preserving all 16 journal steps. The next run, `focused-roles-fOCtt5YX`, passed two cases before its third failed during ISO deletion for VM 8094, allocation `290d6ead-7c9b-47ba-be11-59334aef31cc`. The persistent disk was deleted, the ISO task stopped with `OK`, and independent pmx inspection found the VM and all recorded artifacts absent. Supported cleanup then settled the allocation while preserving all 16 original steps, as recorded in `vm8094-history-verification.json`. The original failed reports remain unchanged.

The subsequent run, `focused-roles-CYAzPItT`, passed six cases before the verifier confused a newly allocated VM 8009 with an older, cleaned generation that used the same numeric VMID. This was a harness failure after successful CPI allocation. The corrected verifier binds the live VM to its allocation UUID, checked journal and index, ownership marker, and complete audit evidence. Its adversarial tests, full Python suite, and independent review passed. Normal CPI deletion then removed generation `c13bd2b4-8539-415e-9565-0e76e8f79e88`; the before-and-after records preserve its original steps, and independent checks found no VM or volume artifacts on any of the three primary nodes. Receipts are in `bastion-evidence/reused-vm8009-cleanup/` and `vm8009-generation-reuse-absence.json`. The 11 focused cases without passing results continued in a separate run; the original failed report is not counted as a pass.

The 11-case continuation, `focused-roles-remaining-os2z09wV`, used the v5 standalone CPI and passed three cases. Its fourth case, `policy-weighted_free_space-psingleton-eplural`, failed during CPI `delete_vm` for VM 8009, allocation `ac8fa4f8-cb7b-4b4f-81b0-e81e645790f1`. The 30-second absence check retained `listing_unavailable` without its underlying cause. Later native pmx checks found a successful stopped ISO deletion task and no VM or recorded volume artifacts. Supported task-backed cleanup then marked the allocation Cleaned while preserving all 16 original steps. Receipts are in `vm8009-v5-native/` and `bastion-evidence/vm8009-v5-settlement/`. That continuation ended with the failed case and seven unexecuted cases. Their historical report remains unchanged; the v6 results recorded here provide fresh coverage.

The three ISO-deletion interruptions share a cross-node pattern, but their historical cause remains unknown. A stale NFS listing is a hypothesis, not a verified diagnosis. The correction preserves bounded error categories through the listing and deletion observer without changing absence requirements, the deadline, or mutation submission. A reviewed private hook is ready to capture immediate raw pmx listings after another failed deletion while preserving the original CPI packet and exception.

The supplemental encrypted target `nfs-persistent-cert-2` is registered, active, shared, and enabled on all six nodes. It uses AES-256-GCM and has a 512 GiB quota. The isolated Director bootstrap now has an owned 64 GiB persistent disk on this target, verified through pmx. The bootstrap allocation alone did not establish plural-P or encrypted-subset coverage; the four supplemental v6 cases recorded here now provide that live lifecycle evidence. The original five targets remain unchanged, and the supplemental fixture requires final cleanup. The pmx NFS shared-option correction is committed as `c8786d8`; CPI changes remain uncommitted.

The isolated Director bootstrap’s initial fixture error, nonexistent storage `nfs-persistent-2`, was corrected to `nfs-persistent-cert-2`. A later metadata update removed VM 100010's ownership marker. The metadata correction and its adversarial tests passed independent review, and a digest-guarded, description-only repair was completed through pmx before the old VM was removed. The next allocation, `8ef654c6-1eb9-4538-8175-225ee0bee5e7`, stopped at a single Planned `vm.Pool.CreatePool` step for target VMID 100055, before QEMU allocation. The v5 correction handles an existing pool safely and supports audited cleanup of this exact preallocation state without deleting the pool. Fresh proof admitted cleanup, and the resulting Cleaned record preserves the original Planned step.

At the v5 checkpoint, VM 100029 ran on node 0 with its ownership marker preserved. Its 32 GiB root used E1, its 10 MiB ISO used `nfs-images`, and its attached 64 GiB persistent disk used `nfs-persistent-cert-2`. Native pmx checks verified these artifacts. The v5 bootstrap succeeded, and authenticated health identified Director UUID `9c88e707-478d-4895-a708-001403623838`. Its deployed CPI SHA-256 was `9b8af2f08a986c1b2f1d9ece3bfd942fad27c638869d31c553aebfddf3777717`. Workload TLS preflight then failed because the guest’s /22 prefix did not match the lab’s flat /16 network. The shared QGA template 30880 is preserved.

Twelve future standalone manifests now use /16 netmasks and the canonical lab gateways, with every assigned IP preserved. Their hashes, exact two-field changes, and actual CLI validation passed independent review. Nineteen Director fixture files received the same correction, with allowed IP sets and storage pools unchanged. The v6 update applied the /16 correction. Guest route checks now reach all three primary nodes directly, and certificate-verified TLS 1.3 connections succeeded to each node.

The v6 `create-env` update succeeded at 20:08:54 UTC on 2026-09-09. VM 100076 replaced VM 100029 while preserving persistent disk CID `84d35e43-a61a-41f4-65ff-d52d3cabad1a` and Director UUID `9c88e707-478d-4895-a708-001403623838`. All 13 services were running, the authenticated API reported no deployments or tasks, and the complete post-update bootstrap audit had no issues or conflicts. The deployed CPI SHA-256 is `3d4fb0c56df47b9226443c5410deda83c32b1848b6bc4e95be18a41f813f0157`.

The Director observer now binds the authenticated guest's SMBIOS UUID to one VM in a fresh cluster inventory through pmx. It no longer assumes that BOSH guest settings contain `vm.id`. Canonical VMID checks and validation of SMBIOS fields reject malformed or ambiguous observations. All 197 storage-placement Python tests and the final independent review passed. After workload enrollment, the full live observer snapshot passed for VM 100076 and Director UUID `9c88e707-478d-4895-a708-001403623838`, with the packaged and deployed binary identities verified.

The SSH and BATS harness corrections passed the full 598-test Python suite. Subsequent corrections to runtime-user handling passed all 600 Python tests. Corrections to Director API parsing, failure evidence, and stemcell preflight then passed all 610 Python tests. The latest full Python suite passed all 619 tests after the wrapper and global-delete harness corrections were applied. Observer and retained-auditor SSH use an explicit trusted host-key file and `IdentitiesOnly=yes`. BATS uses the selected integration variables and restores the actual captured cloud configuration through the same Director endpoint, with readback verification. These harness changes do not change the runtime v6 candidate.

Director enrollment initially passed its audit, but initialization refused because the helper ran as root while the journal belongs to `vcap` with UID/GID 1000:1000 and mode 0700. Inspection proved that the namespace was absent and the journal was empty. The reviewed recovery ran the CPI as `vcap` and initialized an empty, healthy workload namespace. The receipt is `/home/ubuntu/storage-cert-director-20260909/bootstrap/workload-v6-enrollment-v2-launch.json`. The original refusal and its evidence hashes remain unchanged.

The first core Director attempt failed in `director_compilation` before any workflow mutation. The harness passed `--json` to `bosh curl`, then treated the CLI envelope containing `Blocks` as the `/info` response and rejected the Director UUID. Read-only checks confirmed that the Director had no tasks, deployments, releases, or stemcells, and a fresh semantic comparison confirmed that the cloud configuration still matched its captured baseline. The failed attempt remains in `/home/ubuntu/storage-cert-director-20260909/workload-v6/director-report.json`. The correction to API parsing and the new failure-evidence and stemcell preflight checks are applied. A second private package passed preparation and read-only prerequisite checks without uploading a stemcell. Further checks found that BOSH suppresses human-readable task output without a terminal. The explicit `--tty` correction and third private package were completed. Director validation then exposed the wrapper defect recorded below.

The third private package reached stemcell upload, but Task 1 failed with `Broken pipe` before a stemcell was registered or a workload was allocated. The CPI job wrapper resolved `../../packages` through the installed BOSH job symlink and selected the wrong package directory. Running the default wrapper as `vcap` reproduced the failure; an explicit `BOSH_PACKAGES_DIR` and a direct binary invocation both passed. The wrapper correction and realistic symlink regressions passed independent review. The failed upload and its logs remain preserved.

Release `0.0.0-storage-cert.20260909.7` is built and verified with archive SHA-256 `d7d54cb0a9086713096dc1d72b290f7ddcdba46fc118ecd6249f77926d4897b6`. Its job fingerprint is `dbe6bdc4591581b12ba34424dfe1bd560c2edc068c8a74d49bb28a69c06d7973`. Only the CPI wrapper changed in the frozen source snapshot. Both package fingerprints and every regular file's bytes and permissions match v6. The CPI package tar archive differs in staging timestamps and directory permissions, so its tar bytes are not identical. These differences and the preserved initial build refusal are recorded in `/tmp/multi-storage-director-20260909/wrapper-only-v7/candidate-v7-evidence.json`. The 28 lifecycle cases and four-VM batch retain their v6 identities and cover the unchanged implementation. They do not verify the new guest binaries or wrapper.

The first v7 update helper stopped during read-only preparation because the optional `BOSH_CONFIG` file was absent. It did not invoke `create-env` or change the deployed Director. Its partial output remains preserved. The corrected helper completed the supported v7 update successfully on replacement VM 100072. Agent logs recorded steady package compilation, including `postgres-15`. Fresh pmx inspection confirmed that all 13 services were running. The replacement guest's SSH trust was captured through pmx with verified TLS, and strict SSH verification passed. Final binary provenance, Director identity, and persistent-disk verification remain pending.

Read-only guest inspection found that both installed binary hashes differ from their earlier v6 counterparts. The package embeds build metadata, so a separate capture and exact rebuild will verify their provenance. The observed CPI reports the expected source package version and a build date of `2026-09-10T10:25:17Z`. The rebuild must match both captured binaries before their hashes are accepted. No Director workflow or BATS case has passed.

The next-suite preparation and read-only validation passed for 14 constraint cases and six recovery cases. Their validation reports are `constraints-validation-83f61026-1388-4b87-8d27-27b771b883d9.json` and `recovery-validation-3e2f1ec7-62fe-42c2-a388-a3206026ed27.json` beneath `/home/ubuntu/storage-cert-20260909/next-v6-20260910/`. The native pmx commands for the two temporary alias definitions are prepared. Neither definition creation nor live suite execution has run; both await the verified Director. The pmx working tree was reconfirmed clean.

Physical SSH access for the fault controller is installed and verified through pmx. A separate 32 GiB ext4 filesystem is provisioned on `loop0`, with UUID `33f98f3b-b902-45fa-b5f9-620aef430080`. Its `nfs-cert-fault` storage definition is active and empty on all three primary nodes. The native allocation baseline completed on all three nodes, including cross-node readback and normal cleanup. The initial transient read failure remains recorded alongside the successful rechecks in `fault-baseline-v1` and `fault-baseline-completion-v1`. No faults have been injected.

The historical base-case report explicitly sets `complete_release_matrix` to `false`. It proves the base lifecycle and guest mapping for one spread placement, not distribution across all four ephemeral shares. Clone, topology-constraint, journal-recovery, and fault manifests have preparation and read-only validation evidence. Their live assertions remain outstanding, along with BATS, Director workflows, upgrades, and downgrades. Focused singleton-P cases can use the requested five-share layout. The four supplemental v6 cases provide passing plural-P and encrypted-subset lifecycle evidence. The disposable backend filesystem is ready, but fault injection has not run. No passing initial check substitutes for an unexecuted release gate. The [implementation progress record](../plans/multi-storage-placement-progress.md) tracks the software corrections and validation status.

The v6 clone suite passed all four cases after starting at 19:00 EDT on 2026-09-09. The reports are `auto-linked.json`, `explicit-linked.json`, `auto-full.json`, and `direct-import.json` beneath `evidence/clone-v6-live-hhf2qmfw/`. Each report identifies the packaged v6 CPI, records a passing selected case, and verifies actual absence after cleanup. These results bring the completed individual v6 lifecycle cases to 28, in addition to the separate four-VM routing batch.

Final certification must bind every required row to the final candidate. Earlier passing runs remain historical evidence from their recorded builds. The 28 completed v6 cases provide evidence for the focused lifecycle, secondary strategies, supplemental placement, and clone checks against their recorded binaries. They do not establish that the changed v7 wrapper works in a deployed Director or replace verification of the newly compiled guest binaries. The separate four-VM routing check also passed on v6. Results from separate reports may count if they test the final candidate; separate reports alone do not require duplicate execution. Upgrade and downgrade evidence must identify the exact tested release pair.
