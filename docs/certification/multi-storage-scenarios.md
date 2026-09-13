# Run the multi-storage certification scenarios

Run `scripts/_storage_placement_scenarios.py` against a disposable PVE lab to collect placement and lifecycle evidence from the candidate CPI. The runner checks actual volumes and journal identities after operations. It also reads guest filesystem mappings through QEMU Guest Agent to distinguish a root-derived data disk from a dedicated ephemeral disk.

Build and install `cpi` and `pve-cid` together from the same candidate. Prepare the lab topology and enroll the durable journal using the [base lifecycle guide](multi-storage-placement.md). Policy cases retain that namespace and journal throughout configuration changes. Recovery cases create separate disposable authorities, as described below.

## Declare the lab fixtures

Copy [scenario-fixtures.json](../../manifests/examples/multi-storage-placement/scenario-fixtures.json) and replace its example values. The manifest rejects unknown fields. Storage IDs must already exist in PVE unless a case explicitly requires an absent ID. The runner does not create exports or storage definitions.

The full policy suite requires three independent ephemeral NFS members, two independent persistent NFS members, a separate root member, and a fixed ISO target. At least two nodes must reach every P/E member. The split-root and fixed ISO targets must be observable on their permitted nodes. Provide an existing stemcell CID, working VM network properties, and enough capacity for the requested disks.

For a focused run, repeat `--case` for the required policy cases. Each selected plural case needs at least two members of that role; a singleton case needs one. For example, the three `policy-<strategy>-psingleton-eplural` cases accept four ephemeral members and one persistent member. Set `root_storage_ids` and `encrypted_persistent_storage_ids` to empty arrays when the selected cases do not require those capabilities. Split-root and encryption cases still require their respective fixtures. Keep the fixed ISO target and selector-tier fields in the manifest. A focused run supplies evidence only for its selected cases.

The faults and recovery suites accept one E member and one P member without separate root or encrypted fixtures. The constraints suite needs two E and two P members. Every supplied section still receives strict validation, regardless of the selected suite. Fault targets must also satisfy the effective persistent boundary; VM fault phases use the declared VM policy, and the HA phase requires an enabled HA path.

Policy progress reports retain completed rows during checkpoints in later cases, together with active resource identities. A process interruption does not erase earlier case results. The final report includes each returned row once.

Use a path-identity stemcell CID returned by the candidate CPI, such as `:light:<storage>:import/<file>.qcow2` or `:heavy:<storage>:import/<file>.qcow2`. The `template:<vmid>` form is retired. Template cloning still uses the path CID, locating cached templates through the digest in the filename and the matching template tag. Verify that the source exists and choose `expected_root_mechanism` to match the intended clone or import path.

Declare encrypted P members only when the operator can assert their encryption. The tier used to test a selector escape must match an existing backend outside P. An unrelated API failure cannot satisfy a selector rejection case. The stemcell must expose `/` and `/var/vcap/data` through QEMU Guest Agent; unsupported or missing guest evidence fails the case.

VM fixtures must not select a conflicting storage profile or carry the `bosh-retain-ephemeral` tag. The runner sets `retain_ephemeral_on_delete` to false in each VM request so successful cases can verify final volume absence. It preserves other supplied VM properties.

## Validate and execute

Validate fixture syntax without contacting PVE or creating a subprocess. This check does not validate generated configurations through the packaged CPI and supplies no live evidence.

```sh
python3 scripts/_storage_placement_scenarios.py \
  --config /absolute/path/lab-cpi.json \
  --manifest /absolute/path/scenario-fixtures.json \
  --cpi-bin /absolute/path/candidate/bin/cpi \
  --report /absolute/path/evidence/manifest-validation.json \
  --suite all --validate-only
```

Run the declared suites with the following command. Replace the report path for each candidate or lab run.

```sh
python3 scripts/_storage_placement_scenarios.py \
  --config /absolute/path/lab-cpi.json \
  --manifest /absolute/path/scenario-fixtures.json \
  --cpi-bin /absolute/path/candidate/bin/cpi \
  --report /absolute/path/evidence/storage-scenarios.json \
  --suite all
```

Use `--suite policy`, `--suite constraints`, `--suite faults`, `--suite recovery`, or `--suite director` for a focused run. Every supplied manifest section is validated even when its suite is not selected. A failed case stops dependent suites and records their cases as missing. The report retains known resource identities for audited recovery instead of attempting cleanup after an uncertain result.

