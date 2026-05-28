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

.PHONY: all build vet lint fmt tidy test test-unit check clean

all: build

## check: the full local gate — vet, lint, then unit tests. Run before committing.
check: vet lint test-unit

## build: compile the harness binary into bin/ with the version stamped in.
build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/harness

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

## clean: remove build and test output.
clean:
	rm -rf $(BIN_DIR) $(RESULTS)
