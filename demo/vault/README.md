# Demo: extending an established app, live

This demo shows the harness doing its real job on a **non-trivial, already-built
codebase**: a human authors a *new feature requirement* in the control-room wizard, and
sandboxed, independently-verified agents plan it, write failing tests, implement it, pass
an independent security re-gate, and merge it to `main` — driven by a **hosted model** via
OpenRouter, so all you need is an API key.

The target repo is an **established secrets vault**: a Go + templ + htmx + Tailwind +
SQLite app with master-password auth (Argon2id), secrets encrypted at rest (AES-256-GCM),
reveal-on-demand, search, an append-only audit log, and a dashboard. It ships **green** —
tests pass and `gosec`/`govulncheck`/`license-scan` are clean — so the agents always extend
a clean tree.

Why a vault for a security audience: the app is security-sensitive *by construction*, so
the `qa` stage's **gosec + govulncheck** are re-auditing the agents' crypto and auth code
on every change. The harness assumes the agents are hostile and sandboxes them with **zero
network**; demoing that by having untrusted agents extend a *secrets vault* is the thesis
made concrete — *the producer never grades its own work, and an independent security gate
re-audits the diff before anything merges.*

```
requirements   you draft a feature in the Create-Task wizard (/create) — the one human step
  └─ plan        the planner decomposes it into child work items (request_subtask)
       └─ author-tests   a soul writes FAILING Go tests from the spec (proven RED)
            └─ implement   a *different* soul makes them pass (proven RED→GREEN, clean sandbox)
                 └─ qa       a *different* soul re-runs tests + lint + gosec + govulncheck + license-scan
                      └─ integrate   the orchestrator merges to main (autonomous; no approval step)
```

## What's here

```
demo/vault/
  run.sh                  # turnkey: build → images → scaffold scratch repo → run (no seed; you use the wizard)
  Dockerfile              # vault-toolchain sandbox image (FROM go-toolchain: + templ + Tailwind + vault module cache)
  config/
    harness.yaml          # FULL DAG (requirements→plan→author-tests→implement→qa→integrate + resolve), Go/security gate
    infra.dev.yaml        # hosted model via OpenRouter; vault-toolchain sandbox profile
    souls/                # planner, test-author, implementor, security, merge-resolver (+ prompts/)
  app/                    # the ESTABLISHED vault app — its own Go module; copied into the scratch repo
    cmd/ internal/ specs/ Makefile .golangci.yml ...
```

It is a self-contained config, separate from the harness's own `config/`, so it can't
disturb the real pipeline. The target repo is a throwaway created in a temp dir from
`app/` — nothing is written into this repository.

## Prerequisites

- **Docker** running (the sandbox backend).
- An **OpenRouter API key** in `OPENAI_API_KEY` (the openai-compat adapter and the
  requirements-planner send it as the bearer token). The autonomous souls default to
  `deepseek/deepseek-v4-flash`; override with `MODEL=` (must support function calling — the agent
  loop drives the model through structured tool calls). A full-DAG run on real Go is
  demanding, so prefer a capable model. The **requirements-planner** (the Create-Task
  wizard) is pinned separately to the stronger `deepseek/deepseek-v4-pro` and is *not*
  affected by `MODEL=`: it is the one human-in-the-loop conversation and must reliably
  follow the `ledger`/`draft` output protocol that drives the alignment ledger UI, which
  the cheaper flash tier does not do dependably. To change it, edit `requirements_planner.model`
  in `config/harness.yaml`.
- **beads** (`bd`) on your `PATH` (or pass `BD=/path/to/bd`).
- Go + `make` (to build the `harness` binary).

On first run the `go-toolchain` base image and then the `vault-toolchain` image are built
automatically if missing. The base build downloads the Go image + the offline vuln DB and
is slow; the vault layer on top is quick.

## Run it

```bash
export OPENAI_API_KEY='sk-or-...'   # your OpenRouter key
./demo/vault/run.sh
```

