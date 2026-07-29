# Sandbox profile image for the vault demo's `vault-toolchain` profile (named by
# demo/vault/config/souls/*.yaml). Build with the app dir as context so the warmed module
# cache matches the vault's go.mod/go.sum:
#
#   docker build -f demo/vault/Dockerfile -t vault-toolchain demo/vault/app
#
# It bases on the kernel's `go-toolchain-base` image (the stable stage of
# deploy/go-toolchain.Dockerfile — build it and `go-toolchain` first; run.sh does all
# three automatically). That base already carries everything the gate needs offline —
# Go 1.26 + make + git, the gate tools (golangci-lint, gosec, govulncheck + the baked
# offline vuln DB at GOVULNDB, go-licenses), gopls + the language-server manifest, and
# a git identity — so this image adds only what the vault's view layer needs on top:
#   - templ (pinned to the version the committed *_templ.go were generated with, so an
#     in-sandbox `templ generate` cannot drift) and the Tailwind standalone CLI, for a
#     feature that touches views and runs `make generate`.
#   - the vault's module cache, warmed from its own go.mod/go.sum. The base sets GOPROXY
#     to the in-sandbox broker shim (zero-network at run; not running at build time), so
#     the download steps override it per-RUN; a feature that adds a new dependency needs
#     this image rebuilt.
#
# Deliberately NOT `FROM go-toolchain:latest`: that image's last layer is the harness
# shim binary, which changes on every harness commit — and a child's layer cache is keyed
# on its parent image ID, so basing on it would re-download templ/Tailwind/modules and
# re-warm the build cache on every commit. Basing on the stable `go-toolchain-base` and
# copying the two volatile files out of `go-toolchain` as the FINAL layers (bottom of
# this file) means a harness source change rebuilds seconds of COPY here, nothing else.
FROM go-toolchain-base:latest

# templ CLI + Tailwind standalone CLI (need network at build; never at run). The base set
# GOPROXY to the in-sandbox shim (unreachable at build), so point this install at the
# public proxy for just this RUN.
RUN GOPROXY=https://proxy.golang.org go install github.com/a-h/templ/cmd/templ@v0.3.1001
# Pick the Tailwind standalone binary that matches the IMAGE's architecture. The image inherits
# the host's arch (arm64 on Apple Silicon, amd64 on x86), so a hardcoded -linux-x64 binary runs
# under emulation on an arm64 image and crashes with SIGTRAP (exit 133, the "rosetta error") —
# which breaks `make generate` (templ succeeds, tailwind crashes) inside the agent sandboxes.
# `uname -m` reflects the build arch, so this resolves to the native binary on either platform.
RUN ARCH="$(uname -m)" && case "$ARCH" in \
      x86_64)  TW=tailwindcss-linux-x64 ;; \
      aarch64) TW=tailwindcss-linux-arm64 ;; \
      *) echo "unsupported arch for tailwind: $ARCH" >&2; exit 1 ;; \
    esac \
 && curl -sSfL --retry 3 "https://github.com/tailwindlabs/tailwindcss/releases/download/v4.3.0/$TW" \
      -o /usr/local/bin/tailwindcss \
 && chmod +x /usr/local/bin/tailwindcss \
 && /usr/local/bin/tailwindcss --help >/dev/null   # fail the build loudly if the binary can't run

# Warm the vault's module cache (build context = demo/vault/app), overriding the base's
# shim GOPROXY for this one download.
WORKDIR /workspace
COPY go.mod go.sum ./
RUN GOPROXY=https://proxy.golang.org go mod download

# Warm the BUILD cache (GOCACHE), not just the module cache above. Every gate runs in a
# FRESH sandbox by design, so without this each `make test-unit`/`gosec`/`govulncheck` run
# cold-compiles the whole dependency graph + stdlib from scratch — and modernc.org/sqlite
# (pure-Go SQLite) is enormous. Compiling once here at image-build time bakes those compiled
# artifacts into the image so every in-sandbox gate reuses them. The agent's diff only
# recompiles the vault's own changed packages (cheap); stdlib + deps stay cached. Offline-safe:
# the deps are already in the module cache, so no proxy override is needed. `go test
# -run='^$'` compiles the test binaries (what test-unit needs) without running any test. A
# feature that adds a new dependency needs this image rebuilt — same as the module cache above.
COPY . .
RUN go build ./... && go test -run='^$' ./...

# The volatile layers, deliberately LAST (see the header): the in-sandbox GOPROXY shim
# binary + the entrypoint that starts it, taken from the fully-built go-toolchain image.
COPY --from=go-toolchain:latest /usr/local/bin/software-factory /usr/local/bin/software-factory
COPY --from=go-toolchain:latest /usr/local/bin/software-factory-sandbox-init /usr/local/bin/software-factory-sandbox-init
ENTRYPOINT ["/usr/local/bin/software-factory-sandbox-init"]
