# Configuration

The harness is **config-driven**: the workflow, the souls, and the infrastructure
are declarative and validated before anything runs. No behaviour that matters is
hard-coded.

See also: [workflow.md](workflow.md), [components/agent.md](components/agent.md),
[components/sandbox.md](components/sandbox.md).

---

## Separate by rate-of-change

Three concerns change at different speeds and by different people, so they live in
separate files:

```
harness.yaml          # the workflow + policy — changes rarely
souls/*.yaml          # one file per soul — added/tuned constantly
souls/prompts/*.md    # personas as markdown, not inline YAML
infra.<env>.yaml      # sandbox/NATS/broker — differs per environment
```

---

## `harness.yaml` — workflow + policy

```yaml
dag:
  requirements: { kind: human }
  plan:         { role: planner, kind: plan, on_failure: plan, produces: [author-tests] }
  author-tests: { role: test-author,  postcondition: [tests-red], produces: [implement] }
  implement:    { role: implementor,
                  precondition:  blockers-closed,
                  postcondition: [tests-red-then-green],
                  on_failure:    implement,
                  produces:      [qa] }
  qa:           { role: security,
                  postcondition: [tests-pass, "mutation>=0.8", gosec, govulncheck, license-scan],
                  on_failure:    implement,
                  produces:      [integrate] }
  integrate:    { kind: trusted-merge }

integration:                     # how verified work becomes commits on main
  mode: per-item                 #   per-item (default) | epic (atomic feature landing)

checks:                          # command-check postcondition -> shell command
  tests-pass:   go test ./...    #   exit 0 = pass, run at the worktree root
  gosec:        gosec ./...                              # SAST            in the clean
  govulncheck:  govulncheck ./...                        # vulnerabilities verification
  license-scan: go-licenses check ./...                  # deps/licenses   sandbox

independent_checks: [gosec, govulncheck, license-scan]   # run past a failure (see below)

spec_depth: 1                    # spec-slice link-hop horizon (see below)
ambient_specs: []                # files injected into every Brief (see below); opt-in

policy:
  max_retries: 3
  budget:      { tokens: 2_000_000, usd: 20, wall: 2h }   # per issue
  epic_budget: { usd: 200 }
  explore_budget: { tokens: 100_000, turns: 12 }          # per explore call (see below)
  dead_letter: harness.dlq
```

- `spec_depth` bounds the **spec context horizon**: the orchestrator hands each agent
  the bounded spec slice — the issue's referenced spec file plus its cross-linked
  neighbours within this many link hops — rather than the whole `specs/` tree (0 = just
  the referenced file; 1 = it plus its direct neighbours). See
  [specs-process.md](specs-process.md).
