# Remediation plan — demo-run findings (2026-06-18)

Implementation plan for the issues found during the live vault demo run. Findings and
evidence are in [`demo-run-issues.md`](demo-run-issues.md); this is the *how we fix them*.

**Decisions taken (this planning session):**
- Read/consistency cluster (#2,#4,#6,#7,#8) → **systemic read-model refactor** (not surgical
  patches). One authoritative projection is the live read model; beads is the durable log.
- Planner over-bundling (#5) → persona + decomposition-principle fix.
- #9/#1 → small clarity/hardening fixes.
- This run involves **spec updates** (specs/ is source of truth per CLAUDE.md) — checklist at end.

Order to implement: **A (read model)** is the spine and subsumes #7; do its spec work first.
B and C are independent and can land in any order.

---

## Workstream A — Authoritative projection as the read model (#2, #4, #6, #7, #8)

### Problem
Both the scheduler and the control room read beads/Dolt **directly** (`bd list/show` via the
`IssueReader.ListAll` seam and `bd.Ready()`). Those reads are (a) **not read-your-writes
consistent** in Dolt server mode and (b) **don't scale under polling**. One weakness produced
five symptoms: redundant planner re-dispatch (#2), "open" epic-root card while working (#4),
retry-looks-like-duplicate (#6), integrated miscount (#7), `signal: killed` overloads (#8).

### Design
Promote the orchestrator's existing in-flight projection (`internal/orchestrator/inflight.go`,
`inflightProjection`, currently *in-progress only*) into a **full work-graph projection**: the
single-writer's authoritative, event-sourced view of *every* issue it knows — status, role,
attempt, epic, spend, `state_entered_at`, and an explicit **integrated** flag. It is maintained
at the existing transition choke point (`o.transition`) and **rebuilt from beads on startup**
(extend `rebuildInflight`). beads becomes the durable log + cold-start hydration source, not the
hot read path.

Two consumers read the projection instead of beads:

1. **Scheduler** (`schedule.go`): the dispatch filter checks projection status, not just
   `bd.Ready()`. `bd.Ready()` stays the *candidate oracle* (computes "no open blockers +
   precondition"), but a candidate the projection already knows is `in_progress` **or settled
   (closed/blocked)** is skipped. This generalizes today's `o.inflight.has()` skip and kills
   #2 (the just-closed plan root is skipped until beads' ready() catches up).

2. **Control room** (`internal/controlroom/query`): back the `IssueReader` interface with a
   **projection-backed reader** when co-located, fed by a **snapshot-then-stream** sync over the
   existing `internal/controlroom/live` Hub. Keep the current beads-backed `IssueReader` as the
   **standalone fallback** (`harness serve` with no orchestrator/NATS → snapshot from beads, no
   live updates — mirrors how `/events` already 503s standalone).

#### The `integrated` signal (fixes #7 by construction)
beads has only open/closed/blocked/in_progress, so "closed" today conflates *integrated*,
*failure-routed/superseded*, and *closed plan root*. Make integration **explicit and durable**:
stamp an `integrated` tag/label on the child's bead when the integrate stage merges it onto the
epic branch (`results.go` integrate path / `epic*.go`). The projection carries the flag;
`epicSummaries` counts `integrated`, not `closed`, and **excludes the epic root** (its own
id == epic id) and failure-routed beads. Cold-start rebuild re-derives the flag from the tag
(durable in beads) — no git read needed.

### Spec updates (do FIRST — design changes the contract)
- **`specs/observability.md`** — primary. Define the projection as the live read model, beads
  as durable log, and the **snapshot-then-stream** contract (gap-free: snapshot at connect, then
  events). Live vs. history.
- **`specs/components/orchestrator.md`** — extend "Live state vs. durable state": the in-flight
  projection becomes the full work-graph projection and the authoritative read model; scheduler
  dispatch reads projection status.
- **`specs/control-room.md`** — control room reads projection/event stream, not direct beads
  polling; co-located (live) vs standalone (beads snapshot, no live) behavior.
- **`specs/messaging.md`** — any new/extended transition-event subjects + payloads (status,
  role, attempt, spend delta, integrated flag) the projection sync needs.
- **`specs/integration.md`** — `integrated` as a first-class durable state (tag at integrate);
  epic rollup defined over integrated-count, not closed-count.
- **`specs/glossary.md`** — define **Projection / read model** and **Integrated**.

### Code (sketch — confirm during impl)
- `internal/orchestrator/inflight.go` → generalize to full projection (or new `projection.go`);
  add per-issue fields + `integrated`. `rebuildInflight` hydrates all known issues from beads.
- `internal/orchestrator/schedule.go` → dispatch filter consults projection status.
- `internal/orchestrator/results.go` + `epic*.go` → stamp `integrated` tag at integrate; emit
  richer transition events.
- `internal/controlroom/query/query.go` → projection-backed `IssueReader` impl; keep beads-backed
  for standalone; `epicSummaries` counts integrated + excludes root/failure beads.
- `internal/controlroom/live/` → snapshot-then-stream sync.

### Tests
- Projection rebuild from beads == live projection (cold-start parity).
- Scheduler does NOT re-dispatch a just-closed/just-decomposed issue (#2 regression).
- Snapshot-then-stream is gap-free under concurrent transitions.
- `epicSummaries`: integrated count excludes root + failure-routed; matches real epic-branch
  landings (#7 regression).
- Standalone control room degrades to beads snapshot (no panic, no live).

### Risks / open questions
- **Core path change** (scheduler + single-writer). Land behind tests; consider keeping
  beads-backed reader switchable to de-risk rollout.
- Snapshot-then-stream ordering vs. the existing SSE Hub — reuse or extend `live.Hub`?
- Distributed (non-co-located) control room: does it subscribe to NATS directly, or is
  projection-read co-located-only for v1 (standalone = beads snapshot)? Recommend co-located-only
  live for v1; revisit with Phase 5 distribution.

---

## Workstream B — Planner decomposition granularity (#5)

### Problem
The planner bundled 4 concerns into one child ("Wire uptime into server: startTime, shell
component, handler calls, httptest"). The test-author then flailed **~1.35M tokens twice**
(attempt + retry, ~$1.6 on one child) — ~80% of the run's cost.

### Fix
- **Persona (primary):** edit `config/souls/prompts/planner.md` AND
  `demo/vault/config/souls/prompts/planner.md` — require each child be a **single,
  independently testable concern**; explicitly discourage bundling multiple subsystems/handlers
  into one work item; prefer more, smaller children with explicit dependency edges.
- **Spec:** add a decomposition-granularity principle to **`specs/workflow.md`** (the "emergent
  within a stage" section) — breadth stays emergent, but each emitted child must be a coherent,
  independently-verifiable unit. Keeps the principle in the source of truth, not just a prompt.
- **Backstop (optional, decide after re-run):** tighter `max_tool_turns`/token budget for
  test-author so a bad child fails fast/cheap. **Tension:** too tight kills legit complex work —
  leave caps as-is first; only add if runaways persist after the persona fix.

### Tradeoff to document
Smaller children = more children = more fixed per-stage overhead. Net cheaper than a runaway,
but the architecture stays overhead-heavy for tiny features — that's **demo feature selection**,
not a code fix (note in demo README: pick a feature that splits into 2–3 clean slices, e.g. the
one-time secret-share link).

### Validation
Re-run the demo (or a plan-only dry run) and confirm children are single-concern and no stage
approaches its turn cap.

---

## Workstream C — Clarity / hardening (#9, #1)

- **#9 misleading "merged to main" log** (`internal/orchestrator/results.go:637`): name the real
  target — epic branch vs main. Trivial; fold into Workstream A's results.go edits. No spec
  change (observable-log wording; reflects integration.md's epic semantics).
- **#1 wizard double-seed**: harden `config/souls/prompts/requirements-planner.md` to always
  seed exactly **one** root issue in epic mode, and clarify the wizard error
  (`cmd/harness/wizard_seed.go:197`). Spec touch: `specs/specs-process.md` /
  `specs/control-room.md` wizard contract if the one-root-in-epic rule isn't already stated.

---

## Consolidated spec-update checklist (CLAUDE.md: specs/ is source of truth)
- [ ] `specs/observability.md` — projection as read model; snapshot-then-stream (A)
- [ ] `specs/components/orchestrator.md` — full work-graph projection; scheduler reads it (A)
- [ ] `specs/control-room.md` — projection/event reads; co-located vs standalone (A); wizard one-root rule (C)
- [ ] `specs/messaging.md` — transition-event subjects/payloads (A)
- [ ] `specs/integration.md` — `integrated` as durable state; epic rollup over integrated (A/#7)
- [ ] `specs/glossary.md` — Projection / read model; Integrated (A)
- [ ] `specs/workflow.md` — decomposition-granularity principle (B)
- [ ] `specs/specs-process.md` — wizard one-root-in-epic rule, if not covered (C)
- [ ] `IMPLEMENTATION_PLAN.md` — add these as tracked tasks in build order

## Suggested sequencing
1. **A specs** → **A code** (spine; lands #7 correctly + kills #2/#4/#6/#8).
2. **B** (persona + workflow spec) — independent, high cost-impact, cheap.
3. **C** — fold #9 into A's results.go pass; #1 persona + spec.
4. Re-run the demo to validate (and pick a cleaner demo feature).
