# Makefile for github.com/YElayyat/otel-cardinality-processor
#
# Targets
#   all          - vet + lint + test  (default)
#   build        - compile all packages (type-check without producing a binary)
#   test         - unit tests with the race detector
#   fuzz         - fuzz the core cardinality decision for FUZZ_TIME
#   stress-test  - hammer concurrency paths with STRESS_COUNT iterations
#   e2e          - black-box integration tests (downloads ocb if absent, then runs suite)
#   install-ocb  - download the OpenTelemetry Collector Builder (ocb) binary
#   lint         - run golangci-lint (install first with: make install-lint)
#   vet          - run go vet across all packages
#   install-lint - download golangci-lint at the pinned version
#   install-mdatagen - download mdatagen at the pinned version
#   generate     - generate metadata files for all packages
#   clean        - purge build/test caches, local fuzz corpus, and ocb binary
#   help         - list all targets

# ---------------------------------------------------------------------------
# Configuration - override any variable on the command line.
# Example: make test PKG=./cardinalityprocessor/... TIMEOUT=120s
# ---------------------------------------------------------------------------

MODULE      := github.com/YElayyat/otel-cardinality-processor
PKG         := ./...

# Duration passed to -fuzztime. Example: make fuzz FUZZ_TIME=5m
FUZZ_TIME   ?= 30s

# Iterations for the stress target. Example: make stress-test STRESS_COUNT=1000
STRESS_COUNT ?= 500

# Version of the OpenTelemetry Collector Builder (ocb) to download.
# Must match the otelcol_version in test/e2e/builder.yaml.
OCB_VERSION ?= 0.148.0

# Path where the ocb binary is installed. The binary installed by
# "go install ...builder" is named "builder"; we rename it to "ocb" so it
# does not shadow other tools that may already be named "builder".
OCB_BIN     := $(shell go env GOPATH)/bin/ocb

GO          := go
GOFLAGS     := -race
TIMEOUT     := 90s

# Fuzz corpus lives alongside the test file in the processor package.
FUZZ_CORPUS := cardinalityprocessor/testdata/fuzz/FuzzShouldDrop

# Prepend GOPATH/bin so that tools installed with 'go install' (including
# golangci-lint and ocb) are found by every recipe without requiring the
# user to manually update their shell PATH.
export PATH := $(shell go env GOPATH)/bin:$(PATH)

# Redirect the git index file used by golangci-lint to a temp path.
# This prevents failures caused by a stale .git/index.lock file that may be
# left behind by the host environment. In a clean CI environment this env var
# is harmless; git reads the index from the temp file on first access and
# populates it from the object store exactly as normal.
export GIT_INDEX_FILE := /tmp/golangci_git_index

# ---------------------------------------------------------------------------
# Targets
# ---------------------------------------------------------------------------

.PHONY: all build test fuzz stress-test e2e install-ocb lint vet install-lint clean help bench bench-load

## all: vet + lint + test (default target)
all: vet lint test

## build: compile every package to catch type errors (no binary produced for libraries)
build:
	$(GO) build $(PKG)

## test: run the full unit-test suite under the race detector
test:
	$(GO) test $(GOFLAGS) -timeout $(TIMEOUT) -count=1 $(PKG)

## fuzz: run FuzzShouldDrop for FUZZ_TIME (default 30s)
##       Example: make fuzz FUZZ_TIME=5m
fuzz:
	$(GO) test \
		-fuzz=FuzzShouldDrop \
		-fuzztime=$(FUZZ_TIME) \
		./cardinalityprocessor/...

## stress-test: run TestConcurrency STRESS_COUNT times under the race detector
##              to surface data races under sustained concurrent load.
##              Example: make stress-test STRESS_COUNT=1000
stress-test:
	$(GO) test $(GOFLAGS) \
		-timeout 300s \
		-run TestConcurrency \
		-count=$(STRESS_COUNT) \
		./cardinalityprocessor/...

## e2e: compile a custom collector via ocb and run the black-box E2E suite.
##      Downloads ocb automatically if it is not already installed.
##      Requires network access on the first run (module downloads + compilation).
##      Example: make e2e
e2e: $(OCB_BIN)
	$(GO) test $(GOFLAGS) \
		-tags e2e \
		-timeout 15m \
		-v \
		./test/e2e/...

