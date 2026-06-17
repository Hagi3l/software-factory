#!/usr/bin/env bash
# Turnkey "secrets vault" demo for the harness.
#
# Scaffolds a throwaway target repo from the ESTABLISHED vault app (an already-green
# Go/templ/htmx/SQLite codebase), then runs the harness with the control room up. You then
# author a NEW feature requirement live in the Create-Task wizard and watch the full
# pipeline (plan -> author-tests -> implement -> qa -> integrate) take it to a merge on
# `main` — driven by a REMOTE model served by OpenRouter. See demo/vault/README.md.
#
# The scratch repo is wired to a PUBLIC GitHub repo (VAULT_REMOTE): it is reset to the green
# seed at startup, and each landed feature's machine-authored merge commit is pushed there —
# the artifact the audience inspects, and the push that fires the deploy (GitHub Actions ->
# your VPS). Set VAULT_REMOTE='' for a purely-local run.
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
#   VAULT_REMOTE='' ./demo/vault/run.sh                     # purely local; no GitHub push (default: push to the public repo)
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
# Public GitHub repo the merged `main` is pushed to (the artifact the audience inspects).
# Reset to the green seed at startup, then each landed feature is pushed onto it. Set empty
# (VAULT_REMOTE='') for a purely-local run with no GitHub push.
VAULT_REMOTE="${VAULT_REMOTE:-git@github.com:Loxstomper/vault.git}"
IMAGE='vault-toolchain'       # sandbox profile named by the vault souls
BASE_IMAGE='go-toolchain'     # the vault image bases on this kernel image
JAEGER_NAME='harness-vault-jaeger'              # container name (JAEGER=1 only)
JAEGER_IMAGE='jaegertracing/all-in-one:1.76.0'  # single-binary OTLP collector + trace UI (pinned: v2 is a collector-based rewrite)

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$DEMO_DIR/../.." && pwd)"
HARNESS="$REPO_ROOT/bin/harness"
APP_DIR="$DEMO_DIR/app"

say() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }

# ---- preflight -------------------------------------------------------------------------
command -v docker >/dev/null || { echo "error: docker not found (the sandbox backend needs it)"; exit 1; }
command -v "$BD"  >/dev/null || { echo "error: beads CLI '$BD' not found (set BD=/path/to/bd)"; exit 1; }
command -v dolt   >/dev/null || { echo "error: dolt not found — server-mode beads auto-starts a 'dolt sql-server' (brew install dolt)"; exit 1; }
command -v git    >/dev/null || { echo "error: git not found"; exit 1; }
docker info >/dev/null 2>&1   || { echo "error: docker daemon not reachable"; exit 1; }
[ -n "${OPENAI_API_KEY:-}" ] || { echo "error: OPENAI_API_KEY is not set — put your OpenRouter API key in it"; exit 1; }

# ---- build the harness binary ----------------------------------------------------------
say "Building harness"
( cd "$REPO_ROOT" && make build )

# ---- (re)build the sandbox images ------------------------------------------------------
# Always build, even when the image already exists. Docker's layer cache makes an unchanged
# rebuild near-instant, while building UNCONDITIONALLY guarantees a *stale* image — one built
# before its Dockerfile gained, e.g., the gate tools (golangci-lint/gosec/govulncheck/
# go-licenses) — is refreshed rather than silently reused (a stale base is what makes the qa
# stage fail with `No such file or directory`). The vault image bases on the kernel
# go-toolchain image, so build that first. Only the first base build is slow (it downloads
# the Go base + the offline vuln DB); cached rebuilds are fast.
say "Building the base '$BASE_IMAGE' image (cached rebuild is fast; first build downloads the Go base + vuln DB)"
docker build -f "$REPO_ROOT/deploy/go-toolchain.Dockerfile" -t "$BASE_IMAGE" "$REPO_ROOT"
say "Building the '$IMAGE' image (adds templ + Tailwind + the vault module cache)"
docker build -f "$DEMO_DIR/Dockerfile" -t "$IMAGE" "$APP_DIR"

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
  # The flash slug is the flash registry key in infra.dev.yaml AND the `model:` field in the
  # execution souls (implementor, security, merge-resolver); substitute both so they stay
  # consistent (validation cross-checks them). The three -pro-pinned souls — the
  # requirements_planner (harness.yaml), the decomposition planner (souls/planner.yaml), and
  # the test-author (souls/test-author.yaml) — name the separate -pro slug, so this flash-slug
  # sed leaves them untouched: a MODEL= override swaps the execution souls without downgrading
  # the spec/decomposition/test-contract roles that define what correct means.
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
# Keep the harness work store out of git entirely: .beads is the orchestrator's local Dolt
# database, not part of the app. Using .git/info/exclude (local, untracked) rather than a
# committed .gitignore means the machinery never appears in the public repo — not even as an
# ignore rule. (bd init below creates .beads after the seed commit, so it is out of the seed
# regardless; this guarantees it stays out of any later commit too.)
printf '.beads/\n' >> "$SITE/.git/info/exclude"
git -C "$SITE" add .
git -C "$SITE" -c user.email='demo@harness.local' -c user.name='harness demo' \
  commit -qm 'seed: established secrets vault (auth + secrets + audit + dashboard)'
