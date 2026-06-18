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

**Phases 2, 3, 4, 6, and 7 are complete; only Phase 5 has live work, and all of it is
optional or hardware-blocked.** Phase 2 (independent verification) is complete — only
the optional T2.11 decision-note remains (resolved as configuration, not a build item).
Phase 3 (full DAG, decomposition, merge queue) is complete (T3.13 landed). Phase 4
(control room + Create/Resolve wizard) is complete. Phase 6 (agent semantic LSP tooling)
is complete (T6.1–T6.3). Phase 7 (atomic feature integration / epic mode) is complete
(T7.1–T7.8; the vault demo now runs `integration.mode: epic`). The only remaining
*engineering* of new substrate is Phase 5 (production isolation & distribution), and within
it every still-open item is either **optional** (T5.5 gVisor backend, T5.11 warm pools + HA
orchestrator) or **hardware-blocked** (T5.2 Firecracker, needs KVM the dev box lacks).

**Development mode for Phases 2–6: built by hand with Claude Code, human-reviewed —
not self-hosted.** Bootstrap's threshold (a) ("the harness builds itself as a
trusted dev tool, human reviews diffs") assumes a capable model drives the
harness's *own* sandboxed agents; without a hosted key for one, that autonomous
path is deferred. The remaining work is ordinary Go / config / web development, so
it proceeds the same way the kernel was built. **This project is about the *machinery*,
not model quality — nothing below is blocked for lack of a capable model.** A model is
already available in dev (the deterministic `modeltest` server; local Ollama via
`openai-compat`). The wizard and the autonomous self-hosting loop are buildable and
testable *offline*; the only thing a *capable* model gates is the subjective *quality*
of their outputs (good requirements elicitation, trustworthy auto-drafted specs, good
autonomous implementation) — a later validation concern, never an engineering blocker.

## How to read this

- **Completed tasks are collapsed to a one-line `— *done.*` checklist entry.** Phases
  0–1, 2, 3, 4, 6, and 7 are done; the verbose per-task findings were pruned once complete —
  that history lives in git, the code, and the specs they informed (each task updated its
  `(spec)` as it landed).
- **Open tasks (`- [ ]`) keep their full detail.** Only **Phase 5** (production
  isolation) has open lines — the Firecracker backend T5.2 is hardware-blocked and deliberately
  last; the optional items are T5.5 gVisor and T5.11 warm pools + HA. **Phase 7** (atomic feature
  integration / epic mode) is **complete** — T7.1 (the live multi-child-rebase bug),
  T7.2 (`integration.mode` config), T7.3 (merge-queue retargeting), T7.4 (epic-completion
  detection + terminal merge), T7.5 (one-active-epic consent gate + wizard creates the epic
  branch with the spec), T7.6 (board epic hero card), T7.7 (vault demo exercises epic mode),
  and T7.8 (board epic-lineage thread + decoupled grouping chrome) are all done.
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
- **Known flake (infra, not a code bug):** `internal/runner.TestTeardownRunsEvenWhenInvokeErrors` (and
  occasionally its NATS-backed siblings) can stall under the *full-suite* `go test ./...` parallel load —
  many packages each spin an embedded NATS server, and the redelivery-loop teardown becomes timing-sensitive
  under contention — surfacing as a 10-minute package timeout. It passes deterministically in isolation
  (`go test ./internal/runner/` ≈ 0.25s). If `make check` times out here, re-run; a real fix would cap test
  parallelism (`go test -p`) or give the embedded-NATS runner tests a tighter per-test deadline. Not caused by
  any feature work; flagged so a future loop does not chase it as a regression.

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
- [x] **T2.12 Run-all independent scanners** — *done.*
- [x] **T2.10 Trusted-dev policy profile** — *done.*
- [ ] **T2.11** *(optional)* **N-version model diversity — resolved as configuration, not a built-in mechanism.**
  Soul independence (`producer ≠ verifier`) is already enforced; running the verifier on a *different model
  family* than the producer is a **config capability** the harness already provides (a role maps to a set of
  souls via `selector` (T3.3), each soul names its own model/tier (T3.4)), consistent with the config-is-the-
  pipeline principle — the harness *enables and recommends* diversity but the model assignment is the user's.
  No bespoke "second reviewer soul" mechanism is needed. The one piece of buildable work this leaves — a
  non-fatal validation warning on producer/verifier model-family overlap — is tracked as **T2.13**. Decision
  recorded in verification.md ("Model diversity is configured, not mandated").
  ([verification.md](specs/verification.md), [configuration.md](specs/configuration.md))