Then open the control room at <http://127.0.0.1:8080>.

### Draft the feature (the live part)

1. Go to **`/create`** (the Create-Task wizard). Describe the feature you want. The
   requirements-planner converses with you, surfaces decisions in the **alignment ledger**,
   and once intent has converged it drafts a **spec + seed issue(s)**.
2. Click **Approve**. That consent gate seeds a `plan` issue; everything after is autonomous.
3. Watch the **Board** and **Activity** views as the planner fans the work out and each
   child flows author-tests → implement → qa → integrate. When it lands, `git -C <scratch>
   log` shows the provenance trailer and the diff is your feature.

### Watch the telemetry land (optional)

The harness emits OpenTelemetry spans at the broker, orchestrator, and runner — every
invocation is one trace (`invocation → boot → llm-turn ×N → tool-call ×M → gate-run`; see
`specs/observability.md`). To show that live, run with `JAEGER=1`:

```bash
JAEGER=1 ./demo/vault/run.sh
```

This spins a single **Jaeger all-in-one** container (insecure OTLP/gRPC on `4317`, trace UI
on `16686`) and points the demo's `otel.endpoint` at it — no change to the tracked config.
Open <http://127.0.0.1:16686>, pick the **`harness`** service, and each invocation shows up
as a trace waterfall as the pipeline runs. The container is torn down on exit. (The default
run leaves tracing off, as `infra.dev.yaml` ships.)

### A good feature to draft on stage

A **one-time, single-use secret share link**: generate an expiring link that reveals one
secret exactly once, then burns. It is a clean vertical slice (a token table/migration, a
generate handler, a public single-use reveal endpoint, an audit entry, a small UI affordance)
that the planner decomposes nicely — and it is **security-forward**: tokens from
`crypto/rand`, constant-time comparison, expiry/burn semantics, so the `qa` gate's gosec and
govulncheck are doing real work on the agents' output. (The base deliberately ships *without*
this feature; it has the seams — see `app/specs/`.) Other natural options: secret expiry +
rotation reminders, an audit-log viewer with anomaly flagging, or reused-secret detection.

## What to expect

- **It costs a little.** Real API calls across the full DAG; bound by `policy.budget`
  (tokens/USD/wall) in `config/harness.yaml` and the `cost` table in
  `config/infra.dev.yaml`. A flailing run dead-letters rather than running forever.
- **If a stage dead-letters**, the control room's Dead-letter view shows the reason. The
  one human lever is the **spec**: refine the requirement (re-run the wizard) — never the
  agent's code. On slow hardware, raise `limits.wall` (per-invocation) and/or
  `policy.budget.wall` (cumulative).
- **Merge conflicts are handled.** Sibling work items of one feature often touch the same
  files; when a verified candidate can't be cleanly rebased, the `resolve` stage rebases it
  and the result is independently re-gated before it lands.

## How it maps to the real config

This demo runs the **same full DAG** as the shipped `config/harness.yaml`, with two
deliberate tunings for a live run (neither changes the architecture):

| Shipped | Demo | Why |
|---|---|---|
| `qa`/`resolve` include `mutation>=0.8` | dropped | gremlins mutation testing is slow and flaky live; the security-relevant scanners (gosec/govulncheck/license-scan) + lint + the red→green proof carry the story |
| `trusted-dev` profile + `human-approved` + `tcb_paths` | `autonomous`, no TCB globs | the diff is ordinary app code in a throwaway repo, so integrate merges hands-free — flip to `trusted-dev` (+ a `human-approved` postcondition on integrate) to demo the approval gate |

Everything else — the zero-network sandbox, broker-mediated model calls, decomposition,
the red→green producer ≠ verifier proofs, the independent security re-gate, single-writer
beads, the merge queue + conflict resolution, provenance trailers, budgets as the
termination guarantee — is exactly the real machinery, now exercised on a real codebase.
