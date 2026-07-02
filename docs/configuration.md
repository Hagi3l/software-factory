# Configuration

The harness is entirely config-driven: the workflow, the souls, and the infrastructure
are declarative and validated before anything runs (`harness validate`). This is the
practical guide; the authoritative contract is
[`specs/configuration.md`](../specs/configuration.md).

API keys are **never** in config — they come from the environment
(`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`). Config holds everything else, including model
*prices* (not secret).

## The `config/` directory

```
config/
├── harness.yaml            # the DAG, check registry, and policy — environment-independent
├── infra.<env>.yaml        # infrastructure overlay per environment (sandbox, NATS, models, …)
└── souls/
    ├── <name>.yaml         # one soul: identity + role + model + persona + sandbox + selector
    └── prompts/
        └── <name>.md       # the persona prompt a soul boots with
```

`harness validate --config config --env dev` loads `harness.yaml`, the souls, and
`infra.dev.yaml`, and gates startup on any breaking fault. Change `--env` to load a
different overlay.

## `harness.yaml` — the pipeline

This is *what the factory does*, independent of where it runs. Key sections:

### `dag` — the stages

A map of stage name → stage definition. Each stage has a `kind` and, for agent stages,
a `role`, pre/postconditions, an `on_failure` target, and `produces` edges. See
[the pipeline](pipeline.md) for the shipped shape. Stage kinds:

- `human` — a person acts (the `requirements` stage).
- `plan` — decomposition; the planner proposes child issues (no candidate, no gate).
- *(default)* — a sandboxed agent stage that produces a candidate and is gated by its
  `postcondition`.
- `trusted-merge` — the orchestrator merges inline; never dispatched to a runner.
- `resolve` — merge-conflict resolution; spawned out-of-band, gated like qa.

**Stage ≠ role.** A stage names a *role*; the role resolves to one or more souls via
each soul's `selector`. `produces:` is the declarative "what comes next"; the agent
decides how many items to emit within a stage.

### `checks` — the gate command registry

Each command-check postcondition (e.g. `gosec`, `tests-pass`) maps to the shell command
the gate runs in the clean verification sandbox — exit 0 = pass, non-zero = fail
(closed). The shipped checks are the harness's own make targets, so "how QA runs" lives
in one reviewable place:

```yaml
checks:
  tests-pass:    make test-unit
  golangci-lint: make lint
  gosec:         make gosec
  govulncheck:   make govulncheck
  license-scan:  make license-scan
  mutation:      make mutation

independent_checks: [golangci-lint, gosec, govulncheck, license-scan]
```

A metric comparison like `mutation>=0.8` resolves the command registered under its
metric name and grades the trailing numeric token of stdout. The reserved proofs
`tests-red` and `tests-red-then-green` have no command of their own — they reuse
`tests-pass` run against the candidate and/or its base, so the acceptance tests stay a
single source of truth.

Beyond grading the exit code, the gate parses a check's output into structured
**[findings](../specs/verification.md)** — the compact `file:line severity rule: message`
form shown in the [verification view](control-room.md) and threaded into a failed
candidate's retry Brief, instead of the raw dump. The parsing is built-in per-tool infra:
the kernel ships adapters for `go test`, `golangci-lint`, `gosec`, and `govulncheck`,
**selected by the check's name** (so the registry stays a plain name→command map). For the
findings to populate, point the check at the tool's **machine-readable mode** — e.g.
`go test -json`, `golangci-lint --output.json`, `gosec -fmt=json`, `govulncheck -json` (the
exit code, and so the pass/fail grade, is unchanged). A check whose command emits a plain
human summary, or whose tool has no adapter, simply carries no findings — it still grades on
its exit code with its raw output kept as evidence (a graceful fallback, never an error).

`independent_checks` is the optional list of command checks the gate keeps running **past**
a failure, so one `qa` pass surfaces *every* scanner finding at once instead of stopping at
the first — the human triaging the dead-letter queue (or the agent re-routed to `implement`)
fixes them all in one round-trip rather than bouncing the candidate once per scanner. Only
spec-independent scanners belong here; the proofs (`tests-pass`) and the metric (`mutation`)
stay fail-fast — a mutation score on a candidate whose tests are red is meaningless — and
`harness validate` rejects a reserved proof or a metric command in the list. Each name must be
a key in `checks:`; omit the list to keep everything fail-fast. Aggregation never changes the
verdict (a candidate that trips any check still fails the gate), only how much one failing
pass reports. See [verification.md](../specs/verification.md).