- [x] **T2.13 Producer/verifier model-family diversity warning** — *done.*
- [x] **T2.14 `golangci-lint` gate check + the producer-self-check principle** — *done.*

## Phase 3 — Full DAG, decomposition & merge queue

Unwinds the kernel's single-stage, single-soul, trivial-merge simplifications.

- [x] **T3.1 Decomposition planner soul + `plan` stage** — *done.*
- [x] **T3.2 `beads.Apply` self-validates `DependsOn` existence** — *done.*
- [x] **T3.3 Stage ≠ role + selector matching** — *done.*
- [x] **T3.4 Per-role model tiers** — *done.*
- [x] **T3.5 Spec-slice resolution** — *done.*
- [x] **T3.6 Spec-version pinning** — *done.*
- [x] **T3.7 Recompile-the-delta** — *done.*
- [x] **T3.7b Re-derive already-merged work on a spec edit** — *done.*
- [x] **T3.8 Cumulative per-issue budget** — *done.*
- [x] **T3.8b Cumulative epic budget + cross-loop wall-clock** — *done.*
- [x] **T3.9 Merge queue: serialized rebase onto `main`** — *done.*
- [x] **T3.10 Re-gate the merged result** — *done.*
- [x] **T3.11 Conflict-resolution issue** — *done.*
- [x] **T3.12 Orchestrator in-flight projection (consistency under eventually-consistent beads reads)** — *done.*
- [x] **T3.13 Split the sweep cadence off the dispatch tick** — *done.*

## Phase 4 — Control room

The human's read-only window + the wizard (their only action surface). Stack: templ
+ Tailwind standalone CLI + `embed.FS` + htmx/Alpine + SSE.
([control-room.md](specs/control-room.md), [observability.md](specs/observability.md))

- [x] **T4.1 Web server scaffold + asset pipeline** — *done.*
- [x] **T4.2 Read/query layer** — *done.*
- [x] **T4.3 SSE plumbing** — *done.*
- [x] **T4.4 Board view** — *done.*
- [x] **T4.5 Activity feed** — *done.*
- [x] **T4.6 DAG view** — *done.*
- [x] **T4.7 Issue / invocation detail** — *done.*
- [x] **T4.7b Surface transcript + candidate diff on the detail page** — *done.*
- [x] **T4.8 Dead-letter queue view** — *done.*
- [x] **T4.9 OTel spans + export** — *done.*
- [x] **T4.10 Budgets + Provenance views** — *done.*
- [x] **T4.11 Replay** — *done.*
- [x] **T4.12 Requirements-planner conversation loop** — *done.*
- [x] **T4.13 Alignment ledger** — *done.*
- [x] **T4.14 Spec authoring + consent gate** — *done.*
- [x] **T4.15 Resolve mode** — *done.*

### Live transition events + board-in-motion (T4.16–T4.18)

Implements the now-**decided** control-room refinement (was the "coarse live trigger →
precise issue-state event" OPEN): the single-writer orchestrator emits a typed
transition event the board/DAG/DLQ refresh off, giving crisp **animated card moves**
and the anchor for **per-card timers** (time-in-state + total). The three tasks split
backend-emit / transport / frontend-consume, the same way T4.3 (SSE plumbing) and T4.4
(board) were split. Resolves [control-room.md](specs/control-room.md) "The board, in
motion"; specs already updated (orchestrator.md §9, messaging.md `issue.<id>.state`,
control-room.md, observability.md).

- [x] **T4.16 Issue-state transition events + `state_entered_at` stamp** — *done.*
- [x] **T4.17 Issue-state SSE pump** — *done.*
- [x] **T4.18 Board in motion — crisp refresh, animated moves, per-card timers** — *done.*

### Observability surfaces (T4.19–T4.25)

Four new control-room surfaces decided in the post-T4.18 spec pass — the
[live invocation view](specs/control-room.md), the [verification view](specs/control-room.md),
the [merge-queue view](specs/control-room.md), and the [status bar + escalation
alerts](specs/control-room.md). Each is backed by a **named, persisted or emitted**
thing (an enriched event envelope, a `gate-verdict` artifact + recorded souls,
`merge-state` events) rather than a live scrape — so every surface stays inside the
existing invariants (single-writer, producer≠verifier, forensic-unless-running). The
tasks follow the same **emit / transport / consume** split as T4.16–T4.18 and are
listed in dependency order: the cheap status/DLQ surface first, then live invocation
(after the event envelope carries issue/role), then verification (after its gate-verdict
+ soul plumbing), then the merge queue (after merge-state events). Specs already updated
(control-room.md, messaging.md, verification.md, integration.md, security.md,
components/artifact-store.md, glossary.md).

