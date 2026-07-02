# Demo: extending an established app, live

This demo shows the harness doing its real job on a **non-trivial, already-built
codebase**: a human authors a *new feature requirement* in the control-room wizard, and
sandboxed, independently-verified agents plan it, write failing tests, implement it, pass
an independent security re-gate, and merge it to `main` — driven by **Anthropic models via
OpenRouter** (Opus for the roles that define correctness, Sonnet for the rest), so all you need
is an OpenRouter API key.

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
                      └─ integrate   each verified child lands on the epic/<id> branch (autonomous; no approval)
                           └─ land    when every child has drained, one terminal merge advances `main` ONCE
                                └─ deploy   that single push to the public GitHub repo → CI deploys it to your VPS
```

This runs in **epic mode** (`integration.mode: epic`): the feature you drafted is the unit of
integration, so its children integrate onto an `epic/<id>` branch while `main` stays quiescent,
and the orchestrator's **terminal merge advances `main` exactly once** when the whole subtree
drains. That single commit is authored by **`harness`** (the trusted layer, never the agent)
with its provenance trailer, and its subject is the **feature's** title — so the public repo's
history reads like an ordinary project's (a machine author, a full audit trail) and the push
watcher fires **one** deploy per feature, not one per child. See "Push to a public repo +
deploy" below.

## What's here

```
demo/vault/
  run.sh                  # turnkey: build → images → scaffold scratch repo → run (no seed; you use the wizard)
  Dockerfile              # vault-toolchain sandbox image (FROM go-toolchain: + templ + Tailwind + vault module cache)
  config/
    harness.yaml          # FULL DAG (requirements→plan→author-tests→implement→qa→integrate + resolve), Go/security gate
    infra.dev.yaml        # Anthropic models (Opus + Sonnet) via OpenRouter; vault-toolchain sandbox profile
    souls/                # planner, test-author, implementor, security, merge-resolver (+ prompts/)
  app/                    # the ESTABLISHED vault app — its own Go module; copied into the scratch repo
    cmd/ internal/ specs/ Makefile .golangci.yml ...
    README.md             # the PUBLIC repo's front page (realistic vault README + an honest demo banner)
    .github/workflows/deploy.yml  # CI: on push to main, build a static binary and ship it to the VPS