- `ambient_specs` is a list of repo-relative spec files **injected into every agent's
  Brief**, regardless of the issue — for a project's cross-cutting conventions and the spec
  index, which the issue-scoped slice does not reliably carry. It is **opt-in** (empty by
  default); the conventional pair is `[specs/README.md, specs/conventions.md]`. The files are
  prepended ahead of the bounded slice (a stable, cache-friendly prefix), de-duplicated
  against it, and covered by the slice's content hash. Keep them **lean** — they ride in
  every invocation. See [specs-process.md](specs-process.md#ambient-specs).
- `integration.mode` selects how verified work reaches `main` ([integration.md](integration.md)):
  **`per-item`** (default — the kernel behaviour) lands each work item on `main` as its own
  chain verifies; **`epic`** lands a whole feature **atomically** — children integrate onto an
  `epic/<epic_id>` branch and `main` advances exactly once, by the epic's terminal merge, when
  the epic's subtree has drained. Omitting the block defaults to `per-item`. Epic mode v1 runs
  **one epic at a time** (the [wizard](control-room.md) refuses a second approval while one is
  in flight). Validation rejects any `mode` outside `{per-item, epic}`.
- `produces:` are the **declarative depth** transitions the orchestrator applies.
- `precondition` / `postcondition` / `on_failure` are the stage guards (see
  [workflow.md](workflow.md)). Postconditions evaluate in a clean
  [verification sandbox](verification.md).
- A stage is an **agent stage** (it names a `role` souls fulfill) or a **non-agent
  stage** (`kind: human` for requirements, `kind: trusted-merge` for integrate). Two
  kinds are *agent* hybrids that name a `role`: `kind: plan` and `kind: resolve`.
  `kind: plan` is **not sandbox-gated**: a plan stage declares **no postcondition** — the
  planner writes no candidate to grade; its output is the child issues it proposes
  (emergent breadth), which the orchestrator validates structurally (legal roles within
  the declared `produces`, acyclic edges) and writes. The proposals *are* the production,
  so a plan stage runs no gate and does no depth-advance of its own; `produces:` instead
  declares which roles its proposals may target. Validation rejects a `plan` stage with no
  role or with a postcondition. `kind: resolve` is the **merge-conflict-resolution** stage
  (a `merge-resolver` soul): unlike `plan` it **is** sandbox-gated and so **must** declare a
  postcondition — the suite that re-verifies the *resolved* tree in a clean sandbox (the
  two-green-branches guard applies to the resolution too, [verification.md](verification.md)).
  It is never reached through a `produces` edge: the orchestrator spawns a resolve issue only
  when a verified candidate cannot be cleanly rebased onto the current `main`
  ([integration.md](integration.md)). It is excluded from the pipeline-entry computation
  (it shares produces-indegree 0 with the entry stage but is not an entry), and on success it
  `produces: [integrate]` to loop the resolved candidate back into the merge queue, with
  `on_failure: resolve` for a bounded retry. Validation rejects a `resolve` stage with no
  role or no postcondition.
- `checks:` is the **check registry**: it maps each *command-check* postcondition to
  the shell command that realizes it in the verification sandbox (exit 0 = pass). It
  is the bridge from a declared postcondition name to a runnable gate check, so the
  command each check runs is config — not code — and is the single source of truth
  the gate resolves against. A **metric comparison** postcondition (`mutation>=0.8`) is
  backed by a built-in check *kind* for the comparison itself, but it still resolves a
  command — registered under its **metric name** (here `mutation`) — that the gate runs
  in the verification sandbox to *produce* the score; the gate then grades the number the
  command prints against the threshold. The tool invocation (which mutation tool, how to
  reduce its report to a single number) therefore stays in config, keeping the gate
  agnostic to the tool: it reads a number, not a `gremlins` report. A comparison whose
  metric has no registered command is unresolvable — the same config fault as a missing
  command-check entry — which validation rejects at startup. **Reserved proofs**
  (`tests-red-then-green`, `tests-red`) are different: they need no `checks` entry of
  their own. The `tests-red-then-green` proof reuses the **`tests-pass`** acceptance-test
  command, running it against two refs (fail on the base, pass on the candidate — see
  [verification.md](verification.md)); a stage that declares the proof must therefore
  register a `tests-pass` command, which validation enforces. The **`tests-red`** proof
  has the same shape: it too carries no `checks` entry and reuses `tests-pass`, but runs
  it **once** against the candidate and passes iff that command **fails** — proving the
  test author produced real, executing acceptance tests that genuinely fail before any
  implementation exists. A stage declaring `tests-red` must likewise register a
  `tests-pass` command, which validation enforces. Keeping the acceptance
  tests under one `tests-pass` key makes that command a single source of truth shared by
  the `qa` stage's `tests-pass` check, the `author-tests` stage's `tests-red` proof, and
  the `implement` stage's red→green proof. **Independent scanners** (`gosec`,
  `govulncheck`, `license-scan` above) are *ordinary command checks* — they need no
  built-in kind, since the gate already grades a command on its exit code (0 = clean =
  pass; non-zero = findings or a tool error = fail, closed). They are spec-independent
  (generic SAST / vulnerability / dependency-licence layers — see
  [verification.md](verification.md)), so they are declared and resolved exactly like
  `tests-pass`, and each emits its captured report as gate evidence cited by name in the
  provenance trailer (`gosec@<hash>`). Because the verification sandbox is
  **zero-network**, a scanner that needs reference data (the vulnerability database for
  `govulncheck`, licence metadata for `license-scan`) reads it from data baked into the
  role's sandbox image, never the network — the same offline guarantee the build relies
  on (see [sandbox](components/sandbox.md), and rootfs composition in the build plan).
  Beyond grading the exit code, the gate parses each tool's machine-readable output
  (`-json` / `--output.json`) into structured **[findings](verification.md)** for the
  agent's context, the verdict, and dead-letter triage — the parsing is built-in per-tool
  infra (the kernel ships adapters for `go test`, `golangci-lint`, `gosec`,
  `govulncheck`), so the registry stays a name→command map and a deployment adds a scanner
  without writing a parser (a command with no adapter still grades on exit code with its
  raw output as evidence). Grading the verdict over finding *severity* — a `fail_on:`
  threshold — is a **planned refinement**; today every check still fails closed on a
  non-zero exit.
