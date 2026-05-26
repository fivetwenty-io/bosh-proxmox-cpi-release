# Colors
GREEN  := \033[1;32m
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

# BOSH release packaging
RELEASE_NAME := bosh-pve-cpi
RELEASE_ARTIFACT_FIND := find . -type f \( -name 'coverage.*' -o -name '*.prof' -o -name '*.test' -o -name '*.tgz' \) -not -path './.git/*' -not -path './blobs/*' -not -path './.blobs/*' -not -path './$(SRC_ROOT)/vendor/*'

# Go sources — prerequisites for bin/cpi so the binary rebuilds whenever any
# source, go.mod, or go.sum changes (the bare target never rebuilt once built).
GO_SOURCES := $(shell find $(SRC_ROOT) -type f -name '*.go' -not -path '*/vendor/*' 2>/dev/null)

# BOSH release blob config
BLOBS_DIR    := blobs
GO_BLOB_VER  := 1.26.3
GO_BLOB_NAME := go$(GO_BLOB_VER).linux-amd64.tar.gz
GO_BLOB_KEY  := golang-1.26/$(GO_BLOB_NAME)
GO_BLOB_URL  := https://dl.google.com/go/$(GO_BLOB_NAME)
GO_BLOB_SHA  := 2b2cfc7148493da5e73981bffbf3353af381d5f93e789c82c79aff64962eb556

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

##@ Code Quality

.PHONY: fmt
fmt: ## Format Go source files with gofmt
	@echo "$(GREEN)Formatting code...$(RESET)"
	@gofmt -w $$(find $(SRC_ROOT) -name '*.go' -not -path '$(SRC_ROOT)/vendor/*')
	@echo "$(GREEN)✓ Code formatted$(RESET)"

.PHONY: vet
vet: ## Run go vet
	@echo "$(GREEN)Running go vet...$(RESET)"
	@cd $(SRC_ROOT) && go vet ./...
	@echo "$(GREEN)✓ Vet passed$(RESET)"

.PHONY: lint
lint: ## Run golangci-lint (skip with notice if not installed)
	@echo "$(GREEN)Running golangci-lint...$(RESET)"
	@if command -v golangci-lint >/dev/null 2>&1; then \
		cd $(SRC_ROOT) && golangci-lint run --timeout=5m ./...; \
		echo "$(GREEN)✓ Lint passed$(RESET)"; \
	else \
		echo "$(YELLOW)golangci-lint not installed — skipping. Install: go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest$(RESET)"; \
	fi

.PHONY: staticcheck
staticcheck: ## Run staticcheck (skip with notice if not installed)
	@echo "$(GREEN)Running staticcheck...$(RESET)"
	@if command -v staticcheck >/dev/null 2>&1; then \
		cd $(SRC_ROOT) && staticcheck ./...; \
		echo "$(GREEN)✓ Staticcheck passed$(RESET)"; \
	else \
		echo "$(YELLOW)staticcheck not installed — skipping. Install: go install honnef.co/go/tools/cmd/staticcheck@latest$(RESET)"; \
	fi

.PHONY: check
check: vet staticcheck test ## Run vet, staticcheck, and test
	@echo "$(GREEN)✓ All checks passed$(RESET)"

##@ Security

.PHONY: govulncheck
govulncheck: ## Run govulncheck for dependency vulnerabilities
	@echo "$(GREEN)Running govulncheck...$(RESET)"
	@if command -v govulncheck >/dev/null 2>&1; then \
		cd $(SRC_ROOT) && govulncheck ./...; \
		echo "$(GREEN)✓ govulncheck passed$(RESET)"; \
	else \
		echo "$(YELLOW)govulncheck not installed — skipping. Install: go install golang.org/x/vuln/cmd/govulncheck@latest$(RESET)"; \
	fi

.PHONY: gosec
gosec: ## Run gosec security scanner
	@echo "$(GREEN)Running gosec...$(RESET)"
	@if command -v gosec >/dev/null 2>&1; then \
		cd $(SRC_ROOT) && gosec -quiet -fmt text ./...; \
		echo "$(GREEN)✓ gosec passed$(RESET)"; \
	else \
		echo "$(YELLOW)gosec not installed — skipping. Install: go install github.com/securego/gosec/v2/cmd/gosec@latest$(RESET)"; \
	fi

.PHONY: security
security: govulncheck gosec ## Run all security scans
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
release-build: check security bin/cpi ## Full binary release build: check + security + bin/cpi
	@echo "$(GREEN)✓ Release build complete: $(VERSION) ($(COMMIT))$(RESET)"

.PHONY: dev-release
dev-release: ## Build a dev BOSH release tarball at ./$(RELEASE_NAME)-dev-<UTC-timestamp>.tgz
	@./scripts/create-release dev

.PHONY: release
release: ## Build a versioned BOSH release at ./$(RELEASE_NAME)-$$VERSION.tgz (requires VERSION=)
	@if [ -z "$(VERSION)" ] || [ "$(VERSION)" = "dev" ]; then \
		echo "$(YELLOW)ERROR: VERSION= required (e.g., make release VERSION=1.0.0)$(RESET)" >&2; \
		exit 1; \
	fi
	@./scripts/create-release $(VERSION)

.PHONY: release-clean
release-clean: ## Remove local artifacts that can confuse BOSH release packaging
	@rm -rf bin
	@$(RELEASE_ARTIFACT_FIND) -delete

.PHONY: release-hygiene
release-hygiene: ## Fail if local release-confusing artifacts are present
	@tmp="$$(mktemp)"; \
	trap 'rm -f "$$tmp"' EXIT; \
	$(RELEASE_ARTIFACT_FIND) | sort > "$$tmp"; \
	if [ -e bin ]; then echo bin >> "$$tmp"; fi; \
	if [ -s "$$tmp" ]; then \
		echo "$(YELLOW)ERROR: local artifacts must be cleaned before release packaging:$(RESET)" >&2; \
		cat "$$tmp" >&2; \
		echo "Run 'make release-clean' and retry." >&2; \
		exit 1; \
	fi
	@echo "$(GREEN)✓ release hygiene clean$(RESET)"

.PHONY: bosh-clean
bosh-clean: ## Remove local BOSH release artifacts (.dev_builds, .final_builds, dev_releases)
	@rm -rf .dev_builds .final_builds dev_releases
	@echo "$(YELLOW)note: blobs/ cache left in place; remove manually if desired$(RESET)"

##@ Cleanup

.PHONY: clean
clean: release-clean ## Remove coverage files, bin/, and stray release artifacts
	@echo "$(GREEN)✓ Clean complete$(RESET)"

.PHONY: help build install tidy test coverage coverage-html coverage-check fmt vet lint \
        staticcheck check govulncheck gosec security download-blobs upload-blobs sync-blobs \
        release-build dev-release release release-clean release-hygiene bosh-clean clean
