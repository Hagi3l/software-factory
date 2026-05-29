# Implementation Plan

Build order for the harness, derived from [specs/](specs/README.md). The spine is
[bootstrap.md](specs/bootstrap.md): hand-build a minimal kernel that does
`spec → implement → gate → merge` for one issue (the **self-host point**), then
build out the full design.

## Status — self-host point reached; building by hand

Phases 0–1 are **complete**. The kernel does `spec → implement → gate → merge` for
one issue end-to-end: `cmd/harness` exposes `validate`/`seed`/`run`, and the
in-process orchestrator + runner carry a seed issue through implement → gate →
merge to `main` with a provenance trailer. Verified end-to-end against a real model
(local Ollama via `openai-compat`) and real Docker sandboxes.

**Development mode for Phases 2–5: built by hand with Claude Code, human-reviewed —
not self-hosted.** Bootstrap's threshold (a) ("the harness builds itself as a
trusted dev tool, human reviews diffs") assumes a capable model drives the
harness's *own* sandboxed agents; without a hosted key for one, that autonomous
path is deferred. The remaining work is ordinary Go / config / web development, so
it proceeds the same way the kernel was built. **Nothing below is *blocked* by the
missing key** — only the autonomous self-hosting milestone and the requirements
wizard (T4.12) need a capable model *at runtime*; everything builds and tests
offline.

## How to read this

- **Phases 0–1** are **complete** — see Status. Their per-task findings were pruned
  from this plan; that history lives in git, the code, and the specs they informed.
- **Phases 2–5 are atomic tasks** (`T<phase>.<n>`), each a single self-contained,
  verifiable unit of work, listed in dependency order — the same granularity Phase
  0–1 used and the natural unit for one Claude Code session. Cross-task deps are
  noted `(needs Tx.y)` where they aren't the obvious linear predecessor.
- `(spec)` links point at the authoritative contract for each task. If a task needs
  the design to change, **update the spec first.**
- `*(OPEN)*` marks a task whose shape is still undecided in the specs;
  `*(optional)*` marks a nice-to-have.

## The self-host milestone

The kernel from [bootstrap.md](specs/bootstrap.md) is: config → beads → sandbox →
runner/broker → agent loop → gate runner → orchestrator loop. Bootstrap
simplifications hold at the kernel and are unwound across Phases 2–5 — DAG collapses
to `implement → gate → integrate`, merge queue is trivial (single stream, no
rebase/re-gate), NATS is in-process, Docker stands in for Firecracker, no control
room (CLI-driven), the implementor writes its own tests.

## TCB caveat

Per [bootstrap.md](specs/bootstrap.md), the components that *enforce* the guarantees —
orchestrator, runner/broker, sandbox, gate harness — are the Trusted Computing Base.
**TCB-touching changes stay human-reviewed even after self-hosting.** Autonomy is
earned first for non-TCB work (new souls, stages, the control room). While Phases
2–5 are built by hand this is moot — everything is human-reviewed — but the boundary
matters the moment a capable model is wired and autonomy is switched on.

---

## Phase 2 — Independent verification

Turns the kernel's "implementor writes + grades its own tests" into a genuinely
independent, strong gate. This is what *earns* no-human-review — its full payoff
only lands once a capable model drives autonomous runs, but every gate here also
strengthens the human-reviewed loop. ([verification.md](specs/verification.md))

- [x] **T2.1 Postcondition-driven gate checks** — *done.* The gate now grades a
  candidate against the **stage's declared postconditions**, resolved through a config
  **check registry**. Learnings for downstream tasks:
  - The registry is `checks:` in `harness.yaml` (`config.Harness.Checks`,
    `map[name]command`); `gate.Registry.Resolve([]postconditions) → []Check` is the
    bridge, called inside `gate.Runner.Run` *before* provisioning (so a config fault or
    an empty-postcondition stage fails fast with no sandbox spent). `gate.Candidate`
    carries `Postconditions`; the orchestrator sets it from `stage.Postcondition` in
    `runGate`. Command + CLI flags `--gate-build/--gate-test` are gone — config is the
    single source of truth.
  - **Validate vs. gate gap to mind for T2.3/T2.7:** `harness validate` accepts
    reserved proofs (`tests-red-then-green`) and metric comparisons (`mutation>=0.8`)
    as known, but the gate has **no check kind for them yet** and `Resolve` errors
    loudly if asked to run one. No live gap — bootstrap `config/harness.yaml` only
    declares `tests-pass` — but T2.3 (red→green) and T2.7 (mutation) must add their
    check kinds to `gate` *and* keep them resolvable, not just validatable.
  - `internal/config/validate.go`: `reservedPostconditions` replaced the old
    `knownPostconditions`; `knownPostcondition` consults `Checks`; `isMetricComparison`
    is now reusable. Empty check commands are a validation error.
