# Multiple NFS shares implementation plan

This plan implements the [multiple-storage placement design](../designs/multi-storage-placement-design.md). It gives operators independent persistent and ephemeral storage sets, with a selectable, versioned strategy for each set. The ephemeral set covers root disks and dedicated ephemeral disks. Persistent sets support either one share or several shares through the same configuration and allocation path.

The implementation is complete, and the standalone lifecycle, fault, constraint, and recovery cases passed against the lab. We do not claim the Director core and placement rollout gates as passed. They run through the [weekly acceptance pipeline](../certification/scheduled.md). The design remains the source for placement semantics.

## 1. Delivery contract

The first release must deliver all three strategies, strict set validation, capacity domains, request-local placement, durable recovery, lifecycle compatibility, diagnostics, and certification. Each work package below includes its tests and documentation. A package is complete only when its acceptance criteria pass and its findings have been corrected. No release may substitute an unimplemented interface, skipped required test, or deferred recovery path for this scope.

The base configuration binds P to the persistent NFS set and E to a separate ephemeral NFS set. New persistent disks use P. A VM's root and dedicated ephemeral disk use one member of E unless an explicit, permitted selector splits them. When the guest derives ephemeral space from root, the root still uses E. Merely selecting E must not create a dedicated disk or change guest sizing.

Operators choose `spread`, `weighted_free_space`, or `least_utilized` through `storage_sets.<name>.strategy`, including an explicit version. They can change the algorithm without changing handlers or set membership. An alternate named set can use the same members and a different strategy. Selection changes affect new operations; existing volumes retain their actual identities and locations.

The following constraints govern every package:

- Global role bindings define defaults and allowed placement boundaries. Resource selectors can narrow those boundaries but cannot escape them or weaken capacity and encryption requirements.

- Storage selection happens before committing to a node. Every final plan satisfies existing compute, network, locality, AZ, and HA constraints.

- Each physical backing contributes capacity once. Configured capacity domains also limit writes across distinct exports sharing a budget.

- An unknown mutation outcome requires reconciliation. It never authorizes a second allocation on another share.

- Legacy-only requests preserve existing selector precedence, defaults, CID decoding, and failure behavior. Set-enabled requests require usable capacity and accessibility facts.

- Spreading allocates whole disks or VM bundles. It does not stripe a disk, reserve global NAS capacity, rebalance existing data, or guarantee equal IOPS or exact counts.

## 2. Code ownership and dependency order

Paths in this table identify existing integration points. New packages and files named later are deliverables to create, not files already present.

| Area | Existing integration points | Required change |
| --- | --- | --- |
| Configuration | [config.go](../../src/pve_cpi/internal/config/config.go), [context_overrides.go](../../src/pve_cpi/internal/config/context_overrides.go), [job spec](../../jobs/pve_cpi/spec), [JSON template](../../jobs/pve_cpi/templates/cpi.json.erb) | Parse, render, snapshot, and validate placement policy across job and per-request configuration. |
| Resource selectors | [cloudprops_resolver.go](../../src/pve_cpi/internal/cpi/handlers/cloudprops_resolver.go), [storage_tier.go](../../src/pve_cpi/internal/cpi/handlers/storage_tier.go) | Resolve each role atomically, then enforce its global boundary. |
| Inventory and node choice | [storage_info.go](../../src/pve_cpi/internal/pve/storage_info.go), [facts.go](../../src/pve_cpi/internal/placement/facts.go), [create_vm_placement.go](../../src/pve_cpi/internal/cpi/handlers/create_vm_placement.go) | Gather per-node storage facts and select feasible storage/node plans. |
| VM creation | [create_vm.go](../../src/pve_cpi/internal/cpi/handlers/create_vm.go), [create_vm_shape.go](../../src/pve_cpi/internal/cpi/handlers/create_vm_shape.go), [create_vm_disk.go](../../src/pve_cpi/internal/cpi/handlers/create_vm_disk.go) | Thread a concrete plan through every clone, import, root, and ephemeral allocation path. |
| Persistent disks | [create_disk.go](../../src/pve_cpi/internal/cpi/handlers/create_disk.go), [disk_identity.go](../../src/pve_cpi/internal/cpi/handlers/disk_identity.go), [parker.go](../../src/pve_cpi/internal/pve/parker.go) | Select during creation and preserve actual identity through the entire lifecycle. |
| Bootstrap and dependencies | [configdrive.go](../../src/pve_cpi/internal/agent/configdrive.go), [factory.go](../../src/pve_cpi/internal/agent/factory.go), [CPI main](../../src/pve_cpi/cmd/cpi/main.go), [wrapper](../../jobs/pve_cpi/templates/cpi.erb) | Resolve ISO per request and provide a durable journal outside invocation cleanup. |
| Diagnostics and certification | [pve-cid main](../../src/pve_cpi/cmd/pve-cid/main.go), [lifecycle harness](../../scripts/lifecycle), [ERB checks](../../scripts/_test_erb_render.sh), [acceptance workflow](../../.github/workflows/acceptance.yml) | Explain decisions, audit recovery, test rendering, and certify the candidate release. |

Implement the work in the following order. Configuration can merge before runtime integration, but the release must not advertise or accept partially implemented placement. Until the full path is wired, recognized set configuration must fail explicitly before mutation.