## Read the policy evidence

The twelve `policy-<strategy>-p<width>-e<width>` cases combine all three strategies with singleton and plural P/E sets. Each case runs attachment, resize, snapshot creation and deletion, detachment, reattachment, and final deletion. The report checks the full allocation UUID, stable disk token, backing, and actual volume after each operation.

The remaining policy cases verify root-derived storage, split root placement, fixed and following ISO storage, encrypted subset selection, and forbidden selector escapes. `lifecycle-membership-removal` removes set definitions and role bindings before exercising the existing disk. `strategy-change-resume` changes policy before repeating the original VM request and requires the same CID and allocation UUID.

The root evidence comes from the completed allocation's journal record. The runner verifies the envelope checksum and audit fingerprint, reads the recorded source volume, and checks the root mutation's PVE task result. Template sources must still reference the recorded root and auxiliary volumes.

For a mechanism-specific fixture, set the optional policy field `expected_root_mechanism` to `import`, `full_clone`, or `linked_clone`. Select a compatible existing stemcell and clone policy, then use `--suite policy --case policy-spread-psingleton-esingleton` to exercise that layout. Repeat `--case` for additional compatible layouts. The runner rejects a different actual mechanism; a focused run supplies no evidence for omitted cases.

A passing row includes physical volume evidence and final journal disposition. Reports contain binary checksums and policy fingerprints, but omit raw credentials and arbitrary server errors. `passed`, `failed`, and `missing` are distinct outcomes. The runner always leaves `complete_release_matrix` false because these suites do not cover every deployment and upgrade requirement.

## Exercise topology and capacity constraints

The planning cases in the constraints suite invoke the candidate's production `pve-cid storage-plan` command. Positive planning cases must return a feasible observation-only plan and leave journal and volume inventories unchanged. Negative planning cases also invoke the corresponding CPI request, require the expected error class, and require unchanged journal fingerprints and actual storage volumes. A returned resource on a negative case is recorded for cleanup and makes the case fail.

Add a `constraints` object to the shared manifest.

```json
{
  "constraints": {
    "missing_storage_id": "cert-storage-deliberately-absent",
    "alias_storage_ids": ["cert-p-alias-a", "cert-p-alias-b"],
    "domain_storage_ids": ["cert-domain-export-a", "cert-domain-export-b"],
    "restricted_storage": {
      "storage_id": "cert-node-restricted",
      "allowed_node": "pve-a",
      "excluded_node": "pve-b"
    },
    "unequal_storage_ids": ["cert-small", "cert-large"],
    "stale_delay_seconds": 2
  }
}
```

Except for the deliberately missing ID, these fixtures name existing disposable storage IDs. The harness does not create them. The missing ID must be absent in the independently read storage definitions. Alias IDs must describe the same NFS server/export. Domain IDs must describe distinct exports that the operator declares share one underlying capacity budget. The node-restricted export must have an actual PVE `nodes` restriction including the allowed node and excluding the other existing node. Unequal members must report different actual total capacities. The suite inventories every declared fixture on its applicable nodes, including fixtures outside the base P/E sets.

The default scenarios change derived policy only; they do not edit PVE storage definitions. They check regex membership expansion and contraction for P and E, along with missing IDs and alias or P/E overlap rejection. Other cases check node restrictions and HA reachability across pinned AZ nodes. Capacity checks cover shared-domain minimum budgets, a utilization ceiling of one percent of total capacity, and integer overflow. They also verify GiB rounding at 1/1023/1024/1025 MiB and observe unequal capacities.

The stale-observation case delays real storage-status HTTP responses through a loopback TLS proxy. It uses a one-second maximum observation age and a declared delay of 1.5–5 seconds, verifies the delay was exercised, and rejects any attempt to cross a mutation boundary. Its evidence explicitly identifies controlled transport delay; it is not a claim of real backend exhaustion or post-root accounting coverage.

The constraint suite also contains `post_root_acquired_capacity` and `post_root_insufficient_capacity`. Both observe the real completed root allocation and its checksummed journal charges before changing subsequent capacity observations. The first permits exactly the remaining allocation bytes and requires a complete VM. The second provides one byte less, disables fallback, and requires refusal before another role allocation, followed by explicit audited cleanup. These controlled observations test accounting transitions; they do not substitute for backend quota or filesystem exhaustion cases.

A missing fixture leaves its row missing. A fixture whose observed facts disagree with its declaration fails. Runtime regression results and unexecuted lab cases remain separate evidence.

