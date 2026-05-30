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

- **Completed tasks are collapsed to a one-line `— *done.*` checklist entry.** Phases
  0–1 are done (see Status); Phases 2–3 are done bar a few open items kept in full below.
  The verbose per-task findings were pruned once complete — that history lives in git,
  the code, and the specs they informed (each task updated its `(spec)` as it landed).
- **Open tasks (`- [ ]`) keep their full detail** — Phases 4–5, plus the handful left in
  Phases 2–3 (T2.10/T2.11/T2.12, T3.7b, T3.8b).
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

## Testing infrastructure (cross-cutting)

Verifies the kernel's *machinery* — routing, the tool contract, gating, merge,
provenance — deterministically and fast, independently of a capable runtime model.
Specs: [models.md](specs/models.md) (deterministically-fakeable), [components/sandbox.md](specs/components/sandbox.md)
(non-isolating local backend), [bootstrap.md](specs/bootstrap.md) (testing the spine).

- [x] **TE.1 Deterministic end-to-end spine test (fast, no Docker) + Docker variant** — *done.*

---

## Phase 2 — Independent verification

Turns the kernel's "implementor writes + grades its own tests" into a genuinely
independent, strong gate. This is what *earns* no-human-review — its full payoff
only lands once a capable model drives autonomous runs, but every gate here also
strengthens the human-reviewed loop. ([verification.md](specs/verification.md))

- [x] **T2.1 Postcondition-driven gate checks** — *done.*
- [x] **T2.2 Gate evidence persistence** — *done.*
- [x] **T2.3 Red→green proof postcondition** — *done.*
- [x] **T2.4 `author-tests` soul + persona** — *done.*
- [x] **T2.5 Flip `implement` to the red→green proof** — *done.*
- [x] **T2.6 Independent scanners as checks** — *done.*
- [x] **T2.7 Mutation-testing postcondition** — *done.*
- [x] **T2.8 Test↔spec traceability map** — *done.*
- [x] **T2.9 `qa` stage + soul** — *done.*
- [ ] **T2.12** *(optional)* Run-all independent scanners — aggregate every independent-scanner
  finding in one `qa` pass (better DLQ triage) instead of fail-fast, via a per-check
  "independent" config signal the gate honors (keeps proof/measurement checks fail-fast). See T2.9. ([verification.md](specs/verification.md))
- [ ] **T2.10 Trusted-dev policy profile** — a lighter policy profile with a human-approval postcondition for the self-hosting transition (a CLI `approve` command satisfies it). *(OPEN, configuration.md.)* ([bootstrap.md](specs/bootstrap.md), [configuration.md](specs/configuration.md))
- [ ] **T2.11** *(OPEN)* Second, different-model reviewer soul in `qa` (N-version diversity). ([verification.md](specs/verification.md))

## Phase 3 — Full DAG, decomposition & merge queue

Unwinds the kernel's single-stage, single-soul, trivial-merge simplifications.

