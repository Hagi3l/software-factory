# Project-baked sandbox image for get-chilld (P0 dependency strategy A).
#
# Builds on node-toolchain and warms npm dependencies from the app's lockfile so
# zero-network sandboxes can run factory-node-check without a live npm registry.
#
# From software-factory repo root (with get-chilld available as a sibling or path):
#
#   docker build -f deploy/node-toolchain.Dockerfile -t factory/node-toolchain:dev .
#   docker build -f deploy/get-chilld.Dockerfile \
#     --build-arg APP_DIR=../get-chilld \
#     -t factory/get-chilld:dev \
#     ../get-chilld
#
# Or from get-chilld with factory checkout as context parent — simplest path:
#
#   cd /Users/ve/Projects/get-chilld
#   docker build -f ../software-factory/deploy/get-chilld.Dockerfile \
#     -t factory/get-chilld:dev .
#
# Point profiles/node infra at this image for get-chilld runs (or use
# profiles/node/infra.get-chilld.yaml if present).

FROM factory/node-toolchain:dev

WORKDIR /opt/app-cache
# Build context = get-chilld root
COPY package.json package-lock.json ./
RUN npm ci --ignore-scripts 2>/dev/null || npm install --ignore-scripts

# Agents' worktree is still seeded at /work by the sandbox backend; the cache is a
# fallback agents/scripts can copy from if install fails offline.
ENV FACTORY_NODE_MODULES_CACHE=/opt/app-cache/node_modules

WORKDIR /work
