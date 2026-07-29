# Sandbox profile image for the `go-toolchain` profile named by config/souls/*.yaml.
#
# Build from the repo root so the module cache matches the harness's go.mod/go.sum and
# the language-server manifest is COPYd from the tree:
#
#   docker build -f deploy/go-toolchain.Dockerfile --target base -t go-toolchain-base .
#   docker build -f deploy/go-toolchain.Dockerfile -t go-toolchain .
#
# Two tags, one file — a LAYER-CACHE split (run.sh does both; the second build reuses
# every cached layer of the first). `base` holds everything that changes rarely: the
# toolchain, gate tools, the vuln DB, the module cache. The final stage adds the one
# thing that changes on every harness commit — the shim binary — as the LAST layers,
# so a source change rebuilds seconds of COPY, not the module warm. Downstream profile
# images (demo/vault/Dockerfile) build FROM go-toolchain-base and copy the binary out
# of go-toolchain as *their* last layers, so a harness commit no longer cascades into
# re-downloading their tools and module caches either (a child's layer cache is keyed
# on its parent image ID — basing on the stable `base` is what breaks the cascade).
#
# Why each piece exists:
#   - Go 1.26 + git + make: the toolchain the implementor agent and the gate need to
#     build and test the harness.
#   - The module cache is baked in (`go mod download`) so a build resolves cached
#     dependencies offline. For dependencies NOT in the cache, GOPROXY points at the
#     in-sandbox GOPROXY shim (T5.6): `harness sandbox-goproxy` is started by the
#     entrypoint and forwards `go`'s module-proxy requests over the bind-mounted broker
#     socket to the runner, which fetches them from the package proxy and logs them (the
#     one egress chokepoint). A build needing only cached modules never contacts the shim;
#     one needing a new module is mediated by the runner's allowlist (deny-by-default), so
#     the image is identical whether or not a deployment allowlists package fetch.
#   - A default git identity so the agent can commit its candidate branch in-sandbox.
#
# No `safe.directory '*'` crutch: the Docker backend now chowns the seeded worktree to
# the container's exec user after `docker cp` (which would otherwise leave it owned by
# the host uid, tripping git's dubious-ownership guard and breaking VCS stamping). The
# owner matches the process by construction, so the blanket override is gone (T5.4).
#
# Gate tooling (T5.3) — the `qa`/`resolve` stages (config/factory.yaml) run these
# offline in this image, so they are baked in (binaries land in /go/bin, on PATH):
#   - golangci-lint (T2.14 static lint), gosec (SAST), go-licenses (licence policy),
#     gremlins (mutation). These are pure static analysers / test drivers needing only
#     their binary, so a `go install` at build time is the whole story.
#   - govulncheck PLUS an offline copy of the Go vulnerability database under
#     /opt/software-factory/vulndb: govulncheck otherwise fetches the DB from vuln.go.dev, which
#     the zero-network sandbox forbids. GOVULNDB points the `make govulncheck` target
#     (which passes `-db $GOVULNDB`) at the baked file:// copy. The mirror layer is
#     keyed on VULNDB_SNAPSHOT (below), so refreshing the DB is a deliberate act —
#     and the image digest pinned in provenance therefore also pins the exact vuln-DB
#     snapshot a gate verdict was graded against.
#
# Language server (T5.3 → Phase 6) — the per-language server the agent's LSP-backed
# semantic tools resolve lives in the image alongside the toolchain it serves:
#   - gopls (the Go language server), on PATH.
#   - the languageId→server manifest at /etc/harness/language-servers.json (the fixed
#     launch convention, lsmanifest.ManifestPath). It is COPYd from the SAME file the
#     Go package embeds (internal/sandbox/lsmanifest/language-servers.json), so the
#     format the tools resolve and the file the image carries cannot drift.
# Builder stage: compile the harness binary so the in-sandbox GOPROXY shim
# (`harness sandbox-goproxy`, T5.6) is available inside the image. Only cmd + internal +
# the module files are copied (not .git/docs/specs), so the build is lean and deterministic.
# The module/build caches are BuildKit cache mounts — they persist across builds on the
# build host (an incremental recompile, no re-download) and never land in the image
# (only /out/software-factory is copied out).
FROM golang:1.26 AS harness-build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -o /out/software-factory ./cmd/software-factory

