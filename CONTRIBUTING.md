# Contributing to bosh-proxmox-cpi-release

Thank you for helping improve the BOSH Proxmox CPI. We welcome bug reports, documentation fixes, and code contributions. This guide explains how to report a problem, set up a development environment, and submit a change.

## Reporting issues

Open an issue on GitHub when you find a bug or want to propose a feature. A good bug report lets us reproduce the problem without guessing. Please include:

- The Proxmox VE version and the BOSH Director version you are running.

- The CPI release version, or the commit hash if you built from source.

- The BOSH task output and the relevant CPI log lines. The CPI redacts credentials from its logs, but please double check before you paste.

- The steps that trigger the problem, as precisely as you can.

For feature requests, describe the problem you want to solve rather than only the change you have in mind. Knowing the goal helps us weigh alternatives.

## Before you start a large change

For small fixes, a pull request is enough. For anything larger, such as a new CPI method, a new configuration property, or a change in default behavior, please open an issue first and describe your plan. This avoids wasted work when a design needs discussion, and it gives us a place to record the decision.

## Development

### Prerequisites

- Go 1.27 or higher. The BOSH packaging compiles against the `golang-1.27` blob, so local builds should match.

- `golangci-lint`. When it is not installed, `make lint` falls back to `go run` at a pinned version.

- `staticcheck`, `govulncheck`, and `gosec` are optional. The corresponding make targets skip with a notice when a tool is missing.

Go sources live under `src/pve_cpi/`. Direct `go test` and `go build` invocations must run from that directory. The `make` targets re-root automatically, so you can run them from the repository root.

### Running unit tests

```bash
make test
```

This runs all Go tests with race detection and writes the coverage profile to `src/pve_cpi/coverage.out`. Every code change should come with tests that cover it.

### Running the full check suite

```bash
make check
```

This runs the quick gates first, which are `artifacts-check`, `linear-check`, `attribution-check`, `ci-image-check`, `fmt-check`, and `go-blob-check`, and it stops at the first of them that fails. It then runs the slower gates in three lanes at once. The test lane runs the suite once with race detection and collects coverage as it goes, and `coverage-check` then reads that profile instead of running the tests a second time. The analysis lane runs `vet`, `staticcheck`, and `lint`, and the scripts lane runs `erb-check` and `py-test`. Each lane stops at its own first failure, and `make check` fails if any lane fails. The test output streams as the tests run, and the other two lanes print their logs whole once every lane has finished, so a `lint` failure shows up after the test output rather than before it. When we want the old one-at-a-time order, `make check CHECK_LANES=0` runs the same gates serially. CI runs the same target on every push, so a green `make check` locally means CI should pass too. The coverage gate is 80 percent.

### Predicting CI before a push

```bash
make ci
```

A green `make check` on the Mac does not always mean a green CI run, because the workflows run in a digest-pinned `golang` container, they run the AI-attribution check over the commits being pushed, and the Security workflow runs scans that `make check` never touches. `make ci` closes that gap by running those same steps locally, inside that same image, under Docker. It runs the linear-history and attribution checks over the pushed range, then `make check REQUIRE_TOOLS=1`, then `make security REQUIRE_TOOLS=1` when the pushed range changes Go source, `go.mod`, `go.sum`, or vendored code. When the range changes none of those, it prints a one-line note and skips the scans. `CI_SECURITY=1 make ci` forces the scans, and `CI_SECURITY=0 make ci` skips them.

We read the image reference from `.github/workflows/ci.yml`, so there is one place to bump it, and `make ci-image-check` (part of `make check`) fails when another workflow pins a different `golang` digest. The first run builds a small local layer on that digest that holds `python3`, PyYAML, `ruby`, and the pinned `staticcheck`, `golangci-lint`, `govulncheck`, `gosec`, and `trivy`, which are the tools the workflow steps install before they call `make`. The Go module cache, the Go build cache, and the trivy database live in a named Docker volume called `bosh-proxmox-cpi-ci-cache`, never in a host directory. `docker volume rm bosh-proxmox-cpi-ci-cache` starts them cold. The worktree is mounted as it stands, so `make check` and the security scans test the checked-out tree, uncommitted changes included, while the attribution and linear-history checks cover the commits being pushed. That is why the pre-push hook warns when the pushed sha differs from `HEAD`. The security scans also run when the pushed range changes a `package.json`, a `package-lock.json`, a `Dockerfile`, the security workflow, or the root `Makefile`. Each new digest or tool-version set leaves an old `bosh-proxmox-cpi-ci:<hash>` image behind, and `docker image ls bosh-proxmox-cpi-ci` lists them so we can remove the old ones with `docker image rm`. A pull request's CI run tests GitHub's merge ref, while `make ci` tests the branch tip, so a branch that has fallen behind `main` can still differ. `make ci` also stops early when `go.mod` asks for a newer Go than the pinned image carries, and the fix is to bump the image digest first. The `golang` tag is a multi-architecture index, so an Apple Silicon Mac pulls the arm64 build and runs it natively, while CI runs the amd64 build of the same digest.

