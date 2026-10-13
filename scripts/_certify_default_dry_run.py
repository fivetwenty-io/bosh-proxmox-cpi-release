"""The output of a default `certify upgrade --dry-run`, after normalising run-specific values.

certify's own tests compare a default dry run against this text line for line, so a
change that adds, drops, or reorders a step in a run that does not set
`certification.azs` shows up. Timestamps, durations, the run directory name, the
repository and temporary paths, and the random marker payload are replaced with
placeholders by `_normalise_dry_run` in `_certify_test.py`.
"""

DEFAULT_DRY_RUN = """  CPI upgrade matrix: <dry-run:N-1> -> <dry-run:N>  (N-1 -> N from published releases)

================== [T] preflight  (cumulative) ==================
    [PASS] director:slot-guard  slot <TMP>/certification (alias pve-certification)   @T
    [PASS] tool:uv  on PATH   @T
    [PASS] tool:bosh  on PATH   @T
    [PASS] tool:git  on PATH   @T
    [PASS] tool:gh  on PATH   @T
    [PASS] file:<REPO>/ci/integration.scheduled.yml  present   @T
    [PASS] file:manifests/bosh/vars.yml  present   @T
    [PASS] file:manifests/certification/cloud-config-ops.yml  present   @T
    [PASS] file:manifests/bosh/cloud-config.yml  present   @T
    [PASS] checkout:bosh-cpi-certification  <REPO>/.deps/bosh-cpi-certification (dry-run: clone/refresh skipped)   @T
    [PASS] checkout:certification-release  <REPO>/.deps/bosh-cpi-certification/shared/assets/certification-release   @T
==> [T] preflight:vars
    DRY-RUN: <REPO>/scripts/bosh check-vars -o <REPO>/manifests/certification/cloud-config-ops.yml -v certification_vm_cores=2 -v certification_vm_memory_mib=2048 -v certification_vm_disk_mib=8192
    [PASS] preflight:vars   @T
    [PASS] director:slot-free  dry-run   @T
    [PASS] lab:reachable  dry-run   @T
    <-- [T] preflight done  (cumulative)

================== [T] resolve  (cumulative) ==================
    [PASS] release:old-cpi  <dry-run:N-1> -> <dry-run:old-cpi:<dry-run:N-1>>   @T
    [PASS] release:new-cpi  <dry-run:N> -> <dry-run:new-cpi:<dry-run:N>>   @T
    [PASS] stemcell:old  <dry-run>/<dry-run> (light)   @T
    [PASS] stemcell:new  <dry-run>/<dry-run> (light)   @T
    <-- [T] resolve done  (cumulative)

================== [T] deploy-old  (cumulative) ==================
==> [T] net:up
    DRY-RUN: <REPO>/scripts/bosh net-up
    [PASS] net:up   @T
==> [T] director:create-env-old
    DRY-RUN: <REPO>/scripts/bosh create-env
    [PASS] director:create-env-old   @T
==> [T] director:alias
    DRY-RUN: <REPO>/scripts/bosh alias-env
    [PASS] director:alias   @T
    [SKIP] director:cpi-config  certification.cpi_id not set
==> [T] director:cloud-config
    DRY-RUN: <REPO>/scripts/bosh ucc -o <REPO>/manifests/certification/cloud-config-ops.yml -v certification_vm_cores=2 -v certification_vm_memory_mib=2048 -v certification_vm_disk_mib=8192
    [PASS] director:cloud-config   @T
    <-- [T] deploy-old done  (cumulative)

================== [T] deploy-cert  (cumulative) ==================
==> [T] cert:create-release
    DRY-RUN: bosh -n create-release --force --name certification
    [PASS] cert:create-release   @T
==> [T] cert:upload-release
    DRY-RUN: bosh -n upload-release
    [PASS] cert:upload-release   @T
==> [T] cert:upload-stemcell
    DRY-RUN: bosh -n upload-stemcell <dry-run-stemcell:light>
    [PASS] cert:upload-stemcell   @T
==> [T] cert:manifest
    DRY-RUN: bosh int <REPO>/.deps/bosh-cpi-certification/shared/assets/certification-release/certification.yml -o <generated ops> > <TMP>/<RUN>/certification.yml
    [PASS] cert:manifest  dry-run   @T
==> [T] cert:deploy
    DRY-RUN: bosh -n -d certification deploy <TMP>/<RUN>/certification.yml
    [PASS] cert:deploy   @T
    <-- [T] deploy-cert done  (cumulative)

================== [T] write-marker  (cumulative) ==================
==> [T] cert:write-marker
    DRY-RUN: bosh -n -d certification ssh simple/0 -c echo <B64> | base64 -d | sudo bash
    [PASS] cert:write-marker   @T
    <-- [T] write-marker done  (cumulative)

================== [T] capture-pre  (cumulative) ==================
==> [T] capture:pre
    [PASS] capture:pre  dry-run   @T
    <-- [T] capture-pre done  (cumulative)

================== [T] upgrade  (cumulative) ==================
==> [T] director:create-env-new
    DRY-RUN: <REPO>/scripts/bosh create-env
    [PASS] director:create-env-new   @T
    <-- [T] upgrade done  (cumulative)

================== [T] recreate  (cumulative) ==================
==> [T] cert:recreate
    DRY-RUN: bosh -n -d certification recreate
    [PASS] cert:recreate   @T
    <-- [T] recreate done  (cumulative)

================== [T] capture-post  (cumulative) ==================
==> [T] capture:post
    [PASS] capture:post  dry-run   @T
    <-- [T] capture-post done  (cumulative)

================== [T] read-marker  (cumulative) ==================
==> [T] cert:read-marker
    DRY-RUN: bosh -n -d certification ssh simple/0 -c echo <B64> | base64 -d | sudo bash
    [PASS] cert:read-marker   @T
    <-- [T] read-marker done  (cumulative)

================== [T] verify  (cumulative) ==================
    [PASS] verify:director-vm-replaced  dry-run   @T
    [PASS] verify:director-disk-identity  dry-run   @T
    [PASS] verify:deployment-disk-identity  dry-run   @T
    [PASS] verify:cpi-upgraded  dry-run   @T
    [PASS] verify:disk-data-survived  dry-run   @T
    [PASS] verify:disk-serial-stable  dry-run   @T
    [PASS] verify:instance-running  dry-run   @T
    [PASS] verify:no-new-orphans  dry-run   @T
    [PASS] verify:cloud-check  dry-run   @T
    <-- [T] verify done  (cumulative)

================== [T] finalize  (cumulative) ==================
    [SKIP] doc:run-report  --skip-doc
    [SKIP] doc:summary  --skip-doc
    <-- [T] finalize done  (cumulative)

================== [T] teardown  (cumulative) ==================
==> [T] cert:delete-deployment
    DRY-RUN: bosh -n -d certification delete-deployment --force
    [PASS] cert:delete-deployment   @T
==> [T] director:clean-up
    DRY-RUN: bosh -n clean-up --all
    [PASS] director:clean-up   @T
==> [T] director:delete-env
    DRY-RUN: <REPO>/scripts/bosh teardown
    [PASS] director:delete-env   @T
    [SKIP] net:down  the env network is shared with the default slot's Director
    <-- [T] teardown done  (cumulative)

================================================================
  CERTIFY SUMMARY — command: ./scripts/certify upgrade --dry-run --no-color --skip-doc --config <REPO>/ci/integration.scheduled.yml --results-dir <TMP>  (DRY-RUN)
================================================================
    [PASS] director:slot-guard  slot <TMP>/certification (alias pve-certification)
    [PASS] tool:uv  on PATH
    [PASS] tool:bosh  on PATH
    [PASS] tool:git  on PATH
    [PASS] tool:gh  on PATH
    [PASS] file:<REPO>/ci/integration.scheduled.yml  present
    [PASS] file:manifests/bosh/vars.yml  present
    [PASS] file:manifests/certification/cloud-config-ops.yml  present
    [PASS] file:manifests/bosh/cloud-config.yml  present
    [PASS] checkout:bosh-cpi-certification  <REPO>/.deps/bosh-cpi-certification (dry-run: clone/refresh skipped)
    [PASS] checkout:certification-release  <REPO>/.deps/bosh-cpi-certification/shared/assets/certification-release
    [PASS] preflight:vars
    [PASS] director:slot-free  dry-run
    [PASS] lab:reachable  dry-run
    [PASS] release:old-cpi  <dry-run:N-1> -> <dry-run:old-cpi:<dry-run:N-1>>
    [PASS] release:new-cpi  <dry-run:N> -> <dry-run:new-cpi:<dry-run:N>>
    [PASS] stemcell:old  <dry-run>/<dry-run> (light)
    [PASS] stemcell:new  <dry-run>/<dry-run> (light)
    [PASS] net:up
    [PASS] director:create-env-old
    [PASS] director:alias
    [SKIP] director:cpi-config  certification.cpi_id not set
    [PASS] director:cloud-config
    [PASS] cert:create-release
    [PASS] cert:upload-release
    [PASS] cert:upload-stemcell
    [PASS] cert:manifest  dry-run
    [PASS] cert:deploy
    [PASS] cert:write-marker
    [PASS] capture:pre  dry-run
    [PASS] director:create-env-new
    [PASS] cert:recreate
    [PASS] capture:post  dry-run
    [PASS] cert:read-marker
    [PASS] verify:director-vm-replaced  dry-run
    [PASS] verify:director-disk-identity  dry-run
    [PASS] verify:deployment-disk-identity  dry-run
    [PASS] verify:cpi-upgraded  dry-run
    [PASS] verify:disk-data-survived  dry-run
    [PASS] verify:disk-serial-stable  dry-run
    [PASS] verify:instance-running  dry-run
    [PASS] verify:no-new-orphans  dry-run
    [PASS] verify:cloud-check  dry-run
    [SKIP] doc:run-report  --skip-doc
    [SKIP] doc:summary  --skip-doc
    [PASS] cert:delete-deployment
    [PASS] director:clean-up
    [PASS] director:delete-env
    [SKIP] net:down  the env network is shared with the default slot's Director
----------------------------------------------------------------
  PHASE TIMINGS
         preflight
         resolve
         deploy-old
         deploy-cert
         write-marker
         capture-pre
         upgrade
         recreate
         capture-post
         read-marker
         verify
         finalize
         teardown
----------------------------------------------------------------
  45 pass  0 fail  4 skip   total   ->  ALL PASSED
================================================================
  wrote <TMP>/<RUN>/results.json
  wrote <TMP>/<RUN>/junit-steps.xml
"""
