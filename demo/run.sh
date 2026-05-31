#!/usr/bin/env bash
# Turnkey landing-page demo for the harness.
#
# Scaffolds a throwaway target repo in a temp dir, seeds a single landing-page spec, and
# runs the harness end to end (author-tests -> implement -> integrate) against a LOCAL
# model served by Ollama. Watch it in the control room; the result lands on `main` of the
# scratch repo. See demo/README.md.
#
# Usage:
#   ./demo/run.sh
#   MODEL='qwen2.5-coder:7b' ./demo/run.sh          # override the model (must match `ollama list`)
#   OLLAMA_HOST='http://localhost:11434/v1' ./demo/run.sh
#   SERVE_ADDR='127.0.0.1:9000' ./demo/run.sh
#   BD=/path/to/bd ./demo/run.sh                    # override the beads CLI
set -euo pipefail

# ---- knobs (override via env) ----------------------------------------------------------
DEFAULT_MODEL='qwen3.6:27b'
DEFAULT_OLLAMA='http://localhost:11434/v1'
MODEL="${MODEL:-$DEFAULT_MODEL}"
OLLAMA_HOST="${OLLAMA_HOST:-$DEFAULT_OLLAMA}"
SERVE_ADDR="${SERVE_ADDR:-127.0.0.1:8080}"
BD="${BD:-bd}"
IMAGE='go-toolchain'   # sandbox profile named by the demo souls; reused for the shell gate

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$DEMO_DIR/.." && pwd)"
HARNESS="$REPO_ROOT/bin/harness"

say() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }

# ---- preflight -------------------------------------------------------------------------
command -v docker >/dev/null || { echo "error: docker not found (the sandbox backend needs it)"; exit 1; }
command -v "$BD"  >/dev/null || { echo "error: beads CLI '$BD' not found (set BD=/path/to/bd)"; exit 1; }
command -v git    >/dev/null || { echo "error: git not found"; exit 1; }
docker info >/dev/null 2>&1   || { echo "error: docker daemon not reachable"; exit 1; }
curl -fsS "${OLLAMA_HOST%/v1}/api/tags" >/dev/null 2>&1 \
  || echo "warning: could not reach Ollama at $OLLAMA_HOST — is it running, and is '$MODEL' pulled?"

# ---- build the harness binary ----------------------------------------------------------
say "Building harness"
( cd "$REPO_ROOT" && make build )

# ---- ensure the sandbox image exists ---------------------------------------------------
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  say "Building the '$IMAGE' sandbox image (first run only; downloads the Go base image)"
  docker build -f "$REPO_ROOT/deploy/go-toolchain.Dockerfile" -t "$IMAGE" "$REPO_ROOT"
fi

# ---- materialize config (substitute model/endpoint only if overridden) -----------------
CONFIG_DIR="$DEMO_DIR/config"
if [ "$MODEL" != "$DEFAULT_MODEL" ] || [ "$OLLAMA_HOST" != "$DEFAULT_OLLAMA" ]; then
  CONFIG_DIR="$(mktemp -d -t harness-demo-cfg-XXXXXX)/config"
  cp -r "$DEMO_DIR/config" "$CONFIG_DIR"
  # The model name is the registry key in infra.dev.yaml AND the `model:` field in both
  # souls; substitute all of them so they stay consistent (validation cross-checks them).
  for f in "$CONFIG_DIR/infra.dev.yaml" "$CONFIG_DIR/souls/"*.yaml; do
    sed -i.bak -e "s|$DEFAULT_MODEL|$MODEL|g" -e "s|$DEFAULT_OLLAMA|$OLLAMA_HOST|g" "$f"
    rm -f "$f.bak"
  done
  say "Using model '$MODEL' at $OLLAMA_HOST (config materialized in $CONFIG_DIR)"
fi

# ---- scaffold a throwaway target repo --------------------------------------------------
SITE="$(mktemp -d -t harness-demo-site-XXXXXX)"
say "Scaffolding scratch site repo: $SITE"
mkdir -p "$SITE/specs"
cp "$DEMO_DIR/templates/landing-page.md" "$SITE/specs/landing-page.md"
git -C "$SITE" init -q -b main
git -C "$SITE" add .
git -C "$SITE" -c user.email='demo@harness.local' -c user.name='harness demo' \
  commit -qm 'seed: Acme landing page spec'
( cd "$SITE" && "$BD" init --prefix harness >/dev/null )

# ---- validate, seed, run ---------------------------------------------------------------
say "Validating demo config"
"$HARNESS" validate --config "$CONFIG_DIR"

say "Seeding the landing-page issue"
"$HARNESS" seed --config "$CONFIG_DIR" --repo "$SITE" --bd "$BD" \
  --title 'Acme landing page' \
  --description 'A single static index.html landing page for Acme.' \
  --spec specs/landing-page.md

# Ollama ignores the key, but the OpenAI Go SDK the openai-compat adapter uses requires a
# non-empty one.
export OPENAI_API_KEY="${OPENAI_API_KEY:-ollama}"

say "Running the harness — control room at http://$SERVE_ADDR  (Ctrl-C to stop)"
echo "    scratch repo : $SITE"
echo "    when it lands: open $SITE/index.html in a browser; 'git -C $SITE log' shows the provenance trailer"
echo
"$HARNESS" run --config "$CONFIG_DIR" --repo "$SITE" --bd "$BD" --serve-addr "$SERVE_ADDR"