## bench: run all Go micro-benchmarks with memory allocation reporting.
##        Example: make bench
bench:
	$(GO) test -bench=Benchmark -benchmem -count=3 -timeout 120s ./cardinalityprocessor/...
	$(GO) test -bench=Benchmark -benchmem -count=1 -timeout 5m ./test/benchmark/...

## bench-load: run the telemetrygen load test against a custom collector.
##             Requires ocb to be installed (run: make install-ocb).
##             Example: make bench-load
bench-load: $(OCB_BIN)
	@bash scripts/benchmark_telemetrygen.sh

## install-ocb: download the OpenTelemetry Collector Builder at OCB_VERSION.
##              Skipped automatically by 'make e2e' if ocb is already present.
##              Example: make install-ocb OCB_VERSION=0.148.0
$(OCB_BIN):
	@echo "Installing ocb (collector builder) v$(OCB_VERSION)..."
	@GOBIN=$(shell go env GOPATH)/bin $(GO) install \
		go.opentelemetry.io/collector/cmd/builder@v$(OCB_VERSION)
	@mv -f $(shell go env GOPATH)/bin/builder $(OCB_BIN)
	@echo "Installed ocb to $(OCB_BIN)"

install-ocb: $(OCB_BIN)

## lint: run golangci-lint using .golangci.yml (install first: make install-lint)
lint:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo ""; \
		echo "  golangci-lint is not installed. Run:"; \
		echo ""; \
		echo "      make install-lint"; \
		echo ""; \
		exit 1; \
	}
	golangci-lint run $(PKG)

## vet: run go vet across all packages
vet:
	$(GO) vet $(PKG)

## install-lint: download golangci-lint at the version pinned in scripts/install-lint.sh
install-lint:
	@bash scripts/install-lint.sh

## install-mdatagen: download and install mdatagen
MDATAGEN_VERSION ?= latest
install-mdatagen:
	@echo "Installing mdatagen v$(MDATAGEN_VERSION)..."
	@tmp_dir=$$(mktemp -d) && \
	cd $$tmp_dir && \
	go mod init tempmod >/dev/null 2>&1 && \
	go get go.opentelemetry.io/collector/cmd/mdatagen@$(MDATAGEN_VERSION) && \
	GOBIN=$(shell go env GOPATH)/bin $(GO) install go.opentelemetry.io/collector/cmd/mdatagen && \
	rm -rf $$tmp_dir
	
## generate: generate metadata files for all packages
generate: install-mdatagen
	cd cardinalityprocessor && go generate ./...

## clean: purge build cache, test cache, fuzz corpus, and the ocb binary
clean:
	$(GO) clean -cache -testcache
	@if [ -d "$(FUZZ_CORPUS)" ]; then \
		echo "Removing fuzz corpus at $(FUZZ_CORPUS)"; \
		rm -rf "$(FUZZ_CORPUS)"; \
	fi
	@if [ -f "$(OCB_BIN)" ]; then \
		echo "Removing ocb binary at $(OCB_BIN)"; \
		rm -f "$(OCB_BIN)"; \
	fi

## help: list all available targets with their descriptions
help:
	@echo ""
	@echo "Usage: make <target> [VARIABLE=value ...]"
	@echo ""
	@grep -E '^## ' Makefile \
		| sed 's/^## //' \
		| awk -F ':' '{ printf "  %-20s %s\n", $$1, $$2 }'
	@echo ""
	@echo "Configurable variables:"
	@printf "  %-20s %s\n" "PKG"          "Go package scope  (default: $(PKG))"
	@printf "  %-20s %s\n" "OCB_VERSION"  "Collector builder (default: $(OCB_VERSION))"
	@printf "  %-20s %s\n" "FUZZ_TIME"    "Fuzz duration     (default: $(FUZZ_TIME))"
	@printf "  %-20s %s\n" "STRESS_COUNT" "Stress iterations (default: $(STRESS_COUNT))"
	@printf "  %-20s %s\n" "TIMEOUT"      "Test timeout      (default: $(TIMEOUT))"
	@echo ""
