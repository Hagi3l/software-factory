# Sandbox profile image for the `node-toolchain` profile (profiles/node).
#
# Build from the repo root:
#   docker build -f deploy/node-toolchain.Dockerfile -t factory/node-toolchain:dev .
#
# Contains: Node 22 LTS, corepack (pnpm/yarn), git, factory-node-check, and a
# slim software-factory binary for any in-sandbox helpers. Package installs that
# need the network must go through a future package-proxy shim (or a project
# image that bakes node_modules); git worktrees do not include ignored deps.
#
# Gate tools live as scripts on PATH (factory-node-check), not make targets —
# the profile's checks: map points at them so any Node/TS repo with standard
# package.json scripts can be graded without a per-repo Makefile.

FROM node:22-bookworm AS factory-build
# Optional: not required for node profile; keep stage for symmetry / future shims.
WORKDIR /src

FROM node:22-bookworm

RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates curl bash jq \
 && rm -rf /var/lib/apt/lists/* \
 && corepack enable \
 && git config --global user.email "agent@factory.local" \
 && git config --global user.name "factory agent" \
 && git config --global init.defaultBranch main

# Pin common global CLIs agents and gates use; project deps still come from lockfiles.
RUN npm install -g typescript eslint prettier 2>/dev/null || true

COPY deploy/scripts/factory-node-check /usr/local/bin/factory-node-check
RUN chmod +x /usr/local/bin/factory-node-check

WORKDIR /work
# Default command is overridden by the sandbox backend (sleep forever + docker exec).
CMD ["sleep", "infinity"]