`golangci-lint` is an ordinary command check (graded on exit code) on the `qa` and
`resolve` gates. It is the *same* `make lint` the implementor runs in-loop for fast
feedback before it submits — one command, run by the agent for speed and re-run by the
gate for trust (a producer self-check earns no trust; only the gate's independent re-run
advances the transition — see [verification.md](../specs/verification.md)).

### `policy` — termination & autonomy

```yaml
policy:
  max_retries: 3
  budget:        { tokens: 2_000_000, usd: 20, wall: 2h } # per issue, across the retry loop
  epic_budget:   { usd: 200 }                             # aggregate across an epic
  explore_budget: { tokens: 100_000, turns: 12 }          # per explore call; omit to disable explore
  dead_letter: harness.dlq
  profile:     trusted-dev                                # or: autonomous
  tcb_paths:                                              # the Trusted Computing Base boundary
    - internal/orchestrator/**
    - internal/runner/**
    - internal/broker/**
    - internal/sandbox/**
    - internal/gate/**
    - internal/messaging/**
    - config/**
```

- **`max_retries` + `budget` + `epic_budget`** are the termination guarantee. Any
  zero/omitted budget dimension is uncapped. Per-issue budget covers the whole
  `on_failure` loop (the `wall` here is the *cross-loop* sum, distinct from the
  per-invocation sandbox ceiling in the infra overlay).