FROM golang:1.26 AS base

RUN apt-get update \
 && apt-get install -y --no-install-recommends make git ca-certificates jq curl \
 && rm -rf /var/lib/apt/lists/*

RUN git config --global user.email "agent@factory.local" \
 && git config --global user.name "harness agent" \
 && git config --global init.defaultBranch main

# Gate-tool + language-server binaries (need network at build; never at run). Pinned
# to a tag so a rebuild is reproducible; bump deliberately. They install into /go/bin.
RUN go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.5.0 \
 && go install github.com/securego/gosec/v2/cmd/gosec@v2.22.9 \
 && go install golang.org/x/vuln/cmd/govulncheck@v1.1.4 \
 && go install github.com/google/go-licenses@v1.6.0 \
 && go install github.com/go-gremlins/gremlins/cmd/gremlins@v0.5.0 \
 && go install golang.org/x/tools/gopls@v0.20.0

# Offline Go vulnerability database (v1 layout: index/*.json + ID/<id>.json). Mirrored
# at build time so `govulncheck -db file:///opt/software-factory/vulndb` runs under zero-network.
# The layer is keyed on VULNDB_SNAPSHOT: Docker's cache would otherwise never re-run it
# (an unchanged Dockerfile prefix means an eternally frozen snapshot, however often the
# image is "rebuilt"), so refreshing the DB = bumping the arg — either the default below
# or `--build-arg VULNDB_SNAPSHOT=$(date +%F)`. Parallelized, with retries so one blip
# among thousands of per-ID fetches doesn't fail a long build; the per-ID set is the
# full snapshot so any module in the cache resolves.
ARG VULNDB_SNAPSHOT=2026-07-03
RUN set -eux; \
    : "vuln DB snapshot: ${VULNDB_SNAPSHOT}"; \
    base=https://vuln.go.dev; \
    mkdir -p /opt/software-factory/vulndb/index /opt/software-factory/vulndb/ID; \
    for f in db modules vulns; do \
        curl -sSfL --retry 3 "$base/index/$f.json" -o "/opt/software-factory/vulndb/index/$f.json"; \
    done; \
    jq -r '.[].id' /opt/software-factory/vulndb/index/vulns.json \
      | xargs -P 16 -I{} curl -sSfL --retry 3 "$base/ID/{}.json" -o "/opt/software-factory/vulndb/ID/{}.json"
ENV GOVULNDB=file:///opt/software-factory/vulndb

# Language-server manifest at the fixed launch convention. Same file the lsmanifest Go
# package embeds — one source of truth, copied into the image, never duplicated.
RUN mkdir -p /etc/harness
COPY internal/sandbox/lsmanifest/language-servers.json /etc/harness/language-servers.json

# Warm the harness module cache. This lives in `base` — ABOVE the binary copy — so a
# harness source change (which changes the binary on every commit) never re-runs the
# download; only a go.mod/go.sum change does.
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

# GOPROXY points at the in-sandbox shim (started by the entrypoint), GOFLAGS keeps module
# resolution in -mod=mod. Cached deps resolve from the baked cache without contacting it; a
# new dep is fetched through the runner's broker (mediated + logged) or denied by its
# allowlist. GONOSUMCHECK is NOT set: go.sum + the checksum DB still pin every module — the
# sumdb is served through the same shim path (/sumdb/...), preserving supply-chain integrity.
# (Set AFTER the module warm above: at image-build time the shim isn't running, so the warm
# needs the default proxy. Downstream images that warm their own caches override per-RUN.)
ENV GOPROXY=http://127.0.0.1:8123 GOFLAGS=-mod=mod

# Final stage: the volatile layers, deliberately LAST (see the header). The in-sandbox
# GOPROXY shim (T5.6): the harness binary + the entrypoint that starts it.
FROM base
COPY --from=harness-build /out/software-factory /usr/local/bin/software-factory
COPY deploy/harness-sandbox-init.sh /usr/local/bin/software-factory-sandbox-init
RUN chmod +x /usr/local/bin/software-factory-sandbox-init
ENTRYPOINT ["/usr/local/bin/software-factory-sandbox-init"]