## Exercise backend faults

The optional `faults` object declares the namespace, disk size, backend fixtures, and proxy target. Its namespace must match the enrolled lab authority. Empty backend fixtures remain missing cases.

Populate `faults.backends` with the following object after replacing its values with verified lab identities. Set `faults.proxy_storage_id` to the storage that the response-loss cases may allocate on.

```json
{
  "storage_id": "nfs-persistent-01",
  "nfs_server": "nas-lab.example.com",
  "ssh_host": "nas-lab.example.com",
  "ssh_user": "root",
  "mount_path": "/srv/certification/nfs-persistent-01",
  "filesystem_uuid": "REPLACE_WITH_FILESYSTEM_UUID",
  "marker_nonce": "REPLACE_WITH_UNIQUE_FIXTURE_NONCE",
  "export_client": "192.0.2.0/24",
  "quota_uid": 65534,
  "inode_limit": 10000
}
```

To control a PVE-hosted backend through pmx, add this optional object to that backend fixture. Omitting `transport` preserves the portable SSH mode; selecting pmx never falls back to direct SSH.

```json
{
  "transport": {
    "kind": "pmx",
    "binary": "/absolute/path/pmx",
    "config": "/absolute/path/physical-pmx.yml",
    "context": "lab",
    "node": "sm-0",
    "identity_file": "/absolute/path/physical-fault-ssh",
    "known_hosts_file": "/absolute/path/physical-fault-known-hosts",
    "host_key_alias": "cpi-storage-fault-sm-0"
  }
}
```

The example binds the CPI lab's physical NFS host. Supply an existing pmx config, a dedicated private key, and a known-hosts file containing the pinned public key under `host_key_alias`; the nested CPI context is a different target. All file paths must be absolute. Context, node, and host-key alias are literal identities, not command fragments. The controller uses root SSH through pmx with the explicit identity, batch mode, strict host-key checking, the exclusive known-hosts file and alias, and a connection timeout. Ambient `PMX_*` variables are removed so they cannot redirect the binding.

The controller fingerprints the binary, config, private key, known-hosts file, context, node, and host-key alias. It verifies that binding before each invocation and retains its fingerprint in the remote fault state. Restoration must use the same files and target, including after a process restart; preserve those inputs until the fault is restored. This prevents an updated executable, changed trust material, or retargeted config from silently changing recovery authority. The remote program still independently checks its filesystem and export identities. Native pmx storage/node commands remain the interface for independent PVE inspection; the fixed remote program handles filesystem fault operations that have no native pmx equivalent.

The mount must contain a root-owned `.bosh-storage-fault-fixture.json` that other users cannot write. Its object must contain exactly `namespace`, `nonce`, and `filesystem_uuid`, matching the manifest. The export must occupy a dedicated ext4 or XFS filesystem with one mount identity and no overlapping exports or nested mounts. The NFS server name must resolve only to addresses on the controlled SSH host. Quota cases require ext4 user quotas and `all_squash`, with the export's non-root `anonuid` equal to the fixture's `quota_uid`.

The controller verifies those identities before changing the export, read-only state, quota, or inode availability. It accepts no arbitrary shell command hooks. The proxy cases exercise a lost allocation response, an ambiguous backend error, and a process interruption after submission. Set `faults.vm_phases` to a unique subset of `["root", "ephemeral", "iso", "ha"]` to exercise both response loss and process interruption at each declared VM phase. An omitted phase produces missing evidence.

VM fault reports distinguish captured submissions and direct artifact observations from established ownership. A clone can lack its ownership marker when the response is lost, so the report preserves that exact audit conflict and the incomplete audit result. Neither an observed artifact nor a completed remote task settles the unknown journal step or authorizes cleanup.

The Linux host must provide Python 3, `findmnt`, `ip`, `exportfs`, and `mount`. SSH host keys must already be trusted because the controller uses batch mode and strict host-key checking. For an inode test, the free-inode count must not exceed the declared limit; the controller caps that limit at 200000. The controller writes restoration state before applying a fault and refuses to restore a changed filesystem or another run's state.

If interruption leaves a backend fault active, use the exact storage ID and run ID retained in its evidence. This command restores only that declared backend and does not run scenarios or initialize authority.

