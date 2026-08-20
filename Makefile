# Colors
GREEN  := \033[1;32m
RED    := \033[1;31m
YELLOW := \033[1;33m
BLUE   := \033[1;34m
CYAN   := \033[1;36m
RESET  := \033[0m

.DEFAULT_GOAL := help

# Source root: Go sources live under src/pve_cpi (matches BOSH packaging layout)
SRC_ROOT := src/pve_cpi

# Build metadata
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

PKG     := github.com/fivetwenty-io/bosh-pve-cpi/internal/version
LDFLAGS := -X '$(PKG).Version=$(VERSION)' \
           -X '$(PKG).Commit=$(COMMIT)' \
           -X '$(PKG).BuildDate=$(DATE)'

COVERAGE_THRESHOLD := 80

# Set REQUIRE_TOOLS=1 (CI does) to make a missing staticcheck, govulncheck,
# gosec, or trivy a hard failure instead of a skip, so a broken install step
# cannot quietly turn a gate into a no-op.
REQUIRE_TOOLS ?= 0
define require_tool
	if [ "$(REQUIRE_TOOLS)" = "1" ]; then \
		echo "$(RED)✗ $(1) is required (REQUIRE_TOOLS=1) but not installed$(RESET)"; \
		exit 1; \
	fi
endef

# BOSH release packaging
RELEASE_NAME := bosh-pve-cpi
# Tarballs are written under dev_releases/ (dev) or releases/ (final), never at the repo root.
RELEASE_DEV_DIR  := dev_releases/$(RELEASE_NAME)
RELEASE_ARTIFACT_FIND := find . -type f \( -name 'coverage.*' -o -name '*.prof' -o -name '*.test' \) -not -path './.git/*' -not -path './blobs/*' -not -path './.blobs/*' -not -path './$(SRC_ROOT)/vendor/*' -not -path './dev_releases/*' -not -path './releases/*'
# Separate find for release tarballs in their canonical output dirs.
RELEASE_TGZ_FIND := find dev_releases releases -type f -name '*.tgz' 2>/dev/null || true
# Loose tarballs at the repo root are always erroneous; release-hygiene asserts none exist.
RELEASE_ROOT_TGZ_FIND := find . -maxdepth 1 -name 'bosh-pve-cpi-*.tgz'

# Go sources — prerequisites for bin/cpi so the binary rebuilds whenever any
# source, go.mod, or go.sum changes (the bare target never rebuilt once built).
GO_SOURCES := $(shell find $(SRC_ROOT) -type f -name '*.go' -not -path '*/vendor/*' 2>/dev/null)

# BOSH release blob config
BLOBS_DIR    := blobs
GO_BLOB_VER  := 1.26.6
GO_BLOB_NAME := go$(GO_BLOB_VER).linux-amd64.tar.gz
GO_BLOB_KEY  := golang-1.26/$(GO_BLOB_NAME)
GO_BLOB_URL  := https://dl.google.com/go/$(GO_BLOB_NAME)
GO_BLOB_SHA  := 708effb774be8237570d0add163225abbdfaf4fca28b2611df167beba4feef89

##@ General

.PHONY: help
help: ## Display this help message
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make $(CYAN)<target>$(RESET)\n"} \
	    /^[a-zA-Z_-]+:.*?##/ { printf "  $(CYAN)%-20s$(RESET) %s\n", $$1, $$2 } \
	    /^##@/ { printf "\n$(YELLOW)%s$(RESET)\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Build

.PHONY: build
build: bin/cpi ## Build CPI binary (alias for bin/cpi)

bin/cpi: $(GO_SOURCES) $(SRC_ROOT)/go.mod $(SRC_ROOT)/go.sum ## Build CPI binary to bin/cpi with version ldflags
	@echo "$(GREEN)Building bin/cpi $(VERSION)...$(RESET)"
	@mkdir -p bin
	@cd $(SRC_ROOT) && go build -ldflags "$(LDFLAGS)" -o ../../bin/cpi ./cmd/cpi
	@echo "$(GREEN)✓ bin/cpi built$(RESET)"

.PHONY: install
install: ## Install CPI binary via go install
	@echo "$(GREEN)Installing cpi...$(RESET)"
	@cd $(SRC_ROOT) && go install -ldflags "$(LDFLAGS)" ./cmd/cpi
	@echo "$(GREEN)✓ cpi installed$(RESET)"

.PHONY: tidy
tidy: ## Run go mod tidy
	@echo "$(GREEN)Tidying modules...$(RESET)"
	@cd $(SRC_ROOT) && go mod tidy
	@echo "$(GREEN)✓ Modules tidied$(RESET)"

##@ Testing

.PHONY: test
test: ## Run all tests with race detection
	@echo "$(GREEN)Running tests...$(RESET)"
	@cd $(SRC_ROOT) && go test -race -count=1 -timeout=120s ./...
	@echo "$(GREEN)✓ Tests passed$(RESET)"

.PHONY: coverage
coverage: ## Generate coverage profile and print summary
	@echo "$(GREEN)Generating coverage report...$(RESET)"
	@cd $(SRC_ROOT) && go test -coverprofile=coverage.out ./...
	@cd $(SRC_ROOT) && go tool cover -func=coverage.out
	@echo "$(GREEN)✓ Coverage report generated$(RESET)"

.PHONY: coverage-html
coverage-html: coverage ## Generate HTML coverage report
	@echo "$(GREEN)Generating HTML coverage report...$(RESET)"
	@cd $(SRC_ROOT) && go tool cover -html=coverage.out -o coverage.html
	@echo "$(GREEN)✓ $(SRC_ROOT)/coverage.html written$(RESET)"

.PHONY: coverage-check
coverage-check: coverage ## Fail if total line coverage < $(COVERAGE_THRESHOLD)%
	@echo "$(GREEN)Checking coverage threshold ($(COVERAGE_THRESHOLD)%)...$(RESET)"
	@total=$$(cd $(SRC_ROOT) && go tool cover -func=coverage.out | grep '^total:' | awk '{print $$3}' | tr -d '%'); \
	echo "Total coverage: $${total}%"; \
	if [ -z "$$total" ]; then \
		echo "$(YELLOW)WARNING: could not parse total coverage$(RESET)"; \
		exit 1; \
	fi; \
	ok=$$(awk "BEGIN {print ($$total >= $(COVERAGE_THRESHOLD)) ? \"yes\" : \"no\"}"); \
	if [ "$$ok" != "yes" ]; then \
		echo "$(YELLOW)FAIL: coverage $${total}% < $(COVERAGE_THRESHOLD)%$(RESET)"; \
		exit 1; \
	fi
	@echo "$(GREEN)✓ Coverage threshold met$(RESET)"

.PHONY: py-test
py-test: ## Run every scripts/*_test.py unit suite with python3 (offline; mocks all PVE traffic)
	@echo "$(GREEN)Running Python script tests...$(RESET)"
	@command -v python3 >/dev/null 2>&1 || { echo "$(RED)✗ python3 is required for py-test but not installed$(RESET)"; exit 1; }
	@set -e; for t in scripts/*_test.py; do \
		echo "$(CYAN)== $$t$(RESET)"; \
		python3 "$$t"; \
	done
	@echo "$(GREEN)✓ Python script tests passed$(RESET)"

.PHONY: bats
bats: ## Run the BOSH Acceptance Tests against the configured PVE lab (see docs/certification/bats.md)
	@if command -v ruby >/dev/null 2>&1 && command -v bundle >/dev/null 2>&1; then \
		./scripts/bats run; \
	else \
		echo "$(YELLOW)ruby/bundler not installed — skipping BATS. Install ruby 3.3+ (e.g. brew install ruby)$(RESET)"; \
	fi

.PHONY: certify-upgrade
certify-upgrade: ## Run the BOSH Director Upgrade Test against the configured PVE lab (see docs/certification/upgrade.md)
	@./scripts/certify upgrade

.PHONY: certify-upgrade-dry-run
certify-upgrade-dry-run: ## Print every command the Director Upgrade Test would run, without executing any
	@./scripts/certify upgrade --dry-run

##@ Code Quality

.PHONY: fmt
fmt: ## Format Go source files with gofmt
	@echo "$(GREEN)Formatting code...$(RESET)"
	@gofmt -w $$(find $(SRC_ROOT) -name '*.go' -not -path '$(SRC_ROOT)/vendor/*')
	@echo "$(GREEN)✓ Code formatted$(RESET)"

.PHONY: fmt-check
fmt-check: ## Fail if any Go source file is not gofmt-formatted
	@echo "$(GREEN)Checking gofmt...$(RESET)"
	@unformatted=$$(gofmt -l $$(find $(SRC_ROOT) -name '*.go' -not -path '$(SRC_ROOT)/vendor/*')); \
	if [ -n "$$unformatted" ]; then \
		echo "$(YELLOW)FAIL: files need gofmt (run 'make fmt'):$(RESET)"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	@echo "$(GREEN)✓ gofmt clean$(RESET)"

.PHONY: vet
vet: ## Run go vet
	@echo "$(GREEN)Running go vet...$(RESET)"
	@cd $(SRC_ROOT) && go vet ./...
	@echo "$(GREEN)✓ Vet passed$(RESET)"

# Pinned golangci-lint version — update here when upgrading.
# go run is used as a fallback when the binary is not present, ensuring CI
# (golang:1.26 image) runs lint without baking golangci-lint into the image.
GOLANGCI_LINT_VERSION := v2.12.2

.PHONY: lint
lint: ## Run golangci-lint (binary if installed, else go run @pinned version)
	@echo "$(GREEN)Running golangci-lint $(GOLANGCI_LINT_VERSION)...$(RESET)"
	@if command -v golangci-lint >/dev/null 2>&1; then \
		cd $(SRC_ROOT) && golangci-lint run --timeout=5m ./...; \
	else \
		cd $(SRC_ROOT) && go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run --timeout=5m ./...; \
	fi
	@echo "$(GREEN)✓ Lint passed$(RESET)"

.PHONY: staticcheck
staticcheck: ## Run staticcheck (skip with notice if not installed)
	@echo "$(GREEN)Running staticcheck...$(RESET)"
	@if command -v staticcheck >/dev/null 2>&1; then \
		(cd $(SRC_ROOT) && staticcheck ./...) || exit 1; \
		echo "$(GREEN)✓ Staticcheck passed$(RESET)"; \
	else \
		$(call require_tool,staticcheck); \
		echo "$(YELLOW)staticcheck not installed — skipping. Install: go install honnef.co/go/tools/cmd/staticcheck@latest$(RESET)"; \
	fi

.PHONY: go-blob-check
go-blob-check: ## Fail if the packaged Go blob is older than the go.mod toolchain requirement
	@echo "$(GREEN)Checking Go blob against go.mod...$(RESET)"
	@required=$$(awk '/^go [0-9]/ {print $$2; exit}' $(SRC_ROOT)/go.mod); \
	oldest=$$(printf '%s\n%s\n' "$${required}" "$(GO_BLOB_VER)" | sort -V | head -1); \
	if [ "$${required}" != "$(GO_BLOB_VER)" ] && [ "$${oldest}" = "$(GO_BLOB_VER)" ]; then \
		echo "$(RED)✗ go.mod requires go $${required} but the packaged blob is $(GO_BLOB_VER)$(RESET)"; \
		echo "$(RED)  BOSH compilation pins GOTOOLCHAIN=local, so the package build fails on a compile VM.$(RESET)"; \
		echo "$(RED)  Bump GO_BLOB_VER (and packages/golang-1.26/*) to $${required}, then run 'make download-blobs upload-blobs'.$(RESET)"; \
		exit 1; \
	fi; \
	echo "$(GREEN)✓ Go blob $(GO_BLOB_VER) satisfies go.mod ($${required})$(RESET)"

.PHONY: check
check: fmt-check vet go-blob-check py-test staticcheck lint coverage-check test ## Run fmt-check, vet, go-blob-check, py-test, staticcheck, lint, coverage-check, and test (cheap-fast checks first)
	@echo "$(GREEN)✓ All checks passed$(RESET)"

##@ Security

.PHONY: govulncheck
govulncheck: ## Run govulncheck for dependency vulnerabilities
	@echo "$(GREEN)Running govulncheck...$(RESET)"
	@if command -v govulncheck >/dev/null 2>&1; then \
		(cd $(SRC_ROOT) && govulncheck ./...) || { echo "$(RED)✗ govulncheck found vulnerabilities$(RESET)"; exit 1; }; \
		echo "$(GREEN)✓ govulncheck passed$(RESET)"; \
	else \
		$(call require_tool,govulncheck); \
		echo "$(YELLOW)govulncheck not installed — skipping. Install: go install golang.org/x/vuln/cmd/govulncheck@latest$(RESET)"; \
	fi

.PHONY: gosec
gosec: ## Run gosec security scanner
	@echo "$(GREEN)Running gosec...$(RESET)"
	@if command -v gosec >/dev/null 2>&1; then \
		(cd $(SRC_ROOT) && gosec -quiet -fmt text ./...) || { echo "$(RED)✗ gosec found issues$(RESET)"; exit 1; }; \
		echo "$(GREEN)✓ gosec passed$(RESET)"; \
	else \
		$(call require_tool,gosec); \
		echo "$(YELLOW)gosec not installed — skipping. Install: go install github.com/securego/gosec/v2/cmd/gosec@latest$(RESET)"; \
	fi

# Gitignored local lab state that intentionally holds credentials (guarded by
# .gitignore and the manifest script checks, not by rotation); the secret
# scanner must not fail the build on it.
TRIVY_SKIP := --skip-files manifests/bosh/creds.yml \
	--skip-files "manifests/bosh/creds.yml.*" \
	--skip-files manifests/envs/cpitest/artifacts_ssh \
	--skip-files config/private.yml \
	--skip-dirs .e2e-results

.PHONY: trivy
trivy: ## Run trivy filesystem scan for HIGH/CRITICAL CVEs (skips gracefully if trivy is not installed)
	@echo "$(GREEN)Running trivy fs scan...$(RESET)"
	@if command -v trivy >/dev/null 2>&1; then \
		trivy fs --severity HIGH,CRITICAL --exit-code 1 $(TRIVY_SKIP) . || { echo "$(RED)✗ trivy found HIGH/CRITICAL CVEs$(RESET)"; exit 1; }; \
		echo "$(GREEN)✓ trivy passed$(RESET)"; \
	else \
		$(call require_tool,trivy); \
		echo "$(YELLOW)trivy not installed — skipping. Install: https://trivy.dev/latest/getting-started/installation/$(RESET)"; \
	fi

.PHONY: security
security: govulncheck gosec trivy ## Run all security scans (govulncheck, gosec, trivy)
	@echo "$(GREEN)✓ Security scans complete$(RESET)"

##@ Blobs

.PHONY: download-blobs
download-blobs: ## Download upstream Go blob into blobs/ and register with BOSH
	@mkdir -p $(BLOBS_DIR)/golang-1.26
	@echo "$(GREEN)==> Go $(GO_BLOB_VER)$(RESET)"
	@if [ -f $(BLOBS_DIR)/$(GO_BLOB_KEY) ]; then \
		echo "   already present: $(BLOBS_DIR)/$(GO_BLOB_KEY)"; \
	else \
		curl -fL --progress-bar -o $(BLOBS_DIR)/$(GO_BLOB_KEY) $(GO_BLOB_URL); \
	fi
	@echo "$(GO_BLOB_SHA)  $(BLOBS_DIR)/$(GO_BLOB_KEY)" | shasum -a 256 -c
	@echo "$(GREEN)==> Registering blob with BOSH$(RESET)"
	bosh add-blob $(BLOBS_DIR)/$(GO_BLOB_KEY) $(GO_BLOB_KEY)
	@echo "$(GREEN)✓ blob downloaded + registered$(RESET)"

.PHONY: upload-blobs
upload-blobs: ## Upload locally-added blobs to the configured blobstore
	bosh upload-blobs

.PHONY: sync-blobs
sync-blobs: ## Sync blobs/ from the configured blobstore
	bosh sync-blobs

##@ Release

.PHONY: release-build
release-build: bin/cpi ## Build Go CPI binary only (no BOSH tarball). Use 'make release' to build the full BOSH tarball.
	@echo "$(GREEN)✓ CPI binary built: bin/cpi $(VERSION) ($(COMMIT))$(RESET)"

.PHONY: dev-release
dev-release: ## Build a dev BOSH release tarball under dev_releases/$(RELEASE_NAME)/ and print RELEASE_TGZ=...
	@./scripts/create-release dev

.PHONY: release
release: ## Build a versioned BOSH release under dev_releases/ or releases/ (requires VERSION=X.Y.Z). Prints RELEASE_TGZ=<path>.
	@if [ -z "$(VERSION)" ] || [ "$(VERSION)" = "dev" ]; then \
		echo "$(YELLOW)ERROR: VERSION= required for a final release (e.g., make release VERSION=1.0.0)$(RESET)" >&2; \
		echo "$(YELLOW)For a dev tarball use: make dev-release$(RESET)" >&2; \
		exit 1; \
	fi
	@./scripts/create-release $(VERSION)

.PHONY: release-clean
release-clean: ## Remove bin/, coverage artifacts, and release tarballs under dev_releases/ and releases/
	@rm -rf bin
	@$(RELEASE_ARTIFACT_FIND) -delete
	@$(RELEASE_TGZ_FIND) | xargs rm -f 2>/dev/null; true
	@echo "$(GREEN)✓ release artifacts cleaned$(RESET)"

.PHONY: release-hygiene
release-hygiene: ## Assert no bosh-pve-cpi-*.tgz exists at the repo root. Exits 1 if any are found.
	@found="$$($(RELEASE_ROOT_TGZ_FIND))"; \
	if [ -n "$$found" ]; then \
		echo "$(YELLOW)ERROR: loose release tarballs found at repo root — must not exist:$(RESET)" >&2; \
		echo "$$found" >&2; \
		echo "Run 'make release-clean' or 'rm bosh-pve-cpi-*.tgz' to remove them." >&2; \
		exit 1; \
	fi
	@echo "$(GREEN)✓ release hygiene clean — no loose tarballs at repo root$(RESET)"

.PHONY: bosh-clean
bosh-clean: ## Remove local BOSH release artifacts (.dev_builds, .final_builds, dev_releases)
	@rm -rf .dev_builds .final_builds dev_releases
	@echo "$(YELLOW)note: blobs/ cache left in place; remove manually if desired$(RESET)"

##@ Presentations

# Slidev deck source (Markdown). bunx @slidev/cli is fetched on demand — bun only, never npm/npx.
SLIDES_ARCH_DIR  := docs/presentations/architecture
SLIDES_ARCH_PDF  := architecture.pdf
SLIDES_ARCH_DIST := dist

.PHONY: slides-architecture
slides-architecture: ## Launch the architecture Slidev deck in the live presenter (bunx @slidev/cli)
	@echo "$(GREEN)Starting architecture slides presenter...$(RESET)"
	@cd $(SLIDES_ARCH_DIR) && bunx @slidev/cli slides.md

.PHONY: slides-architecture-export
slides-architecture-export: ## Export the architecture deck to a PDF (docs/presentations/architecture/architecture.pdf)
	@echo "$(GREEN)Exporting architecture deck to PDF...$(RESET)"
	@cd $(SLIDES_ARCH_DIR) && bunx @slidev/cli export slides.md --output $(SLIDES_ARCH_PDF)
	@echo "$(GREEN)✓ $(SLIDES_ARCH_DIR)/$(SLIDES_ARCH_PDF) written$(RESET)"

.PHONY: slides-architecture-build
slides-architecture-build: ## Build the architecture deck to a static SPA (docs/presentations/architecture/dist)
	@echo "$(GREEN)Building architecture deck (static SPA)...$(RESET)"
	@cd $(SLIDES_ARCH_DIR) && bunx @slidev/cli build slides.md --out $(SLIDES_ARCH_DIST)
	@echo "$(GREEN)✓ $(SLIDES_ARCH_DIR)/$(SLIDES_ARCH_DIST) built$(RESET)"

SLIDES_INTRO_DIR  := docs/presentations/intro-overview
SLIDES_INTRO_PDF  := intro-overview.pdf
SLIDES_INTRO_DIST := dist

.PHONY: slides-intro-overview
slides-intro-overview: ## Launch the intro-overview Slidev deck in the live presenter (bun + @slidev/cli)
	@echo "$(GREEN)Starting intro-overview slides presenter...$(RESET)"
	@cd $(SLIDES_INTRO_DIR) && bun install --silent && bunx @slidev/cli slides.md

.PHONY: slides-intro-overview-export
slides-intro-overview-export: ## Export the intro-overview deck to a PDF (docs/presentations/intro-overview/intro-overview.pdf)
	@echo "$(GREEN)Exporting intro-overview deck to PDF...$(RESET)"
	@cd $(SLIDES_INTRO_DIR) && bun install --silent && bunx @slidev/cli export slides.md --output $(SLIDES_INTRO_PDF)
	@echo "$(GREEN)✓ $(SLIDES_INTRO_DIR)/$(SLIDES_INTRO_PDF) written$(RESET)"

.PHONY: slides-intro-overview-build
slides-intro-overview-build: ## Build the intro-overview deck to a static SPA (docs/presentations/intro-overview/dist)
	@echo "$(GREEN)Building intro-overview deck (static SPA)...$(RESET)"
	@cd $(SLIDES_INTRO_DIR) && bun install --silent && bunx @slidev/cli build slides.md --out $(SLIDES_INTRO_DIST)
	@echo "$(GREEN)✓ $(SLIDES_INTRO_DIR)/$(SLIDES_INTRO_DIST) built$(RESET)"

##@ Documentation

# Architecture narrative (Markdown) → single readable index.html with rendered
# Mermaid diagrams. markdown-it is fetched via bun install — bun only, never npm/npx.
DOCS_ARCH_DIR := docs/architecture

.PHONY: docs-architecture-html
docs-architecture-html: ## Compile docs/architecture/*.md into a single docs/architecture/index.html
	@echo "$(GREEN)Building architecture HTML...$(RESET)"
	@cd $(DOCS_ARCH_DIR) && bun install --silent && bun build-index.mjs
	@echo "$(GREEN)✓ $(DOCS_ARCH_DIR)/index.html built$(RESET)"

DOCS_INTRO_DIR := docs/intro-overview

.PHONY: docs-intro-overview-html
docs-intro-overview-html: ## Compile docs/intro-overview/*.md into a single docs/intro-overview/index.html
	@echo "$(GREEN)Building intro-overview HTML...$(RESET)"
	@cd $(DOCS_INTRO_DIR) && bun install --silent && bun build-index.mjs
	@echo "$(GREEN)✓ $(DOCS_INTRO_DIR)/index.html built$(RESET)"

##@ Cleanup

.PHONY: clean
clean: release-clean ## Remove coverage files, bin/, and stray release artifacts
	@echo "$(GREEN)✓ Clean complete$(RESET)"

.PHONY: help build install tidy test coverage coverage-html coverage-check py-test bats fmt vet lint \
        staticcheck check govulncheck gosec trivy security download-blobs upload-blobs sync-blobs \
        release-build dev-release release release-clean release-hygiene bosh-clean \
        slides-architecture slides-architecture-export slides-architecture-build \
        slides-intro-overview slides-intro-overview-export slides-intro-overview-build \
        docs-architecture-html docs-intro-overview-html clean
