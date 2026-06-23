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
#   OPENOBSERVE=1 ./demo/vault/run.sh                       # spin an OpenObserve container; export ALL THREE signals (traces+logs+metrics) to it
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
# OpenObserve (OPENOBSERVE=1 only): a single-binary, multi-signal OTLP backend — unlike Jaeger
# (traces only) it ingests traces + logs + metrics on one authenticated endpoint, so the demo
# can show the WHOLE record (the T5.12/T5.13 logs+metrics work) land in one place. The image
# tag is overridable so a schema bump (dashboard v5 is pinned in observe/) can be tracked.
OPENOBSERVE_NAME='harness-vault-openobserve'
OPENOBSERVE_IMAGE="${OPENOBSERVE_IMAGE:-public.ecr.aws/zinclabs/openobserve:v0.14.7}"
OO_EMAIL='admin@admin.com'          # OO root user — login is by EMAIL (ingestion token = base64(email:password))
OO_PASSWORD='admin'                 # ephemeral container, dies on exit — not a real secret
OO_ORG='default'                    # OO default organization (REST path + OTLP `organization` header)

# JAEGER and OPENOBSERVE both retarget the single otel.endpoint, so they are mutually exclusive.
if [ -n "${JAEGER:-}" ] && [ -n "${OPENOBSERVE:-}" ]; then
  echo "error: set JAEGER=1 OR OPENOBSERVE=1, not both — otel.endpoint is one export target"; exit 1
fi

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
# Fail fast if the control-room port is already taken (a leftover harness from a prior run, or
# any other local server) — otherwise the conflict only surfaces at the very last `harness run`
# line, after the full (minutes-long) image build + scaffold. bash /dev/tcp suffices; this
# script already relies on bashisms.
_sa_host="${SERVE_ADDR%:*}"; _sa_port="${SERVE_ADDR##*:}"
if (exec 3<>"/dev/tcp/${_sa_host}/${_sa_port}") 2>/dev/null; then
  exec 3>&- 3<&-
  echo "error: $SERVE_ADDR is already in use — free it (lsof -i :${_sa_port}) or set SERVE_ADDR=127.0.0.1:9000"; exit 1
fi

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

# ---- materialize config (model/endpoint subs, and the Jaeger/OpenObserve OTLP endpoint) ----
# Copy the tracked config to a temp dir whenever we need to rewrite it — for a MODEL/ENDPOINT
# override and/or to point OTel at the Jaeger or OpenObserve container — so demo/vault/config
# stays pristine.
CONFIG_DIR="$DEMO_DIR/config"
MODEL_OVERRIDDEN=
if [ "$MODEL" != "$DEFAULT_MODEL" ] || [ "$MODEL_ENDPOINT" != "$DEFAULT_ENDPOINT" ]; then
  MODEL_OVERRIDDEN=1