```sh
python3 scripts/_storage_placement_scenarios.py \
  --config /absolute/path/lab-cpi.json \
  --manifest /absolute/path/scenario-fixtures.json \
  --cpi-bin /absolute/path/candidate/bin/cpi \
  --report /absolute/path/evidence/backend-restoration.json \
  --suite faults --restore-backend nfs-persistent-01 --fault-run-id REPLACE_WITH_RECORDED_HEX32
```

## Exercise recovery with disposable authorities

The recovery suite creates fresh namespaces and private journals beneath a directory you reserve for these scenarios. It never edits the journal in the source CPI configuration. Create the workspace on durable storage, give it to the user running the suite, and set its mode to `0700`. Keep it separate from every operator journal and avoid symlink paths.

Add the following object to the scenario manifest, replacing the paths and storage ID with the intended lab values.

```json
"recovery": {
  "workspace": "/absolute/durable/certification-recovery",
  "cases": [
    "journal_corrupt", "journal_missing", "journal_stale",
    "unreturned_cid", "context_isolation", "mixed_legacy_managed"
  ],
  "disk_size_mb": 16,
  "legacy_storage": "legacy-disk-store",
  "contexts": [
    "/absolute/path/cluster-a-cpi.json",
    "/absolute/path/cluster-b-cpi.json"
  ]
}
```

The context files must define persistent storage sets and reach two different PVE clusters. They must share the process-scoped trust and policy settings, including the CA bundle; only settings that request contexts can override may differ. The suite verifies their observed cluster identities, then submits concurrent requests with separate request contexts. This checks routing between subprocesses. The Go tests provide the separate evidence for cache isolation within one process.

The journal fault cases first allocate a disposable disk, then corrupt, remove, or replace only that scenario's retained authority. They require the next allocation to fail without changing the applicable volume inventory or guest configurations. The suite restores its retained authority and deletes the disk only after those checks pass. An unexpected allocation or inventory change leaves the authority copies and resources available for investigation.

The unreturned-CID case discards the CPI process's output, discovers the exact disk CID through the journal audit, and records adoption before cleanup. The mixed lifecycle case allocates a legacy disk and a managed disk, removes set bindings from a derived configuration, and checks both disks through existence, resize, and deletion operations.

Inspect every result row. A missing fixture produces `missing`, and a failed assertion produces `failed`; neither counts as a passing live scenario. Keep the private scenario directories with the report until all allocations have a verified disposition. A failed run can leave resources that require the journal cleanup procedure. The suite does not infer task settlement from absence or synthesize a settlement decision.

## Exercise Director workflows and rollout

Use `--suite director` with an existing, dedicated Director that has no deployments. The suite uploads the declared compilation release, applies the supplied cloud config, and deploys one service instance with a persistent disk and one errand instance. The three VM types must be distinct. The deployment name must begin with `storage-cert-`. Supply the deployment and cloud-config files as resolved JSON, which BOSH accepts as YAML; the suite adds unique storage-observation tags to its VM types.

Add a `director` object to the fixture manifest. All paths are absolute. The archive checksum identifies the candidate source release. `linux_cpi_sha256` identifies the actual Linux CPI binary installed on the Director; it can differ from the native binary passed to `--cpi-bin` on macOS.

```json
"director": {
  "dedicated_director": true,
  "environment": "storage-cert-director",
  "namespace": "storage-cert-director-owned",
  "ssh": {"host": "director.example.test", "user": "vcap"},
  "candidate_archive": "/absolute/artifacts/candidate.tgz",
  "candidate_sha256": "REPLACE_WITH_CANDIDATE_ARCHIVE_SHA256",
  "linux_cpi_sha256": "REPLACE_WITH_DEPLOYED_LINUX_BINARY_SHA256",
  "compilation_release": "/absolute/artifacts/storage-workload.tgz",
  "deployment_manifest": "/absolute/fixtures/workload.json",
  "cloud_config": "/absolute/fixtures/cloud.json",
  "baseline_cloud_config": "/absolute/fixtures/original-cloud.json",
  "compilation_vm_type": "storage-compilation",
  "expected_root_mechanism": "full_clone",
  "observer_policy": {
    "expected_root_mechanism": "full_clone",
    "vm_cloud_properties": {"cpu": 2, "memory": 2048, "disk": 8192},
    "ephemeral_storage_ids": ["nfs-ephemeral-1"],
    "root_storage_ids": [],
    "persistent_storage_ids": ["nfs-persistent-1"],
    "fixed_iso_storage_id": "nfs-images",
    "ephemeral_size_mib": 2048,
    "disk_size_mib": 2048,
    "encrypted_persistent_storage_ids": [],
    "escaping_tier_criteria": {}
  },
  "workload_vm_type": "storage-service",
  "errand_vm_type": "storage-errand",
  "errand_name": "storage-check",
  "resurrection_timeout_seconds": 1200
}
```