| Package | Depends on | Design milestone | Completion evidence |
| --- | --- | --- | --- |
| P1. Schema and selectors | None | 1 | Rendering, validation, override, and compatibility tests pass. |
| P2. Strategy registry | P1 policy types | 1 | All three algorithms pass fixed-vector and contract tests. |
| P3. Inventory and accounting | P1 | 2 | Node, backing, domain, and arithmetic tests pass. |
| P4. Feasible plans | P2, P3 | 2 | Joint storage/node decisions satisfy adversarial fixtures. |
| P5. Durable journal | P1 identity contract | 3 prerequisite | Separate-process crash, locking, and recovery tests pass. |
| P6. VM allocation | P4, P5 | 3 | Root, ephemeral, clone, ISO, and HA integration tests pass. |
| P7. Persistent allocation | P4, P5 | 3 | Creation, hint, stable-ID, and retry tests pass. |
| P8. Lifecycle and recovery tools | P6, P7 | 4 | Old and new volumes remain manageable after policy changes. |
| P9. Diagnostics and operator assets | P4, P5, P8 | 4 | Dry runs agree with allocation decisions and perform no mutations. |
| P10. Certification and release | P1–P9 | 5 | Offline, live, upgrade, rollback, and documentation gates pass. |

## 3. P1: Implement configuration and atomic role selection

Add typed storage-placement configuration beside the existing configuration types. Keep `vm_storage`, `disk_storage`, `stemcell_storage`, and `iso_storage` available and retain the design's required legacy scalars. Add the following fields through both the BOSH property schema and rendered JSON:

| Field | Validation and default |
| --- | --- |
| `storage_sets` | Each named set requires exactly one membership selector and an explicit strategy name/version. |
| `names` / `name_pattern` | Require a nonempty literal list or a valid case-sensitive Go RE2 pattern. Reject duplicate literal names. |
| `types`, `shared` | Omission means shared NFS. Reject contradictory predicates in this release. |
| `encrypted` | Preserve the operator-assertion meaning used by tiers; never infer it from NFS. |
| `strategy` | Accept only installed names, versions, and their defined fields. |
| `min_free_mb` | Accept nonnegative integers in MiB with checked byte conversion; default to zero. |
| `max_utilization_pct` | Accept integers from 1 through 100. Omission adds no set-specific ceiling. |
| `ephemeral_storage_set`, `persistent_storage_set`, `root_storage_set` | Require nonblank references to defined sets when present. Root is optional. |
| `require_disjoint_storage_sets` | Preserve explicit false. Default to true when both global P and E exist. |
| `storage_capacity_domains` | Require nonempty literal member lists. Validate existence and backing uniqueness after inventory resolution. |
| `storage_status_max_age_seconds` | Default to 5; accept integers from 1 through 60. |
| `storage_placement_namespace` | Require a nonblank stable namespace for set-enabled allocation. |
| `storage_allocation_journal_dir` | Require an absolute durable directory for set-enabled allocation. Validate its runtime access separately. |

Use presence-aware decoding for new fields. Reject nulls, blank selectors, fractional integers, wrong types, and unknown keys inside the new configuration structures. Preserve explicit false and zero values in ERB. Do not use permissive string lookup or numeric coercion that would reinterpret malformed input as omission. Recognized new cloud properties must receive the same validation before legacy fallback can run.

Add an explicit activation classifier before dependency construction. An unused set is inert. Any created disk selected through a set or constrained by a global role boundary activates set mode for the whole creation operation. Include legacy-selected companion resources in its plan and journal while preserving their supported backend types. A persistent-only binding adds future-access validation to an otherwise legacy VM request without activating set-managed VM allocation. Keep live discovery, disjointness, and journal-readiness checks out of unrelated existing-CID lifecycle operations. Static malformed configuration still fails validation. Test E-only, P-only, root-only, per-resource-only, unused-set, and mixed-backend configurations, including lifecycle calls while an unrelated set is unhealthy.

Implement an atomic selector resolver for each role. At each existing layer, inspect the competing set, scalar, and tier selectors together. The highest layer containing a valid explicit choice wins. Reject competing selectors at that layer when a new set selector participates. Preserve legacy-only precedence, including the dedicated ephemeral tier-before-pool rule. Keep BOSH's already-merged request separate from the CPI's local profile layers.

After precedence resolution, validate the selected membership against the global role boundary. A root boundary comes from global `root_storage_set`, or from E when no separate global root set exists. Apply the maximum reserve and strictest ceiling from both the selected policy and its boundary. Effective persistent or ephemeral encryption requirements must pass both sets' assertions and existing scalar/tier restrictions. Reject contradictory assertions for aliases of the same storage ID.

Extend `context_overrides.go` and request dependency construction so a cpi-config entry can supply the storage sets, role bindings, capacity domains, namespace, and status-age policy for its cluster. Replace set/domain maps as whole values and deep-copy mutable data. Validate the complete effective configuration after overrides. Keep the journal directory as process-level policy, with namespace-scoped records, so request properties cannot redirect filesystem writes. Reject attempts to override this protected directory rather than silently ignoring them.

When a request switches clusters while inheriting active storage bindings, require an explicit placement namespace and storage-set definition for that entry. This prevents identically named storage IDs in another cluster from silently inheriting the wrong policy. The operator's cpi-config entry establishes its global boundaries; VM and disk cloud properties remain constrained within them. Fingerprint the effective policy and cluster identity without credentials. Configuration maps must never change during a request.

Use the stable namespace as the journal authority key and store verified cluster enrollment separately. API endpoint aliases must resolve to the same authority. Reject rebinding a namespace with live or unresolved allocations to a different cluster, and require audit when cluster continuity cannot be established. Keep separate fingerprints for normalized caller intent and effective placement policy. Include explicit request selectors in intent, but exclude mutable global strategy definitions and secrets. Add fixtures for API aliases, cluster switching, policy edits between process invocations, and caller-intent changes under the same agent ID.

Acceptance requires round-trip ERB-to-Go tests for the design's examples, a pinned persistent subset, each algorithm, regex membership, explicit false, and omitted defaults. Add negative cases for every validation rule and cross-layer selector combination. Run legacy fixtures unchanged. Test two concurrent requests using different clusters and verify that neither policy, agent dependency, nor journal namespace leaks into the other.