- [ ] **T2.2 Gate evidence persistence** *(carried from Phase 1)* — `gate.Runner` writes each check's captured stdout/stderr to the artifact store (the orchestrator gains an `artifact.Store`); `gate.Report` returns `ArtifactRef`s; the orchestrator cites the hashes in the `Verified:` provenance trailer. (needs T2.1) ([components/artifact-store.md](specs/components/artifact-store.md), [verification.md](specs/verification.md))
- [ ] **T2.3 Red→green proof postcondition** — the gate checks out the base ref as well as the candidate and requires the acceptance tests to **fail on base** and **pass on candidate** (proves the tests aren't vacuously green). New check type that runs against two refs. (needs T2.1) ([verification.md](specs/verification.md))
- [ ] **T2.4 `author-tests` soul + persona** — a `test-author` role/soul (config + markdown persona) that writes *failing* acceptance tests from the spec and never reads the implementation. ([configuration.md](specs/configuration.md), [verification.md](specs/verification.md))
- [ ] **T2.5 Wire `author-tests` into the DAG** — `author-tests` produces `implement`; the seed issue enters at `author-tests`; `implement`'s postcondition becomes `tests-red-then-green`; the implementor persona is updated to "make the existing tests pass, don't author them." (needs T2.3, T2.4) ([workflow.md](specs/workflow.md), [verification.md](specs/verification.md))
- [ ] **T2.6 Independent scanners as checks** — `gosec` (SAST), `govulncheck` (vuln), dependency/license scan, each a gate check emitting evidence. (needs T2.1, T2.2) ([verification.md](specs/verification.md), [security.md](specs/security.md))
- [ ] **T2.7 Mutation-testing postcondition** — integrate a Go mutation tool (e.g. `gremlins`), run as a gate check, gate on a minimum score. *(OPEN: score + operators — pick a default.)* (needs T2.1) ([verification.md](specs/verification.md))
- [ ] **T2.8 Test↔spec traceability map** — the test author emits, per test, the spec heading + sentence it claims to encode; harvested to the artifact store and surfaced in provenance. (needs T2.4) ([verification.md](specs/verification.md), [specs-process.md](specs/specs-process.md))
- [ ] **T2.9 `qa` stage + soul** — a `security`/QA role/soul (distinct from the implementor) whose gate runs the mutation + scanner postconditions in the clean verification sandbox; `on_failure: implement`, `produces: integrate`. (needs T2.6, T2.7) ([workflow.md](specs/workflow.md), [verification.md](specs/verification.md))
- [ ] **T2.10 Trusted-dev policy profile** — a lighter policy profile with a human-approval postcondition for the self-hosting transition (a CLI `approve` command satisfies it). *(OPEN, configuration.md.)* ([bootstrap.md](specs/bootstrap.md), [configuration.md](specs/configuration.md))
- [ ] **T2.11** *(OPEN)* Second, different-model reviewer soul in `qa` (N-version diversity). ([verification.md](specs/verification.md))

## Phase 3 — Full DAG, decomposition & merge queue

Unwinds the kernel's single-stage, single-soul, trivial-merge simplifications.

- [ ] **T3.1 Decomposition planner soul + `plan` stage** — a `planner` soul/persona that reads a seed issue + its spec slice and proposes child issues (with dependency edges) via `request_subtask`; `plan` produces `author-tests`; the seed enters at `plan`. ([workflow.md](specs/workflow.md))
- [ ] **T3.2 `beads.Apply` self-validates `DependsOn` existence** *(carried from Phase 1)* — a prefix-independent referential-integrity check on each proposed dep target, closing the bd-1.0.4 foreign-prefix gap (a hostile proposal naming a foreign-prefix dep is currently accepted silently). TCB beads code. ([architecture.md](specs/architecture.md), [workflow.md](specs/workflow.md))
- [ ] **T3.3 Stage ≠ role + selector matching** — a role maps to a *set* of souls; the orchestrator picks one per issue by matching the issue's tags against each soul's `selector`. Generalize the kernel's `stage==role` assumption (`stageForRole`). ([configuration.md](specs/configuration.md))
- [ ] **T3.4 Per-role model tiers** — resolve the model per issue from the selected soul, so cheap models serve easy roles and frontier models the hard ones. (builds on T3.3) ([models.md](specs/models.md), [configuration.md](specs/configuration.md))
- [ ] **T3.5 Spec-slice resolution** — a new `internal/spec` package that builds the bounded slice (referenced file + linked neighbours to a configured depth) and populates `Brief.Spec`, so the agent no longer reads the whole `specs/` tree from the worktree. ([specs-process.md](specs/specs-process.md), [components/agent.md](specs/components/agent.md))
- [ ] **T3.6 Spec-version pinning** — the Brief pins the content hash of its spec slice; the hash is stored on the issue. (needs T3.5) ([specs-process.md](specs/specs-process.md))
- [ ] **T3.7 Recompile-the-delta** — on a spec-file edit, the orchestrator diffs which issues referenced it and invalidates / re-derives the affected in-flight issues; already-merged work may spawn new issues for the diff. (needs T3.6) ([specs-process.md](specs/specs-process.md))
- [ ] **T3.8 Cumulative per-issue / epic budget** *(carried from Phase 1)* — surface `Usage` on the Result envelope (the runner already tallies it); the orchestrator accumulates spend across the `on_failure` loop per issue/epic and dead-letters on breach; a per-model cost table converts tokens → USD. ([workflow.md](specs/workflow.md))
- [ ] **T3.9 Merge queue: serialized rebase onto `main`** — pop `integrate` issues in issue-graph topological order and rebase each candidate onto the *current* `main` tip in a sandbox (replaces the kernel's bare provenance-commit advance). ([integration.md](specs/integration.md))
- [ ] **T3.10 Re-gate the merged result** — after rebase, re-run the full gate suite in a clean verification sandbox against the *rebased* result before advancing `main` (catches two-green-branches breakage). (needs T3.9) ([integration.md](specs/integration.md))
- [ ] **T3.11 Conflict-resolution issue** — on a rebase conflict, spawn a sandboxed resolution issue (proposes a rebase), block, loop. *(OPEN: `integrate` as its own role vs. orchestrator function.)* (needs T3.9) ([integration.md](specs/integration.md))

## Phase 4 — Control room

The human's read-only window + the wizard (their only action surface). Stack: templ
+ Tailwind standalone CLI + `embed.FS` + htmx/Alpine + SSE.
([control-room.md](specs/control-room.md), [observability.md](specs/observability.md))

- [ ] **T4.1 Web server scaffold + asset pipeline** — `internal/controlroom` + a `harness serve` command; `go generate` running `templ generate` + the Tailwind standalone CLI; `embed.FS` for htmx, Alpine, and compiled CSS; a base templ layout. Single self-contained binary, no runtime toolchain. ([control-room.md](specs/control-room.md))
- [ ] **T4.2 Read/query layer** — render-ready reads over beads + the artifact store + git provenance, decoupled from the views. ([control-room.md](specs/control-room.md), [observability.md](specs/observability.md))
- [ ] **T4.3 SSE plumbing** — NATS events → an SSE endpoint consumed by the htmx SSE extension; the live-update substrate for the board and feed. ([messaging.md](specs/messaging.md), [control-room.md](specs/control-room.md))
- [ ] **T4.4 Board view** — kanban over beads issues by stage, live via T4.3. (needs T4.2, T4.3) ([control-room.md](specs/control-room.md))
- [ ] **T4.5 Activity feed** — `harness.agent.<id>.events` streamed to the browser (what agents are doing right now). (needs T4.3) ([control-room.md](specs/control-room.md))
- [ ] **T4.6 DAG view** — the issue dependency graph rendered server-side to SVG (Go → DOT/d2), hover/drill via Alpine + htmx. No client-side graph lib. (needs T4.2) ([control-room.md](specs/control-room.md))
- [ ] **T4.7 Issue / invocation detail** — Brief, transcript, candidate diff, gate evidence, budget, retries, from beads + the artifact store. (needs T4.2) ([control-room.md](specs/control-room.md))
- [ ] **T4.8 Dead-letter queue view** — the escalations needing a human; the primary action surface; links into Resolve (T4.15). (needs T4.2) ([control-room.md](specs/control-room.md), [workflow.md](specs/workflow.md))
- [ ] **T4.9 OTel spans + export** — emit spans at the broker, orchestrator, and runner (boot, llm-turn, tool-call, gate-run) and metrics (latency, throughput, cost); export to a trace backend (Tempo/Jaeger). ([observability.md](specs/observability.md))
- [ ] **T4.10 Budgets + Provenance views** — budgets (token/$/wall burn vs. caps) from OTel metrics; provenance (trace a merged commit → issue → soul → model → prompt → evidence). (needs T4.2, T4.9) ([control-room.md](specs/control-room.md))
- [ ] **T4.11 Replay** — reconstruct an invocation's full decision trail from the broker-captured transcript + the artifact store, live or after the fact. (needs T4.7) ([observability.md](specs/observability.md))
- [ ] **T4.12 Requirements-planner conversation loop** — the trusted, **non-sandboxed** LLM that drives toward aligned, testable intent, streaming over SSE; reuses the canonical model layer. *(Needs a capable model at runtime.)* ([control-room.md](specs/control-room.md), [specs-process.md](specs/specs-process.md))
- [ ] **T4.13 Alignment ledger** — forks rendered as selectable chips (with tradeoffs); each item agreed/open with a one-line rationale; freeform typing always available. (needs T4.12) ([control-room.md](specs/control-room.md))
- [ ] **T4.14 Spec authoring + consent gate** — the planner drafts spec markdown + seed issues (keeping link integrity + the README index); on explicit human **APPROVE**, the spec is committed to git, the decisions sidecar written, the conversation transcript stored, and the seed issues created through the single-writer path. (needs T4.12) ([specs-process.md](specs/specs-process.md), [control-room.md](specs/control-room.md))
- [ ] **T4.15 Resolve mode** — Create and Resolve are one component; Resolve pre-loads the escalation + spec slice + the agent transcript that raised it and shows the spec diff + blast radius before commit. (needs T4.14) ([control-room.md](specs/control-room.md), [specs-process.md](specs/specs-process.md))

## Phase 5 — Production isolation & distribution

Replaces the bootstrap stand-ins (Docker, in-process NATS, local-repo push, files
store) with the production stack.

- [ ] **T5.1 vsock broker transport** — `broker.Listen`/`Serve` over vsock and a `sandbox.Endpoint` of `vsock`+`cid:port` (currently unix-only); the transport Firecracker needs. ([messaging.md](specs/messaging.md), [components/runner.md](specs/components/runner.md))
- [ ] **T5.2 Firecracker sandbox backend** — a KVM-microVM backend implementing the `Backend`/`Sandbox` interface: rootfs seeding, vsock I/O (T5.1), resource limits incl. disk, deterministic teardown. The production isolation target. (needs T5.1) ([components/sandbox.md](specs/components/sandbox.md))
- [ ] **T5.3 Rootfs / base-image composition** — per-role toolchain images with the module/package cache baked in for offline (zero-network) builds. *(OPEN in sandbox.md.)* ([components/sandbox.md](specs/components/sandbox.md))
- [ ] **T5.4 Sandbox seeded-worktree ownership** *(carried from Phase 1)* — the Docker backend `chown`s the seeded worktree to the container user (and the Firecracker backend seeds correct ownership), dropping the `safe.directory` / VCS-stamping workaround the bootstrap profile image relies on. ([components/sandbox.md](specs/components/sandbox.md))
- [ ] **T5.5** *(optional)* gVisor backend (medium-trust). ([components/sandbox.md](specs/components/sandbox.md))
- [ ] **T5.6 Vetted package mirror/proxy** — route package fetches through a pinning/scanning/logging proxy on the broker allowlist; a read-through cache amortizes downloads without weakening egress control. ([security.md](specs/security.md), [components/runner.md](specs/components/runner.md))
- [ ] **T5.7 Scoped short-lived secret minting** — the runner mints a per-task git token scoped to push *only* the task branch, injected for the invocation lifetime and dying with the sandbox (replaces the bootstrap local-repo push). ([components/runner.md](specs/components/runner.md), [security.md](specs/security.md))
- [ ] **T5.8 Distributed NATS** — an external cluster with concrete JetStream stream defs (retention / replicas / max-age — the messaging.md OPEN) and runners across hosts, swapped in for the embedded in-process server. ([messaging.md](specs/messaging.md))
- [ ] **T5.9 S3/MinIO artifact backend** — an `artifact.Store` implementation for distributed deployments (config `bucket`), shared across hosts and the control room. ([components/artifact-store.md](specs/components/artifact-store.md))
- [ ] **T5.10 Provenance signing + key custody** — sign commits/artifacts with the harness identity and verify on read. *(OPEN, security.md.)* ([security.md](specs/security.md))
- [ ] **T5.11** *(optional)* Warm sandbox pools + HA orchestrator via NATS-KV leader election. *(OPEN.)* ([components/runner.md](specs/components/runner.md), [components/orchestrator.md](specs/components/orchestrator.md))

---

## Open decisions affecting the plan

These are `OPEN:` in the specs and may reshape tasks above:

- Mutation score threshold + operators (T2.7).
- `integrate` as its own role/soul vs. orchestrator-owned with sandboxed conflict help (T3.11).
- HA orchestrator: single instance (fine for v1) vs. leader election (T5.11).
- Condition-expression language for pre/postconditions (shell exit-code vs. CEL) — affects
  config validation + the gate runner. T2.1 landed the shell-exit-code form: command-check
  postconditions resolve to commands via the `checks:` registry in `harness.yaml`; the gate
  runs them via `sh -c` (exit 0 = pass). `harness validate` still gates *bare* identifiers
  (reserved proofs, known metrics) against explicit registries that must be extended as new
  built-in check kinds are added (e.g. T2.3 red→green, T2.7 mutation).
- Rootfs / base-image composition per role (T5.3).
- Exact module set drawn into the TCB boundary — must be pinned before autonomy is switched on for harness work.
