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
set -euo pipefail

# ---- knobs (override via env) ----------------------------------------------------------
DEFAULT_MODEL='deepseek/deepseek-v4'
DEFAULT_ENDPOINT='https://openrouter.ai/api/v1'
MODEL="${MODEL:-$DEFAULT_MODEL}"
MODEL_ENDPOINT="${MODEL_ENDPOINT:-$DEFAULT_ENDPOINT}"
SERVE_ADDR="${SERVE_ADDR:-127.0.0.1:8080}"
BD="${BD:-bd}"
IMAGE='vault-toolchain'       # sandbox profile named by the vault souls
BASE_IMAGE='go-toolchain'     # the vault image bases on this kernel image

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

# ---- materialize config (substitute model/endpoint only if overridden) -----------------
CONFIG_DIR="$DEMO_DIR/config"
if [ "$MODEL" != "$DEFAULT_MODEL" ] || [ "$MODEL_ENDPOINT" != "$DEFAULT_ENDPOINT" ]; then
  CONFIG_DIR="$(mktemp -d -t harness-vault-cfg-XXXXXX)/config"
  cp -r "$DEMO_DIR/config" "$CONFIG_DIR"
  # The model name is the registry key in infra.dev.yaml, the requirements_planner model,
  # AND the `model:` field in every soul; substitute all so they stay consistent
  # (validation cross-checks them).
  for f in "$CONFIG_DIR/infra.dev.yaml" "$CONFIG_DIR/harness.yaml" "$CONFIG_DIR/souls/"*.yaml; do
    sed -i.bak -e "s|$DEFAULT_MODEL|$MODEL|g" -e "s|$DEFAULT_ENDPOINT|$MODEL_ENDPOINT|g" "$f"
    rm -f "$f.bak"
  done
  say "Using model '$MODEL' at $MODEL_ENDPOINT (config materialized in $CONFIG_DIR)"
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
( cd "$SITE" && "$BD" init --prefix harness >/dev/null )

cleanup() { [ -n "${KEEP_SITE:-}" ] || rm -rf "$SITE"; }
trap cleanup EXIT

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
echo
"$HARNESS" run --config "$CONFIG_DIR" --repo "$SITE" --bd "$BD" --serve-addr "$SERVE_ADDR"