## 4. P2: Implement all strategy algorithms and freeze version 1

Create `internal/storageplacement` with immutable request, candidate, ranking, and policy types. Implement a registry keyed by strategy name/version. Its `Rank(RequestSnapshot, []EligibleCandidate) ([]RankedCandidate, error)` contract orders every eligible input exactly once. A central validator rejects missing, duplicated, foreign, or mutated candidates. The package must not depend on PVE clients, handler dependencies, filesystem journals, or allocation APIs.

Use length-prefixed UTF-8 fields with unsigned 32-bit big-endian byte lengths for canonical tuples. Reject fields too large to encode. Freeze field order, algorithm/version representation, backing canonicalization, comparison direction, and tie behavior in versioned golden fixtures. Use explicit domain/member tags for hierarchical ranking so identical text in different identity classes cannot share a draw accidentally.

Implement `spread` v1 with descending SHA-256 rendezvous scores over the design's strategy/version, namespace, allocation key, allocation group, and backing identity tuple. Canonical backing identity breaks exact ties. One backing receives one rank regardless of how many nodes report it.

Implement `weighted_free_space` v1 with residual usable bytes after the planned allocation, floored at one byte for an exact fit. Generate a 256-bit `crypto/rand` seed once per planning attempt. Derive each draw through HMAC-SHA-256, convert its leading 52 bits using `u = (x + 1) / (2^52 + 1)`, and sort ascending by `-ln(u)/weight`. Reject invalid or nonfinite results. Freeze the seed, weights, and ordering for that attempt. Rank domains by residual domain budget, then members within each domain by their residual member budgets.

Include allocation group in the weighted tuple. When clone mechanisms require different bytes, compute candidate-specific domain residuals and use their minimum as the domain's single weight. Preserve each member's own residual for its within-domain ranking. Apply the exact-fit floor and freeze these inputs for the attempt or split-role continuation. Add golden fixtures with linked and full-clone alternatives inside one domain so neither map order nor an arbitrary representative controls its weight.

Implement `least_utilized` v1 by projected domain utilization, then projected member utilization. Use rendezvous order for ties. Compare ratios with overflow-safe arithmetic before using diagnostic floating-point values. Undeclared backings form separate domains. `spread` keeps equal backing weights but must still pass domain admission checks.

Use namespace plus BOSH `agent_id` for VM ranking identity, with `vm_bundle` for bundled placement or `root` and `ephemeral` for split groups. Keep this ranking key separate from the journal's allocation-generation UUID. For persistent creation, generate the allocation UUID before ranking and derive the correlation token from that UUID. Retain both across internal retries. Use the token as a stable disk ID only for parked disks; free-floating disks retain volume-based CIDs. An independent `create_disk` invocation always gets a new identity because the API has no general idempotency token.

Acceptance requires fixed vectors for encoding, ordering, ties, version rejection, domain hierarchy, exact-fit weights, and injected seeds. Property tests must cover input permutation, one-to-one output, repeatability, and membership changes. Deterministic distribution tests must distinguish approximate count spreading from capacity weighting without flaky runtime randomness. Add a test registry implementation to prove handlers need no strategy-name switch; unsupported algorithms must fail rather than fall back.

## 5. P3: Gather inventory and enforce real capacity limits

Extend the PVE adapter's canonical storage parsing with disabled state and reliable content/access fields. For initial discovery, fetch cluster definitions once and all relevant storage statuses once per candidate node with bounded concurrency and the existing request deadline. P4 rereads selected definitions before mutation. Include capacity-domain members even when they are outside the immediately selected role set. Inject time and API seams for deterministic tests.

Represent definition membership separately from runtime eligibility. Missing literal members are configuration errors that list every missing ID. A present but inactive member can be excluded while healthy members remain. Regex membership freezes for the operation. Failed authorization or inventory retrieval must produce an observation error, never a successful empty list.

For each set-constrained node/backing pair, require enabled, active, shared NFS with `images`, usable total/free bytes, and node access. Preserve supported backend types for unconstrained companion resources while requiring usable facts for their joint capacity charges. Reject impossible statistics, including negative values, zero totals, or available bytes above total. Consolidate healthy fresh reports of one backing with minimum available space. Refresh inconsistent totals and report an error if the contradiction remains. A failed node observation excludes that pair without discarding healthy pairs elsewhere.

Reject exact backing aliases within a set. Permit policy aliases across sets, but compare global P against E and explicit root membership by both ID and backing. Enforce disjointness before allocation. Do not attempt DNS or NAS topology inference to manufacture backing independence.

Implement capacity-domain validation and accounting in this package. Each backing belongs to at most one declared domain. Reject missing members, duplicate memberships, and contradictory aliases. Require usable observations for every member contributing to a domain envelope. Its available and total bytes are the respective minima after backing consolidation. Preserve individual member quota checks and aggregate planned writes across the domain.

Build a checked-byte ledger with separate planned, acquired, and outstanding charges. Include actual root base virtual size, requested expansion, rounded ephemeral and persistent sizes, and peak scratch or auxiliary writes required by the chosen mechanism. Infrastructure stores receive their own charges when involved. Existing persistent disks and retained volumes already reflected in free-space statistics must not be charged again as new allocations.

Attach observation-generation metadata to each ledger refresh. Retire an immediate allocation charge only after task completion, volume readback, and a newly fetched status response. Never reuse a pre-completion response with the reduced ledger. Keep the charge conservatively or fail if that observation is unavailable. Test a still-fresh cached response from before root allocation, a delayed status response, and sparse-volume growth separately. Do not claim that observed sparse usage reserves the remaining virtual size.

