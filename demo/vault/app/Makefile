# Vault demo app — build/test/gate targets.
#
# The harness `qa` stage postconditions (the demo config's `checks:`) resolve to the
# targets below; the gate runs them in a clean, zero-network verification sandbox (exit 0
# = pass; non-zero = findings or tool error => fail closed). The gate tools
# (templ, tailwind, golangci-lint, gosec, govulncheck + offline vuln DB, go-licenses) are
# baked into the vault-toolchain sandbox image (see demo/vault/Dockerfile).

GO      ?= go
PKG     := ./...
RESULTS := test/results
TAILWIND ?= tailwindcss

.PHONY: generate build run test test-unit compile lint vet gosec govulncheck license-scan check tidy

## generate: regenerate templ Go + compile Tailwind CSS (committed artifacts).
generate:
	templ generate
	$(TAILWIND) -i assets/app.tw.css -o internal/web/static/app.css --minify

## build: compile the vault binary.
build:
	$(GO) build -o bin/vault ./cmd/vault

## run: build and run locally on 127.0.0.1:8000.
run: build
	./bin/vault

## test: alias for the unit suite.
test: test-unit

## test-unit: run all unit tests, emitting go test -json to test/results/.
test-unit:
	@mkdir -p $(RESULTS)
	$(GO) test -json $(PKG) >$(RESULTS)/test-unit.json 2>$(RESULTS)/test-unit.stderr \
		|| (cat $(RESULTS)/test-unit.stderr; exit 1)

## compile: build the whole tree INCLUDING the test binaries, without running a single
## test (`-run='^$$'` matches no test). This is the `compiles` companion to the `tests-red`
## proof on the author-tests stage: in a compiled, statically-typed language a test that
## references a not-yet-defined symbol fails to *compile*, which also exits nonzero — so
## `tests-red` (tests-pass must FAIL) would pass on a suite that never executed an assertion
## (a *vacuous* red). Pairing `tests-red` with `compiles` makes "red" mean compiles AND
## tests-fail: the suite built and an assertion genuinely failed. See specs/verification.md
## "Tests-red proof".
compile:
	$(GO) build $(PKG) && $(GO) test -run='^$$' $(PKG)

## vet: go vet.
vet:
	$(GO) vet $(PKG)

## lint: golangci-lint (configured by .golangci.yml).
lint:
	golangci-lint run

## gosec: SAST scan of all packages (qa gate). A finding or tool error fails closed.
gosec:
	gosec ./...

## govulncheck: known-vulnerability scan (qa gate). In-sandbox this reads the offline DB
## the image sets via GOVULNDB=file:///opt/harness/vulndb; on a host with GOVULNDB unset
## it falls back to govulncheck's online default.
govulncheck:
	govulncheck $(if $(strip $(GOVULNDB)),-db $(GOVULNDB),) ./...

## license-scan: dependency/licence policy (qa gate). --ignore the app's own module since
## go-licenses classifies the local packages too. modernc.org/mathutil (pulled in by the
## pure-Go SQLite driver) carries a plain BSD-3-Clause LICENSE that go-licenses' classifier
## fails to recognize; it is verified-permissive, so it is ignored rather than failing closed.
license-scan:
	go-licenses check \
		--ignore github.com/harness-demo/vault \
		--ignore modernc.org/mathutil \
		./...

## tidy: reconcile go.mod/go.sum.
tidy:
	$(GO) mod tidy

## check: the local fast gate — vet + lint + unit tests.
check: vet lint test-unit