The runner host needs BOSH CLI, Ruby with its standard YAML/JSON libraries, and trusted SSH host keys. The Director must permit fixed read-only commands through `sudo -n python3`. Its journal must already be enrolled, and its namespace must differ from the local create-env namespace. The adapter checks that both authorities identify the same observed PVE cluster. It also verifies that the authenticated BOSH endpoint has the same Director UUID as the local service observed through SSH. It reads the installed CPI binary, BOSH job and package fingerprints, checksummed journal records, and the Director VM's agent settings through fixed paths. It accepts no arbitrary remote command or file path.

The workload needs guest-agent filesystem observation, a fresh compilation package, and an errand that succeeds. Compilation runs with VM reuse disabled. While deployment runs, the observer must see each compilation VM's actual root, ephemeral disk, configdrive, and guest mounts before BOSH removes it. Missing that observation fails the case. The errand runs with `--keep-alive`, allowing the same physical checks before final deployment cleanup.

Recreation and resurrection must preserve each persistent disk's CID, full allocation UUID, stable token, size, and NFS backing. Resurrection requires `hm.resurrector_enabled: true` and `director.auto_fix_stateful_nodes: true` in the Director deployment. The adapter verifies the rendered settings before deleting the service VM through BOSH. It requires a new successful Health Monitor `scan and fix` task for the exact deployment and a finished problem-resolution event naming the exact instance UUID. It records the prior global resurrection setting, enables it for the case, and restores it after successful verification. A failure retains the prior setting in the report for recovery.

The resource-policy-removal case removes explicit set selectors from cloud-config VM and disk types, recreates the service under the remaining global defaults, and verifies persistent identity. The fixture must therefore contain explicit resource selectors and global E/P bindings. Successful core workflows end with an audited deployment deletion and semantic readback of the restored baseline cloud config. A failed workflow retains resource identities and task IDs in the report.

### Supply the rollout fixtures

Add a `rollout` object inside the dedicated `director` fixture. The candidate release archive, archive checksum, and Linux binary checksum come from the surrounding Director fixture. Supply the older release archive and its required checksums; no command resolves “latest.”

```json
{
  "rollout": {
    "candidate_manifest": "/srv/storage-cert/director-candidate.json",
    "scalar_manifest": "/srv/storage-cert/director-scalar.json",
    "baseline_manifest": "/srv/storage-cert/director-baseline.json",
    "state_file": "/srv/storage-cert/bootstrap/state.json",
    "vars_store": "/srv/storage-cert/bootstrap/vars.json",
    "baseline_archive": "/srv/storage-cert/artifacts/baseline.tgz",
    "baseline_sha256": "<64 lowercase hex characters>",
    "baseline_linux_cpi_sha256": "<64 lowercase hex characters>"
  }
}
```

The three manifests must be fully interpolated JSON, which BOSH accepts as YAML input. Manifest, state, and variable-store files must already exist as private files owned by the invoking user. Use canonical absolute paths without symlinks. The state must describe the same existing Director VM that the remote observer reads from BOSH agent settings. The harness will not create a new bootstrap state to conceal a mismatch.

All three manifests must name the same Director and differ only in their CPI release reference and placement bindings. Each CPI release URL must name its exact local archive through `file://`; the embedded release name/version and archive checksum are checked again before each update. Keep other release pins, network settings, storage definitions, disk sizes, and credentials unchanged. All phases use the same explicit VM pool and fixed non-`local` ISO pool, with `iso_storage_follow_vm_storage: false`. Other ISO policies are certified by the policy suite.

The candidate manifest enables the intended global placement bindings. The scalar manifest uses the candidate release with both global and per-resource set selectors removed. The baseline manifest uses the older release with the same scalar policy. Preserve the remote journal namespace and directory in all three manifests. Their bootstrap CPI must retain the separate local namespace and durable journal path from the runner configuration.

The runner first reruns the declared disposable errand without `--keep-alive` while the candidate is installed and verifies its VM and volumes are gone with terminal journal records. It then removes resource and global bindings, recreates the Director VM with the candidate through `bosh create-env --recreate`, and recreates the workload to exercise scalar allocation while retaining its managed persistent disk. It then downgrades the CPI, recreates that workload through the older binary, and updates back to the candidate. The final recreation must return to managed allocation. Each phase checks actual persistent CIDs, full allocation UUIDs, stable tokens, backing identities, volume sizes, guest attachment, and mounts. Bootstrap persistent disks and both journal authorities must survive the updates.