For each backing and domain, require enough available space after outstanding writes to retain the largest applicable reserve. Enforce the strictest utilization ceiling, including global boundary limits and existing global gates. Use overflow-safe integer comparisons. Set mode treats these constraints as hard even when a legacy gate is configured to warn. Do not sum free bytes across different members to fit one bundled allocation.

Acceptance requires unequal quotas, shared domains spanning P/E, overlapping roles with explicit permission, aliases, invalid statistics, stale observations, partial node failures, overflow, rounding, and exact boundaries. Include a test where both disks fit individually but their bundle does not, and another where root is already acquired before ephemeral revalidation. Verify one shared backing is neither multiplied by node count nor charged twice after allocation.

## 6. P4: Build and revalidate feasible allocation plans

Add a handler-facing planner adapter that combines storage snapshots with existing placement facts. Parse sizes and selectors before node selection, and resolve existing persistent CIDs to actual locations. Build the feasible intersection of explicit nodes, AZ order, memory, network, PCI, disk locality, and permitted HA/DLB nodes.

For bundled placement, rank eligible E backings and then choose the best feasible node with existing compute scoring inside the current AZ. Reuse the same backing rank across nodes. For split placement, rank root first, debit its backing/domain ledger, and then rank feasible ephemeral targets. Skip a root choice with no complete continuation. Resolve the compatible clone mechanism before calculating its final capacity charges, but let the requested storage rank determine the selected target.

Implement a lazy iterator over feasible plans. Bound it by the finite candidate inventory, context deadline, and existing placement attempt budget. Do not materialize a node-by-root-by-ephemeral Cartesian product. Report search-budget exhaustion separately from proven lack of capacity, since a bounded search may stop before examining every possibility.

Preflight infrastructure targets and future persistent accessibility. With global P, a VM candidate must reach at least one eligible P member. The base deployment preflight must additionally verify access from every configured deployment and permitted HA node to all P/E members and infrastructure targets. A future disk request can narrow P or require more space, so its own `create_disk` check remains mandatory.

Return a concrete request-local plan with node, role targets, backing/domain IDs, clone mechanism, rounded sizes, charge ledger, ISO target, policy fingerprint, strategy version/seed, observation times, and rejection reasons. No consumer may re-resolve its target from mutable global defaults after accepting the plan.

Immediately before each mutation, refresh expired facts and revalidate outstanding charges. Freeze discovery and policy for the current operation; refreshed status can exclude a member but cannot admit a newly matched export. A safe new planning attempt can refresh rankings and entropy within that frozen policy/membership, while preserving allocation identity. A submitted mutation holds its plan until its outcome is known.

Reread selected storage definitions before each mutation and compare their backing identity and safety fields with the plan. Reject a changed server/export, disabled state, content capability, or node restriction before issuing the next write. Preserve the old concrete identity for reconciliation; never reinterpret it against a repointed storage ID. Test definition changes between root and ephemeral creation and during recovery. Document the remaining race with external operator edits instead of presenting this comparison as a transactional storage lock.

For a resumed operation, look up the existing generation before planning from current defaults. Reconcile or return its existing resources using recorded intent and policy. Before any remaining mutation, check the recorded target against current global boundaries and infrastructure constraints. An excluded target blocks for audit; an allowed target retains its recorded ranking and seed. Test permissive and restrictive policy edits separately from explicit caller-intent changes.

Acceptance requires fixtures where the best compute node cannot access the selected storage, one backing appears on more nodes than another, an explicit split shares a capacity domain, and a future persistent subset is unreachable. Test cancellation, stale facts during a long clone, finite search exhaustion, and changing configuration during a submitted task. No infeasible case may allocate a VMID or volume as a side effect of planning.

Define typed planner and executor errors, then map them to the existing CPI error contract at the handler boundary. Configuration and unsupported-policy errors are nonretryable. Observation failures and capacity exhaustion can use bounded internal retries only before mutation or after verified cleanup. A fresh retry of `create_disk` can create another identity, so never mark an ambiguous submitted outcome as safe for automatic caller retry. Return a reconciliation error with correlation details and preserve its record. Cancellation after submission follows that same rule. Test the emitted `ok_to_retry` value as well as the error text.

## 7. P5: Implement durable allocation records before wiring mutations

Create `internal/allocationjournal` and inject it through CPI dependencies. Store versioned records beneath the configured durable directory, keyed by validated namespace, with verified cluster enrollment inside the namespace record. Endpoint aliases must not create separate journal trees. Encode or hash namespace components for paths; never accept path traversal. Use private directories and files, reject unsafe ownership or symlink targets, and validate file creation, atomic replacement, file fsync, directory fsync, and cross-process locking before an allocation can begin.

The job must create the Director's allocation journal directory on persistent storage and make it writable by the actual CPI execution user. Extend the wrapper and deployment wiring as needed; do not assume the job's working directory survives replacement. For `create-env`, require and document a host path outside the temporary installation tree. Preserve it through bootstrap, update, teardown tooling, and backups unless an explicit audit authorizes removal.

Use a short directory/index lock to create or find records and an exclusive lock per allocation while reconciling or mutating it. Always acquire the index lock before a record lock when both are needed, and release the index lock before API calls. Never hold a global journal lock through a clone. Do not nest journal locks inside PVE locks. Process exit must release operating-system locks; a stale marker file alone must not establish ownership.

Index the active VM generation by namespace and agent ID, not by its fingerprint. Under the index lock, atomically find or create that generation before taking its record lock. Identical concurrent calls must converge on one record; a different intent conflicts with the same active generation. A later deletion must update the generation without acquiring locks in reverse order. Add separate-process create/create, create/delete, and cleanup/recreate tests to prove this behavior.