- **`profile`** — `trusted-dev` requires human approval on *every* integrate;
  `autonomous` requires it only for diffs touching `tcb_paths`. See
  [the pipeline](pipeline.md#the-approval-gate-trusted-dev).
- **`tcb_paths`** — the operational definition of "which modules are the TCB" (the
  trust-enforcing core). A candidate diff touching any glob always needs human review.
- **`explore_budget`** — the fixed per-call cap (tokens and/or turns) on the
  [`explore` tool](../specs/components/agent.md#explore--distilled-comprehension)'s nested
  read-only comprehension sub-loop. It is the feature switch: **omitting it disables `explore`**
  (no cap = no helper loop). When set, at least one soul on the reserved `explorer` role must
  exist (see below), and the runner meters each explore call against this cap, harvesting a
  `partial-budget` answer on a breach rather than failing the parent task.

### `spec_depth` — the context horizon

```yaml
spec_depth: 1
```

How many cross-link hops of the spec tree each agent receives: `1` = the issue's
referenced spec file plus its direct neighbours. Bounds the agent's context to the
relevant contract rather than the whole `specs/` tree.

### `ambient_specs` — always-injected conventions + index

```yaml
ambient_specs: [specs/README.md, specs/conventions.md]   # opt-in; empty by default
```

Repo-relative spec files injected into **every** agent's Brief, regardless of the issue.
The issue-scoped slice (`spec_depth`) carries the *contract*; `ambient_specs` carries the
project's cross-cutting **conventions and gotchas** (parameterized-SQL-only, `crypto/rand`,
"run `make generate` after a `.templ` edit", "no new modules — the sandbox is zero-network")
and its spec **index**, which are the same for every issue. Without them a stateless agent
rediscovers conventions every run or trips one and dead-letters; reaching them via cross-links
is fragile (a conventions file beyond `spec_depth` falls out of the slice, and raising
`spec_depth` to reach it taxes *every* slice), so they are delivered out-of-band instead — which
also lets `spec_depth` stay low.

The conventional pair is the `specs/README.md` index (a thin hub of pointers — the agent
`read_file`s any other spec on demand) and a `specs/conventions.md`. The files are **prepended
ahead of** the bounded slice (a stable, cache-friendly prefix the model layer's prompt cache
reuses), **de-duplicated** against it (an ambient file that is also the governing spec or a
neighbour is injected once), and **covered by the spec content hash** — so editing a conventions
file is pinned in provenance and re-derives in-flight/merged work exactly like a contract edit.
Keep them **lean**: they ride in every invocation.

Opt-in (empty by default). `harness validate` rejects an absolute path, a `../` escape outside
the repo, or a duplicate entry; file *existence* is resolved best-effort at dispatch (a missing
ambient file is logged and omitted, never wedges a run). See
[specs-process.md](../specs/specs-process.md#ambient-specs).

### `integration` — how work reaches `main`

```yaml
integration:
  mode: per-item     # per-item (default) | epic
```

`mode` selects how verified work becomes commits on `main` (see
[integration.md](../specs/integration.md)):

- **`per-item`** (default — the kernel behaviour): each work item lands on `main` as its own
  chain verifies.
- **`epic`**: a whole feature lands **atomically** — children integrate onto an
  `epic/<epic_id>` branch and `main` advances exactly once, by the epic's terminal merge, when
  the epic's subtree drains. v1 runs **one epic at a time**.

Omitting the block defaults to `per-item`. `harness validate` rejects any `mode` outside
`{per-item, epic}`.

### `requirements_planner` — the wizard's model

```yaml
requirements_planner:
  model:           claude-opus-4-8
  persona:         souls/prompts/requirements-planner.md
  max_tokens:      16384           # optional — per-reply output ceiling (not the context window)
  max_tool_turns:  40              # optional — read-only exploration round-trips per human turn
  turn_timeout:    10m             # optional — wall-clock budget for one human turn
  sandbox_profile: go-toolchain    # optional — enables read-only codebase exploration
  base_ref:        main            # optional — branch the read-only checkout is seeded at
```

The trusted LLM behind the Create-Task wizard — the one place a human is in the loop. Its
conversation runs host-side (the model layer directly, not the runner/broker); its `model`
resolves through the infra `models` registry like a soul's. Omitting this whole block disables
the wizard.

`max_tokens` (optional) bounds **one reply's output** — distinct from the model's input context
window; size it to fit prose + the full alignment ledger + a complete draft (which inlines the
spec markdown) without truncating, since a half-emitted ledger/draft block is dropped. `0` (or
omitted) uses the adapter default. `max_tool_turns` (optional) caps the read-only exploration
round-trips within a single human turn (default 16); raise it for a model that should explore a
large codebase deeply. `turn_timeout` (optional, a Go duration like `10m`; default 5m) bounds one
human turn's wall-clock — **raise it alongside `max_tool_turns`**, or a deep exploration the turn
cap would allow is instead cut short by the clock and surfaces as an error to the human.

`sandbox_profile` (optional) enables **read-only codebase exploration**: when set to a profile
from the infra `sandbox.profiles` registry, the planner provisions a read-only, zero-network
sandbox over the integration repo and gains the agent's read tools
(`read_file`/`list_dir`/`search` + the LSP comprehension tools), so it grounds specs and seed
issues in the real code. Use a profile whose image carries the language server (the same one
the souls use) for precise semantic results; otherwise they degrade to text search. `base_ref`
(optional) is the branch the read-only checkout is seeded at, defaulting to the repo's current
branch. Omit `sandbox_profile` to keep the planner a pure conversation. Exploration is
read-only; the planner still writes nothing but the consent-gated spec + seed issues.

## A soul

```yaml
# config/souls/implementor-go.yaml
name:     implementor-go
role:     implementor          # the DAG role it binds to
model:    claude-opus-4-8      # resolved through infra.<env>.yaml's models registry
persona:  souls/prompts/implementor-go.md
tools:    [fs, shell, git]
sandbox:  go-toolchain         # logical profile; resolved to a concrete image via infra
selector: { lang: go }         # which work this soul claims for its role
max_tool_turns: 35             # optional — per-invocation ReAct turn cap (0/unset = kernel default)
```

A **soul** is identity + persona + model + tools + sandbox. It is *stateless* — it
carries no cross-task memory; all durable state lives in beads, git, and the specs. A
role can map to several souls; the `selector` picks which soul claims a given work
item.

`max_tool_turns` (optional) caps how many ReAct turns one invocation of this soul may take
before the loop gives up and returns `failed` (which routes to `on_failure`). Unset or `0`
uses the kernel default. Lower it for a soul prone to flailing — e.g. a test-author that fills
turns with tiny tool calls without ever submitting — so a doomed attempt fails fast and retries
(or dead-letters) instead of burning the full default budget; the gate still independently
verifies the result, so a tight cap trades only a stuck attempt's wasted time. It is the
sandboxed-soul analog of the wizard's `requirements_planner.max_tool_turns`.

> **The reserved `explorer` role.** A soul on role `explorer` fulfills the
> [`explore` tool](../specs/components/agent.md#explore--distilled-comprehension), not a DAG
> stage — the explorer is invoked as a tool by the runner, never scheduled — so it is exempt
> from the "every soul's role backs a stage" check. It is otherwise an ordinary soul (a cheap
> `model`, an explore `persona`, a `selector`), but its `tools` must be a subset of the
> read-only comprehension tools (`find_symbol`, `references`, `definition`, `implementation`,
> `hover`, `diagnostics`, `read_file`, `list_dir`, `search`) and must **never** include
> `explore` (no recursion) — `harness validate` rejects a writer or `explore` in an explorer's
> allowlist. Enable the feature with `policy.explore_budget`; when it is set, at least one
> `explorer` soul must exist. A second explorer on a different model family (routed by a
> `selector` tag) is the recommended way to keep the verify path's blind spots from correlating
> with the producer's.

> **Model diversity** (`producer ≠ verifier` strengthened): the harness *enables and
> recommends* running the verifier on a different model family than the producer, but
> the assignment is yours — it's a config capability, not a built-in mechanism.
> `harness validate` emits a non-fatal **warning** if a producer and its verifier share
> a model family. Family is the **trainer org**, not the provider adapter: a model served
> through an aggregating gateway (e.g. OpenRouter — every model is one `openai-compat`
> entry) is identified by the `vendor/` prefix of its slug (`deepseek/deepseek-v4-pro` →
> `deepseek`), so two genuinely different vendors behind one gateway are *not* falsely
> flagged, and two tiers of the same vendor (deepseek flash vs pro) still are. For a
> bare-slug `openai-compat` model with no vendor in its key, set `family:` on the registry
> entry to declare it (see below).

## `infra.<env>.yaml` — the infrastructure overlay

*Where* the factory runs. Swapping environments (dev → prod) is a matter of swapping
this file — the pipeline in `harness.yaml` is unchanged (location transparency).

```yaml
sandbox:
  backend: docker            # bootstrap stand-in for Firecracker (Phase 5)
  egress:  broker-only       # zero direct network; all I/O via the broker
  limits:  { cpu: 2, mem: 2Gi, wall: 30m }   # per-invocation ceiling
  max_concurrency: 1         # optional — same-role invocations a runner serves at once (1 = serial)
  profiles:                  # soul.sandbox name -> concrete artifact for this backend
    go-toolchain:
      image: harness/go-toolchain@sha256:…   # docker/gvisor: `image`; firecracker: `rootfs`
nats:
  url: ""                    # "" = embedded in-process server (dev); set = external cluster (T5.8)
  jetstream: { replicas: 1, max_age: 168h }   # replicas: per-stream replication; max_age: result retention
broker:
  allowlist: [llm-api, nats, git, package-proxy]   # the only egress the sandbox is granted
  package_proxy: https://proxy.golang.org           # Go module proxy fetches route to (default)
git:                          # where the candidate branch is pushed + how it's authed (T5.7)
  # remote: https://github.com/acme/widgets.git     # "" = local-repo apply (dev); set = real remote
  # github_app:                                      # mints a per-task, short-lived scoped push token
  #   app_id: "123456"
  #   installation_id: "7891011"
  #   repository: acme/widgets
  #   private_key: /run/secrets/harness_github_app.pem   # PATH to the App PEM; a runtime secret
artifacts:
  backend: files             # files (single-host dev) | s3 (distributed: S3/MinIO)
  path: ./.harness/artifacts # files backend root (relative paths resolve against the repo)
  # backend: s3              # for s3, set bucket + (endpoint | region); creds come from the env
  # bucket: harness-artifacts
  # endpoint: minio.internal:9000   # MinIO/non-AWS host[:port]; prefix http:// for plaintext dev
  # region: us-east-1               # required when endpoint is empty (derives the AWS endpoint)
otel:
  endpoint: localhost:4317   # "" = off, "stdout" = offline dev, host:port = OTLP/gRPC (traces + metrics + logs)
  # tls: true                # dial with the host's root CAs (an authenticated public backend); default off = insecure (local collector)
  # headers:                 # sent with every export — auth + routing for backends like OpenObserve
  #   organization: default          # routing metadata — a literal is fine
  #   authorization: ${OTEL_OTLP_AUTH}   # credential — MUST be an ${ENV_VAR} ref, never a literal secret
signing:                     # provenance-commit signing (T5.10); omit/disable to leave commits unsigned
  # enabled: true
  # key: /run/secrets/harness_ed25519        # SSH private signing key (the harness identity); a runtime secret
  # allowed_signers: /etc/harness/allowed_signers   # principal -> harness public key, for verify-on-read
models:
  claude-opus-4-8:
    provider: anthropic
    effort: medium   # output_config.effort (low|medium|high|xhigh|max); omit for the model default
    cost: { input_per_mtok: 15, output_per_mtok: 75, cache_write_per_mtok: 18.75, cache_read_per_mtok: 1.5 }
  claude-sonnet-4-6:
    provider: anthropic
    cost: { input_per_mtok: 3, output_per_mtok: 15, cache_write_per_mtok: 3.75, cache_read_per_mtok: 0.3 }
```

- **`models`** is the registry souls resolve against. Each entry names a `provider`
  (`anthropic`, `openai`, or `openai-compat` with an `endpoint`) and an optional `cost`
  block — the per-million-token price table the orchestrator uses to convert recorded
  token usage into USD for budget enforcement. Prices are not secrets; API keys come
  from the environment. An optional `family:` declares the model's trainer org for the
  diversity warning above — only needed for a bare-slug `openai-compat` model (an
  aggregator `vendor/model` slug and the direct `anthropic`/`openai` providers are
  identified automatically) or to override the inferred value.
- **`effort`** (optional) sets the reasoning-effort level the adapter sends on every call
  to that model — `low`|`medium`|`high`|`xhigh`|`max`, the intelligence↔latency↔cost dial.
  Lower effort means fewer, more-consolidated tool calls and less deliberation, which bounds
  turn count (and so wall-clock) on a live run; omit it to leave the model at its provider
  default. **Honored on `provider: anthropic`** (mapped to the native `output_config.effort`)
  **and `provider: openai-compat`** (see `effort_param`); validation rejects it on `provider:
  openai` (not yet wired) and rejects an unknown level.
- **`effort_param`** (required alongside `effort` on `provider: openai-compat`) selects the
  wire form the effort level rides on, because that surface is heterogeneous — OpenRouter
  carries a level-based effort control two ways. `reasoning` sends the unified
  `reasoning: {effort}` object (OpenAI o-series, DeepSeek, Gemini-thinking, Claude pre-4.6);
  `verbosity` sends the top-level `verbosity` field, which is what Claude 4.6+/5 map to
  `output_config.effort` (their `reasoning.effort` is a silent no-op). Rejected on the other
  providers (native `anthropic` has one wire form; native `openai` is not yet wired). If an
  Anthropic-family slug sets `effort_param: reasoning`, `harness validate` warns that it will
  silently do nothing — use `verbosity`.
- **`prompt_caching`** (optional, `true`/`false`) opts the model into prompt caching — the
  adapter marks the re-sent stable prefix (persona + the Brief's spec/ambient context)
  cacheable so a gateway bills it at the cache-read rate (~0.1×) instead of full price every
  turn, the single largest cost saving on the agent loop. **Honored only on `provider:
  openai-compat`**, where caching is opt-in because that surface is mixed: OpenAI/DeepSeek-style
  backends cache automatically (no marker) and a strict local server may reject the marker, so
  it is set only for a backend that needs *and* accepts it — e.g. an Anthropic model served
  through OpenRouter. The native `anthropic` adapter caches **unconditionally** and needs no
  flag; validation rejects `prompt_caching` on `anthropic`/`openai`. Cache read/write token
  counts normalize into the usage accounting either way.
- **`sandbox.backend`** selects the isolation backend and is honored at startup (not
  decorative): `docker` (the bootstrap default — weak shared-kernel isolation, dev only)
  and `gvisor` (medium-trust: the same container boot pinned to the `runsc` runtime, so
  the host needs gVisor registered as a Docker runtime) both work today; `firecracker`
  (the production microVM target) is **not yet available** and selecting it fails closed
  with a clear error rather than silently degrading to Docker. Whichever backend, the
  sandbox is `broker-only`: zero direct network, every call mediated by the broker against
  the `allowlist`.
- **`broker.allowlist`** is the deny-by-default egress set (a destination not listed is
  refused at the broker). `package-proxy` permits Go module fetches: the zero-network
  sandbox can't reach a proxy directly, so the image runs an in-sandbox GOPROXY shim
  (`harness sandbox-goproxy`) that forwards `go`'s module-proxy requests over the broker to
  the runner, which fetches from **`broker.package_proxy`** (default the public
  `proxy.golang.org`) and logs every pull (T5.6). Omit `package-proxy` and dependency
  fetches are denied — a build then resolves only from the image's baked module cache.
  Integrity is `go.sum` + the public checksum DB (pinning, served through the same shim
  path) plus the `qa` gate's post-fetch `govulncheck`/license scan, so the public proxy is
  the deliberate default and a private vetted mirror is an optional `package_proxy` swap.
  Allowlisting `package-proxy` also grants the **gate verification sandbox** the same egress
  (T5.6a), so a candidate that adds a brand-new dependency can be re-gated against the
  identical pinned bytes; the verifier is otherwise deny-all. Omitting it keeps both the
  agent sandbox and the verifier on the baked module cache only.
  See [security.md](../specs/security.md) Control 2.
- **`git`** configures where the candidate branch is pushed and how that push is
  authenticated (T5.7). An **empty `remote`** (the dev default) keeps the bootstrap
  local-repo apply — the runner lands the branch into the local source repo, no token. A
  **set `remote`** routes the push to a real git remote. **`git.github_app`** (optional)
  makes the runner mint a **GitHub App installation token** per task: scoped to the
  repository with `contents:write`, minted just before the push and **revoked the instant
  it completes** (its ~1h TTL is the backstop). A GitHub token can't be scoped to a branch,
  so "only the task branch" is enforced by the broker's branch guard — the token supplies the
  repo scope, the guard the branch scope. The agent never holds the token or the remote URL;
  the push happens host-side on the trusted runner. The App **`private_key`** is a runtime
  secret referenced by **path** (like the signing key and API keys — never committed or baked
  into an image; existence is not checked at validate time). A `remote` with no `github_app`
  pushes unauthenticated (valid for a `file://` remote). `git` must be in `broker.allowlist`
  for any push to be brokered. See [security.md](../specs/security.md) Control 3.
- **`sandbox.max_concurrency`** (optional, default `1`) is how many invocations a runner
  serves **per role** at once. `1` is serial — a role's ready siblings run back-to-back; `>1`
  lets that many same-role invocations run concurrently, the lever that fans out a wide
  decomposition instead of serializing its children on the slowest stage. It is bounded by
  memory, not just this number: each sandbox may use up to `limits.mem`, so peak RAM is roughly
  `max_concurrency × mem` per busy role plus the orchestrator's separate gate sandbox — size it
  to the host (e.g. `2` at `4Gi` on an ~8 GiB VM). The orchestrator's verification gates run off
  a single result consumer, so they stay serial regardless; this knob speeds the agent
  (author/implement) stages.
- **`sandbox.profiles`** resolves the logical profile a soul names (`sandbox: go-toolchain`)
  to the concrete artifact this backend boots — an `image` for docker/gvisor, a `rootfs`
  for firecracker. The soul stays env-agnostic; dev points the name at a local tag, prod
  at a digest-pinned image/rootfs. Pin by digest (`@sha256:…`) so provenance records the
  exact toolchain bytes. `harness validate` fails if any `soul.sandbox` has no entry here
  for the active backend.
- **`nats`** points the run at its messaging substrate. An **empty `url`** runs the
  **embedded in-process NATS server** (the dev/bootstrap shape; expose it on a TCP
  listener for `harness approve` with `harness run --nats-addr host:port`). A **set
  `url`** (e.g. `nats://nats-1:4222,nats://nats-2:4222`) connects to that **external
  cluster** instead — the distributed deployment (T5.8), same code and no rebuild
  (location transparency); the embedded server is not started and `--nats-addr` is
  ignored. `jetstream.replicas` is the per-stream replication factor (1 on the single
  embedded server; `>1` needs an external cluster of at least that size — `harness
  validate` rejects `>1` with no `url`), and `jetstream.max_age` bounds the **result
  stream's** retention (the work, dead-letter, and approvals streams are deliberately
  unbounded — work is consume-once, the others must survive until a human acts).
- **`artifacts`** selects the content-addressed store. `files` (the dev default) keeps
  evidence under `path` on one host. `s3` is the distributed backend (AWS S3 or any
  S3-compatible service, e.g. MinIO) so runners on many hosts and the control room share
  one `bucket`; set an `endpoint` (host[:port], optional `http://`/`https://` scheme — a
  bare host implies TLS) for MinIO/non-AWS, or a `region` to derive the AWS endpoint.
  Credentials are **never** in config — like model API keys, the s3 backend reads them
  from the environment (`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` /
  `AWS_SESSION_TOKEN`). The bucket must already exist; the backend never creates it.
  `harness validate` fails an `s3` config that names no bucket or no endpoint/region.
- **`otel.endpoint`** defaults to off; `stdout` is for offline dev, a `host:port` is
  OTLP/gRPC to an external collector (a Phase 5 deployment step). All **three** OTel
  signals — traces, metrics, and logs — export off this one endpoint, so a single
  multi-signal backend (e.g. OpenObserve) ingests the whole record; a trace-only viewer
  (Jaeger) takes the spans and ignores the rest. Logs are batched at Info+ and carry only
  the trusted side's `slog` (see [observability.md](../specs/observability.md)).
- **`otel.tls`** selects transport security for the dial: off (default) is insecure — the
  local-collector posture `localhost:4317` expects; `true` uses the host's root CAs for an
  authenticated public backend reached over the internet.
- **`otel.headers`** are sent with every export — the auth + routing metadata a backend
  like OpenObserve requires. A header whose **name** looks like a credential
  (`authorization`, `*-key`, `*-token`, …) must carry an `${ENV_VAR}` reference, resolved
  from the environment at startup, **never a literal secret** — the same key-handling
  discipline as model API keys. Routing metadata (`organization`, `stream-name`) may be a
  plain literal. `harness validate` rejects a literal credential or a malformed
  `${ENV_VAR}` reference.
- **`signing`** turns on cryptographic signing of the harness-authored provenance commit
  (SSH signing, `gpg.format=ssh`; see [security.md](../specs/security.md)). With
  `enabled: true` and a `key` (path to the harness's SSH **private** signing key) the
  orchestrator signs every integration commit, so `main`'s tip is provably the harness's,
  not just labeled with its name. The key is a **runtime-provisioned secret** like an API
  key — referenced by path, never committed or baked into an image; its existence is *not*
  checked at `harness validate` (a missing key fails loudly on the first merge).
  `allowed_signers` (a public file mapping the `harness@localhost` principal to the public
  key) drives **verify-on-read**: the control room's provenance view shows each merged
  commit's signature verdict (signed / unsigned / unverified). Omitting the block, or
  leaving `enabled` false, leaves commits unsigned — the unchanged dev default.
  `harness validate` rejects `enabled: true` with no `key`.

### Using a local model (no API key)

Add an `openai-compat` entry pointing at a local endpoint (e.g. Ollama) and name it
from a soul's `model`:

```yaml
models:
  local-llama:
    provider: openai-compat
    endpoint: http://localhost:11434/v1
```

This is how the kernel was validated end-to-end without a hosted key.
</content>