### Installing the git hooks

```bash
make hooks
```

This points `core.hooksPath` at the repo's `.githooks/` directory. Four hooks run from then on:

- `pre-commit`

  Refuses the commit if the index holds AI-agent working state, a path that `.gitignore` matches, or a blob over 5 MB. It then checks the staged Go files with `gofmt` and refuses the commit if any of them need formatting. It takes well under a second. Bypass one commit with `git commit --no-verify`, or set `ALLOW_LARGE_FILES=1` when a large file belongs in the commit.

- `commit-msg`

  Refuses a commit message that names an AI tool as an author.

- `pre-merge-commit`

  Refuses a merge commit, because this repository keeps a linear history. Bypass once with `git merge --no-verify`.

- `pre-push`

  Runs `make ci` with the range being pushed, so a push never lands a commit CI will reject. Docker has to be running, and the hook stops with a message when it is not. Bypass one push with `SKIP_CHECKS=1 git push` when you know CI already covered the commit.

### Keeping agent working state out of the tree

AI coding agents are welcome to work in this checkout, and they may leave scratch directories, plans, session journals, and work-preservation bundles behind. None of that is part of the product, so none of it is ever committed. The `.gitignore` block headed "AI coding-agent working state" keeps the known directory names out of `git add`, and `scripts/_tracked_artifacts_check.sh` backs that block up in three places. The pre-commit hook audits the index, `make check` audits every tracked path, and CI runs `make check` on every push. When we adopt a new tool, we add its directory to both the `.gitignore` block and the pattern list in the script.

### Running security scans

```bash
make security
```

This runs `govulncheck`, `gosec`, and `trivy`. We run these scans before every release, and CI runs them as well.

### Running lifecycle tests

This step is optional. Green unit tests are enough for a pull request. If you want to validate a change against a real cluster, a local harness exercises the canonical CPI methods end to end:

```bash
export CPI_CONFIG=~/.bosh-proxmox-cpi/cpi.json
export STEMCELL_PATH=/path/to/bosh-stemcell-*.tgz
./scripts/lifecycle
```

The harness needs a live Proxmox VE cluster and will create and destroy real VMs and disks on it. See [CPI certification](docs/certification/index.md) for the prerequisites and the config schema.

## Submitting a pull request

1. Fork the repository and create a branch for your change.

2. Make the change, with tests.

3. Run `make check` and make sure it passes.

4. If the change is operator visible, add an entry to the `Unreleased` section of [CHANGELOG.md](CHANGELOG.md). Behavior, properties, packaging, and documentation count; refactors, tests, and CI plumbing usually do not.

5. Open a pull request against `main`. Describe what the change does and why. Link the related issue if one exists.

Keep each pull request focused on one change. A small, focused pull request is easier to review and lands faster than a large one that mixes concerns.

### Keeping the history linear

The history of `main` is a straight line, and we keep it that way on purpose. Every change lands as a rebase or a squash, never as a merge commit, so `git log`, `git bisect`, and `git blame` all read cleanly. GitHub refuses a merge commit on `main` no matter who pushes it. On a pull request, the merge button offers only "Rebase and merge" and "Squash and merge".

When a branch falls behind `main`, we bring it up to date with `git rebase main` rather than merging `main` into it. A branch that carries a merge commit fails the "Linear history" CI check, and the same check runs locally as part of `make check`. The repo-managed hooks from `make hooks` also refuse a merge commit when we run `git merge`.

### Commit messages

Write commit messages that describe the code change, not the process that produced it. This repository follows the Conventional Commits style: a type prefix such as `fix:`, `feat:`, `docs:`, or `ci:`, followed by a short summary in the imperative mood. Look at `git log` for examples.

## Releasing (maintainers)