Provide an explicit initialization and recovery procedure that permits only one active journal authority per namespace. Test the chosen filesystem's locking and durability behavior. Before failover or relocation, fence the previous writer and restore a consistent backup, then audit against PVE. Two independent directories with the same namespace must not be treated as synchronized merely because each has a valid local lock. Journal initialization and failover are operator procedures, not side effects of the read-only planner.

Persist the allocation UUID, namespace, cluster identity, and VM key or persistent correlation token so recovery can identify the operation. Include the creation-intent and policy fingerprints, frozen targets, and ranking inputs to preserve its placement decision. Record intended VMID and volume names, UPIDs, discovered volids, acquired and outstanding charges, outcome, and exact return CID as execution proceeds. Store no API credentials, agent settings, mbus credentials, or secret-bearing request payloads. Store hashes of canonical nonsecret request fields when needed for matching.

| State | Required durable evidence | Permitted next action |
| --- | --- | --- |
| `planned` | Identity, selected plan, intended resources, and ownership correlation exist before mutation. | Reconcile first if recovering; submit only when evidence establishes that no prior mutation occurred. |
| `submitted` | The task ID and target are recorded after API submission. | Poll or reconcile that task on that target. |
| `observed` | Readback identifies actual owned resources and updates charges. | Continue the same allocation or clean up verified owned resources. |
| `ready_to_return` | Exact CID and completed resource evidence are durable. | Return/resume the matching VM result or expose the disk CID for adoption audit. |
| `reconciliation_required` | The uncertain outcome and available evidence are preserved. | Audit and reconcile; do not allocate an alternative for this record. |
| `adopted` | An explicit audit records adoption and verifies the resource's actual identity. | Continue its lifecycle while retaining creation and recovery evidence. |
| `deleted` | Deletion verifies absence of the owned resource and records the disposition of dependent or retained artifacts. | Keep a tombstone and permit a later VM generation with the same agent ID. |
| `cleaned` | Failed-attempt cleanup verifies removal of all resources that would prevent safe retry. | Keep the failed generation's evidence and permit a new safe allocation generation. |

Represent multiple mutation steps within one record, including clone, resize, ephemeral allocation, and ISO creation. A single top-level state must not erase an earlier acquired resource when a later step is submitted. Record the next step's intent before issuing its API request. Cross-check anticipated names, reserved VMID, allocation UUID provenance, and PVE ownership even when the process crashed before persisting a returned UPID.

Permit a matching VM request to resume only when namespace, cluster continuity, agent ID, and creation-intent fingerprint match. A changed caller intent with a live record must fail with reconciliation guidance. A changed global strategy definition alone must not create a duplicate generation. Before returning a recorded VM CID, verify that the VM still exists and belongs to the allocation. An external disappearance requires reconciliation before recreation. Never deduplicate a fresh persistent request by VM hint, size, or cloud properties. `ready_to_return` is not proof that BOSH received the response.

Unknown record versions, corrupt records, failed durability writes, or uncertain ownership must block implicated operations without deleting evidence. After journal loss or stale restoration, require a provenance audit before admitting new set allocations for the affected namespace. Persist an initialization/recovery marker and cross-check PVE allocation provenance so an empty directory cannot silently masquerade as an unused installation. Do not make ordinary lookup or deletion of an existing CID depend on current set membership.

Acceptance requires separate-process tests for lock contention and process death, plus crash injection before and after each persistence/API boundary. Test filesystem-full and fsync errors, short writes, malformed records, changed requests, stale backups, missing journals with existing provenance, and crashes before UPID persistence or CID delivery. Each test must assert the exact remaining resources and whether another allocation is permitted.

Add verified deletion followed by recreation with the same agent ID, stale returnable CIDs, duplicate journal authorities, endpoint aliases, and a policy edit before recovery. Audit fixtures must contain historical targets excluded by current regex membership and partial scan failures. A partial scan must leave the relevant state unresolved rather than certify absence. Terminal records remain available for audit without blocking a valid later generation. Normal VM deletion that retains ephemeral storage uses `vm_deleted_retained` after verifying that the VM is absent and that the recorded holding VM owns the retained volume. Close the generation while retaining resource authority, and require complete artifact absence before terminal cleanup. Failed creation with retained artifacts remains unresolved and cannot silently admit another attempt.

## 8. P6: Integrate VM, root, ephemeral, ISO, and HA paths

Thread the plan through every `create_vm` execution path, including template clone/import alternatives and rollback. Replace set-mode reads of global `VMStorage` with the planned role target. Keep the legacy branch behavior covered by its existing tests.

Rank the target before choosing linked versus full clone. In automatic mode, use a compatible linked clone on the selected template backing or a supported full clone/import to the other selected backing. Reject unknown template backing in set mode. Reject linked-only mode for multi-member root or ephemeral sets; allow a singleton only when the actual base supports it. Use the existing shared base mechanisms without introducing a replica-cache prerequisite.

Read the actual base virtual size and validate requested root expansion and image format. Preserve guest disk bus and agent device mapping. Bundle root and dedicated ephemeral capacity on E by default, and support the explicit root split defined by the design. Track each acquired volume in the journal before proceeding to another mutation.

Change agent dependency creation to accept the request's resolved ISO target. Preserve explicit ISO overrides. When following is enabled, retain the existing meaning of the `local` sentinel and use selected root storage wherever that resolution previously used global VM storage. Follow root only when it supports ISO and is shared and reachable; otherwise validate the configured fallback. Fail before VM allocation when neither works. Upload, attach, rollback, and delete must use actual ISO provenance without mutating shared configuration.

Verify every disk and ISO against all already permitted HA/DLB nodes. A storage strategy may reject a target but cannot widen or narrow operator HA policy to make the target work. Recheck storage and task state during migration races, and preserve existing ownership of HA and resurrection behavior.

