# Build and test entry points for the harness.
#
# test-* targets follow the convention documented in CLAUDE.md: they emit
# `go test -json` to test/results/ (gitignored) as <target>.json plus a
# <target>.stderr, so failures are triaged with jq rather than by scrolling raw
# output. A non-parsable .json usually means a compile error — check the .stderr.

GO      ?= go
BIN_DIR := bin
BIN     := $(BIN_DIR)/harness
PKG     := ./...
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)
RESULTS := test/results

# Control-room build tooling. templ is a `go install` dependency; the Tailwind standalone
# CLI is a static binary fetched on demand into bin/ (gitignored), keeping the deployable
# harness a single self-contained binary while the toolchain stays build-time only.
TEMPL          ?= templ
TAILWIND       := $(BIN_DIR)/tailwindcss
TAILWIND_VER   := v4.3.0
# The standalone CLI names the macOS asset "macos", but `uname -s` is "Darwin" — map it
# so the release URL resolves on a Mac (otherwise the fetch 404s to a "Not Found" file).
TAILWIND_OS    := $(shell uname -s | tr '[:upper:]' '[:lower:]' | sed 's/darwin/macos/')
TAILWIND_ARCH  := $(shell uname -m | sed 's/x86_64/x64/;s/aarch64/arm64/')
TAILWIND_URL   := https://github.com/tailwindlabs/tailwindcss/releases/download/$(TAILWIND_VER)/tailwindcss-$(TAILWIND_OS)-$(TAILWIND_ARCH)

.PHONY: all build generate tailwind vet lint fmt tidy test test-unit test-e2e-docker check clean \
	gosec govulncheck license-scan mutation

all: build

## check: the full local gate — vet, lint, then unit tests. Run before committing.
check: vet lint test-unit

## build: compile the harness binary into bin/ with the version stamped in.
build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/harness

## generate: run all code/asset generators (control-room templ + Tailwind). The
## generated *_templ.go and compiled CSS are committed, so this is only needed after
## editing *.templ or the Tailwind input — a plain `make build` does not require it.
generate: tailwind
	TAILWIND="$(abspath $(TAILWIND))" TEMPL="$(TEMPL)" $(GO) generate $(PKG)

## tailwind: fetch the pinned Tailwind standalone CLI into bin/ if absent (build tool only).
tailwind: $(TAILWIND)
$(TAILWIND):
	@mkdir -p $(BIN_DIR)
	@echo "fetching tailwindcss $(TAILWIND_VER) -> $(TAILWIND)"
	@curl -sSL -o $(TAILWIND) "$(TAILWIND_URL)"
	@chmod +x $(TAILWIND)

## vet: run go vet across all packages.
vet:
	$(GO) vet $(PKG)

## lint: run golangci-lint (configured by .golangci.yml).
lint:
	golangci-lint run

## fmt: format all packages.
fmt:
	$(GO) fmt $(PKG)

## tidy: reconcile go.mod/go.sum with imports.
tidy:
	$(GO) mod tidy

## test: alias for the unit-test suite.
test: test-unit

## test-unit: run all unit tests, emitting go test -json to test/results/.
test-unit:
	@mkdir -p $(RESULTS)
	$(GO) test -json $(PKG) >$(RESULTS)/test-unit.json 2>$(RESULTS)/test-unit.stderr \
		|| (cat $(RESULTS)/test-unit.stderr; exit 1)

## test-e2e-docker: run the Docker-backed spine e2e (build tag docker_e2e). Needs a
## running Docker daemon and the `go-toolchain` image (build it from
## deploy/go-toolchain.Dockerfile, or set HARNESS_E2E_IMAGE); the test skips if absent.
## Excluded from `check` because it is slow and infrastructure-dependent.
test-e2e-docker:
	@mkdir -p $(RESULTS)
	$(GO) test -json -tags docker_e2e -run TestSpineE2EDocker ./cmd/harness/ \
		>$(RESULTS)/test-e2e-docker.json 2>$(RESULTS)/test-e2e-docker.stderr \
		|| (cat $(RESULTS)/test-e2e-docker.stderr; exit 1)

# --- qa-gate checks ---------------------------------------------------------------
# The `qa` stage's postconditions (config/harness.yaml) resolve to these targets; the
# gate runs them in a clean verification sandbox (exit 0 = pass; non-zero = findings or
# a tool error => fail, closed). They are NOT part of `make check` — they need tools
# (golangci-lint, gosec, govulncheck, go-licenses, gremlins) and reference data (the
# offline vuln DB) that are baked into the go-toolchain sandbox image (T5.3,
# deploy/go-toolchain.Dockerfile) so they run offline under the zero-network invariant.
# Run them on a host only with those tools installed.

## gosec: SAST scan of all packages (qa gate). A finding or tool error fails closed.
gosec:
	gosec ./...

## govulncheck: known-vulnerability scan (qa gate). In-sandbox this reads an offline
## vulnerability database baked into the role image (no network): the go-toolchain
## image sets GOVULNDB=file:///opt/harness/vulndb (T5.3), which this target passes via
## `-db`. On a host with GOVULNDB unset it falls back to govulncheck's online default.
govulncheck:
	govulncheck $(if $(strip $(GOVULNDB)),-db $(GOVULNDB),) ./...

## license-scan: dependency/licence policy (qa gate). Rejects disallowed licences.
## --ignore the harness's own module: go-licenses classifies the local packages too,
## but this internal repo carries no LICENSE file, so without the ignore it fails on
## its own "Unknown license type" rather than on a dependency's licence (the policy
## the gate actually enforces). Scoping to third-party deps keeps the check meaningful.
license-scan:
	go-licenses check --ignore github.com/Loxstomper/harness ./...

## mutation: print the mutation score (0..1) the `mutation>=0.8` gate grades. The gate
## reads the trailing numeric token of stdout, so emit only the score last and fail
## closed on anything unparseable. gremlins is the kernel default tool; the exact
## invocation/field is finalized when the role image bakes it (T5.3).
mutation:
	@mkdir -p $(RESULTS)
	@gremlins unleash --output $(RESULTS)/mutation.json $(PKG) >/dev/null 2>&1 || true
	@jq -r '.test_efficacy' $(RESULTS)/mutation.json

## clean: remove build and test output.
clean:
	rm -rf $(BIN_DIR) $(RESULTS)