The scalar transition must give every managed VM generation in both the bootstrap and Director journals a supported disposition before the old binary is invoked. The candidate performs those deletions; the old binary cannot update their journal records. VM-only recreation preserves persistent disks, and the runner verifies the bootstrap VM changed and its predecessor is absent. Ready or adopted managed VM records block downgrade, even when their physical resources appear absent. Managed persistent disks may remain returnable and continue through the independent identity checks. The same boundary is checked again immediately before installing the baseline.

Before downgrade, the runner retains a checksum-verified candidate executable and private configuration under `/var/vcap/store/pve_cpi/storage-certification/<run-id>/`. The old executable is tested through the Director; the retained candidate remains the independent audit tool for records that the older release may not understand. The report records both exact release identities and the retained audit paths. No journal is deleted, reset, or automatically enrolled. Failed or ambiguous phases retain the state and audit tooling for inspection. Successful phases restore the candidate, delete the disposable workload through BOSH, verify its terminal journal records, and restore the supplied baseline cloud config.

The rollout performs no live action during `--validate-only`. A missing rollout fixture produces missing rows, while still cleaning a successfully completed core workload. Offline tests verify the command and assertion boundaries; release certification requires a real disposable-lab run.

## Keep the remaining release evidence separate

These scenario reports supplement the [minimum release matrix](../plans/multi-storage-placement-plan.md). They do not turn an offline regression into a passing live test. Preserve the candidate checksum with every report and use the [candidate artifact workflow](candidate-artifacts.md) for BATS and upgrade certification.

| Requirement | Executable evidence | Evidence still required |
| --- | --- | --- |
| Strategy, singleton/plural, and root/ISO policy | The policy suite checks production plans, actual devices, and guest mounts. | Run every row against the candidate in the lab. |
| Removed membership and persistent identity | The policy suite checks lifecycle operations after set removal. | Exercise retained data through the Director workflows. |
| Backend and interruption behavior | The fault suite applies only its declared controls. | Supply each fixture and retain its actual fault and restoration evidence. |
| Lost authority, unreturned CID, and mixed lifecycle | The recovery suite uses isolated authorities. | Run the cases with valid fixtures; subprocess context checks do not replace the Go cache-isolation tests. |
| Regex membership, aliases, HA, and capacity boundaries | Production Go tests cover deterministic validation and planning. | Run the declared constraint fixtures, including the two post-root accounting cases. |
| Clone and import mechanisms | The runner verifies the actual recorded mechanism, source, and completed root task. | Run explicit fixtures for every supported mechanism and compatible layout. |
| Compilation, errands, recreation, and resurrection | The Director suite observes actual storage and guest mappings during each fixed workflow. | Run the dedicated Director fixture and the separate BATS lane. |
| Rollout and rollback | The Director rollout suite pins both artifacts, preserves authorities, and checks persistent identity through policy removal, downgrade, and create-env upgrade. | Run the explicit rollout fixtures and retain both deployed binary checksums. |

The offline runner tests check report handling and sequencing. The Go configuration test also constructs policies through Python and loads them with the production schema. Neither test suite contacts the lab. Missing fixtures, unexecuted workflows, and incomplete audits remain unmet release gates.

Director fixtures must declare `director.expected_root_mechanism` and use the same value in `director.observer_policy.expected_root_mechanism`. Before changing cloud config or uploading a release, the runner compares its observer policy with the dedicated Director cloud config and deployed CPI policy. The separate `director.observer_policy` preserves standalone expectations when suites run together. The comparison covers role members, split-root use, fixed ISO storage, disk sizes, and compilation VM properties. A Director-only fixture may use singleton E/P sets, empty split-root and encrypted-member lists, and an empty escaping-tier object. Do not copy these expectations from unrelated standalone scenarios. Compilation observation failures retain bounded private diagnostics; they do not count as successful mount proof.

For Director `clone_mode: auto`, declare `auto` as the Director root expectation when the workflow covers automatic mechanism selection. Each allocation must still prove its actual supported mechanism and source; the observer does not assume every selected NFS target uses the same mechanism. Explicit `full_clone` or `linked_clone` expectations remain useful for tests designed to require one mechanism.
