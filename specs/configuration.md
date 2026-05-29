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
  plan:         { role: planner,      produces: [author-tests] }
  author-tests: { role: test-author,  postcondition: [tests-red], produces: [implement] }
  implement:    { role: implementor,
                  precondition:  blockers-closed,
                  postcondition: [tests-red-then-green],
                  on_failure:    implement,
                  produces:      [qa] }
  qa:           { role: security,
                  postcondition: [tests-pass, "mutation>=0.8", gosec, deps-scan],
                  on_failure:    implement,
                  produces:      [integrate] }
  integrate:    { kind: trusted-merge }

checks:                          # command-check postcondition -> shell command
  tests-pass: go test ./...      #   exit 0 = pass, run at the worktree root
  gosec:      gosec ./...        #   in the clean verification sandbox
  deps-scan:  govulncheck ./...

policy:
  max_retries: 3
  budget:      { tokens: 2_000_000, usd: 20, wall: 2h }   # per issue
  epic_budget: { usd: 200 }
  dead_letter: harness.dlq
```

- `produces:` are the **declarative depth** transitions the orchestrator applies.
- `precondition` / `postcondition` / `on_failure` are the stage guards (see
  [workflow.md](workflow.md)). Postconditions evaluate in a clean
  [verification sandbox](verification.md).
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
  the `implement` stage's red→green proof.
- `policy` is the **termination guarantee** — see budgets in
  [workflow.md](workflow.md).

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

**Roles vs. souls.** The DAG references a *role*; souls *fulfil* it. A role may
map to a **set** of souls; the orchestrator picks one per issue by matching the
issue's tags against each soul's `selector`. With a single soul per role this is
trivially 1:1 and needs no extra ceremony; adding a specialized soul later needs
no DAG change. The issue's tags are set by the decomposition planner at
issue-creation.

These are **two distinct bindings on an issue, stored separately.** The
*stage/role* an issue belongs to (what routes it to `work.<role>`) is recorded in
the issue's metadata, set when the orchestrator creates it. The issue's *tags* are
the selector input above. Keeping them apart means a soul `selector` (e.g.
`lang: go`) never collides with the role binding that drives dispatch.

**Concurrency is not a soul concern.** Many invocations of the same soul run in
parallel across runners; you scale throughput by adding runners, not by defining
more souls.

**Personas are markdown**, not inline YAML — consistent with specs, and prompts
are long and want diffing/review.

---

## `infra.<env>.yaml` — environment

```yaml
sandbox:
  backend: firecracker     # docker for dev overlay
  egress:  broker-only
  limits:  { cpu: 2, mem: 2Gi, wall: 30m }
nats:
  url: nats://...
  jetstream: { ... }
broker:
  allowlist: [llm-api, nats, package-mirror, git]
artifacts:
  backend: files           # files (dev) | s3 (distributed) — see artifact-store
  path: ./.harness/artifacts
otel:
  endpoint: ...            # trace/metric export; see observability.md
models:                    # registry: model name (used by soul.model) → provider adapter
  claude-opus-4-7: { provider: anthropic }
  gpt-4o:          { provider: openai }
  llama-3.3-70b:   { provider: openai-compat, endpoint: http://ollama:11434/v1 }
  # API keys come from env / the secret store, NEVER from config
```

The `models` registry maps the `model` name a soul declares to a provider adapter
(and endpoint for OpenAI-compatible backends). The runner resolves it at call time;
see [models.md](models.md). Keys are injected from the environment, never written
into config files.

Dev overlays (e.g. `infra.dev.yaml`) swap Firecracker for Docker without touching
the workflow or souls.

---

## Validation is a safety feature

In an autonomous pipeline a config typo fails silently and badly. A `harness
validate` step must run before anything executes and check:

- every DAG `role` resolves to ≥1 soul, and every soul's `role` exists;
- every `produces:` / `on_failure:` target is a defined stage;
- every `precondition`/`postcondition` reference is known — a command-check
  postcondition must have a `checks:` entry; a metric/reserved one must be recognized;
- soul `selector`s are well-formed; persona files exist;
- the DAG `produces:` transitions don't create an unreachable or trivially-looping
  definition.

> Treat config validation as a gate on startup, not a nicety.

---

## OPEN questions

- Config format: YAML assumed; HCL/CUE are candidates if stronger typing/validation
  is wanted.
- Hot-reload vs. restart on config change — restart is fine for v1.
- A condition-expression language for predicates (shell exit-code vs. CEL) — TBD.