Acceptance requires actual selected-target assertions for root-only guest layouts, dedicated ephemeral disks, explicit splits, every strategy, singleton/multiple E, alternate clone paths, and both root buses already supported by the CPI. Test post-root ephemeral failure, ISO failure, cleanup failure, and uncertain task completion. A fallback succeeds only after the previous allocation is proven absent or its owned resources are verified removed.

If retention keeps a failed attempt's resource, record its actual identity and stop automatic fallback. Expose it through audit without adopting it into a new attempt. Add this case to rollback and VM-deletion tests, and verify that the ledger never releases retained physical usage to justify a replacement allocation.

## 9. P7: Integrate persistent creation and stable identity

Generate the allocation UUID and disk correlation token before placement in `create_disk.go`. Resolve the effective persistent selector and boundary, then plan against the VM's actual current node when a hint exists. Without a hint, select a reachable configured node and eligible shared target. Validate the actual requested size and image format before reserving an allocation VMID.

Recheck a hinted VM's location immediately before attaching or allocating through it. If migration invalidates access, refresh and reconcile the current state within the existing retry budget. Do not silently allocate on an unreachable share or move the VM to satisfy a disk policy.

Pass the selected target through the existing backend allocation and the configured CID path. Preserve volume-based free-floating CIDs and stable-ID parked CIDs. For parked IDs, retain `bpd-` plus 16 lowercase hexadecimal digits, deriving the 8 bytes from SHA-256 over a versioned domain separator and the allocation UUID. Keep the full UUID in journal and ownership provenance. Reject a detected shortened-token collision without adopting the other volume; regenerate only before mutation. Freeze the derivation in golden tests.

Record actual volume identity and ownership before the exact CID becomes returnable. Keep diagnostic set/strategy provenance outside the CID and assert the existing 255-character limit with long set, storage, and node names. Test both CID modes through creation, crash recovery, and lifecycle operations. Preserve legacy decoding and cross-cluster ownership safeguards.

Acceptance requires singleton and plural P, each strategy, explicit subsets, alternate policy aliases, no-hint requests, VM migration races, insufficient capacity, quota/inode failures, and read-only exports. Repeated independent same-size calls must create distinct disks. Internal retries must retain identity and never duplicate an uncertain allocation. Assert that a P selection cannot escape to the legacy `disk_storage` on failure.

## 10. P8: Complete lifecycle, parker, and recovery integration

Audit attach, detach, snapshot, resize, delete, VM deletion, disk transfer, stemcell cleanup, and retained ephemeral paths. Resolve current volids through existing CID/stable-ID mechanisms. Set labels and regexes must never become lookup or deletion authorities. Removing a member from policy must leave its existing reachable disks manageable.

Thread actual storage identities into parker creation, reuse, transfer, and VMID collision scanning. A parker holding multiple stores must reach every relevant backing. Scan the applicable actual backing set rather than only global `DiskStorage`, and retain ownership and VMID-band protections. Do not broaden cleanup into a storage-wide deletion sweep.

Resize persistent disks on their current backing and validate only incremental growth with applicable backend capabilities. Preserve the existing unsupported result for placement changes through `update_disk`. A removed policy membership must not force migration or make current identity undecodable. Protect linked-clone dependencies during stemcell deletion and include templates, imports, ISOs, snapshots, and retained volumes in drain inspection.

Extend disk audit tooling with journal/provenance correlation. Report unreturned exact CIDs, ambiguous ownership, missing resources, and stale records. Adoption and cleanup require explicit audit decisions; neither age nor a journal label authorizes deletion. Keep `ready_to_return` and unresolved live records until that decision is recorded durably. Read-only diagnostics must not perform the reconciliation writes or cleanup they describe.

Integrate journal transitions with successful VM/disk deletion, failed-attempt cleanup, and audit-confirmed adoption. Record tombstones only after verifying actual resource disposition. Complete resource deletion can succeed even if an unrelated current set is unhealthy; a failure to persist its tombstone must remain visible for reconciliation before identity reuse. Audit from recorded targets and historical provenance, not just the current storage set, and report incomplete historical access explicitly.

Acceptance requires full disk lifecycles after set removal, regex changes, algorithm changes, recreation, and restart. Include free-floating and parked disks, parkers with multiple stores, retained ephemeral disks, foreign volumes with colliding names, old CIDs, and cross-cluster VMID collisions. Verify journal recovery cannot delete an unrelated volume even when its record names that volume.

## 11. P9: Deliver diagnostics and operator configuration

Implement `pve-cid storage-plan` using the production resolver and planner. Support a configuration file, a sanitized JSON request containing a `create_vm` or `create_disk` method and its arguments, and machine-readable JSON output. Human-readable output must show how the selector source and frozen membership produced the ranked candidates. Include backing and domain identities, strategy and version, seed, capacity ages, and constraints. Explain the selected node, clone mechanism, and ISO target, along with the reasons other candidates were rejected.

Include journal findings and exact recoverable CIDs when the caller can read the private journal. Mark the output as an observation rather than a reservation. The command must not allocate VMIDs, submit tasks, mount NFS, rewrite journals, or acquire mutation ownership. Redact credentials and agent secrets from errors as well as normal output.

Add metrics for allocations, rejected candidates, fallback attempts, and reconciliation outcomes. Use bounded configured set/role/strategy dimensions; keep allocation UUIDs, arbitrary regex text, raw errors, and disk IDs out of labels. Put detailed correlation fields in structured logs with the existing redaction rules. Test diagnostics against the same fixtures used for real planning so explanations cannot drift from behavior.

Provide validated operator examples for the two-set base case, singleton persistent pinning, alternate strategies on the same members, regex membership, explicit root split, capacity domains, and separate cpi-config entries. Explain that a root override also moves guest ephemeral space when no dedicated disk exists. Include the design's shared infrastructure targets and keep the base template target inside E.

