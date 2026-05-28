# Implementation Plan

Build order for the harness, derived from [specs/](specs/README.md). The spine is
[bootstrap.md](specs/bootstrap.md): hand-build a minimal kernel that does
`spec → implement → gate → merge` for one issue (the **self-host point**), then let
the harness build its own remaining features as beads issues.

## How to read this

- **Phases 0–1** (scaffolding + kernel) are broken into **atomic tasks** — each is
  one self-contained unit of work with a single concern, completable and verifiable
  on its own. These are designed to map 1:1 onto future beads seed issues (the
  harness's own unit: one issue = one invocation).
- **Phases 2–5** are **coarse** on purpose. They are the post-kernel work the harness
  largely builds for itself; detailing them now risks planning against a kernel that
  doesn't exist yet. Expand each into atomic tasks when its phase begins.
- Tasks within a phase are listed in dependency order. Cross-task deps are noted as
  `(needs Tx.y)` only where they aren't the obvious linear predecessor.
- `(spec)` links point at the authoritative contract for that task.

## The self-host milestone

Phase 1 ends at the kernel from [bootstrap.md](specs/bootstrap.md): config →
beads → sandbox → runner/broker → agent loop → gate runner → orchestrator loop.
Bootstrap simplifications hold until then — DAG collapses to
`implement → gate → integrate`, merge queue is trivial (single stream, no
rebase/re-gate), NATS is in-process, Docker stands in for Firecracker, no control
room (CLI-driven), the implementor writes its own tests.

## TCB caveat

Per [bootstrap.md](specs/bootstrap.md), the components that *enforce* the guarantees —
orchestrator, runner/broker, sandbox, gate harness — are the Trusted Computing Base.
**TCB-touching changes stay human-reviewed even after self-hosting.** Autonomy is
earned first for non-TCB work (new souls, stages, the control room).

---

## Phase 0 — Scaffolding & foundations

> **Environment findings (from T0.1, 2026-05-28):**
> - Go toolchain `go1.26.3` (darwin/arm64); `go.mod` pins `go 1.26`. Module path `github.com/Loxstomper/harness` (from git remote `git@github.com:Loxstomper/harness.git`).
> - Build tooling: `make` is present, `just` is **not** → the `Makefile` is canonical. Targets: `build vet lint fmt tidy test test-unit clean`. `make test-unit` emits `go test -json` to `test/results/` per CLAUDE.md.
> - `bd` (beads CLI) is now **installed** at `/home/clanker/.local/bin/bd`, v0.62.0 (was missing at T0.1 time) — T1.3/T1.4 are unblocked.
> - `golangci-lint` v2.12.2 **installed** (`brew install golangci-lint`); `make check` (vet + lint + test-unit) is the local gate and is clean. The `misspell` linter is `locale: US`, so use US spellings in Go comments (`behavior`/`neighbors`/`fulfill`/`modeled`) — specs may stay British. `golangci-lint config verify` passes. `.golangci.yml` was pruned of the cruft templated from another project (the `internal/controlplane/...` rules, pgx/SQL `errcheck` exclusions, and the `websocket.Dial` rule); the generic `_test\.go` relaxation and the `.*_templ\.go$` exclusion are kept (the latter for the planned templ control room, Phase 4).
> - T0.2 domain types live in a new dependency-free leaf package `internal/core` (`Soul`, `Issue`, `Brief`, `Result`, `ResultStatus`, plus `Branch`/`Evidence`/`ArtifactRef`/`Proposal`) — NOT in `internal/agent`. Rationale: keep pure data types out of the behavioural `agent` package (which will import `model`/`broker`) so `beads`/`orchestrator`/`runner`/`gate` can share them without coupling or import-cycle risk. This adds `core` to the T0.1-enumerated layout. The behavioural `Agent` interface stays deferred to T1.13.
> - Issue lifecycle status (ready/in_progress/blocked/done) is NOT yet an enum — the specs don't enumerate fixed values and `bd` isn't installed. `core.Issue` has no Status field; model it during beads integration (T1.3/T1.4). The only status enum defined so far is `core.ResultStatus` (done/failed/needs-spec-clarification).
> - T0.3 canonical model types live in `internal/model` (`ToolDef`, `ToolCall`, `ToolResult`, `Message`, `Response`, `Usage`, `StopReason`, `Role`, plus a `JSONSchema = json.RawMessage` alias). A canonical `Request` type is intentionally deferred to T1.8 (adapter interface) — the spec only describes it in prose and its optional fields (prompt caching, extended thinking) are shaped by the streaming adapter interface. `ToolResult`/`Role`/`StopReason`/`Usage` field shapes were derived from models.md prose (provider tool-call divergence, usage normalization incl. cache tokens, the finish reasons the loop branches on); they are normalized canonical values that adapters translate to/from provider wire formats.
> - T0.4 messaging lives in a new package `internal/messaging` (NOT `internal/nats`, which would alias-collide with the upstream `nats` client package). Deps added: `github.com/nats-io/nats.go v1.52.0` + `github.com/nats-io/nats-server/v2 v2.14.1` (now direct in go.mod). The embedded server uses `DontListen` + in-process client connect (`nats.InProcessServer`) — true in-process transport, swappable for a TCP listener when distribution lands (Phase 5). Subjects are built via helpers (`WorkSubject`/`ResultSubject`/`AgentEventsSubject`/`ControlSubject`) + constants (`SubjectDLQ`, `WorkStreamSubjects`, `ResultStreamSubjects`, `ControlSubjects`). JetStream streams: `HARNESS_WORK` (WorkQueue retention, ack=lease), `HARNESS_RESULT` (Limits, 7d max-age), `HARNESS_DLQ` (Limits, no max-age). Consumer helpers: `EnsureWorkConsumer(role, ackWait)`, `EnsureResultConsumer`. Events/control are core NATS (no stream). Retention values are bootstrap defaults — the spec marks concrete retention/replicas as OPEN.

- [x] **T0.1 Go module + repo layout** — `go.mod`, `cmd/harness/`, `internal/{config,beads,sandbox,runner,broker,agent,model,gate,orchestrator,artifact}/`, build target (`make`/`just` running `go build`). ([architecture.md](specs/architecture.md))
- [x] **T0.2 Core domain types** — `Soul`, `Brief`, `Result` (status: `done|failed|needs-spec-clarification`), `Issue`, status enum. Pure structs, no behaviour. ([components/agent.md](specs/components/agent.md))
- [x] **T0.3 Canonical model types** — `ToolDef`, `ToolCall`, `ToolResult`, `Message`, `Response`, `Usage`, `StopReason`, `Role`. ([models.md](specs/models.md))
- [x] **T0.4 In-process NATS + subject taxonomy** — embed `nats-server` (still spoken in-process per location transparency); define subject constants (`harness.work.<role>`, `harness.result.<role>`, `harness.agent.<id>.events`, `harness.dlq`, `harness.control.*`) and JetStream stream/consumer setup for work/result/dlq. ([messaging.md](specs/messaging.md))

**Phase 0 complete.** Scaffolding, core domain types, canonical model types, and the in-process NATS layer are in place. `bd` (beads CLI) v0.62.0 is now installed (T1.3/T1.4 unblocked). `golangci-lint` v2.12.2 is installed and `make check` passes.

## Phase 1 — Kernel → self-host point

> **T1.1 findings (2026-05-28):**
> - Config package implemented in `internal/config`: `Harness`/`Stage`/`Policy`/`Budget` (harness.yaml), `Infra` + `SandboxConfig`/`SandboxLimits`/`NATSConfig`/`JetStreamConfig`/`BrokerConfig`/`ArtifactsConfig`/`OTelConfig`/`ModelProvider` (infra.<env>.yaml), and a custom `Duration` type. Loaders: `LoadHarness`/`LoadSouls`/`LoadInfra` plus aggregate `Load(dir, env)` → `*Config{Root, Harness, Souls, Infra}` with `(*Config).PersonaPath(soul)`.
> - Dependency added: `gopkg.in/yaml.v3 v3.0.1` (now direct in go.mod) — the only YAML lib; chosen for direct struct unmarshal. Verified yaml.v3 parses underscore int literals natively (`tokens: 2_000_000` → 2000000), so the spec's syntax loads as written.
> - **Souls unmarshal directly into `core.Soul`** (no parallel `config.Soul` — single source of truth). Added the missing `Selector map[string]string` field + yaml tags to `core.Soul`; soul.go's doc promised "loading a soul YAML populates this struct directly" but the struct had omitted `selector`. This completes the T0.2 soul type.
> - **Strict parsing**: loaders use `yaml.Decoder.KnownFields(true)` — an unknown/typo'd key is a loud load error, per configuration.md's "validation is a safety feature". An empty document parses to the zero value (no error). Cross-file/completeness checks are deferred to T1.2.
> - **Durations**: `config.Duration` (a `time.Duration` with `UnmarshalYAML`) parses Go-duration strings (`2h`, `30m`, `168h`); used by `Budget.Wall`, `SandboxLimits.Wall`, `JetStreamConfig.MaxAge`.
> - **`nats.jetstream` modeled minimally** as `JetStreamConfig{Replicas, MaxAge}` — concrete stream defs (subjects/retention) stay in `internal/messaging`; retention/replicas/max-age are OPEN in messaging.md, so only env-varying knobs surface in infra. `sandbox.limits.mem` kept as a string ("2Gi", k8s-style quantity) — parsed by the sandbox backend (T1.6), not the loader.
> - The loader does NOT validate cross-file references (role→soul resolution, produces/on_failure targets, persona existence) — that is **T1.2 `harness validate`**, the next task.

> **T1.2 findings (2026-05-28):**
> - `(*config.Config).Validate() error` in `internal/config/validate.go` is the startup gate. It **accumulates all problems** (not fail-fast) into a `*ValidationError{Problems []string}` (sorted, deterministic) — operators fix config once and re-run. Checks: stage shape (each stage is agent XOR non-agent, i.e. `role` xor `kind`); `produces`/`on_failure` targets defined; role↔soul both ways (every agent-stage role has ≥1 soul, every soul's role is used by some stage); soul name uniqueness; selector keys/values non-empty; persona files exist on disk (via `PersonaPath`); pre/postcondition references known; produces-graph acyclic + reachable; **plus** model-registry cross-check (every `soul.model` ∈ `infra.models`; openai-compat entries need an endpoint) — the latter was flagged as validate's job in load.go's doc comments.
> - **Condition registry**: the condition-expression language is OPEN (shell vs CEL), so validate gates pre/postconditions against an explicit registry in validate.go: bare preconditions `{blockers-closed}`, bare postconditions `{tests-red-then-green, tests-pass, gosec, deps-scan}`, and comparison form `<metric><op><number>` with known metrics `{mutation}` (e.g. `mutation>=0.8`). **Extend these sets as new conditions/metrics land** (affects T1.19 gate runner). An unrecognized guard is treated as a typo and fails loud.
> - **Reachability model**: roots = stages with produces-indegree 0 (in the canonical DAG that's *both* `requirements` AND `plan`, since the config has no `requirements→plan` produces edge — the seed issue enters at `plan`). A pure DAG always has every node reachable from its indegree-0 roots, so the reachability check's real catch is a cycle-isolated component (reported alongside the cycle). `on_failure` self-routes (e.g. `implement.on_failure: implement`) are role-flow feedback, NOT produces edges, so they do **not** trip the cycle check — only `produces` edges are walked.
> - **NOT wired into the CLI yet** — `Validate()` is a library function with full unit coverage; `cmd/harness/main.go` is still the version stub. The `harness validate` subcommand (arg parsing) is deferred to **T1.21** (`harness` CLI), which the plan already scopes for `validate`/`run`/`seed` together.
> - Env note: in this Linux dev box `golangci-lint` v2.12.2 lives in `$(go env GOPATH)/bin` but is NOT on `PATH` by default — `make check` needs `export PATH="$PATH:$(go env GOPATH)/bin"` first (T0.1 findings described the darwin/brew install). All 41 unit tests pass; `make check` clean.

- [x] **T1.1 Config schema + loader** — structs + YAML unmarshal for `harness.yaml` (dag + policy), `souls/*.yaml`, `infra.<env>.yaml`; persona paths resolved to markdown files. ([configuration.md](specs/configuration.md))
- [x] **T1.2 `harness validate`** — startup gate: every DAG role resolves to ≥1 soul; every `produces:`/`on_failure:`/condition target is defined; persona files exist; selectors well-formed; DAG not unreachable/trivially-looping. Fail loud. ([configuration.md](specs/configuration.md))
- [x] **T1.3 beads read integration** — wrap `bd` to query ready work (no open blockers + precondition holds) and read issue fields into `Issue`/`Brief`. ([components/orchestrator.md](specs/components/orchestrator.md))

> **T1.3 findings (2026-05-28):**
> - `internal/beads.Client` shells out to the `bd` CLI (no Go library — bd owns its own db/versioning; funneling all access through this one package is what keeps the single-writer invariant enforceable). Read methods: `Ready(ctx) ([]core.Issue, error)` → `bd ready --json --limit 0` (bd's `ready` already applies blocker-aware "open + no active blocker" semantics; `--limit 0` drops the default 10-row page); `Get(ctx, id) (core.Issue, error)` → `bd show <id> --json`. Constructor `New(WithBinary, WithDir)` — `WithDir` sets the cwd bd auto-discovers `.beads/` from.
> - **Role-storage convention (load-bearing for T1.4):** the harness role/stage is stored in bd issue **metadata** under key `role` (`beads.MetadataKeyRole`), NOT in labels. Rationale: the spec uses an issue's *labels/tags* for soul-`selector` matching (Phase 3), a distinct concept from the stage binding that routes an issue to `work.<role>`. Verified `metadata` round-trips as a JSON object through `bd ready`/`show --json` (bd 0.62.0). `core.Issue.Role` is populated from `metadata.role`; missing/non-string metadata → empty Role (read path stays robust, never errors on foreign metadata). **T1.4 must write the role via this same metadata key** (`bd update <id> --metadata '{"role":"..."}'` or on create).
> - bd JSON field map → `core.Issue`: `id`→ID, `title`→Title, `description`→Body, `metadata.role`→Role. bd emits a JSON *array* for ready/list AND for `show` (one-element array). bd writes `--json` to stdout, advisory warnings to stderr — the client parses stdout only and folds stderr into the error on non-zero exit. `core.Issue` was left unchanged (no Status/Tags field added yet — not needed for the ready→dispatch read path; model them in T1.4 when status transitions land).
> - Testing: unit tests drive the decode/mapping via an injectable `run` seam with canned bd JSON (fast, no bd needed); integration tests (`TestReadyIntegration` etc.) drive the **real `bd` binary** in a temp db — they `t.Skip` if `bd` is absent from PATH but run here. bd quirks learned: a temp dir whose name contains a `.` is rejected by bd as an "invalid database name" (`t.TempDir()` paths are fine); `bd q <title>` is the quiet create that outputs only the new ID. All 53 unit+integration tests pass; `make check` clean.
- [ ] **T1.4 beads single-writer transitions** — orchestrator-only status writes with lease/TTL on `in_progress`; apply validated proposals (create child issues + `blocked-by` edges) with acyclicity check. ([components/orchestrator.md](specs/components/orchestrator.md), [architecture.md](specs/architecture.md))
- [ ] **T1.5 Sandbox interface** — microVM-shaped from day one: explicit rootfs/worktree seeding, no casual bind mounts, local-socket I/O, resource limits, deterministic teardown. ([components/sandbox.md](specs/components/sandbox.md))
- [ ] **T1.6 Docker sandbox backend** — implements T1.5: seed writable worktree at brief base ref, unix-socket transport to runner, enforce limits, unconditional teardown. ([components/sandbox.md](specs/components/sandbox.md))
- [ ] **T1.7 Broker protocol** — local-socket RPC framing + request/response types for the brokered calls: model completion, git push, event publish. Deny-by-default; everything else rejected. ([messaging.md](specs/messaging.md), [components/runner.md](specs/components/runner.md))
- [ ] **T1.8 Provider adapter interface + Anthropic adapter** — canonical ↔ Anthropic wire (messages, tool defs, tool_use/tool_result, usage), streaming first-class. ([models.md](specs/models.md))
- [ ] **T1.9 OpenAI-compatible adapter** — covers OpenAI + local Ollama/vLLM via `endpoint`; canonical ↔ `tool_calls`/`tool` role + usage normalization. *(v1 per models.md; parallelizable with T1.8.)* ([models.md](specs/models.md))
- [ ] **T1.10 Model registry + resolution** — map `soul.model` → adapter (+ endpoint) from `infra.<env>.yaml`; keys from env, never config. (needs T1.8) ([configuration.md](specs/configuration.md), [models.md](specs/models.md))
- [ ] **T1.11 Runner: pull + provision** — JetStream pull consumer on its role(s); provision sandbox (T1.6), seed worktree, inject Brief + scoped git token; ack only after harvest (ack = the lease). ([components/runner.md](specs/components/runner.md))
- [ ] **T1.12 Runner: broker impl** — relay model completion (via adapter T1.10, runner holds key), git push (task branch only), event publish; log every call; tally `Usage` for budgets. ([components/runner.md](specs/components/runner.md), [security.md](specs/security.md))
- [ ] **T1.13 Agent inner loop** — boot soul, build context from Brief, ReAct loop {canonical request → broker model call → execute tool calls → append → budget check}; terminate on submit / budget / escalate. ([components/agent.md](specs/components/agent.md))
- [ ] **T1.14 Workspace tools** — `read_file`, `write_file`, `edit_file`, `list_dir`, `search`, `run` (build/test/lint) executing in-sandbox on the worktree. ([components/agent.md](specs/components/agent.md))
- [ ] **T1.15 Lifecycle tools** — `submit`, `escalate` (raise `needs-spec-clarification`), `request_subtask` (propose child issue); assemble the `Result` envelope. ([components/agent.md](specs/components/agent.md))
- [ ] **T1.16 Budget enforcement** — per-invocation turn/token caps enforced from tallied `Usage`; breach → stop/escalate. (The kernel's termination guarantee.) ([workflow.md](specs/workflow.md))
- [ ] **T1.17 Gate runner** — fresh verification sandbox (orchestrator-controlled, distinct from producer's), clean checkout of candidate branch, run `build` + `test`, report pass/fail. Producer ≠ verifier. ([verification.md](specs/verification.md))
- [ ] **T1.18 Artifact store interface + files backend** — content-addressed local files; runner harvests transcript + gate evidence before teardown, referenced by hash. (Minimal — full evidence set comes in Phase 2.) ([components/artifact-store.md](specs/components/artifact-store.md))
- [ ] **T1.19 Orchestrator loop** — reconciliation control loop: query ready → dispatch `work.<role>` → consume Result → validate proposals → gate (T1.17) → on accept: fast-forward merge + apply `produces:`; on reject: `on_failure`/retry; on breach: dead-letter (`harness.dlq` + mark blocked); sweep expired leases. Idempotent throughout. ([components/orchestrator.md](specs/components/orchestrator.md), [workflow.md](specs/workflow.md))
- [ ] **T1.20 Provenance trailer on merge** — write `Soul | Model | Issue | Prompt-SHA | Verified:` trailer; SHA/evidence hashes point into the artifact store (T1.18). ([security.md](specs/security.md), [integration.md](specs/integration.md))
- [ ] **T1.21 `harness` CLI** — `validate`, `run` (in-process orchestrator + one runner), `seed` (CLI stand-in for the wizard: write spec + create seed issue via the single-writer path). End-to-end: one spec → merged commit. ([bootstrap.md](specs/bootstrap.md))

**→ Self-host point reached.** From here the harness can implement Phases 2–5 as
beads issues, human reviewing diffs ("trusted-dev mode" per bootstrap.md).

---

## Phase 2 — Independent verification *(coarse)*

Earns no-human-review for non-TCB work. ([verification.md](specs/verification.md))

- [ ] Independent `author-tests` stage — a soul distinct from `implement` writes failing acceptance tests (defends against correlated errors).
- [ ] Red→green proof as a first-class postcondition (fail on base, pass on impl).
- [ ] Mutation testing postcondition + minimum-score gate. *(OPEN: score + operators.)*
- [ ] Independent scanners — `gosec` (SAST), dependency/vuln/license scans, policy-as-code.
- [ ] Test↔spec traceability map — per-test spec heading+sentence, harvested as evidence.
- [ ] Trusted-dev policy profile — human-approval postcondition for the self-hosting transition. *(OPEN, configuration.md.)*
- [ ] *(OPEN)* Second different-model reviewer soul in `qa` (N-version diversity).

## Phase 3 — Full DAG, decomposition & merge queue *(coarse)*

- [ ] Decomposition planner soul (`plan` stage) — reads seed issue + spec, emits `implement` issues with dependency edges.
- [ ] Emergent breadth — child-issue proposals validated for DAG-legality before write. ([workflow.md](specs/workflow.md))
- [ ] Spec slice resolution + context horizon — referenced file + linked neighbours to configured depth. ([specs-process.md](specs/specs-process.md))
- [ ] Spec-version pinning + recompile-the-delta — pin slice content hash per Brief; on spec edit, invalidate/re-derive affected issues. ([specs-process.md](specs/specs-process.md))
- [ ] Serialized merge queue — rebase onto current `main`, **re-gate the merged result**, conflict → sandboxed resolution issue. ([integration.md](specs/integration.md)) *(OPEN: integrate as its own role vs orchestrator function.)*
- [ ] Role→soul `selector` matching by issue tags; per-role model tiers. ([configuration.md](specs/configuration.md))

## Phase 4 — Control room *(coarse)*

Stack: templ + Tailwind standalone CLI + `embed.FS` + htmx/Alpine + SSE. ([control-room.md](specs/control-room.md), [observability.md](specs/observability.md))

- [ ] Web server scaffold + asset embedding (`go generate`: `templ generate` + Tailwind CLI).
- [ ] Board (kanban over beads, live via NATS→SSE).
- [ ] DAG view (server-side SVG, hover/drill via Alpine).
- [ ] Activity feed (NATS events → SSE).
- [ ] Issue/invocation detail (Brief, transcript, candidate diff, gate evidence, budget, retries).
- [ ] Dead-letter queue view — the primary human action surface.
- [ ] Budgets view (OTel metrics) + Provenance view (commit → issue→soul→model→prompt→evidence).
- [ ] Requirements **wizard** — steered conversation + alignment ledger + spec authoring + explicit-approval consent gate; Create and Resolve as one component. Trusted, not sandboxed. ([specs-process.md](specs/specs-process.md))
- [ ] OTel spans at broker/orchestrator/runner → trace backend; **replay** of an invocation's decision trail.

## Phase 5 — Production isolation & distribution *(coarse)*

- [ ] Firecracker sandbox backend (vsock transport, KVM microVM) — production target. ([components/sandbox.md](specs/components/sandbox.md))
- [ ] *(optional)* gVisor backend (medium-trust).
- [ ] Vetted package mirror/proxy — supply-chain mediation: pin, scan, log fetches. ([security.md](specs/security.md))
- [ ] Scoped short-lived secret minting — per-task git token that pushes only the task branch. ([components/runner.md](specs/components/runner.md))
- [ ] Distributed NATS (external cluster, JetStream replicas/retention) + runners across hosts.
- [ ] S3/MinIO artifact backend. ([components/artifact-store.md](specs/components/artifact-store.md))
- [ ] Provenance signing + key custody. *(OPEN, security.md.)*
- [ ] *(optional)* Warm sandbox pools; HA orchestrator via NATS-KV leader election. *(OPEN.)*

---

## Open decisions affecting the plan

These are `OPEN:` in the specs and may reshape tasks above:

- Mutation score threshold + operators (T2 mutation gate).
- `integrate` as its own role/soul vs. orchestrator-owned with sandboxed conflict help (Phase 3).
- HA orchestrator: single instance (fine for v1) vs. leader election (Phase 5).
- Condition-expression language for pre/postconditions (shell exit-code vs. CEL) — affects T1.2/T1.19.
- Exact module set drawn into the TCB boundary — must be pinned before autonomy is switched on for harness work.
