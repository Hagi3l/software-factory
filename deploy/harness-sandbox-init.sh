#!/bin/sh
# harness-sandbox-init — the sandbox container's PID 1 wrapper (T5.6).
#
# It starts the in-sandbox GOPROXY shim, then execs the container's main command. The shim
# (`harness sandbox-goproxy`) bridges `go`'s module-proxy requests to the runner's broker
# over the bind-mounted broker socket — the one egress chokepoint a zero-network sandbox
# has — so `go mod download` of a not-yet-cached dependency is mediated and logged by the
# runner instead of needing direct network. The image sets GOPROXY to this shim.
#
# Why this is safe everywhere, including the gate's verification sandbox and any run that
# has NOT allowlisted the package-proxy destination: the shim only forwards; the runner
# decides. A build that needs only cached modules never contacts the shim; a build that
# needs a new module hits it, and the runner allows or denies per its allowlist
# (deny-by-default). So an unconfigured deployment behaves exactly as before (new deps
# fail), and an allowlisted one fetches — no per-image variation, no lockstep deploy.
#
# The runner passes `sleep <forever>` as the command (the container stays alive between
# `docker exec`s); we background the shim and exec that so it remains PID 1.
set -e

harness sandbox-goproxy \
  --broker "unix:${HARNESS_BROKER_SOCK:-/run/harness/broker.sock}" \
  --addr "${HARNESS_GOPROXY_ADDR:-127.0.0.1:8123}" \
  >/tmp/harness-goproxy.log 2>&1 &

exec "$@"