```

It is a self-contained config, separate from the harness's own `config/`, so it can't
disturb the real pipeline. The target repo is a throwaway created in a temp dir from
`app/` — nothing is written into this repository.

## Prerequisites

- **Docker** running (the sandbox backend).
- An **OpenRouter API key** in `OPENAI_API_KEY` (the openai-compat adapter sends it as the
  bearer token; the runner holds it host-side, never in config, and the sandbox stays
  zero-network). Every role runs on an Anthropic model **via OpenRouter** in two tiers, set in
  `config/infra.dev.yaml`:
  - **`anthropic/claude-opus-4.8`** for the two roles that define *what correct means* and gate
    everything below them — the **decomposition planner** (the `plan` stage; a wrong split
    wastes every soul below it) and the **test-author** (the `author-tests` stage; its tests
    are the spec made executable, the contract the factory trusts in place of a human and the
    ceiling on what the implementor can build). Set in `config/souls/planner.yaml` and
    `config/souls/test-author.yaml`.
  - **`anthropic/claude-sonnet-5`** for the rest — the **requirements-planner** (the
    Create-Task wizard; interactive, so latency matters, and strong tool-protocol following
    keeps the alignment ledger reliable), the **implementor**, the **security/qa** verifier, and
    the **merge-resolver**. This tier runs at `effort: medium` (sent as OpenRouter `verbosity`,
    the field Claude 4.6+/5 map to `output_config.effort`) to trim deliberation and cost; the
    Opus roles stay at their default. `MODEL=` swaps this shared Sonnet slug across those roles
    (an OpenRouter slug) without touching the pinned Opus roles. The wizard's model is
    `requirements_planner.model` in `config/harness.yaml`.
  - *(If you switch to first-party Anthropic — `provider: anthropic`, an `ANTHROPIC_API_KEY` —
    each model entry can take an optional `effort:` (`low`|`medium`|`high`|`xhigh`|`max`), the
    `output_config.effort` intelligence↔latency↔cost dial. It's not wired on the openai-compat
    /OpenRouter path, so the demo as shipped runs at the models' default effort.)*
- **beads** (`bd`) on your `PATH` (or pass `BD=/path/to/bd`), and **dolt** on your `PATH`
  (`brew install dolt`). The demo runs beads in **server mode**: `run.sh` has `bd init`
  auto-start a persistent per-run `dolt sql-server` (data under the scratch repo's
  `.beads/dolt/`, stopped and torn down on exit) so the constant orchestrator/control-room
  polling hits a *warm* engine instead of cold-starting Dolt on every `bd` call — a list drops
  from ~0.7s to ~0.2s and stops stampeding under concurrency, which is what prevents `bd list`
  timeouts during a busy run. Nothing leaks into the public repo (`.beads/` stays git-excluded).
- Go + `make` (to build the `harness` binary).
- *(only for `OPENOBSERVE=1`)* **`curl`** on your `PATH` — `run.sh` uses it to health-check
  OpenObserve and POST the dashboard. Not needed for a default or `JAEGER=1` run.
- *(optional, for the public-repo + deploy story)* a **public GitHub repo** you can push to
  (default `git@github.com:Loxstomper/vault.git`; override with `VAULT_REMOTE=`) and a **VPS**
  for the deploy. Run with `VAULT_REMOTE=''` to stay purely local — the full pipeline still
  runs and merges, it just doesn't push or deploy.

On every run the `go-toolchain` base image and then the `vault-toolchain` image are
(re)built automatically. Docker's layer cache makes an unchanged rebuild near-instant, so
building unconditionally costs almost nothing while guaranteeing a stale image (one built
before its Dockerfile gained the gate tools) is refreshed rather than silently reused. Only
the first base build is slow — it downloads the Go image + the offline vuln DB; the vault
layer on top is quick.

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
   child flows author-tests → implement → qa → integrate onto the epic branch. The board's
   **epic hero card** rolls up the feature's progress (integrated X/Y, spend vs the epic
   budget). When the last child drains, the terminal merge lands the whole feature on `main`
   in **one** commit; `git -C <scratch> log` shows that single provenance trailer (citing the
   epic id) and the diff is your feature.

### Push to a public repo + deploy

This is the "no smoke and mirrors" surface: the audience inspects a **real public GitHub
repo** and watches a feature they didn't write appear on it. Set `VAULT_REMOTE` to that repo
(it defaults to `git@github.com:Loxstomper/vault.git`; the host running `run.sh` needs push
access — a deploy key or SSH key).

- **At startup**, `run.sh` resets the public repo to the green baseline: it force-pushes the
  seed to `main` and to an immutable `seed` ref. Every run starts from an identical pristine
  state, so the demo is repeatable on stage.
- **On the terminal merge**, a watcher pushes the harness's verified, machine-authored
  commit to the public `main`. In epic mode `main` advances exactly once per feature (when the
  epic drains), so the watcher pushes once and the deploy fires once — for the whole feature,
  not each child. This is a *trusted-layer egress* — the sandboxed agents never touch the
  network; only this post-merge push of an already-gate-verified commit does.
- **The push fires the deploy.** [`app/.github/workflows/deploy.yml`](app/.github/workflows/deploy.yml)
  builds a single static binary (pure-Go SQLite + embedded assets — no runtime deps) and
  ships it to your VPS over SSH, where it runs as a `vault` systemd service behind Caddy at
  `https://vault.lochie.dev`. The one-time VPS setup (Reserved IP, DNS, SSH hardening, deploy
  user, Caddy, systemd) is the runbook in [`DEPLOY.md`](DEPLOY.md) — do it once and snapshot.
  The full loop on stage: **describe → agents build → commit appears on GitHub (machine author
  + provenance) → Actions go green → refresh the live URL → the feature is there.**

The work-store prefix is `vault`, so issue ids read `vault-12` (not `harness-12`) in the
commit trailer, and the harness work store (`.beads`) is kept out of git via the scratch
repo's `.git/info/exclude` — so the public repo only ever shows the vault and the feature.

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

Two things to expect:

- **`failed to upload metrics: ... unknown service ...MetricsService`** in the harness log is
  benign. The harness exports both traces and metrics to the one OTLP endpoint; Jaeger is a
  *tracing* backend (its OTLP receiver implements traces only), so the periodic metrics push
  is refused. Traces are unaffected — they still flow and render in the UI.
