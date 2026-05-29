# Sandbox profile image for the `go-toolchain` profile named by config/souls/*.yaml.
#
# Build from the repo root so the module cache matches the harness's go.mod/go.sum:
#
#   docker build -f deploy/go-toolchain.Dockerfile -t go-toolchain .
#
# Why each piece exists:
#   - Go 1.26 + git + make: the toolchain the implementor agent and the gate need to
#     build and test the harness.
#   - The module cache is baked in (`go mod download`) and GOPROXY=off because the
#     sandbox runs with NO network (the zero-network invariant) — `go build`/`go test`
#     must resolve every dependency offline from this cache. A task that adds a new
#     dependency therefore needs the image rebuilt (or a vetted mirror — Phase 5).
#   - `safe.directory '*'`: the runner seeds the worktree via `docker cp`, which
#     preserves the host uid on `.git` while the container runs as root. Without this,
#     git's dubious-ownership guard fails (exit 128) and breaks `go build`'s default
#     VCS stamping — which silently fails the gate. (See IMPLEMENTATION_PLAN.md.)
#   - A default git identity so the agent can commit its candidate branch in-sandbox.
#
# NOT YET baked in (T5.3/T5.6): the `qa` stage (config/harness.yaml) runs gosec,
# govulncheck, go-licenses, and a mutation tool (gremlins) offline in this image. Those
# binaries — and govulncheck's vulnerability database, which it otherwise fetches from the
# network — must be added here before the live qa gate can pass under the zero-network
# invariant. Until then the qa checks resolve and dispatch but fail closed for lack of
# tooling. Installing them is rootfs composition (T5.3) plus a vetted offline DB (T5.6).
FROM golang:1.26
RUN apt-get update \
 && apt-get install -y --no-install-recommends make git ca-certificates \
 && rm -rf /var/lib/apt/lists/*
RUN git config --global user.email "agent@harness.local" \
 && git config --global user.name "harness agent" \
 && git config --global init.defaultBranch main \
 && git config --global --add safe.directory '*'
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
ENV GOPROXY=off GOFLAGS=-mod=mod
