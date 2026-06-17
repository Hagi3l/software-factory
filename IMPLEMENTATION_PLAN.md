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

**Phases 2, 3, 4, and 6 are complete; only Phase 5 has live work, and all of it is
optional or hardware-blocked.** Phase 2 (independent verification) is complete — only
the optional T2.11 decision-note remains (resolved as configuration, not a build item).
Phase 3 (full DAG, decomposition, merge queue) is complete (T3.13 landed). Phase 4
(control room + Create/Resolve wizard) is complete. Phase 6 (agent semantic LSP tooling)
is complete (T6.1–T6.3). The only remaining *engineering* of new substrate is Phase 5
(production isolation & distribution), and within it every still-open item is either
**optional** (T5.5 gVisor backend, T5.11 warm pools + HA orchestrator) or
**hardware-blocked** (T5.2 Firecracker, needs KVM the dev box lacks).

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
  0–1, 2, 3, 4, and 6 are done; the verbose per-task findings were pruned once complete —
  that history lives in git, the code, and the specs they informed (each task updated its
  `(spec)` as it landed).
- **Open tasks (`- [ ]`) keep their full detail.** Two open lines: **Phase 5** (production
  isolation — the Firecracker backend T5.2 is hardware-blocked and deliberately last; the
  optional items are T5.5 gVisor and T5.11 warm pools + HA), and **Phase 7** (atomic
  feature integration / epic mode — net-new feature work, not hardware-blocked; T7.1 is also
  a live bug fix that unblocks any multi-child feature today).
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

- [ ] **T7.1 Fix the re-gate ref form (unblocks any multi-child rebase)** — the merge
  queue's step-3 re-gate seeds its sandbox with `git clone` + `git checkout <ref>`, where a
  clone exposes branches only as remote-tracking refs. `rebaseOntoMain` hands the re-gate the
  **fully-qualified** `refs/heads/integration/<id>` (`integrationRef`), which `git checkout`
  cannot resolve in a clone (no local ref by that path; it does not DWIM) → the merge loops
  forever on `pathspec … did not match`, classified transient and redelivered with no cap.
  Return the **short** branch name (`integration/<id>`) for the re-gate's `BaseRef` (DWIM
  resolves it to `origin/integration/<id>`, exactly as the candidate gate uses
  `candidate/<id>`), keeping the fully-qualified form only for `update-ref`/`-d`. Bug fix to
  intended behaviour (the code comment already states the intent) — **no spec change**. Also
  fixes per-item multi-child features today, and is the foundation epic mode's per-child
  re-gate leans on. Regression test: drive `Merge` down the rebase path and assert the re-gate
  is called with `integration/<id>`. *(spec)* [integration.md](specs/integration.md)
- [ ] **T7.2 `integration.mode` config + validation** — parse the top-level
  `integration: { mode: per-item | epic }` block (default `per-item` when absent), thread it
  into the run config, and have `harness validate` reject any other value. *(spec)*
  [configuration.md](specs/configuration.md)
- [ ] **T7.3 Retarget the merge queue per epic** *(needs T7.1, T7.2)* — under
  `mode: epic`: create `epic/<epic_id>` off `main` when the epic begins and commit the
  [wizard](specs/control-room.md)'s approved spec onto it (its first commit, **not** `main`);
  retarget every child-level integration op — rebase target, fast-forward check, re-gate base,
  conflict detection, and the `resolve` stage's rebase — from `main` to the epic branch.
  Everywhere the per-item queue reads "`main`", read "the epic branch." *(spec)*
  [integration.md](specs/integration.md)
- [ ] **T7.4 Epic-completion detection + terminal merge** *(needs T7.3)* — detect drain via
  an `epic_id` aggregate read (subtree all-closed **and** zero in-flight in the subtree),
  evaluated on the **slow sweep cadence** (like `epic_budget`, not the dispatch hot path); on
  drain, advance the epic **root issue** to its terminal merge — a **merge commit** to `main`
  (first parent `main`, second the epic branch tip), subject = feature title, **two-tier
  provenance** (per-child commits preserved under the merge; whole-feature layer on the merge
  commit). v1 (`main` quiescent — one epic) skips re-gating at the terminal step (the last
  child's rebased re-gate already verified the whole feature); strict **all-or-nothing** — any
  child dead-letter / `resolve` failure / `epic_budget` breach leaves the epic unmerged and the
  branch abandoned, `main` untouched. Idempotent terminal merge (existing provenance-trailer
  check). *(spec)* [integration.md](specs/integration.md),
  [components/orchestrator.md](specs/components/orchestrator.md)
- [ ] **T7.5 One-active-epic consent gate** *(needs T7.4)* — under `mode: epic` the wizard's
  approval refuses to seed a second epic while one is in flight, reporting the in-flight
  feature instead. Keeps `main` quiescent during an epic (which is what lets T7.4 skip the
  terminal re-gate). *(spec)* [control-room.md](specs/control-room.md),
  [integration.md](specs/integration.md)
- [ ] **T7.6 Board epic hero card** *(needs T7.4)* — render the epic on the
  [board](specs/control-room.md): every card gets an **epic badge** (root id) + a left-border
  tint hashed deterministically from `epic_id` (colour never the sole channel); the epic
  **root card is the hero** — distinct treatment, a **progress indicator** (children
  integrated / total) and live spend vs. `epic_budget`, carried through an `integrating` state
  to `done` as the terminal merge lands. No new data (rides existing `epic_id`). *(spec)*
  [control-room.md](specs/control-room.md)
- [ ] **T7.7 Vault demo exercises epic mode** *(needs T7.4)* — set `integration.mode: epic`
  in `demo/vault/config/harness.yaml`; confirm a wizard-drafted, multi-child feature lands as
  **one** merge commit and fires **one** deploy. The deploy path needs **zero** changes —
  `run.sh`'s push watcher and `deploy.yml` already key on a `main` advance, which now happens
  once per feature. Update `demo/vault/README.md`'s "How it maps to the real config" table to
  note the epic-mode tuning. *(spec)* [integration.md](specs/integration.md)

---

## Deferred & follow-ups (filed, not blocking)

- Control-room tooltip on producer/verifier model-family overlap — the souls/config view now exists (T4.26) but the diversity-warning tooltip on it was never wired *(from T2.13)*.
- Live-streaming replay (reconstruct the decision trail as the invocation runs) — needs the broker to emit structured per-turn events; overlaps the activity feed *(from T4.11)*.
- Wire `Reader.Replay` to read `issue.Transcript` so non-merged (dead-lettered/in-flight) invocations replay too — the transcript is now stamped onto the issue (T4.15) and reachable; only the read is still merge-trailer-bound *(from T4.11, T4.15)*.
- Consolidate the status bar's 2–3 per-page SSE connections (page content + status bar + alerts.js) onto one connection or h2c *(from T4.19)*.
- Client-side live wall/token ticker on the invocation budget meter (mid-invocation spend isn't persisted to beads) *(from T4.21)*.
- Thread `BudgetCaps` to the board cards so a `budget.wall` tint can render there (currently caps reach only the Budgets view) *(from T4.18)*.
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