- `independent_checks:` is the optional list of command checks the gate keeps running
  **past** a failure, so one `qa` pass aggregates *every* independent-scanner finding
  instead of stopping at the first — better dead-letter triage, since the human (or the
  agent re-routed to `implement`) fixes them all in one round-trip rather than bouncing the
  candidate once per scanner (see [verification.md](verification.md)). Only the
  spec-independent scanners belong here; a reserved proof or a metric's measurement command
  is **not** eligible (a mutation score on red tests is meaningless — those stay fail-fast),
  and validation rejects such an entry. Each listed name must be a key in `checks:`. Omitting
  the list (or leaving it empty) keeps every check fail-fast. The gate still stops at the
  first *non-independent* failure, and aggregation never changes the verdict — a candidate
  that trips any check still fails the gate; it only changes how much a single failing pass
  reports.
- **Human-approval** (`human-approved`) is a postcondition kind evaluated by the
  **orchestrator**, not a `checks` command — it reads orchestrator/beads state, not the
  repository, so it carries no `checks` entry. It holds only when a human has explicitly
  approved the issue's *current candidate* via `harness approve <issue>` (`harness reject
  <issue>` denies); the approval is **bound to the candidate sha** — like an evidence hash
  pins to bytes — so any re-gate after a change invalidates a stale approval. It fails
  **closed**, and its failure does **not** route `on_failure` (it burns no retry): the issue
  **parks in an awaiting-approval escalation** (blocked, carrying its candidate ref and the
  provenance the gate already verified) until a human approves or rejects. On **approve** the
  preserved provenance is replayed onto the merge — the candidate was already gate-verified,
  so it is not re-graded; the merge queue re-gates only if a rebase onto a moved `main` is
  needed (the existing two-green-branches guard, [integration.md](integration.md)) — then it
  lands. On **reject** it routes a fix attempt through the normal `on_failure`/retry machinery
  (→ back to spec when no route or budget remains). It is the gate that realizes the
  trusted-dev transition and the permanent TCB-review boundary (`policy` below, [bootstrap.md](bootstrap.md)).
- `policy.explore_budget` is the **fixed** per-call cap on the
  [`explore` tool](components/agent.md#explore--distilled-comprehension)'s nested read-only
  sub-loop (tokens and/or turns). It is deliberately fixed, not a fraction of the parent's
  remaining budget, so an explore call behaves the same wherever in an invocation it is made;
  the runner meters it against this cap and, on a breach, harvests a `partial-budget` answer
  rather than failing the parent — the parent's `budget` above is still the real ceiling
  (every explorer stream draws against it). Omitting the block disables `explore` (no cap =
  no helper loop). See [models.md](models.md).
- `policy` is the **termination guarantee** (budgets + retry caps — see
  [workflow.md](workflow.md)) and the **autonomy profile**. `policy.profile` is
  `trusted-dev` or `autonomous`: **trusted-dev** requires a `human-approved` postcondition on
  *every* `integrate` (the self-hosting transition — a human reviews every diff);
  **autonomous** requires it only when a candidate's diff touches the **TCB**.
  `policy.tcb_paths` is the glob set that marks a diff TCB-touching (the orchestrator,
  runner/broker, sandbox config, gate harness — e.g. `internal/orchestrator/**`,
  `internal/runner/**`, `internal/broker/**`); a candidate hitting any requires approval
  regardless of profile, which is how *TCB-touching changes stay human-reviewed permanently*
  ([bootstrap.md](bootstrap.md)). This same list is the operational definition of the TCB
  boundary. The concrete glob set is **not** a hardcoded kernel default: it is a
  per-deployment operational-review decision that must be reviewed and pinned for the
  target repository before autonomy is switched on for harness work, so the boundary
  reflects what actually constitutes the TCB *there* ([bootstrap.md](bootstrap.md)).

---

## `souls/*.yaml` — identity

```yaml
name:    implementor-go
role:    implementor                  # binds to the DAG role
model:   claude-opus-4-7
persona: souls/prompts/implementor-go.md   # markdown, diffable, reviewable
tools:   [fs, shell, git]
sandbox: go-toolchain                 # a sandbox profile (see infra)
selector: { lang: go }                # how this soul is chosen for an issue
```

The `persona` path is resolved **relative to the config root** (the directory passed
to `--config`), not to the soul file — hence the `souls/` prefix even though the soul
itself lives under `souls/`. A bare `prompts/implementor-go.md` would not resolve.

**Roles vs. souls.** The DAG references a *role*; souls *fulfil* it. A role may
map to a **set** of souls; the orchestrator picks one per issue by matching the
issue's tags against each soul's `selector`. With a single soul per role this is
trivially 1:1 and needs no extra ceremony; adding a specialized soul later needs
no DAG change. The issue's tags are set by the decomposition planner at
issue-creation and **threaded forward across the stages of an epic** (like the
candidate base), so a `lang=go` epic routes every stage — author-tests, implement,
qa — to the matching soul.

**Selection algorithm.** Given an issue's role:

- if **one** soul fulfills the role, it is used unconditionally — the trivial 1:1
  case, so an untagged issue still dispatches even when that soul declares a
  selector (the kernel relies on this);
- if **several** souls fulfill it, the orchestrator keeps those whose `selector`
  the issue's tags **satisfy** (every selector key present in the tags with the
  same value) and picks the **most specific** — the one with the largest matching
  selector. An **empty selector matches anything**, so a soul with no selector is a
  catch-all *default* for its role that a specialized soul beats. If no soul matches
  the issue is not dispatched (a planner/config fault for a human);
- ties at equal specificity break **deterministically** by soul name (souls load
  name-sorted), so selection is reproducible.

These are **two distinct bindings on an issue, stored separately.** The
*stage/role* an issue belongs to (what routes it to `work.<role>`) is recorded in
the issue's **metadata**, set when the orchestrator creates it. The issue's *tags*
are the selector input above and ride in the issue's **labels** — one `key=value`
label per tag (e.g. selector `{lang: go}` ↔ label `lang=go`). Keeping them in
separate stores means a soul `selector` never collides with the role binding that
drives dispatch.

**Concurrency is not a soul concern.** Many invocations of the same soul run in
parallel across runners; you scale throughput by adding runners, not by defining
more souls.

**Personas are markdown**, not inline YAML — consistent with specs, and prompts
are long and want diffing/review.

**The explorer is a helper soul on a reserved role.** The
[`explore` tool](components/agent.md#explore--distilled-comprehension) resolves to a soul
declaring the reserved role `explorer` — a role the **DAG never references** (it is invoked
as a tool, not scheduled as a stage), so it is exempt from the "every soul's role is a DAG
stage" rule below. Otherwise it is an ordinary soul: a cheap `model`, an explore `persona`, a
read-only `tools` allowlist (the comprehension subset; **never** `explore` itself — no
recursion), and a `selector`. It is selected per issue by the **same selector algorithm** as
any role, so a deployment with more than one explorer can route tagged issues between them:

```yaml
name:     explorer                     # the producer-path default
role:     explorer                     # reserved helper role — not in the DAG
model:    haiku-cheap
persona:  souls/prompts/explorer.md
tools:    [find_symbol, references, definition, implementation, hover, diagnostics,
           read_file, list_dir, search]
selector: {}                           # catch-all default
```

**Verify-path explore is a policy choice — a shared, same-family explorer is fine.** One
explorer serving both the producer and the qa/security path is acceptable: explore is
additive and never load-bearing, so a correlated explorer blind spot can degrade a
verifier's *search*, never its *grade* — [verification.md](verification.md) records the
decision and the reasoning. Stricter postures stay available as pure config: a second
explorer soul on a different [model family](#infraenvyaml--environment) with a
distinguishing selector, or giving the verify path *no* `explore` at all (it reads raw,
fully independent). The family-overlap [advisory](#advisories--non-fatal-warnings) does
**not** extend to explorers — it covers the producer/verifier *main* models, where family
overlap weakens the grade itself.

---

## `infra.<env>.yaml` — environment

```yaml
sandbox:
  backend: firecracker     # docker for dev overlay
  egress:  broker-only
  limits:  { cpu: 2, mem: 2Gi, wall: 30m }
  profiles:                # logical soul.sandbox name -> concrete backend artifact
    go-toolchain:
      image: harness/go-toolchain@sha256:…    # docker/gvisor read `image`
      # rootfs: /var/lib/harness/go-toolchain.ext4   # firecracker reads `rootfs`
nats:
  url: nats://...
  jetstream: { ... }
broker:
  allowlist: [llm-api, nats, package-mirror, git]
artifacts:
  backend: files           # files (dev) | s3 (distributed) — see artifact-store
  path: ./.harness/artifacts
otel:
  endpoint: ...            # OTLP export of traces + metrics + logs; "" off, "stdout" dev; see observability.md
  headers:                 # sent with every export (auth for backends like OpenObserve)
    organization: default  # credential VALUES come from env, never literal in config
    authorization: ${OTEL_OTLP_AUTH}
models:                    # registry: model name (used by soul.model) → provider adapter
  claude-opus-4-7:
    provider: anthropic
    cost: { input_per_mtok: 15, output_per_mtok: 75, cache_write_per_mtok: 18.75, cache_read_per_mtok: 1.5 }
  gpt-4o:          { provider: openai }
  llama-3.3-70b:   { provider: openai-compat, endpoint: http://ollama:11434/v1 }
  # API keys come from env / the secret store, NEVER from config
```

The `models` registry maps the `model` name a soul declares to a provider adapter
(and endpoint for OpenAI-compatible backends). The runner resolves it at call time;
see [models.md](models.md). Keys are injected from the environment, never written
into config files.

The optional `family:` field declares a model's **correlated-blind-spot family** — the
organization that trained the weights — for the producer/verifier diversity warning
([verification.md](verification.md)). It is only needed when the registry key does not
already carry the vendor: an aggregator slug (`deepseek/deepseek-v4-pro` served via an
`openai-compat` gateway like OpenRouter) derives the family from the `vendor/` prefix
automatically, and a direct `anthropic`/`openai` provider is itself one vendor — so
`family:` is for a **bare-slug** `openai-compat` model (e.g. two distinct Ollama models on
one endpoint, which would otherwise both fall back to the provider tag and read as one
family) or to override the inferred value. Not a secret; it lives in config.

The optional `cost` block is the **per-million-token price** — the table that converts a
recorded token `Usage` into USD so the orchestrator can enforce the dollar half of the
[budget](workflow.md) that bounds the `on_failure` loop. Each dimension (full-rate input,
output, cache write, cache read) bills independently; an absent block (or a zero rate)
prices that dimension at $0, so a model with no `cost` contributes nothing to USD
accounting — its spend is still bounded by the token and retry caps, which never depend on
the table. Prices are not secrets, so unlike API keys they live in config.

Two optional **capability fields** tune how the adapter calls a model — pure cost/quality
dials the agent stays unaware of (see [models.md](models.md) "Optional capability fields"):
- `effort: low|medium|high|xhigh|max` — the reasoning-effort dial; the adapter sends it as
  the provider's effort field (Anthropic `output_config.effort`). Validation rejects it on a
  provider with no equivalent.
- `prompt_caching: true` — opt the model into prompt caching on the `openai-compat` adapter
  (marking the re-sent stable prefix cacheable). It is opt-in there because that surface is
  mixed — some backends cache automatically and others reject the marker — so it is set only
  for a backend that needs *and* accepts it (e.g. an Anthropic model via OpenRouter). The
  native `anthropic` adapter caches unconditionally and needs no flag.

**Per-role model tiers** are expressed here, not by any separate construct: a soul
names its `model`, so assigning a cheaper model to one role's soul and a frontier model
to another's *is* the tier policy. Because the model is resolved from the soul the
orchestrator selects per issue, the tier is per issue and is recorded in provenance.
The bootstrap runs the `security`/`qa` soul on a mid-tier model and the rest on the
frontier model — the qa candidate is re-graded by the independent gate, so a cheaper
model there is the lowest-risk economy (see [models.md](models.md)).

The `profiles` registry resolves the **logical sandbox profile** a soul names
(`soul.sandbox`, e.g. `go-toolchain`) to a **concrete, backend-specific bootable
artifact** — a (digest-pinned) `image` for Docker/gVisor, a `rootfs` for Firecracker —
exactly as `models` resolves a soul's `model` to a provider. The soul therefore stays
backend- and environment-agnostic: the same `go-toolchain` name resolves to a local
docker tag in dev and a pinned rootfs in prod, no soul edit. Resolution runs where the
orchestrator/runner build the sandbox spec, so the backend only ever boots a concrete
artifact (the Docker→Firecracker swap stays config). The producer and its verifier
resolve the **same** profile — the gate must grade on the producer's toolchain — to the
same concrete image. The resolved digest is recorded in provenance, pinning the bytes
the code was built and graded in (*provenance by construction*); see
[components/sandbox.md](components/sandbox.md). Limits stay global at `sandbox.limits`
(the single per-invocation ceiling), not per-profile.

Dev overlays (e.g. `infra.dev.yaml`) swap Firecracker for Docker without touching
the workflow or souls.

---

## Validation is a safety feature

In an autonomous pipeline a config typo fails silently and badly. A `harness
validate` step must run before anything executes and check:

- every DAG `role` resolves to ≥1 soul, and every soul's `role` exists — **except** the
  reserved `explorer` role, which the DAG never references (it is invoked as a tool, not a
  stage); if `policy.explore_budget` is set, at least one `explorer` soul must exist, and an
  `explorer` soul's `tools` must be the read-only comprehension subset and must not include
  `explore` (no recursion);
- every soul's `sandbox` profile resolves to a `sandbox.profiles` entry that carries
  the field the active `sandbox.backend` needs (`image` for docker/gvisor, `rootfs`
  for firecracker) — an unresolvable profile is the same silent config fault as a
  missing model or check command;
- every `produces:` / `on_failure:` target is a defined stage;
- every `precondition`/`postcondition` reference is known — a command-check
  postcondition must have a `checks:` entry; a metric/reserved one must be recognized;
- soul `selector`s are well-formed (no empty keys/values); two souls fulfilling the
  **same role** must not share an identical selector — that would make one
  unreachable (selection always picks the same one); persona files exist;
- the DAG `produces:` transitions don't create an unreachable or trivially-looping
  definition;
- `integration.mode`, if present, is one of `per-item` / `epic`.
- `sandbox.backend`, if present, is one of `docker` / `gvisor` / `firecracker`.
  Validation checks only that the value is *well-formed*; whether the selected backend
  is **available on this host** is a runtime concern, resolved when the backend is
  bound. An unavailable backend (e.g. `firecracker` without KVM) **fails closed** at
  that point — a fatal startup error, never a silent downgrade to a weaker backend than
  was asked for (see [components/sandbox.md](components/sandbox.md#selection-is-honored-and-fails-closed)).

> Treat config validation as a gate on startup, not a nicety.

---

## Advisories — non-fatal warnings

Validation above gates startup on **faults**: a config that would break at run time
(a role no soul fulfils, a `produces:` target that doesn't exist, a postcondition with
no resolvable command) fails fast, before anything executes. Alongside it sits a second,
softer channel — `config.Warnings()` — for **advisories**: configurations that are
legal and runnable but worth a second look. These do **not** fail startup; `harness
validate` prints them to stderr and exits `0`. The split is deliberate, and it follows
the *config is the pipeline, so the harness recommends but does not force* principle —
the same rationale as [verification.md](verification.md)'s "Model diversity is
configured, not mandated": faults the harness must reject because they cannot run;
advisories it surfaces and leaves to the operator's judgement.

The known advisories are:

- **Producer/verifier model-family overlap** — the verifier role shares a model family
  with the producer, weakening the N-version independence
  [verification.md](verification.md) recommends (correlated blind spots). Family is the
  trainer org, derived per model from `family:`, else the aggregator slug's `vendor/`
  prefix, else the provider tag — so two models behind one gateway (OpenRouter) are still
  compared by their real vendor. The model assignment is the operator's call, so this is
  advised, never forced.
- **A package proxy named but not on the broker allowlist** — dead config: the named
  proxy is unreachable, and package fetches are denied deny-by-default
  ([security.md](security.md) Control 2).
- **A git remote named but not allowlisted** — the candidate-branch push to that remote
  would be denied ([security.md](security.md) Control 3).

Both surfaces read the same channel: `harness validate` prints these to stderr (exit
`0`), and the control-room [config view](control-room.md) renders them as an
**Advisories** section, putting the same signal where the operator inspects the running
factory. A clean config produces no advisories.

---

## OPEN questions

- **TCB module boundary — the exact `policy.tcb_paths` glob set.** The set of paths
  whose changes stay human-reviewed even after autonomy is switched on (operationally
  the orchestrator, runner/broker, sandbox, and gate harness — see
  [bootstrap.md](bootstrap.md)) is **not** a hardcoded kernel default; it is a
  per-deployment policy value. The concrete globs must be **reviewed and pinned for the
  target repository before autonomy is enabled for harness work** — too narrow lets an
  agent edit the very code that enforces the guarantees; too broad needlessly blocks
  autonomous progress. Tracked as OPEN until that review happens.
- Config format: YAML assumed; HCL/CUE are candidates if stronger typing/validation
  is wanted.
- Hot-reload vs. restart on config change — restart is fine for v1.
- ~~A condition-expression language for predicates (shell exit-code vs. CEL).~~
  **Decided:** the **shell-exit-code form** (shipped). Command-check postconditions
  resolve to commands via the `checks:` registry above; the gate runs them with
  `sh -c` (exit 0 = pass). Bare identifiers (reserved proofs, metric comparisons like
  `mutation>=0.8`) are validated against explicit registries at `harness validate`
  time. CEL is not used.