# --server: run beads against a persistent, per-run `dolt sql-server` (bd auto-starts it and
# records its pid/port under .beads/) instead of cold-starting the embedded Dolt engine on
# every `bd` invocation. The orchestrator's reconcile loop and the control-room views poll
# beads constantly during a run; a fresh engine cold-start per call (~0.7s each, and they
# *contend* under concurrency — 8 at once stretch to ~4s) is what caused the `bd list`
# timeouts. A warm server drops a list to ~0.2s with no stampede. Data lives in .beads/dolt/
# (repo-scoped, torn down with the scratch repo, git-excluded like the rest of .beads);
# cleanup() runs `bd dolt stop` before removing $SITE so the server doesn't orphan.
# --non-interactive: bd init drops into a wizard when stdin is a tty (the normal case when
# you run this script by hand). Its stdout is hidden below, so the prompt would be invisible
# and bd would hang forever waiting on stdin. Force the non-interactive path.
( cd "$SITE" && "$BD" init --prefix vault --server --non-interactive >/dev/null )
"$BD" -C "$SITE" dolt status >/dev/null 2>&1 || { echo "error: beads dolt sql-server did not come up (see $SITE/.beads/dolt-server.log)"; exit 1; }

# ---- reset the public repo to the green seed -------------------------------------------
# The audience inspects this repo, so each run starts it from an identical pristine baseline
# and then the landed feature is pushed on top. `seed` is an immutable browsable ref; `main`
# is force-reset to it. The seed commit's tree is just the app (no .beads, no build
# artifacts), so the public repo only ever shows the vault and the feature the agents add.
if [ -n "$VAULT_REMOTE" ]; then
  say "Resetting public repo to the green seed: $VAULT_REMOTE"
  git -C "$SITE" remote add public "$VAULT_REMOTE"
  git -C "$SITE" push --force public main                  # main → pristine seed
  git -C "$SITE" push --force public "HEAD:refs/heads/seed" # immutable baseline ref
fi

# Background watcher: when the orchestrator advances local main (a feature integrated), push
# it to the public repo. This is a trusted-layer egress of an already-gate-verified, merged
# commit — the agents never touch the network or the remote; only this post-merge push does.
push_main_watcher() {
  local last
  last="$(git -C "$SITE" rev-parse refs/heads/main 2>/dev/null || true)"
  while true; do
    sleep 4
    local cur
    cur="$(git -C "$SITE" rev-parse refs/heads/main 2>/dev/null || true)"
    [ -n "$cur" ] && [ "$cur" != "$last" ] || continue
    if git -C "$SITE" push public main >/dev/null 2>&1; then
      printf '\n\033[1;32m==> pushed main → %s (%s) — deploy triggered\033[0m\n' "$VAULT_REMOTE" "${cur:0:8}"
      last="$cur"
    fi
  done
}

cleanup() {
  [ -z "${PUSH_PID:-}" ] || kill "$PUSH_PID" >/dev/null 2>&1 || true
  # Stop the per-run dolt sql-server before removing the repo, so it doesn't linger as an
  # orphan holding its port and the deleted .beads/dolt files. Unconditional even under
  # KEEP_SITE — a kept repo's server can be restarted on demand with `bd -C <repo> dolt start`.
  "$BD" -C "$SITE" dolt stop >/dev/null 2>&1 || true
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

# Start the post-merge push watcher (no-op when VAULT_REMOTE is empty).
if [ -n "$VAULT_REMOTE" ]; then push_main_watcher & PUSH_PID=$!; fi

say "Running the harness — control room at http://$SERVE_ADDR  (Ctrl-C to stop)"
echo "    scratch repo : $SITE"
[ -z "$VAULT_REMOTE" ] || echo "    public repo  : $VAULT_REMOTE (reset to the green seed; the feature is pushed on merge)"
echo "    next step    : open http://$SERVE_ADDR/create and draft a feature requirement"
echo "                   (e.g. a one-time, single-use secret share link). Approve it in the"
echo "                   wizard, then watch the Board and Activity views take it to a merge."
if [ -n "$VAULT_REMOTE" ]; then
  echo "    when it lands: the merge is pushed to $VAULT_REMOTE (machine-authored commit + provenance"
  echo "                   trailer) — which fires the deploy. 'git -C $SITE log' shows the same locally."
else
  echo "    when it lands: 'git -C $SITE log' shows the provenance trailer; the diff is the feature."
fi
[ -z "${JAEGER:-}" ] || echo "    telemetry    : open http://127.0.0.1:16686 (service 'harness') to watch each invocation as a trace"
echo
"$HARNESS" run --config "$CONFIG_DIR" --repo "$SITE" --bd "$BD" --serve-addr "$SERVE_ADDR"
