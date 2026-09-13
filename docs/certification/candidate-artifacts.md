# Certify a candidate CPI artifact

The acceptance workflow can test a CPI release artifact before publication. Select the successful Candidate Release run, its artifact name, and the expected SHA256. The workflow downloads that artifact from this repository, checks the checksum, and reads the release name and version from `release.MF`.

The artifact must contain exactly one `.tgz` file. The archive must contain one regular `release.MF` with a supported CPI release name. A filename or GitHub artifact label does not establish the release version.

## Build the candidate

Run the [Candidate Release workflow](../../.github/workflows/candidate-release.yml) on the branch you intend to certify.

```sh
gh workflow run candidate-release.yml \
  --repo fivetwenty-io/bosh-proxmox-cpi-release \
  --ref multi-storage-placement
```

The workflow runs the CI gate, compiles both package binaries with the verified Go blob, and builds a source release archive. Its run summary supplies the artifact name, source commit, release version, and tarball checksum. It retains the artifact for 30 days through [GitHub artifact storage](https://github.com/actions/upload-artifact#retention-period), without publishing a release or contacting PVE.

## Run the acceptance workflow

Use the following command after replacing the build run ID, artifact name, and checksum with the candidate's values.

```sh
gh workflow run acceptance.yml \
  --repo fivetwenty-io/bosh-proxmox-cpi-release \
  -f candidate_run_id=BUILD_RUN_ID \
  -f candidate_artifact=ARTIFACT_NAME \
  -f candidate_sha256=SHA256
```

Supply all three candidate fields together. With no candidate fields, scheduled and manual runs continue to select published releases. Candidate runs upgrade from the latest published CPI to the selected archive, then bootstrap the BATS Director with that same archive. The workflow records the build commit alongside the archive's actual version and checksum.

The existing `skip_certify` and `skip_bats` inputs remain available for focused runs. A skipped suite supplies no evidence for that suite's release gate. All runs share the lab concurrency group described in [scheduled acceptance](scheduled.md).

## Inspect or test a local archive

Inspect the candidate before using it in a lab. This command validates the manifest and checksum without extracting the archive or contacting PVE.

```sh
python3 scripts/_release_artifact.py /absolute/path/candidate.tgz \
  --sha256 SHA256
```

The local upgrade runner accepts an archive through `--new-cpi`. Select the intended environment and the published version that should precede the upgrade.

```sh
scripts/certify --env cpitest \
  --old-cpi PUBLISHED_VERSION \
  --new-cpi /absolute/path/candidate.tgz
```

Run reports retain the versions and SHA256 values read from the resolved archives. If artifact resolution fails, the report records an unresolved identity instead of attributing the failure to an unverified version. Local runs record a source commit only when `PVE_CPI_RELEASE_COMMIT` is supplied; the acceptance workflow obtains it from the selected build run.

## Record multi-storage evidence

A candidate upgrade and BATS run do not cover the complete multi-storage matrix. The [multi-storage lifecycle checks](multi-storage-placement.md) verify the base topology, actual P/E placement, and selector failures. The [implementation plan](../plans/multi-storage-placement-plan.md) also requires the two-node topology with three independent ephemeral NFS shares, two persistent NFS shares, and shared infrastructure storage. Retain the selected artifact's checksum with the results for placement, lifecycle operations, interrupted operations, upgrades, and downgrades. Mark any unexecuted case explicitly so the release record cannot mistake missing evidence for a passing test.