Update configuration, persistent-disk, ConfigDrive, create-env, multi-cluster, operations, troubleshooting, and permissions documentation. Document journal initialization, backup, restoration, adoption, and cleanup using the completed tooling. Explain the required PVE inventory and storage permissions through the existing permissions inventory, without claiming a new NAS-level encryption or availability guarantee.

Acceptance requires executable configuration fixtures, CLI parsing and no-mutation tests, redaction assertions, bounded metric labels, and documented error remedies. Review the completed prose, then validate local links and command examples.

## 12. P10: Certify the complete feature

Add focused tests alongside each package and integration tests through handler/PVE seams. Use real temporary files and separate processes for durability tests, deterministic seeds and clocks for planner tests, and PVE fakes that expose tasks and volume readback for fault injection. Tests must assert outcomes and ownership, not merely repeat helper calculations.

The following matrix defines the minimum release evidence. Every row must pass; a missing lab capability is an unmet release gate.

| Scenario | Required evidence |
| --- | --- |
| Global P/E defaults | Root and all dedicated ephemeral volumes are on E; persistent volumes are on P, including calls without explicit resource selectors. |
| Root-derived guest ephemeral | Agent mount mapping remains correct and root uses E without adding a disk. |
| Policy choices | Singleton/plural P and E, all three algorithms on both roles, and algorithm changes preserve the documented placement semantics. |
| Overrides and encryption | Subsets succeed; escaping scalars/tiers, conflicting selectors, and weakened assertions fail before mutation. |
| Membership and topology | Regex additions/removals, missing IDs, aliases, disjointness, node restrictions, and HA reachability produce the intended decisions. |
| Capacity | Unequal members, shared domains, quota differences, overflow, rounding, stale observations, and post-root charges cannot manufacture capacity. |
| Backend failures | Outages, inode/quota exhaustion, read-only exports, API errors, and unknown task outcomes produce safe failure or verified fallback. |
| VM operations | Compilation VMs, errands, recreation, resurrection, root splits, clone modes, ISO following, and fixed ISO targets use the effective policy. |
| Persistent lifecycle | Create, attach, detach, snapshot, resize, delete, and parker transfers work after removing policy membership. |
| Recovery | Every crash window, unreturned CID, corrupt/lost/stale journal, and ambiguous ownership case follows the documented state transition. |
| Compatibility | Legacy-only configuration and CIDs work unchanged; no shared configuration mutates across requests. |
| Rollout and rollback | A candidate release survives Director upgrade, global/per-resource policy removal, create-env updates, and the certified binary downgrade path. |

Run these offline gates from the repository root after integration. The existing `make check` includes formatting, vet, lint, static analysis, Python tests, coverage, and race tests. Extend its coverage of new shell/render checks where needed instead of relying on a developer to remember them.

```sh
rtk proxy make check REQUIRE_TOOLS=1
rtk proxy bash scripts/_test_erb_render.sh
rtk proxy make security REQUIRE_TOOLS=1
rtk proxy make build
```

Build and test the packaged release as well as the standalone binary. Verify that the CPI and diagnostic binary packaging, rendered configuration, journal directory setup, and supported execution platforms work together. A package lacking its required recovery/diagnostic tooling cannot pass merely because unit tests succeed.

Use a lab with at least three independent NFS members in E and two in P, plus the declared infrastructure targets. Include at least two PVE nodes and a separate fixture with exports sharing a capacity domain. Restrict destructive fault tests to disposable lab resources. Record actual volids, backend identities, node visibility, agent disk mapping, journal transitions, and before/after owned-resource inventories.

Extend the existing lifecycle harness with the matrix's placement assertions and fault scenarios. Run BATS and upgrade certification against the candidate artifact, not only the latest published release. The current acceptance workflow resolves the latest release, so add an explicit candidate-artifact input and record its checksum/version in reports. Preserve the existing published-release certification mode.

```sh
rtk proxy ./scripts/bats run --env cpitest --headline-pass-fail-only --no-color
rtk proxy ./scripts/certify upgrade --env cpitest --headline-pass-fail-only --no-color
```

These commands require the prepared lab and candidate artifact configuration. They are release execution steps, not checks performed while writing this plan. A BATS dependency skip is a failure for this gate. Extend the harness for policy-specific assertions that generic BATS does not cover, and archive reports under the existing certification structure with commit, artifact checksum, PVE versions, sanitized topology, and tested policy fingerprints.

## 13. Rollout, rollback, and completion criteria

Before rollout, register and validate the PVE NFS definitions, declare shared capacity domains, provision the durable journal, and audit existing provenance. Preflight all VM types, disk types, extensions, compilation settings, errands, resurrection settings, and create-env configuration for conflicting selectors. Verify every configured deployment/HA node can access the base topology's P/E and infrastructure members.

Canary explicit resource selectors first, with the complete feature installed and recovery tooling available. Verify actual placement, guest mounts, task recovery, and capacity reporting. Then enable global P/E bindings to cover requests that omit selectors. Exercise at least one pinned persistent subset and one multi-member persistent deployment. Change the selected strategy and confirm that only subsequent allocations use the new fingerprint.

Rollback placement policy by restoring the previous configuration for future allocations. To leave set mode entirely, remove both global bindings and per-resource set selectors; removing only resource selectors continues to use global defaults. Keep the storage definitions, existing volumes, stable identities, and journal records intact. Continue lifecycle operations by actual CID location.

Certify a binary downgrade with existing allocated disks and retained journal evidence. If the old binary cannot interpret new records, reconcile all in-flight work before the downgrade and retain the journal for the newer audit tooling. Do not delete recovery evidence to make downgrade validation pass. Document the exact tested release pair and any precondition enforced by tooling.

