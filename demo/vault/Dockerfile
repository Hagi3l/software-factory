# Sandbox profile image for the vault demo's `vault-toolchain` profile (named by
# demo/vault/config/souls/*.yaml). Build with the app dir as context so the warmed module
# cache matches the vault's go.mod/go.sum:
#
#   docker build -f demo/vault/Dockerfile -t vault-toolchain demo/vault/app
#
# It bases on the kernel's `go-toolchain` image (build it first from
# deploy/go-toolchain.Dockerfile; run.sh does this automatically). That base already carries
# everything the gate needs offline — Go 1.26 + make + git, the gate tools (golangci-lint,
# gosec, govulncheck + the baked offline vuln DB at GOVULNDB, go-licenses), gopls + the
# language-server manifest, and a git identity — so this image adds only what the vault's
# view layer needs on top:
#   - templ (pinned to the version the committed *_templ.go were generated with, so an
#     in-sandbox `templ generate` cannot drift) and the Tailwind standalone CLI, for a
#     feature that touches views and runs `make generate`.
#   - the vault's module cache, warmed from its own go.mod/go.sum. The base sets GOPROXY=off
#     (zero-network at run), so the download step re-enables a proxy just for itself; a
#     feature that adds a new dependency needs this image rebuilt.
FROM go-toolchain:latest

# templ CLI + Tailwind standalone CLI (need network at build; never at run). The base set
# GOPROXY=off, so re-enable a proxy just for the install.
RUN GOPROXY=https://proxy.golang.org go install github.com/a-h/templ/cmd/templ@v0.3.1001
RUN curl -sSfL https://github.com/tailwindlabs/tailwindcss/releases/download/v4.3.0/tailwindcss-linux-x64 \
      -o /usr/local/bin/tailwindcss \
 && chmod +x /usr/local/bin/tailwindcss

# Warm the vault's module cache (build context = demo/vault/app). The base image set
# GOPROXY=off; override it for this one download so the deps resolve, then it reverts.
WORKDIR /workspace
COPY go.mod go.sum ./
RUN GOPROXY=https://proxy.golang.org go mod download

# Warm the BUILD cache (GOCACHE), not just the module cache above. Every gate runs in a
# FRESH sandbox by design, so without this each `make test-unit`/`gosec`/`govulncheck` run
# cold-compiles the whole dependency graph + stdlib from scratch — and modernc.org/sqlite
# (pure-Go SQLite) is enormous. Compiling once here at image-build time bakes those compiled
# artifacts into the image so every in-sandbox gate reuses them. The agent's diff only
# recompiles the vault's own changed packages (cheap); stdlib + deps stay cached. Offline-safe:
# the deps are already in the module cache, so no proxy is needed (GOPROXY stays off). `go test
# -run='^$'` compiles the test binaries (what test-unit needs) without running any test. A
# feature that adds a new dependency needs this image rebuilt — same as the module cache above.
COPY . .
RUN go build ./... && go test -run='^$' ./...