- [x] **T4.19 Status bar + DLQ escalation alerts** — *done.*
- [x] **T4.20 Agent-event envelope: issue id + role** — *done.*
- [x] **T4.21 Live invocation view** — *done.*
- [x] **T4.22 Gate-verdict record + producing-soul attribution** — *done.*
- [x] **T4.23 Verification view** — *done.*
- [x] **T4.24 Merge-state transition events** — *done.*
- [x] **T4.25 Merge-state SSE pump + merge-queue view** — *done.*
- [x] **T4.26 Config view** — *done.*
- [x] **T4.27 Ledger: batched forks, free-text, discuss/defer states + soft approval gate** — *done.*
- [x] **T4.28b Configurable exploration depth + live "it's working" progress** — *done.*
- [x] **T4.29 Wizard structured output via tool calls (replace fenced blocks)** — *done.*
- [x] **T4.30 Board auto-scroll to the work frontier** — *done.*
- [x] **T4.31 Live public-repo vault demo (GitHub push + deploy) + descriptive commit subjects** — *done.*
- [x] **T4.32 Wizard edits existing specs + maintains the README index (new-specs-only coverage)** — *done.*
- [x] **T4.32a Approve-view diff for spec edits** — *done.*

## Phase 5 — Production isolation & distribution

Replaces the bootstrap stand-ins (Docker, in-process NATS, local-repo push, files
store) with the production stack.

**Prioritization (decided 2026-06-04): the Firecracker backend (T5.2) is the *lowest*
priority of Phase 5 — moved to the end of the list below.** Every other Phase-5 task
(and all of Phase 6) is buildable and testable in this dev environment: the vsock
transport landed against a real loopback (T5.1), the base images, package mirror,
scoped secrets, distributed NATS, S3 store, and signing are ordinary Go/infra work, and
Docker remains a satisfactory `Backend` for development and human-reviewed runs.
Firecracker alone needs **KVM / bare-metal-or-nested-virt hardware that this environment
does not provide**, so it can be neither exercised nor verified here — building it now
would mean shipping an untested microVM backend ahead of the substrate that *is*
verifiable. Nothing depends on it: T5.1 is the only piece Firecracker *needed*, the
remaining tasks target the transport/security/distribution layer the Docker backend also
uses, and Phase 6 is explicitly Firecracker-independent. So the build order is now
**T5.1 (done) → the rest of Phase 5 + Phase 6 → T5.2 last**, switched on when capable
isolation hardware is available. The tasks keep their original IDs (other tasks reference
them); only the *order of attention* changes.

- [x] **T5.1 vsock broker transport** — *done.*
- [x] **T5.3 Rootfs / base-image composition** — *done.*
- [x] **T5.3a Harness kernel passes its own `gosec` gate (self-host readiness)** — *done.*
- [x] **T5.4 Sandbox seeded-worktree ownership** *(carried from Phase 1)* — *done.*
- [ ] **T5.5** *(optional)* gVisor backend (medium-trust). ([components/sandbox.md](specs/components/sandbox.md))
- [x] **T5.6 Package proxy on the broker allowlist** — *done.*
- [x] **T5.6a Gate-verifier package egress** — *done.*
- [x] **T5.7 Scoped short-lived secret minting** — *done.*
- [x] **T5.8 Distributed NATS** — *done.*
- [x] **T5.9 S3/MinIO artifact backend** — *done.*
- [x] **T5.10 Provenance signing + key custody** — *done.*
- [ ] **T5.11** *(optional)* Warm sandbox pools + HA orchestrator via NATS-KV leader election. *(OPEN.)* ([components/runner.md](specs/components/runner.md), [components/orchestrator.md](specs/components/orchestrator.md))
- [ ] **T5.2 Firecracker sandbox backend** ***(lowest priority — see the Phase 5 prioritization note above; deliberately last)*** — a KVM-microVM backend implementing the `Backend`/`Sandbox` interface: rootfs seeding, vsock I/O (T5.1, done), resource limits incl. disk, deterministic teardown. The production isolation target. **Blocked on hardware, not on code:** needs KVM (bare-metal or nested virt) that the dev environment lacks, so it cannot be built-and-verified here — do it only once such hardware is available, after the rest of Phase 5 + Phase 6 (all of which the Docker backend supports for dev/human-reviewed runs). Kept as ID T5.2 (referenced elsewhere) despite its end-of-list position. (needs T5.1) ([components/sandbox.md](specs/components/sandbox.md))

