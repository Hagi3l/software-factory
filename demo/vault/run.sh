#!/usr/bin/env bash
# Turnkey "secrets vault" demo for the harness.
#
# Scaffolds a throwaway target repo from the ESTABLISHED vault app (an already-green
# Go/templ/htmx/SQLite codebase), then runs the harness with the control room up. You then
# author a NEW feature requirement live in the Create-Task wizard and watch the full
# pipeline (plan -> author-tests -> implement -> qa -> integrate) take it to a merge on
# `main` — driven by a REMOTE model served by OpenRouter. See demo/vault/README.md.
#
# Unlike demo/run.sh this does NOT seed an issue: the requirement is drafted on stage via
# the wizard at /create. The repo ships green, so the agents extend a clean tree.
#
# Requires OPENAI_API_KEY to hold your OpenRouter API key (the openai-compat adapter and
# the requirements-planner both send it as the bearer token).
#
# Usage:
#   OPENAI_API_KEY='sk-or-...' ./demo/vault/run.sh
#   MODEL='anthropic/claude-opus-4.8' ./demo/vault/run.sh   # override the model (OpenRouter slug)
#   MODEL_ENDPOINT='https://openrouter.ai/api/v1' ./demo/vault/run.sh
#   SERVE_ADDR='127.0.0.1:9000' ./demo/vault/run.sh
#   BD=/path/to/bd ./demo/vault/run.sh                      # override the beads CLI
#   KEEP_SITE=1 ./demo/vault/run.sh                         # don't delete the scratch repo on exit
#   JAEGER=1 ./demo/vault/run.sh                            # spin a Jaeger container; export OTel traces to it
set -euo pipefail

# ---- knobs (override via env) ----------------------------------------------------------
DEFAULT_MODEL='deepseek/deepseek-v4-flash'
DEFAULT_ENDPOINT='https://openrouter.ai/api/v1'
MODEL="${MODEL:-$DEFAULT_MODEL}"
MODEL_ENDPOINT="${MODEL_ENDPOINT:-$DEFAULT_ENDPOINT}"
SERVE_ADDR="${SERVE_ADDR:-127.0.0.1:8080}"
BD="${BD:-bd}"
IMAGE='vault-toolchain'       # sandbox profile named by the vault souls
BASE_IMAGE='go-toolchain'     # the vault image bases on this kernel image
JAEGER_NAME='harness-vault-jaeger'              # container name (JAEGER=1 only)
JAEGER_IMAGE='jaegertracing/all-in-one:latest'  # single-binary OTLP collector + trace UI

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$DEMO_DIR/../.." && pwd)"
HARNESS="$REPO_ROOT/bin/harness"
APP_DIR="$DEMO_DIR/app"

say() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }

# ---- preflight -------------------------------------------------------------------------
command -v docker >/dev/null || { echo "error: docker not found (the sandbox backend needs it)"; exit 1; }
command -v "$BD"  >/dev/null || { echo "error: beads CLI '$BD' not found (set BD=/path/to/bd)"; exit 1; }
command -v git    >/dev/null || { echo "error: git not found"; exit 1; }
docker info >/dev/null 2>&1   || { echo "error: docker daemon not reachable"; exit 1; }
[ -n "${OPENAI_API_KEY:-}" ] || { echo "error: OPENAI_API_KEY is not set — put your OpenRouter API key in it"; exit 1; }

# ---- build the harness binary ----------------------------------------------------------
say "Building harness"
( cd "$REPO_ROOT" && make build )

# ---- ensure the sandbox images exist ---------------------------------------------------
# The vault image bases on the kernel go-toolchain image; build that first if missing.
if ! docker image inspect "$BASE_IMAGE" >/dev/null 2>&1; then
  say "Building the base '$BASE_IMAGE' image (first run only; downloads the Go base + vuln DB)"
  docker build -f "$REPO_ROOT/deploy/go-toolchain.Dockerfile" -t "$BASE_IMAGE" "$REPO_ROOT"
fi
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  say "Building the '$IMAGE' image (adds templ + Tailwind + the vault module cache)"
  docker build -f "$DEMO_DIR/Dockerfile" -t "$IMAGE" "$APP_DIR"
fi

# ---- materialize config (model/endpoint subs, and the Jaeger OTLP endpoint) ------------
# Copy the tracked config to a temp dir whenever we need to rewrite it — for a MODEL/ENDPOINT
# override and/or to point OTel at the Jaeger container — so demo/vault/config stays pristine.
CONFIG_DIR="$DEMO_DIR/config"
MODEL_OVERRIDDEN=
if [ "$MODEL" != "$DEFAULT_MODEL" ] || [ "$MODEL_ENDPOINT" != "$DEFAULT_ENDPOINT" ]; then
  MODEL_OVERRIDDEN=1