- **The Jaeger UI looks wrong in dark mode.** Jaeger v1's UI is light-only with no theme
  toggle; what you see is the *browser* auto-darkening a light-only site (Chrome's "Auto Dark
  Mode" flag, or a Dark-Reader-style extension). Disable auto-dark for `127.0.0.1:16686` to
  get the intended UI — there is no demo-side or server-side lever for this.

### See the WHOLE record — all three signals (optional)

Jaeger shows traces. But the harness ships **three** OTel signals off one endpoint — traces,
**logs** (the trusted side's `slog`, trace-correlated), and **metrics** (token spend, gate
pass/fail, invocations) — and for a security audience the point is that the *complete,
tamper-evident record* lands in a real backend, authenticated. Jaeger is trace-only and refuses
the rest; **OpenObserve** is a single binary that ingests all three. Run with `OPENOBSERVE=1`:

```bash
OPENOBSERVE=1 ./demo/vault/run.sh
```

This spins one **OpenObserve** container (authenticated OTLP/gRPC on `5081` — all three signals
ride that one port — and the UI/REST API on `5080`), points the demo's `otel.endpoint` at it
with the org/stream/auth headers an authenticated backend needs, and auto-provisions a
four-panel **completeness overview** dashboard (one chart per signal, plus a **Pipeline — log
records** table that columnates every slog record) and a **Pipeline** logs *saved view* (Logs →
Saved Views) that opens the explorer straight into issue/role/soul/event columns. Open
<http://127.0.0.1:5080>, log in as `admin@admin.com` / `admin` (OpenObserve logs in by
email, so the username is the full address), and watch traces,
logs, and metrics arrive together as the pipeline runs. The container is **ephemeral** (`--rm`,
no volume, `--memory`-capped so it doesn't crowd the gate sandboxes) — all data dies with it on
exit, by design. `JAEGER=1` and `OPENOBSERVE=1` are mutually exclusive (one `otel.endpoint`).

How the auth stays clean: OpenObserve's ingestion token is just `base64(email:password)`;
`run.sh` derives it locally and exports it as `OTEL_OTLP_AUTH`, which the materialized overlay
references as `authorization: ${OTEL_OTLP_AUTH}` — the harness expands it host-side at export
time, so the credential lives in the environment, never in config (the same discipline as the
model API key, and what `harness validate`'s credential-header rule enforces). The dashboard
JSON and provisioning details live in [`observe/`](observe/); it is pinned to OpenObserve's
dashboard v5 schema and POSTed **best-effort** — if a bumped `OPENOBSERVE_IMAGE` drifts the
schema, the demo still runs and you import the JSON from the UI.

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

This demo runs the **same full DAG** as the shipped `config/harness.yaml`, with three
deliberate tunings for a live run (none changes the architecture — epic mode just retargets
the same merge queue per feature):

| Shipped | Demo | Why |
|---|---|---|
| `qa`/`resolve` include `mutation>=0.8` | dropped | gremlins mutation testing is slow and flaky live; the security-relevant scanners (gosec/govulncheck/license-scan) + lint + the red→green proof carry the story |
| `trusted-dev` profile + `human-approved` + `tcb_paths` | `autonomous`, no TCB globs | the diff is ordinary app code in a throwaway repo, so integrate merges hands-free — flip to `trusted-dev` (+ a `human-approved` postcondition on integrate) to demo the approval gate |
| per-item integration (no `integration` block) | `integration.mode: epic` | the operator drafts a *feature*, so the whole thing lands as **one** commit on `main` — children integrate onto an `epic/<id>` branch and the terminal merge advances `main` once when the subtree drains, so the public-repo push + VPS deploy fires once per feature, not per child. v1 runs one epic at a time |
| `author-tests` postcondition `[tests-red]` only | `[tests-red, compiles]` (+ a `compiles: make compile` check) | Go is compiled and statically typed, so a test referencing a not-yet-defined symbol fails to *compile* — also a nonzero exit, so `tests-red` (tests-pass must FAIL) alone would pass on a suite that never ran an assertion (a *vacuous* red). Pairing it with `compiles` (`go build ./... && go test -run='^$' ./...` — builds the test binaries, runs nothing) makes "red" mean **compiles AND tests-fail**, and forces the test author to commit the minimal compiling API skeleton the implementor inherits as a compiler-checked contract. The shipped `config/harness.yaml` leaves `tests-red` unpaired (language-neutral kernel; a compiled target opts in). See specs/verification.md "Tests-red proof" |
| no `ambient_specs` (the harness has no `specs/conventions.md`) | `ambient_specs: [specs/README.md, specs/conventions.md]` | the vault's engineering conventions (parameterized-SQL-only, `crypto/rand`, encryption-non-negotiable, `make generate` after a `.templ` edit, **no new modules** under the zero-network sandbox) are the same for every change and live in [`app/specs/conventions.md`](app/specs/conventions.md), so they ride in **every** agent's Brief — the agent never has to rediscover them or trip one and dead-letter. `app/specs/README.md` is a thin index the agent `read_file`s the rest from (T3.14) |

Everything else — the zero-network sandbox, broker-mediated model calls, decomposition,
the red→green producer ≠ verifier proofs, the independent security re-gate, single-writer
beads, the merge queue + conflict resolution, provenance trailers, budgets as the
termination guarantee — is exactly the real machinery, now exercised on a real codebase.