The feature is complete when P1–P11 pass, every release-matrix row has evidence, all review findings are remediated, and operator documents match the shipped schema and tools. Trace design sections 4–7 to the configuration, strategy, planner, allocation, lifecycle, and diagnostic tests. Link design section 8 to certification reports and every adversarial disposition in section 10 to a passing regression case. Publish only after that trace contains no uncovered requirement.

| Design review finding | Owning packages | Required regression |
| --- | --- | --- |
| Root-derived ephemeral escaped E | P4, P6 | Root-only guest layout uses E and preserves its mount mapping. |
| Algorithms were not replaceable | P1, P2, P9 | Changing configured name/version changes subsequent ranking without a handler change. |
| Scalars could bypass global sets | P1 | A higher-layer scalar outside the global boundary fails before mutation. |
| Linked-only mode defeated spreading | P4, P6 | Multi-member linked-only fails; automatic cloning honors the selected target. |
| Future persistent feasibility was overstated | P4, P7 | A later inaccessible subset fails on the VM's current node without migration. |
| Retry behavior implied exactly-once allocation | P5, P7, P8 | Crashes before UPID persistence and CID delivery preserve one uncertain allocation and expose its evidence. |
| Split-role strategy composition was undefined | P2, P4 | Root-first ranking charges shared capacity before selecting ephemeral storage. |
| Capacity could be counted incorrectly | P3, P6 | Duplicate node reports and post-root rechecks do not inflate capacity or double-charge root. |
| Regex and hidden callers could break separation | P1, P6, P10 | Overlapping regexes fail when disjointness is enforced; compilation and resurrection calls inherit global P/E. |
| Infrastructure and retained data were omitted | P6, P8 | Drain inspection includes base dependencies, ISOs, snapshots, and retained volumes. |
| HA and rollback rules were ambiguous | P4, P10 | HA membership remains unchanged; removing only resource selectors retains global set placement. |
| Completed VM records blocked recreation or returned stale CIDs | P5, P8 | Concurrent callers share an active generation; verified deletion permits a new generation with the same agent ID. |
| Caller intent and global policy shared one fingerprint | P1, P4, P5 | A strategy edit resumes existing resources while a restrictive boundary blocks remaining mutations. |
| Free-floating disks were assigned parked-ID semantics | P2, P7, P8 | Both CID modes and the length limit survive placement and recovery; collisions never authorize adoption. |
| Charge retirement used old observations or IDs were repointed | P3, P4 | A pre-completion status cannot release root's charge; a changed backing stops the next mutation. |
| Journal authorities and historical scans were incomplete | P5, P8, P10 | Fenced failover and historical-target audits preserve uncertainty on partial scans. |
| Partial opt-in affected unrelated legacy operations | P1, P3, P6 | Mixed-role/backend fixtures preserve unconstrained behavior, and unrelated unhealthy sets do not block lifecycle calls. |
| Cluster overrides and aliases changed allocation authority | P1, P5 | Endpoint aliases share one authority; namespace rebinding with live records fails. |
| Retention contradicted cleanup-before-fallback | P6, P8 | A retained failed-attempt volume stops fallback and remains visible to audit. |
| The lab minimum differed from the design | P10 | Certification records at least three E exports, two P exports, and two PVE nodes. |
| Candidate-dependent clone sizes left domain weights undefined | P2, P3, P4 | Fixed vectors use the minimum feasible domain residual and preserve member-specific weights. |
| Clone sizing could occur after ranking | P3, P4, P6 | Candidate mechanisms supply charges before ranking, and execution uses the selected candidate's validated mechanism. |

## 14. P11: Publish the implementation and validation report

After every implementation, lab, lifecycle, recovery, rollout, and rollback gate has reached a terminal result, publish a detailed prose report under `docs/certification/`. Write it for operators, maintainers, and release reviewers who did not participate in the implementation. The report must explain what shipped, how placement behaves, what was tested, what evidence supports each conclusion, and any limits demonstrated by the tests. Do not replace explanations with raw command output or a list of artifact paths.

Include the final configuration schema and effective precedence rules; persistent, root, and dedicated ephemeral placement behavior; strategy selection and versioning; capacity-domain accounting; retry and fallback behavior; durable journal and recovery semantics; lifecycle behavior after policy or membership changes; compatibility; rollout; and rollback. Describe the certified lab topology, including the two PVE nodes, NFS exports, roles, capacity domains, quotas, and relevant failure injection. Record the exact source commit, candidate release version and checksum, packaged binary checksums, PVE versions, policy fingerprints, and sanitized infrastructure identifiers needed to reproduce or audit the result.

Support the prose with tables that map requirements to implementation locations and tests, summarize the complete certification matrix and its outcomes, inventory test artifacts and checksums, compare strategy behavior, and record observed placement by storage role. Include Mermaid graphs for the placement decision flow, storage-role topology, and crash-recovery state transitions. Generate graphs from the final behavior and evidence; keep their labels readable in rendered Markdown and provide adjacent prose that explains the conclusions without requiring the diagrams.

Report every failed attempt that changed the implementation or test procedure, the diagnosed cause, the remediation, and the successful rerun evidence. Distinguish product defects from harness or environment defects. Preserve failed-run artifacts and link them from the report; never rewrite a failed result as a pass. Redact credentials, tokens, private keys, and secrets while retaining stable non-secret correlation identifiers.

Before completion, verify every table row and graph against the retained receipts, logs, inventories, and source. Check local links and Mermaid syntax, review the complete report's prose, and confirm that the report contains no placeholders, TODOs, deferred sections, unsupported claims, or unresolved findings. The feature is not complete until we commit a certification run report in the established format with the implementation. All cited evidence must remain at the documented locations.