fi
if [ -n "$MODEL_OVERRIDDEN" ] || [ -n "${JAEGER:-}" ] || [ -n "${OPENOBSERVE:-}" ]; then
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
if [ -n "${OPENOBSERVE:-}" ]; then
  # Repoint otel at the OpenObserve container: its OTLP/gRPC port (5081, plaintext h2c -> tls
  # stays false) plus the org/stream routing + auth headers an authenticated multi-signal
  # backend needs (T5.12). The `authorization` value stays an ${ENV_VAR} ref — never a literal
  # secret — so it passes `harness validate`'s credential-header rule; run.sh exports
  # OTEL_OTLP_AUTH below once OO is up. `organization`/`stream-name` are routing metadata, so a
  # literal is fine. awk (not sed) because we replace one line with a multi-line block; matches
  # only the 2-space `endpoint: ""` (the model endpoints are deeper-indented + non-empty).
  awk '
    /^  endpoint: ""/ && !oo_done {
      print "  endpoint: \"127.0.0.1:5081\""
      print "  tls: false"
      print "  headers:"
      print "    organization: default"
      print "    stream-name: default"
      print "    authorization: ${OTEL_OTLP_AUTH}"
      oo_done = 1
      next
    }
    { print }
  ' "$CONFIG_DIR/infra.dev.yaml" > "$CONFIG_DIR/infra.dev.yaml.oo" \
    && mv "$CONFIG_DIR/infra.dev.yaml.oo" "$CONFIG_DIR/infra.dev.yaml"
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
# --skip-hooks: do NOT install beads' git hooks. By default bd init points the repo's
# core.hooksPath at .beads/hooks, so every git operation fires a beads post-checkout/post-merge
# that re-syncs the work store from .beads/issues.jsonl. The harness drives heavy git activity
# on THIS repo during a run (the merger adds a detached worktree per rebase and fast-forwards
# main), so those hooks fire mid-run and re-import a stale jsonl snapshot over the orchestrator's
# authoritative Dolt writes — silently reverting just-closed stage issues back to in_progress
# (observed: every intermediate author-tests/implement/qa issue stuck open after a clean merge).
# The orchestrator is the single beads writer via the warm Dolt server; it never wants a git
# hook reconciling beads behind its back, so the hooks are pure foot-gun here. Disable them.
( cd "$SITE" && "$BD" init --prefix vault --server --non-interactive --skip-hooks >/dev/null )
"$BD" -C "$SITE" dolt status >/dev/null 2>&1 || { echo "error: beads dolt sql-server did not come up (see $SITE/.beads/dolt-server.log)"; exit 1; }

# ---- reset the public repo to the green seed -------------------------------------------
# The audience inspects this repo, so each run starts it from an identical pristine baseline
# and then the landed feature is pushed on top. `seed` is an immutable browsable ref; `main`
# is force-reset to it. The seed commit's tree is just the app (no .beads, no build
# artifacts), so the public repo only ever shows the vault and the feature the agents add.
if [ -n "$VAULT_REMOTE" ]; then
  say "Resetting public repo to the green seed: $VAULT_REMOTE"
  git -C "$SITE" remote add public "$VAULT_REMOTE"
  # Degrade to LOCAL-ONLY instead of aborting the whole demo if the push fails. Under
  # `set -e` a bare `git push` to the default SSH remote would exit the script — BEFORE the
  # control room ever binds — on a stage laptop with no deploy key / no network / no access.
  # The README's "stays local" promise must not require remembering VAULT_REMOTE=''.
  if git -C "$SITE" push --force public main &&
     git -C "$SITE" push --force public "HEAD:refs/heads/seed"; then
    :                                                      # main → pristine seed; immutable `seed` ref
  else
    say "WARN: push to $VAULT_REMOTE failed (no SSH key / network / access?) — continuing LOCAL-ONLY (no public push, no deploy)"
    git -C "$SITE" remote remove public 2>/dev/null || true
    VAULT_REMOTE=''                                        # the watcher + on-screen messaging below already no-op when empty
  fi
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
  # OO runs with --rm so it self-removes on stop; force-remove anyway in case it's still up.
  [ -z "${OPENOBSERVE:-}" ] || docker rm -f "$OPENOBSERVE_NAME" >/dev/null 2>&1 || true
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