fi
if [ -n "$MODEL_OVERRIDDEN" ] || [ -n "${JAEGER:-}" ]; then
  CONFIG_DIR="$(mktemp -d -t harness-vault-cfg-XXXXXX)/config"
  cp -r "$DEMO_DIR/config" "$CONFIG_DIR"
fi
if [ -n "$MODEL_OVERRIDDEN" ]; then
  # The model name is the flash registry key in infra.dev.yaml AND the `model:` field in
  # every soul; substitute both so they stay consistent (validation cross-checks them). The
  # requirements_planner is pinned to the separate -pro registry key and is intentionally
  # NOT rewritten, so a MODEL= override swaps the souls without downgrading the planner.
  for f in "$CONFIG_DIR/infra.dev.yaml" "$CONFIG_DIR/harness.yaml" "$CONFIG_DIR/souls/"*.yaml; do
    sed -i.bak -e "s|$DEFAULT_MODEL|$MODEL|g" -e "s|$DEFAULT_ENDPOINT|$MODEL_ENDPOINT|g" "$f"
    rm -f "$f.bak"
  done
  say "Using model '$MODEL' at $MODEL_ENDPOINT (config materialized in $CONFIG_DIR)"
fi
if [ -n "${JAEGER:-}" ]; then
  # Repoint otel.endpoint from "" (off) to the Jaeger container's OTLP/gRPC port on the host.
  # Matches only the empty-string value, so the model `endpoint:` URL above is untouched.
  sed -i.bak 's|^  endpoint: "".*|  endpoint: "127.0.0.1:4317"|' "$CONFIG_DIR/infra.dev.yaml"
  rm -f "$CONFIG_DIR/infra.dev.yaml.bak"
fi

# ---- scaffold a throwaway target repo from the established app -------------------------
SITE="$(mktemp -d -t harness-vault-site-XXXXXX)"
say "Scaffolding scratch vault repo: $SITE"
# Copy the app tree, then let its .gitignore exclude build artifacts from the commit.
cp -a "$APP_DIR/." "$SITE/"
rm -rf "$SITE/bin" "$SITE/test/results" "$SITE"/*.db 2>/dev/null || true
git -C "$SITE" init -q -b main
git -C "$SITE" add .
git -C "$SITE" -c user.email='demo@harness.local' -c user.name='harness demo' \
  commit -qm 'seed: established secrets vault (auth + secrets + audit + dashboard)'
# --non-interactive: bd init drops into a wizard when stdin is a tty (the normal case
# when you run this script by hand). Its stdout is hidden below, so the prompt would be
# invisible and bd would hang forever waiting on stdin. Force the non-interactive path.
( cd "$SITE" && "$BD" init --prefix harness --non-interactive >/dev/null )

cleanup() {
  [ -n "${KEEP_SITE:-}" ] || rm -rf "$SITE"
  [ -z "${JAEGER:-}" ] || docker rm -f "$JAEGER_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# ---- start Jaeger (OTLP collector + trace UI), if requested ----------------------------
# Single container: insecure OTLP/gRPC on 4317 (matches the harness exporter — no auth, no
# headers) and the trace UI on 16686. The exporter dials lazily and degrades gracefully, so
# this need only be reachable by the time the first span is exported.
if [ -n "${JAEGER:-}" ]; then
  say "Starting Jaeger (OTLP collector + trace UI) — http://127.0.0.1:16686"
  docker rm -f "$JAEGER_NAME" >/dev/null 2>&1 || true   # clear any stale container from a prior run
  docker run -d --name "$JAEGER_NAME" \
    -e COLLECTOR_OTLP_ENABLED=true \
    -p 16686:16686 -p 4317:4317 \
    "$JAEGER_IMAGE" >/dev/null
fi

# ---- validate, run ---------------------------------------------------------------------
say "Validating demo config"
"$HARNESS" validate --config "$CONFIG_DIR"

# The openai-compat adapter (and the requirements-planner) send OPENAI_API_KEY as the
# bearer token to OpenRouter.
export OPENAI_API_KEY

say "Running the harness — control room at http://$SERVE_ADDR  (Ctrl-C to stop)"
echo "    scratch repo : $SITE"
echo "    next step    : open http://$SERVE_ADDR/create and draft a feature requirement"
echo "                   (e.g. a one-time, single-use secret share link). Approve it in the"
echo "                   wizard, then watch the Board and Activity views take it to a merge."
echo "    when it lands: 'git -C $SITE log' shows the provenance trailer; the diff is the feature."
[ -z "${JAEGER:-}" ] || echo "    telemetry    : open http://127.0.0.1:16686 (service 'harness') to watch each invocation as a trace"
echo
"$HARNESS" run --config "$CONFIG_DIR" --repo "$SITE" --bd "$BD" --serve-addr "$SERVE_ADDR"
