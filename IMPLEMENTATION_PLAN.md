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
> - `bd` (beads CLI) is **not installed** — blocks T1.3/T1.4 (beads integration) until it's on PATH.
> - `golangci-lint` is **not installed** — `make lint` is unavailable until it's installed.
> - `.golangci.yml` is **stale**: its path/exclusion rules reference an `internal/controlplane/...` layout (server, auth, store, agent/controlplane) that does NOT match this repo's `internal/{config,beads,sandbox,runner,broker,agent,model,gate,orchestrator,artifact}` layout — it was templated from another project and currently matches nothing here. Prune or rewrite it when the control room (Phase 4) lands.

- [x] **T0.1 Go module + repo layout** — `go.mod`, `cmd/harness/`, `internal/{config,beads,sandbox,runner,broker,agent,model,gate,orchestrator,artifact}/`, build target (`make`/`just` running `go build`). ([architecture.md](specs/architecture.md))
- [ ] **T0.2 Core domain types** — `Soul`, `Brief`, `Result` (status: `done|failed|needs-spec-clarification`), `Issue`, status enum. Pure structs, no behaviour. ([components/agent.md](specs/components/agent.md))
- [ ] **T0.3 Canonical model types** — `ToolDef`, `ToolCall`, `ToolResult`, `Message`, `Response`, `Usage`, `StopReason`, `Role`. ([models.md](specs/models.md))
- [ ] **T0.4 In-process NATS + subject taxonomy** — embed `nats-server` (still spoken in-process per location transparency); define subject constants (`harness.work.<role>`, `harness.result.<role>`, `harness.agent.<id>.events`, `harness.dlq`, `harness.control.*`) and JetStream stream/consumer setup for work/result/dlq. ([messaging.md](specs/messaging.md))

## Phase 1 — Kernel → self-host point

- [ ] **T1.1 Config schema + loader** — structs + YAML unmarshal for `harness.yaml` (dag + policy), `souls/*.yaml`, `infra.<env>.yaml`; persona paths resolved to markdown files. ([configuration.md](specs/configuration.md))
- [ ] **T1.2 `harness validate`** — startup gate: every DAG role resolves to ≥1 soul; every `produces:`/`on_failure:`/condition target is defined; persona files exist; selectors well-formed; DAG not unreachable/trivially-looping. Fail loud. ([configuration.md](specs/configuration.md))
- [ ] **T1.3 beads read integration** — wrap `bd` to query ready work (no open blockers + precondition holds) and read issue fields into `Issue`/`Brief`. ([components/orchestrator.md](specs/components/orchestrator.md))
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