Releases are tag driven. Pushing a tag of the form `vX.Y.Z` runs the release workflow, which gates on the full CI check suite and builds the BOSH release tarball with a pinned `bosh` CLI. It then publishes a GitHub Release that carries the tarball, a sha256 checksum file, and a manifest snippet ready to paste into a deployment.

To cut a release from `main`, we first rename the `Unreleased` section of [CHANGELOG.md](CHANGELOG.md) to the new version, date it, open a fresh empty `Unreleased` section above it, update the link references at the bottom of the file, and merge that to `main`. Then we tag it:

```bash
git tag -a v1.2.3 -m "Version 1.2.3"
git push origin v1.2.3
```

A tag with a prerelease suffix, such as `v1.2.3-rc.1`, is published as a GitHub prerelease.

The build syncs blobs from the private S3 blobstore, so the repository needs two Actions secrets, `BLOBSTORE_ACCESS_KEY_ID` and `BLOBSTORE_SECRET_ACCESS_KEY`, which hold a key with read access to the bucket named in `config/final.yml`. We set them with `gh secret set`. The workflow fails with instructions when they are missing.

### Releasing a patch for an older line

Once `main` has moved on to the next minor version, we ship a fix for an older line from its own release branch, named `release/X.Y`. We cut the branch from the line's `vX.Y.0` tag, and we bring each fix over from `main` with `git cherry-pick -x`, so every commit on the branch names the `main` commit it came from. Pushes to the branch run CI, Security, and CodeQL, just as pushes to `main` do.

The branch keeps its own CHANGELOG.md. Before we tag a patch there, we add a dated section for that version to the branch's CHANGELOG, describe each fix in it, and add the version's link reference at the bottom of the file. A 0.8.1, for example, goes like this:

```bash
git switch -c release/0.8 v0.8.0
git cherry-pick -x <commit on main>
# add the 0.8.1 section to CHANGELOG.md and commit it on the branch
git push origin release/0.8
git tag -a v0.8.1 -m "Version 0.8.1"
git push origin v0.8.1
```

After the branch release ships, we record it on `main` as well. We add a commit to `main` that gives the version its own dated section in CHANGELOG.md and moves the entries for the backported fixes out of `Unreleased` into that section, so the next minor release doesn't present those fixes as new. We keep that commit separate from the `chore(release)` commit that tracks the final release metadata.

### Checking tags and marking the latest release

The release workflow checks every tag before it builds anything, and `scripts/_release_tag.py` holds the rules it applies. When `release/X.Y` exists, a patch tag `vX.Y.Z` whose Z is above zero has to be a final tag, and its commit has to be on that branch. The workflow refuses such a tag when its commit is on `main` but not on the branch, because `main` carries newer work that must not ship under the older line's version.

Every other tag has to point at a commit on `main`. That covers every tag whose Z is zero and every patch or prerelease for a line that has no release branch. We cut 0.7.1 through 0.7.3 that way, because the 0.7 line never had a release branch. When the workflow refuses a tag, its error names the rule the tag broke and says how to fix it.

The workflow also decides which release GitHub marks as Latest. A release gets the badge only when its tag is the highest final version tag in the repository, so a 0.8.2 that ships after 0.9.0 leaves 0.9.0 as the latest release. Prereleases never get the badge, and they don't count when the workflow looks for the highest tag.

### Finalizing a release for bosh.io

After the GitHub Release is published, we finalize the release so [bosh.io](https://bosh.io/releases) can index it. bosh.io ignores GitHub Releases, and it reads only the final release metadata tracked in `releases/` and `.final_builds/` on `main`. We download the published tarball and then run these commands, which need `config/private.yml` with blobstore write credentials:

```bash
bosh finalize-release --version X.Y.Z bosh-proxmox-cpi-X.Y.Z.tgz
git add releases/bosh-proxmox-cpi .final_builds
git commit -m "chore(release): track final release metadata for X.Y.Z"
git push origin main
```

Skipping this step means the new version never appears on bosh.io.

A release from a branch gets its metadata on both `main` and `release/X.Y`. We finalize on `main` exactly as above, because that's the copy bosh.io reads. Then we cherry-pick that metadata commit onto `release/X.Y` with `-x` and push the branch, so a later patch on the same line finalizes on top of a complete record. If the cherry-pick conflicts in an `index.yml`, we keep the entries from both sides.

## License

This project is licensed under the [Apache License, Version 2.0](LICENSE). By submitting a contribution, you agree that it will be licensed under the same terms.