# ---- start OpenObserve (multi-signal OTLP backend), if requested -----------------------
# Single self-contained binary: authenticated OTLP/gRPC on 5081 (traces + logs + metrics all
# ride the one port) and the UI/REST API on 5080. Ephemeral by design — `--rm`, no volume, so
# every ingested signal dies with the container — and `--memory`-capped so it can't eat into
# the gate sandboxes' share of the ~8Gi VM (a single gate already wants up to 4Gi).
if [ -n "${OPENOBSERVE:-}" ]; then
  command -v curl >/dev/null || { echo "error: curl not found (OPENOBSERVE=1 needs it to health-check + provision the dashboard)"; exit 1; }
  say "Starting OpenObserve (OTLP traces+logs+metrics + UI) — http://127.0.0.1:5080"
  docker rm -f "$OPENOBSERVE_NAME" >/dev/null 2>&1 || true   # clear any stale container from a prior run
  docker run -d --rm --name "$OPENOBSERVE_NAME" \
    --memory=1g \
    -e ZO_ROOT_USER_EMAIL="$OO_EMAIL" \
    -e ZO_ROOT_USER_PASSWORD="$OO_PASSWORD" \
    -p 5080:5080 -p 5081:5081 \
    "$OPENOBSERVE_IMAGE" >/dev/null

  # OO's ingestion credential IS base64(email:password) — exactly the string its own Ingestion
  # page shows. Derive it locally (offline, no API round-trip) and export it as the env var the
  # materialized overlay's `authorization: ${OTEL_OTLP_AUTH}` header references; the harness
  # expands it host-side (config.OTelConfig.ResolveHeaders) when it builds the exporter, so the
  # secret lives in the environment, never in config — the same key discipline as the model key.
  OO_TOKEN="$(printf '%s:%s' "$OO_EMAIL" "$OO_PASSWORD" | base64 | tr -d '\n')"
  export OTEL_OTLP_AUTH="Basic $OO_TOKEN"

  say "Waiting for OpenObserve to be healthy"
  oo_ready=
  for _ in $(seq 1 60); do
    if curl -fsS "http://127.0.0.1:5080/healthz" >/dev/null 2>&1; then oo_ready=1; break; fi
    sleep 1
  done
  [ -n "$oo_ready" ] || { echo "error: OpenObserve did not become healthy on :5080"; docker logs "$OPENOBSERVE_NAME" 2>&1 | tail -n 30; exit 1; }

  # Provision the completeness overview dashboard (traces + logs + metrics panels) via OO's REST
  # API. BEST-EFFORT: the JSON is pinned to OO's dashboard v5 schema, which evolves with the
  # product, so a mismatch (e.g. a bumped OPENOBSERVE_IMAGE) must NOT abort the demo — telemetry
  # still lands, and the file can be imported by hand from the UI. JSON: demo/vault/observe/.
  if curl -fsS -X POST "http://127.0.0.1:5080/api/$OO_ORG/dashboards?folder=default" \
       -H "Authorization: Basic $OO_TOKEN" -H 'Content-Type: application/json' \
       --data-binary @"$DEMO_DIR/observe/completeness-dashboard.json" >/dev/null 2>&1; then
    say "Provisioned the 'completeness overview' dashboard (incl. the 'Pipeline — log records' table)"
  else
    say "Dashboard auto-provision skipped (OO API/schema may differ for $OPENOBSERVE_IMAGE) — telemetry still lands; import demo/vault/observe/completeness-dashboard.json from the UI"
  fi

  # Provision the 'Pipeline' logs Saved View: opens the Logs explorer straight into columns
  # (issue / harness_issue_id / role / soul / attempt / body) instead of raw JSON. Same
  # best-effort discipline as the dashboard — a saved view is an opaque OO frontend-state blob,
  # so a bumped OPENOBSERVE_IMAGE could drift its shape; a mismatch must not abort the demo (you
  # add the columns by hand in Logs -> Saved Views). The dashboard's table panel COALESCEs the
  # two issue/role key conventions into one column; the raw Logs explorer can't, so the saved
  # view carries both columns and each row populates whichever its emitter used. JSON: observe/.
  if curl -fsS -X POST "http://127.0.0.1:5080/api/$OO_ORG/savedviews" \
       -H "Authorization: Basic $OO_TOKEN" -H 'Content-Type: application/json' \
       --data-binary @"$DEMO_DIR/observe/pipeline-savedview.json" >/dev/null 2>&1; then
    say "Provisioned the 'Pipeline' logs saved view (Logs -> Saved Views -> Pipeline)"
  else
    say "Saved-view auto-provision skipped (OO API/schema may differ for $OPENOBSERVE_IMAGE) — add the columns by hand in Logs -> Saved Views, or import demo/vault/observe/pipeline-savedview.json"
  fi
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
[ -z "${OPENOBSERVE:-}" ] || echo "    telemetry    : open http://127.0.0.1:5080 (login $OO_EMAIL / $OO_PASSWORD) — traces, logs & metrics in the 'completeness overview' dashboard"
echo
"$HARNESS" run --config "$CONFIG_DIR" --repo "$SITE" --bd "$BD" --serve-addr "$SERVE_ADDR"