## Phase 6 — Agent semantic tooling (LSP)

Replaces the agent's text-only `search`/`edit_file` floor with **intent-first, LSP-backed**
comprehension and transformation tools: the agent states *what* it wants (find this symbol,
rename this) and the trusted tool layer resolves it **LSP-first with a text fallback** — it
never picks the mechanism, so "prefer semantic, fall back to grep/sed" is a structural property,
not a persona nudge. The surface is **language-neutral**; the backing server is per-language,
resolved from the sandbox image (T5.3) — the same canonical-interface / thin-adapter split the
model layer uses (*provider adapter : model :: language server : semantic tool*). Buildable and
testable in dev against a `go-toolchain` image carrying `gopls`; **does not depend on Firecracker**
(T5.2). New spec contract: [components/agent.md](specs/components/agent.md) "Semantic tools
(LSP-backed)" + [components/sandbox.md](specs/components/sandbox.md) "Per-language language server".
**Demo scope (Go+htmx+templ+tailwind):** ship the `go` (gopls) `languageId` entry only — `.templ`
and `.css` ride the text floor (templ compiles to `_templ.go`, which gopls already sees; tailwind
is a build step, not a navigable language).

- [x] **T6.1 Per-language LSP session manager** — *done.*
- [x] **T6.2 Comprehension (read) semantic tools** — *done.*
- [x] **T6.3 Transformation (write) semantic tools** — *done.*

---

## Phase 7 — Atomic feature integration (epic mode)

Adds an opt-in **`integration.mode: epic`** that lands a whole feature **atomically**:
children integrate onto an `epic/<epic_id>` branch and `main` advances exactly once, by
the epic's terminal merge, when the epic's subtree drains. Default **`per-item`** is the
existing kernel behaviour, untouched. Motivation: a `main` push triggers a deploy, so the
unit of integration should be the *feature* the human drafted in the wizard, not the
individual work item. Reuses existing primitives — the serialized merge queue, the
`resolve` stage, and the `epic_id` already threaded across every issue (the
[`epic_budget`](specs/workflow.md) key) — so the change is mostly *retargeting* the queue
per epic, not new machinery. Spec contract:
[integration.md](specs/integration.md) "Atomic feature integration (epic mode)",
[configuration.md](specs/configuration.md), [control-room.md](specs/control-room.md)
"Epics on the board", [components/orchestrator.md](specs/components/orchestrator.md). The
**vault demo exercises it** (T7.7).

- [x] **T7.1 Fix the re-gate ref form (unblocks any multi-child rebase)** — *done.*
  `rebaseOntoMain` now returns the **short** branch name `integration/<id>` (new
  `integrationBranch` helper) as the re-gate's ref, while keeping the fully-qualified
  `refs/heads/integration/<id>` (`integrationRef`, refactored to build on the short form) for
  the `update-ref` publish + `update-ref -d` cleanup, which do not DWIM. The re-gate's sandbox
  seeds by `git clone` + `git checkout <ref>`, and a clone has no local `refs/heads/*` (only
  `origin/*`), so only the short name DWIM-resolves there — exactly as the candidate gate uses
  `candidate/<id>`. Fixes the `pathspec … did not match` loop that hung any multi-child rebase,
  and is the foundation epic mode's per-child re-gate leans on. Regression: `TestGitMergerReGatesRebasedResult`
  now asserts the re-gate is called with `integration/iss-1`; the real-git `merge_integration_test.go`
  rebase path exercises it end-to-end. No spec/doc change (bug fix to already-documented intent).
- [x] **T7.2 `integration.mode` config + validation** — *done.* Added the optional top-level
  `integration: { mode: per-item | epic }` block (`config.Integration`, `Harness.Mode()`
  accessor defaulting to `per-item` so an absent block and explicit `per-item` are identical).
  `validateIntegration` rejects any mode outside `{per-item, epic}` (same enum pattern as the
  policy profile). The full `Config` is already threaded to the orchestrator (`Options.Config`),
  so `cfg.Harness.Mode()` is reachable for T7.3. Docs: `docs/configuration.md` gained an
  `integration` section. Tests: valid/empty/epic accepted, unknown rejected, `Mode()` default.