- [x] **T3.1 Decomposition planner soul + `plan` stage** — *done.*
- [x] **T3.2 `beads.Apply` self-validates `DependsOn` existence** — *done.*
- [x] **T3.3 Stage ≠ role + selector matching** — *done.*
- [x] **T3.4 Per-role model tiers** — *done.*
- [x] **T3.5 Spec-slice resolution** — *done.*
- [x] **T3.6 Spec-version pinning** — *done.*
- [x] **T3.7 Recompile-the-delta** — *done.*
- [ ] **T3.7b Re-derive already-merged work on a spec edit** — when a spec edit drifts the pinned hash of
  a *closed/merged* issue, spawn new issue(s) for the delta. Deferred from T3.7 (underspecified) because
  it must: (a) dedupe across an epic's many closed issues that share one spec path (else one edit spawns
  N duplicate epics); (b) decide which stage to re-enter (pipeline entry / planner vs. author-tests);
  (c) stay idempotent (re-pin or mark the closed issues so they don't respawn every tick). Likely needs a
  beads query for closed issues by spec path. (needs T3.7) ([specs-process.md](specs/specs-process.md))
- [x] **T3.8 Cumulative per-issue budget** — *done.*
- [ ] **T3.8b Cumulative epic budget + cross-loop wall-clock** *(carried from T3.8)* — enforce the
  cross-issue `epic_budget` (sum spend over all issues of one epic) and a cumulative wall-clock cap.
  Both need design first: (a) **epic identity** — there is no epic id on issues today (an epic is
  implicit: a seed + all its `advance`/planner descendants), so this needs an `epic_id` metadata key
  threaded forward (like Base) or a beads ancestry query, then an aggregate-spend read. (b) **wall** —
  `core.Issue` has no `SpentWall` field and `core.Result` carries no invocation duration; the runner
  must stamp elapsed time and the orchestrator thread/accumulate it like Spent*. The per-invocation
  wall ceiling (sandbox limit) already exists; this is the cumulative cross-loop cap. (needs T3.8)
  ([workflow.md](specs/workflow.md))
- [x] **T3.9 Merge queue: serialized rebase onto `main`** — *done.*
- [x] **T3.10 Re-gate the merged result** — *done.*
- [x] **T3.11 Conflict-resolution issue** — *done.*

## Phase 4 — Control room

The human's read-only window + the wizard (their only action surface). Stack: templ
+ Tailwind standalone CLI + `embed.FS` + htmx/Alpine + SSE.
([control-room.md](specs/control-room.md), [observability.md](specs/observability.md))

- [x] **T4.1 Web server scaffold + asset pipeline** — *done.* `internal/controlroom`
  (`Server`: `http.ServeMux`, exact-match routes via Go 1.22+ method patterns, `Handler()`
  for httptest, `ListenAndServe(ctx, addr)` with ctx-driven graceful drain) + `harness serve`
  (`--addr`, SIGTERM = clean stop). Views are **templ** in `internal/controlroom/views`
  (base `Layout` + `Home`/`Placeholder`); nav is a single source of truth (`views.NavItems`)
  the server iterates to register placeholder routes. Assets embedded via
  `internal/controlroom/assets` (`//go:embed static`): vendored htmx 2.0.4 + htmx-ext-sse
  2.2.2 + Alpine 3.14.9 (committed) and Tailwind-compiled `app.css`. Build pipeline is
  `make generate` → `go generate` (`generate.go`): `templ generate` first (so `*_templ.go`
  carry the class strings), then the pinned Tailwind v4 standalone CLI (`make tailwind`
  fetches it into gitignored `bin/`; `app.tw.css` uses `@source` globs at the views dir).
  Generated `*_templ.go` + compiled CSS are committed so a plain `go build` needs no
  toolchain — the binary is self-contained. Later views (T4.2+) are thin templ panels
  rendered into `Layout`; SSE attaches via the already-loaded htmx-ext-sse (T4.3).
  ([control-room.md](specs/control-room.md))
- [x] **T4.2 Read/query layer** — *done.* `internal/controlroom/query`: a `Reader` over
  three ports (`IssueReader`, `ArtifactReader`, `ProvenanceReader`) returning presentation
  structs — `Board(stageOrder)` (issues grouped by stage, columns in pipeline order, empty
  stages skipped, unassigned last), `DeadLetters` (blocked issues — the action surface),
  `IssueDetail` (stitches beads + git provenance + artifact-store availability into named
  evidence links; merged work reads the trailer, in-flight falls back to the issue's
  TraceMap), `RecentProvenance`, and `Artifact` (content stream). Decoupled from views (no
  templ import); fully fake-tested. Supporting single-source moves: **`Provenance` (+ render
  `Trailer`/`CommitMessage` and new inverse `ParseCommitMessage`) relocated to `core`** so
  the orchestrator's write side and the control room's read side share one format —
  `GitProvenance` (shells out to `git log`, `run` seam for tests) parses it back. Added
  beads `List(status)` + `ListAll` (`bd list --all/--status --flat`; closed issues are
  hidden from bd's default list — these surface them). Promoted the artifact-kind taxonomy
  (`prompt`/`transcript`/`traceability-map`/`gate-evidence`) into `core` (was duplicated
  literals in runner/gate). **Not yet wired into the server** — the views consume the Reader
  in T4.4+. **T4.6 (DAG) will need dependency-edge reads** (`bd dep`), deliberately deferred
  from here since the board/DLQ/detail/provenance views don't need the edge list.
  ([control-room.md](specs/control-room.md), [observability.md](specs/observability.md))
- [ ] **T4.3 SSE plumbing** — NATS events → an SSE endpoint consumed by the htmx SSE extension; the live-update substrate for the board and feed. ([messaging.md](specs/messaging.md), [control-room.md](specs/control-room.md))
- [ ] **T4.4 Board view** — kanban over beads issues by stage, live via T4.3. (needs T4.2, T4.3) ([control-room.md](specs/control-room.md))
- [ ] **T4.5 Activity feed** — `harness.agent.<id>.events` streamed to the browser (what agents are doing right now). (needs T4.3) ([control-room.md](specs/control-room.md))
- [ ] **T4.6 DAG view** — the issue dependency graph rendered server-side to SVG (Go → DOT/d2), hover/drill via Alpine + htmx. No client-side graph lib. (needs T4.2) ([control-room.md](specs/control-room.md))
- [ ] **T4.7 Issue / invocation detail** — Brief, transcript, candidate diff, gate evidence, budget, retries, from beads + the artifact store. (needs T4.2) ([control-room.md](specs/control-room.md))
- [ ] **T4.8 Dead-letter queue view** — the escalations needing a human; the primary action surface; links into Resolve (T4.15). (needs T4.2) ([control-room.md](specs/control-room.md), [workflow.md](specs/workflow.md))
- [ ] **T4.9 OTel spans + export** — emit spans at the broker, orchestrator, and runner (boot, llm-turn, tool-call, gate-run) and metrics (latency, throughput, cost); export to a trace backend (Tempo/Jaeger). ([observability.md](specs/observability.md))
- [ ] **T4.10 Budgets + Provenance views** — budgets (token/$/wall burn vs. caps) from OTel metrics; provenance (trace a merged commit → issue → soul → model → prompt → evidence). (needs T4.2, T4.9) ([control-room.md](specs/control-room.md))
- [ ] **T4.11 Replay** — reconstruct an invocation's full decision trail from the broker-captured transcript + the artifact store, live or after the fact. (needs T4.7) ([observability.md](specs/observability.md))
- [ ] **T4.12 Requirements-planner conversation loop** — the trusted, **non-sandboxed** LLM that drives toward aligned, testable intent, streaming over SSE; reuses the canonical model layer. *(Needs a capable model at runtime.)* Scope note: control-room.md gives this planner three jobs — elicit testable intent (this task), author/maintain `specs/` markdown, and gate on human approval. The conversation loop is T4.12; the **spec-authoring persona and its link-integrity ownership** (specs-process.md: "every link resolves; every spec maps to ≥1 issue") land in **T4.14** — keep that validation a first-class postcondition on the planner's output there, not an afterthought. ([control-room.md](specs/control-room.md), [specs-process.md](specs/specs-process.md))
- [ ] **T4.13 Alignment ledger** — forks rendered as selectable chips (with tradeoffs); each item agreed/open with a one-line rationale; freeform typing always available. (needs T4.12) ([control-room.md](specs/control-room.md))
- [ ] **T4.14 Spec authoring + consent gate** — the planner drafts spec markdown + seed issues (keeping link integrity + the README index); on explicit human **APPROVE**, the spec is committed to git, the decisions sidecar written, the conversation transcript stored, and the seed issues created through the single-writer path. The single-writer seam already exists — `beads.Apply` (the validated, referential-integrity-checked write `cmd/harness/seed.go` already uses, "written exactly as the orchestrator would write"); the wizard reuses it, plus a `produces`-legality check on the batch mirroring `acceptPlan`'s validation of planner output. So no separate orchestrator-integration task is needed. (needs T4.12) ([specs-process.md](specs/specs-process.md), [control-room.md](specs/control-room.md))
- [ ] **T4.15 Resolve mode** — Create and Resolve are one component; Resolve pre-loads the escalation + spec slice + the agent transcript that raised it and shows the spec diff + blast radius before commit. (needs T4.14) ([control-room.md](specs/control-room.md), [specs-process.md](specs/specs-process.md))

## Phase 5 — Production isolation & distribution

Replaces the bootstrap stand-ins (Docker, in-process NATS, local-repo push, files
store) with the production stack.

- [ ] **T5.1 vsock broker transport** — `broker.Listen`/`Serve` over vsock and a `sandbox.Endpoint` of `vsock`+`cid:port` (currently unix-only); the transport Firecracker needs. ([messaging.md](specs/messaging.md), [components/runner.md](specs/components/runner.md))
- [ ] **T5.2 Firecracker sandbox backend** — a KVM-microVM backend implementing the `Backend`/`Sandbox` interface: rootfs seeding, vsock I/O (T5.1), resource limits incl. disk, deterministic teardown. The production isolation target. (needs T5.1) ([components/sandbox.md](specs/components/sandbox.md))
- [ ] **T5.3 Rootfs / base-image composition** — per-role toolchain images with the module/package cache baked in for offline (zero-network) builds. *(OPEN in sandbox.md.)* ([components/sandbox.md](specs/components/sandbox.md))
- [ ] **T5.4 Sandbox seeded-worktree ownership** *(carried from Phase 1)* — **drop the current workaround:** the bootstrap `go-toolchain` image relies on `git config --global --add safe.directory '*'` to tolerate the seeded worktree being owned by the host uid (the Docker backend seeds via `docker cp host/. container:workdir`, preserving host ownership; no `chown` today). Replace it by having the Docker backend `chown` the seeded worktree to the container user (and the Firecracker backend seed correct ownership), so the `safe.directory` / VCS-stamping crutch can be removed. ([components/sandbox.md](specs/components/sandbox.md))
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

- ~~Mutation score threshold + operators~~ — **decided (T2.9):** the kernel commits
  `mutation>=0.8` (>=, 0.8) on the live qa stage; still config, tunable per role.
- ~~Gate fail-fast vs. run-all for independent scanners~~ — **decided (T2.9): keep
  fail-fast.** Deliberate for proof/measurement checks (a mutation score is meaningless on
  red tests). Aggregating all independent-scanner findings in one pass (better DLQ triage)
  needs a per-check "independent" config signal and is filed as **T2.12** (optional).
- ~~`integrate` as its own role/soul vs. orchestrator-owned with sandboxed conflict help~~ —
  **decided (T3.11): orchestrator-owned.** The trusted layer does the rebase + final `git` write;
  only conflict resolution is handed to a role (the sandboxed `resolve` stage / `merge-resolver`
  soul), which proposes a rebased candidate the orchestrator re-gates and merges.
- HA orchestrator: single instance (fine for v1) vs. leader election (T5.11).
- Condition-expression language for pre/postconditions (shell exit-code vs. CEL) — affects
  config validation + the gate runner. T2.1 landed the shell-exit-code form: command-check
  postconditions resolve to commands via the `checks:` registry in `harness.yaml`; the gate
  runs them via `sh -c` (exit 0 = pass). `harness validate` still gates *bare* identifiers
  (reserved proofs, known metrics) against explicit registries that must be extended as new
  built-in check kinds are added. T2.3 added the red→green kind (reuses the `tests-pass`
  command, run against two refs); T2.7 added the metric-comparison kind (`mutation>=0.8`
  parsed by `core.ParseMetricComparison`, the score read from the registered command's
  stdout and graded by `core.CompareMetric`, failing closed on a nonzero exit or
  unparseable output).
- Rootfs / base-image composition per role (T5.3).
- Exact module set drawn into the TCB boundary — must be pinned before autonomy is switched on for harness work.
