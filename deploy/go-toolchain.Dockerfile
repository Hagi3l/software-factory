# Sandbox profile image for the `go-toolchain` profile named by config/souls/*.yaml.
#
# Build from the repo root so the module cache matches the harness's go.mod/go.sum and
# the language-server manifest is COPYd from the tree:
#
#   docker build -f deploy/go-toolchain.Dockerfile -t go-toolchain .
#
# Why each piece exists:
#   - Go 1.26 + git + make: the toolchain the implementor agent and the gate need to
#     build and test the harness.
#   - The module cache is baked in (`go mod download`) and GOPROXY=off because the
#     sandbox runs with NO network (the zero-network invariant) — `go build`/`go test`
#     must resolve every dependency offline from this cache. A task that adds a new
#     dependency therefore needs the image rebuilt (or a vetted mirror — T5.6).
#   - safe.directory '*': the runner seeds the worktree via `docker cp`, which
#     preserves the host uid on `.git` while the container runs as root. Without this,
#     git's dubious-ownership guard fails (exit 128) and breaks `go build`'s default
#     VCS stamping — which silently fails the gate. (T5.4 replaces this crutch.)
#   - A default git identity so the agent can commit its candidate branch in-sandbox.
#
# Gate tooling (T5.3) — the `qa`/`resolve` stages (config/harness.yaml) run these
# offline in this image, so they are baked in (binaries land in /go/bin, on PATH):
#   - golangci-lint (T2.14 static lint), gosec (SAST), go-licenses (licence policy),
#     gremlins (mutation). These are pure static analysers / test drivers needing only
#     their binary, so a `go install` at build time is the whole story.
#   - govulncheck PLUS an offline copy of the Go vulnerability database under
#     /opt/harness/vulndb: govulncheck otherwise fetches the DB from vuln.go.dev, which
#     the zero-network sandbox forbids. GOVULNDB points the `make govulncheck` target
#     (which passes `-db $GOVULNDB`) at the baked file:// copy. Refreshing the DB means
#     rebuilding the image — and the image digest pinned in provenance therefore also
#     pins the exact vuln-DB snapshot a gate verdict was graded against.
#
# Language server (T5.3 → Phase 6) — the per-language server the agent's LSP-backed
# semantic tools resolve lives in the image alongside the toolchain it serves:
#   - gopls (the Go language server), on PATH.
#   - the languageId→server manifest at /etc/harness/language-servers.json (the fixed
#     launch convention, lsmanifest.ManifestPath). It is COPYd from the SAME file the
#     Go package embeds (internal/sandbox/lsmanifest/language-servers.json), so the
#     format the tools resolve and the file the image carries cannot drift.
FROM golang:1.26

RUN apt-get update \
 && apt-get install -y --no-install-recommends make git ca-certificates jq curl \
 && rm -rf /var/lib/apt/lists/*

RUN git config --global user.email "agent@harness.local" \
 && git config --global user.name "harness agent" \
 && git config --global init.defaultBranch main \
 && git config --global --add safe.directory '*'

# Gate-tool + language-server binaries (need network at build; never at run). Pinned
# to a tag so a rebuild is reproducible; bump deliberately. They install into /go/bin.
RUN go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.5.0 \
 && go install github.com/securego/gosec/v2/cmd/gosec@v2.22.9 \
 && go install golang.org/x/vuln/cmd/govulncheck@v1.1.4 \
 && go install github.com/google/go-licenses@v1.6.0 \
 && go install github.com/go-gremlins/gremlins/cmd/gremlins@v0.5.0 \
 && go install golang.org/x/tools/gopls@v0.20.0

# Offline Go vulnerability database (v1 layout: index/*.json + ID/<id>.json). Mirrored
# at build time so `govulncheck -db file:///opt/harness/vulndb` runs under zero-network.
# Parallelized; the per-ID set is the full snapshot so any module in the cache resolves.
RUN set -eux; \
    base=https://vuln.go.dev; \
    mkdir -p /opt/harness/vulndb/index /opt/harness/vulndb/ID; \
    for f in db modules vulns; do \
        curl -sSfL "$base/index/$f.json" -o "/opt/harness/vulndb/index/$f.json"; \
    done; \
    jq -r '.[].id' /opt/harness/vulndb/index/vulns.json \
      | xargs -P 16 -I{} curl -sSfL "$base/ID/{}.json" -o "/opt/harness/vulndb/ID/{}.json"
ENV GOVULNDB=file:///opt/harness/vulndb

# Language-server manifest at the fixed launch convention. Same file the lsmanifest Go
# package embeds — one source of truth, copied into the image, never duplicated.
RUN mkdir -p /etc/harness
COPY internal/sandbox/lsmanifest/language-servers.json /etc/harness/language-servers.json

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
ENV GOPROXY=off GOFLAGS=-mod=mod