- [x] **T7.3 Retarget the merge queue per epic** *(needs T7.1, T7.2)* — *done (merge-queue
  retargeting; the wizard spec-commit-onto-branch rides with T7.5).* The `Merger.Merge`
  signature gained a fully-qualified `target` ref; everywhere the queue read `refs/heads/main`
  it now reads `target` — rebase target, fast-forward/ancestor check, idempotency log grep,
  conflict detection, and the advance `update-ref` (all inside `Merge`, so one parameter covers
  the spec's first four retarget points). The orchestrator computes the per-issue target via
  `integrationTargetRef`/`integrationBranchName` (new `internal/orchestrator/epic.go`):
  `refs/heads/main` in per-item, `refs/heads/epic/<core.EpicOf(issue)>` in epic mode. `Merge`
  **creates the epic branch off `main` on first use** (idempotent — the only writer of
  integration branches), so child integration works without a pre-existing branch. The
  **resolve-stage rebase** is retargeted in both phases: the trusted re-rebase rides the `target`
  param, and the merge-resolver **agent** now reads its rebase target from a new
  `core.Brief.IntegrationBase` (short branch name, surfaced in the agent prompt's
  `# Integration branch` section) — so a conflicting candidate rebases onto the epic branch
  (where its colliding sibling lives), not `main`. The merge-resolver persona (config + demo)
  was generalized from literal `main` to "the integration branch named in your Brief". Tests:
  `epic.go` helpers (both modes incl. root-folds-into-own-epic), `buildBrief` IntegrationBase,
  agent `buildContext` surfacing, an orchestrator merge-flow test asserting the epic target, and
  a real-git `TestGitMergerEpicTargetIntegration` (two siblings land on an auto-created epic
  branch; **`main` never moves**). Spec note added (merge queue idempotently creates the branch).
  **Deferred to T7.5:** committing the wizard's approved spec *onto* the epic branch as its first
  commit (needs the epic-root id at seed time + the one-active-epic consent gate that defines
  "an epic"); until then epic mode commits the spec to `main` and the epic branch inherits it, so
  the merge mechanics are correct but full spec-off-main atomicity awaits T7.5. *(spec)*
  [integration.md](specs/integration.md)
- [x] **T7.4 Epic-completion detection + terminal merge** *(needs T7.3)* — *done.* New
  `sweepEpicCompletion` (`internal/orchestrator/epic_completion.go`) runs **only under
  `integration.mode: epic`**, on the **slow sweep cadence** alongside `recompileMergedDelta`
  (wired into `tickLoop`'s startup pass + slow-tick branch). It does an `epic_id` **aggregate**
  read (`ListAll` grouped by `core.EpicOf`) and lands a feature when its subtree has **drained**:
  every issue `closed` **and** no member in the in-flight projection. The in-flight clause uses
  `o.inflight.issues()` (the single writer's read-your-writes record) to close the window where a
  just-spawned child is not yet visible in the eventually-consistent `ListAll` but its in-flight
  parent is. **All-or-nothing falls out of the drain test**: a blocked (dead-lettered) or still-open
  child means `drained=false`, so the terminal merge never fires and the epic branch is abandoned
  (`main` untouched). On drain, `terminalMerge` calls the new `Merger.MergeEpic` — a **two-parent
  merge commit** on `main` (first parent `main`, second the epic tip via `git commit-tree -p main
  -p epic`, tree = epic tip's tree), subject = the epic **root's** title, trailer cites the **epic
  id** (whole-feature layer; per-child provenance stays reachable under the second parent —
  two-tier). v1 skips re-gating at the terminal step (`main` quiescent — the last child's rebased
  re-gate already verified the whole feature). **Idempotent**: `MergeEpic` greps `main` for a commit
  citing the epic id and returns `merged=false` if already landed, so the steady-state slow sweep is
  a cheap silent re-check (the root stays closed, so drain is re-detected forever); a `merged=true`
  landing announces `MergeStateLanded` keyed by the epic root (merge-queue view) and logs. Defensive
  `epicTip == mainTip` no-op guards an empty merge. Tests: real-git
  `TestGitMergerEpicTerminalMergeIntegration` (two children land on `epic/feat-1`, `main` quiescent,
  terminal merge has the right two parents/tree/subject/trailer, per-child history reachable,
  repeated call no-ops) + orchestrator sweep tests (`epic_completion_test.go`: drain lands with
  whole-feature provenance, per-item no-op, all-or-nothing for blocked/open/in-progress children,
  in-flight member waits, missing root waits, idempotent `merged=false` absorbed). Docs:
  `docs/pipeline.md` gained a "Where `integrate` lands" epic-mode paragraph. Spec
  ([components/orchestrator.md](specs/components/orchestrator.md) §7,
  [integration.md](specs/integration.md) "Atomic feature integration") already documented this — no
  spec change. **Deferred to T7.5:** the wizard creating `epic/<id>` with the spec as its first
  commit + the one-active-epic consent gate; until then the merge mechanics are complete but the
  spec rides in via `main` inheritance (T7.3's note).
- [x] **T7.5 One-active-epic consent gate + wizard creates the epic branch with the spec**
  *(needs T7.4)* — *done.* Two halves landed in the Create-Task wizard's seeder
  (`cmd/harness/wizard_seed.go`). **One-active-epic consent gate** (`ensureNoActiveEpic`): under
  `integration.mode: epic`, `Seed` refuses a second approval while an epic is in flight, before any
  write. It reads `beads.ListAll`, groups by `core.EpicOf`, and an epic is "active" if any member is
  not `closed` (work in flight) **OR** its subtree has drained but its terminal merge has not yet
  landed it on `main`. The drained-but-unlanded clause (checked via `epicLandedOnMain`, which greps
  `main` for a provenance trailer citing the epic id — the same idempotency check `MergeEpic` uses)
  is load-bearing: seeding a second epic in that window would cut it from a `main` lacking the first
  feature, and the first's terminal merge (whose tree is the epic branch's) would later revert it.
  The refusal names the in-flight feature (root issue title + id). **Single root for v1:** `validate`
  requires exactly one seed issue in epic mode (the epic id is that root's id; more roots would mint
  multiple epic branches + terminal merges); per-item mode is unchanged. **Spec onto the epic branch**
  (`seedEpic` + `commitOntoEpic`): epic mode swaps the last two Seed steps — create the seed issue
  first to learn the epic-root id (= epic id), then commit the spec+sidecar onto a fresh
  `epic/<epic_id>` branch cut from `main`, **not** onto `main`. Done with git plumbing (`add` →
  `write-tree` → `commit-tree -p main` → `update-ref` → `reset -- <files>`) so `main` and the
  working-tree HEAD never move; the working-tree spec files are left on disk (unstaged). **Key
  discovery driving the design:** the orchestrator resolves an issue's spec slice by reading the
  repo's *working tree* (`spec.Resolve(o.opts.Repo, ...)` → `os.ReadFile`), and child sandboxes base
  off `main` (code). So the spec must stay readable from the working tree even though it is committed
  only on the epic branch — hence `commitOntoEpic` leaves the spec files in the working tree
  (uncommitted relative to main) rather than checking out the epic branch. Children get the spec via
  their Brief (read from the working tree); their candidates integrate onto the epic branch; the
  terminal merge brings spec+code to `main` once. No orchestrator changes were needed. **Single
  source of truth:** added `core.EpicBranch(epicID)` (the `epic/<id>` name), now used by both the
  orchestrator (`orchestrator.epicBranch` delegates to it) and the wizard. The merge queue's
  idempotent branch-create (T7.3) is now a no-op for an epic the wizard already opened. Docs:
  `docs/control-room.md` APPROVE step gained an epic-mode paragraph; spec (integration.md "The epic
  branch", control-room.md) already documented this — no spec change. Tests
  (`cmd/harness/wizard_seed_test.go`): `TestSeedEpicCommitsSpecOntoEpicBranch` (spec on epic branch
  cut from main, main quiescent, spec **not** on main, spec readable in working tree, root has empty
  EpicID), `TestValidateEpicRequiresSingleRoot`, `TestSeedEpicRefusesSecondInFlight`,
  `TestSeedEpicGateTracksLanding` (drained-but-unlanded refused; admitted once a terminal-merge
  trailer is on main). *(spec)* [integration.md](specs/integration.md),
  [control-room.md](specs/control-room.md)
- [x] **T7.6 Board epic hero card** *(needs T7.4)* — under `integration.mode: epic` the board
  makes the feature legible with no new data (rides existing `epic_id` via `core.EpicOf`).
  `query.Board` gained an `epicMode bool` + `query.BudgetCaps` param (server passes a new
  `s.epicMode()` reading `cfg.Harness.Mode() == config.IntegrationEpic`, plus `s.budgetCaps`);
  in epic mode every `IssueCard` carries its shared `EpicID` and the epic **root** card a new
  `*query.EpicSummary` hero roll-up (Integrated/Total closed-vs-all, Tokens/USD summed marginal
  Closing* spend matching Budgets + authorizeEpic, caps vs `epic_budget`, State). State is
  `integrating`, flipping to `done` only when the terminal merge has landed on `main` — read
  via the new `Reader.landedOnMain` (`prov.ByIssue(epicID)` greps `main`), best-effort. The
  view (`board.templ`/`board.go`) renders an **epic badge** on every card, a hue-hashed
  left-border tint (`cardStyle`/`epicHue`: FNV-1a → injection-free `hsl()` SafeCSS, colour
  never the sole channel), and an `epicHero` block (state badge, integrated X/Y + progress bar
  via `epicProgressWidth`, spend vs cap when capped); per-item mode renders none of it. `Board`
  echoes an `EpicMode` field. Templ + Tailwind regenerated. Tests: `query_test.go` (badge+hero
  2/3 progress + summed spend, done-on-terminal-merge via fake prov, per-item no-chrome) and
  `board_test.go` (rendered badge/tint/hero/`2/3`/`integrating`, per-item no chrome). Docs:
  `docs/control-room.md` Board row gained an epic-mode paragraph. *(spec)*
  [control-room.md](specs/control-room.md)
- [x] **T7.7 Vault demo exercises epic mode** *(needs T7.4)* — *done.* Added the
  `integration:\n  mode: epic` block to `demo/vault/config/harness.yaml` (with a load-bearing
  comment: the operator drafts a *feature*, so the feature is the unit of integration **and** of
  deploy — children integrate onto `epic/<id>`, `main` advances once at the terminal merge when
  the subtree drains, so `run.sh`'s push watcher fires **one** deploy per feature). The deploy
  path needed **zero** code changes — `run.sh`'s `push_main_watcher` and `deploy.yml`'s
  `on: push: branches: [main]` already key on a `main` advance, which epic mode makes happen
  once per feature. Verified with `harness validate --config demo/vault/config` → `OK` (the lone
  warning is the pre-existing T2.13 producer/verifier family-overlap advisory, unrelated). Docs:
  `demo/vault/README.md` updated — the DAG diagram now shows children landing on the epic branch
  + a terminal `land` step, the post-diagram paragraph explains epic mode + one-deploy-per-feature,
  the draft-feature step notes the board epic hero card and the single epic-id provenance trailer,
  the deploy bullet notes the once-per-feature push, and the "How it maps to the real config" table
  gained a third row (per-item → `integration.mode: epic`). No spec change (integration.md already
  specifies the demo exercises epic mode). **Phase 7 core complete** (T7.1–T7.7); the board
  epic-lineage enhancement is tracked as **T7.8** below. *(spec)*
  [integration.md](specs/integration.md)
- [x] **T7.8 Board epic-lineage thread + decoupled grouping chrome** *(needs T7.6)* — *done.*
  Grouping chrome is now driven by data, not `integration.mode`: `query.Board` populates
  `IssueCard.EpicID` (badge + tint) and a new `IssueCard.ParentID` (lineage edge) whenever an issue
  belongs to a *multi-issue* epic (gate `epicCounts[ep] > 1`), in per-item and epic modes alike,
  while a lone single-issue epic stays bare and the hero roll-up (`IssueCard.Epic`) stays
  epic-mode-only. One colour source: `cardStyle` (board.go) publishes the FNV-hashed hue once as the
  `--epic` CSS custom property, and the left-border tint, the new badge dot (`epicDotStyle`), and the
  JS thread strokes all read `var(--epic)` (the JS never re-hashes). `ParentID` is derived with no new
  beads data via `parentOf` (Base `candidate/<id>` → that producer; else non-root child → epic root;
  root → none); cards emit `data-epic` + `data-parent` and a new embedded
  `internal/controlroom/assets/static/lineage.js` draws a bespoke SVG bézier overlay inside the
  `[data-board-scroll]` content-space container — faint by default, highlighting the whole path through
  a card (ancestors + descendants) on hover/focus, redrawing on `htmx:afterSwap` + resize, honoring
  `prefers-reduced-motion`, terminating at the qa card (blocked-by edges not drawn). Tests:
  `query_test.go` (`TestBoardPerItemModeGroupingButNoHero`, `TestBoardSingleIssueEpicStaysBare`,
  `TestBoardLineageParentID`) and `board_test.go` (`TestBoardLineageChromeRenders` + tightened
  `TestBoardNoEpicChromeInPerItem`); `lineage.js` has no Go-side unit test (no JS harness in-repo) —
  verified by manual/visual check. Docs: `docs/control-room.md` Board row updated; no spec change
  (control-room.md already specified it). **Phase 7 fully complete** (T7.1–T7.8). *(spec)*
  [control-room.md](specs/control-room.md)

---

## Deferred & follow-ups (filed, not blocking)

- Control-room tooltip on producer/verifier model-family overlap — the souls/config view now exists (T4.26) but the diversity-warning tooltip on it was never wired *(from T2.13)*.
- Live-streaming replay (reconstruct the decision trail as the invocation runs) — needs the broker to emit structured per-turn events; overlaps the activity feed *(from T4.11)*.
- ~~Wire `Reader.Replay` to read `issue.Transcript` so non-merged (dead-lettered/in-flight) invocations replay too~~ — **done.** `Reader.Replay` now resolves the transcript hash by preferring the merge trailer (merged replay byte-for-byte unchanged) then falling back to `core.Issue.Transcript` (the orchestrator stamps it for every disposition, T4.15), so dead-lettered/in-flight invocations replay — the case the forensic trail matters most for. `Reader.Invocation.ReplayAvailable` mirrors the same resolution so the live-invocation view's handoff link surfaces for a terminal *blocked* run too, not just merged work. The replay-view notice and `docs/control-room.md` were corrected (the old "retained only for work merged to main" text was stale). Tests: `replay_test.go` (`TestReplayNonMergedFromIssueStamp`, `TestReplayPrefersMergeTrailer`; fixed the stale `TestReplayNotMerged` rationale) and `query_test.go` (`TestInvocationBlockedWithStampOffersReplay`). No spec change — `specs/observability.md` already specifies replay "live or after the fact … when an autonomous change looks wrong"; only the read lagged the design *(from T4.11, T4.15)*.
- Consolidate the status bar's 2–3 per-page SSE connections (page content + status bar + alerts.js) onto one connection or h2c *(from T4.19)*.
- Client-side live wall/token ticker on the invocation budget meter (mid-invocation spend isn't persisted to beads) *(from T4.21)*.
- ~~Thread `BudgetCaps` to the board cards so a `budget.wall` tint can render there~~ — **done.** `query.Board` already received `BudgetCaps`; it now stamps each `IssueCard` with `WallCapped`/`WallPct`/`WallOver` (cumulative `core.Issue.SpentWall` vs `caps.IssueWall`, via the same `meterPct`/`meterOver` the Budgets view uses, so the two surfaces never disagree on a breach). A live (non-closed) card's timer tints toward its `budget.wall` ceiling — amber at ≥80%, rose once over (`timerRowClass`/`wallTint` in `board.go`, reusing the Budgets color tokens), with the exact percent in a hover tooltip (`wallTitle`) so the cue isn't color-alone. Uncapped wall renders no tint (a percent of no cap is meaningless), mirroring the Budgets view. Realizes control-room.md "The board, in motion" ("the in-progress timer tints toward its budget.wall ceiling") — no spec change (it was spec'd optional). Tests: `query_test.go` (`TestBoardCardWallBudget`, `TestBoardCardWallUncapped`) + `board_test.go` (`TestBoardWallTimerTint`, `TestBoardWallNoTintWhenUncapped`). Docs: `docs/control-room.md` Board row *(from T4.18)*.
- Decomposition-preview dry-run before APPROVE (control-room.md OPEN, "leaning defer"; seed issues stay coarse and the autonomous planner decomposes) *(from T4.14)*.
- Control-room surface for the transform log (e.g. a verification-view row weighing text-fallback renames) — the record is harvested onto Evidence; only the read/render is a follow-up *(from T6.3)*.

## Open decisions affecting the plan

These are still `OPEN:` in the specs and may reshape tasks above. (Decisions once open
here — mutation threshold, gate fail-fast, `integrate` ownership, the condition-expression
language — are now recorded in the specs they informed, not duplicated here.)

- HA orchestrator: single instance (fine for v1) vs. leader election (T5.11).
- **Concurrent epics (epic mode).** Phase 7 v1 admits one active epic at a time (T7.5
  refuses a second). Lifting it needs a two-level merge queue (children serialize onto their
  epic branch; epic→`main` merges serialize onto `main`, an in-flight epic rebasing onto a
  moved `main` + re-gating the whole feature at its terminal merge). Deferred; spec'd as OPEN
  in [integration.md](specs/integration.md).
- Exact module set in the TCB boundary — operationally the `policy.tcb_paths` globs (T2.10);
  the concrete list must still be reviewed and pinned before autonomy is switched on for harness work.
