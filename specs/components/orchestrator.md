# Orchestrator

The single **scheduler**, **gatekeeper**, and **sole writer of beads**. There is
exactly one logical orchestrator. It executes no agent work itself.

See also: [../architecture.md](../architecture.md), [../workflow.md](../workflow.md),
[../verification.md](../verification.md), [../integration.md](../integration.md).

---

## Responsibilities

1. **Schedule.** Query beads for ready work (issues with no open blockers whose
   precondition holds) and publish each to its role's [work
   subject](../messaging.md).
2. **Gate.** When an agent returns a candidate, run the stage's postconditions in
   a fresh [verification sandbox](../verification.md) and decide accept / reject /
   escalate.
3. **Advance the graph.** On accept, apply the declarative `produces:` transition
   by creating the next-stage issue. This is how *depth* is created.
4. **Validate proposals.** Agents propose mutations (child issues, status,
   evidence) in their Result; the orchestrator validates DAG-legality and applies
   them. It is the *only* writer of beads — including the seed issues created by
   the [control-room wizard](../control-room.md), which are written through this
   same validated path, never directly.
5. **Route failures.** On reject, apply `on_failure`; on budget/retry breach or
   unrecoverable escalation, dead-letter.
6. **Reconcile.** Detect and recover stranded work after crashes.
7. **Run the merge queue.** Serialized integration to `main`. See
   [../integration.md](../integration.md). Under
   [`integration.mode: epic`](../configuration.md) the queue is **retargeted per
   epic** (children integrate onto `epic/<epic_id>` instead of `main`); the
   orchestrator additionally detects **epic completion** — an `epic_id` aggregate
   read (the subtree is closed and nothing in it is in flight), evaluated on the slow
   sweep cadence like [`epic_budget`](../workflow.md), never the dispatch hot path —
   and on drain advances the epic root issue to its **terminal merge** of the epic
   branch to `main`. v1 admits **one epic at a time**.
8. **Emit telemetry.** Spans for scheduling, gating, and graph transitions. See
   [../observability.md](../observability.md).
9. **Announce state transitions.** As the single writer, every status change is one
   place — so on each transition the orchestrator stamps `state_entered_at` on the
   issue and publishes a typed [issue-state event](../messaging.md). The stamp is the
   anchor the [board](../control-room.md) ticks its "time in current state" counter
   from (client-side); the event lets the live views refresh crisply on the actual
   transition. Both are produced at the *same* single-writer choke point, so they are
   one addition, not two — and an additive observability emit, never a second source
   of truth (beads remains authoritative).

---

## The reconciliation loop

The orchestrator is a control loop, not an event handler with hidden state. All
**authoritative state lives in beads + JetStream** — nothing the orchestrator holds
in memory is a source of truth it could not rebuild from beads — so it can crash and
restart at any point and re-derive everything. Conceptually:

```
loop:
  candidates := bd.ready()                     # no open blockers + precondition ok (may be stale)
  for issue in candidates if projection says neither in-flight nor settled:  # projection = the work-graph projection (below)
      bd.set(issue, in_progress, lease=ttl)    # single-writer transition; also updates the projection
      publish work.<role>  { brief(issue) }    # JetStream, at-least-once

  for result in completions:                   # agents' Result envelopes
      validate(result.proposals)               # DAG-legal? deps exist? acyclic? in budget?
      if gates(result) == pass:
          apply(result.proposals); advance(issue)   # create produces[] issue
      else if retryable(issue):
          on_failure(issue)                     # new fix issue
      else:
          dead_letter(issue)

  for issue in in_progress with expired lease:  # stranded by a dead runner
      reset(issue, ready)                        # JetStream redelivery handles the rest
```

**Idempotency is mandatory.** Every step must be safe to repeat, because a
restart re-runs it. "Already dispatched (or already settled)" is read from the
**work-graph projection** (below), *not* from a fresh beads query — see why next.

Every status write above (`in_progress`, `advance`, `on_failure`, `dead_letter`,
`reset`) routes through the single transition choke point that stamps
`state_entered_at`, publishes the [issue-state event](../messaging.md), **and updates
the work-graph projection** — so the counter anchor, the live nudge, and the live
read surface (both the scheduler's and the control room's) are all maintained at the
same place, exactly once per real transition, and a redelivered result that lands on an
already-settled issue neither re-stamps nor re-announces nor re-dispatches (idempotent,
like every other step).

---

## Live state vs. durable state — the work-graph projection

beads is the **durable** source of truth. It is **not** a strongly read-your-writes
consistent **read surface** under load: the orchestrator's own reconcile loop (plus
the [control room](../control-room.md)) drives enough concurrent traffic that a fresh
read issued moments after a status write may not yet observe that write, and a heavy
poll (`bd list` over the whole graph) can saturate or time out under that traffic. This
lag bites in three places:

1. **Re-dispatching in-flight work.** Treating `bd.ready()` as the authority for
   "already dispatched" re-dispatches a just-claimed issue every tick until the
   `in_progress` write becomes visible — multiplying invocations and corrupting the
   graph with duplicate proposals. Polling faster than write-visibility makes a storm.
2. **Re-dispatching just-*settled* work.** The same window reopens after a *terminal*
   write: a `plan` issue the orchestrator just closed at decomposition (or any
   just-closed/dead-lettered issue) can still come back from a lagging `bd.ready()` and
   be dispatched again before the close is visible — a redundant second invocation that
   is wasteful even when an idempotency guard later discards its output.
3. **Stale / failing control-room reads.** The control room polling beads directly
   shows a card as `open` while its agent runs, or a `closed` card as still in flight,
   and its `bd` reads can be killed under load — the board disagreeing with reality.

All three are the same root cause: a direct beads read is not a consistent, scalable
*read surface*. The fix is one **read model**.

The orchestrator is the **single writer**, so it already knows the live status of
every issue at the instant it writes it. It therefore keeps a **work-graph
projection**: a volatile in-memory view of the live state of **every** issue it knows —
status, role, attempt, epic, spend, `state_entered_at`, lease, the
[`integrated`](../integration.md) marker, and its read-side `blocked-by` edges. (This
generalizes the original *in-flight* cache, which held only `in_progress` issues;
retaining settled issues too is what closes re-dispatch case 2 and lets the control room
read closed/blocked state.) The edges matter because the control room's live **DAG view**,
and the board's sibling **waits-for** overlay, read the dependency graph from the
projection — so a record the orchestrator *creates* (a decomposition child, a routed fix)
must carry its **resolved** `blocked-by` edges at creation, not only its status. `blocked-by`
is otherwise a read-path-only facet a freshly-created issue lacks until the next cold-start
rehydration, which would leave the live graph edge-incomplete in the meantime — the same
read-your-writes gap the projection exists to close, applied to edges rather than status.
Two rules make it correct and cheap:

- **It is derived, never authoritative.** It holds nothing that is not recoverable
  from beads. On restart it is rebuilt from beads (the in-flight set with their leases,
  plus the surrounding graph state) before the first dispatch, so the crash-safety
  guarantees are unchanged — beads remains the truth; the projection is a consistency
  *cache* over it.
- **It is maintained at the one transition choke point.** Every status write updates
  the issue's projected state in the same place it writes beads — a transition *to*
  `in_progress`, *to* `closed`, *to* `blocked`, *to* `open` all update the entry's
  status (rather than only adding/removing an in-flight membership). Because every
  status mutation already routes through that choke point, the projection cannot
  silently drift from beads.

How the hot paths use it:

- **Dispatch.** `bd.ready()` remains the **candidate oracle** — it still computes
  "no open blockers + precondition holds", so the dependency graph is *not*
  re-implemented in memory. The orchestrator then **skips any candidate the projection
  knows is `in_progress` *or* already settled (`closed`/`blocked`).** A stale
  `bd.ready()` that returns a just-claimed *or* just-closed issue is now harmless: the
  projection knows its real state (closes re-dispatch cases 1 and 2).
- **Result gating.** A returning Result is processed only if the projection shows its
  issue `in_progress`; otherwise it is a stale/duplicate redelivery and is ignored.
  This is read from the projection, not a (possibly lagging) beads status read, so a
  valid result is never discarded because beads had not caught up, and a duplicate
  cannot be applied twice (result handling is serial, and the first result transitions
  the issue out of `in_progress` in the projection before the next is processed).
- **Control-room reads.** The control room consumes the projection as its **live read
  model** rather than polling beads — see [the read model](../observability.md) and
  [snapshot-then-stream](#the-projection-is-the-control-rooms-read-model) below.

Because the projection (not beads) answers "what state is this issue in?", the **lease
sweep** scans it in memory, and the **in-flight spec-drift check** iterates it and
re-resolves specs from the worktree — neither needs a beads query. The slow, full-table
sweeps that *do* read beads (e.g. re-deriving already-merged work on a spec edit) run on
a **separate, slower cadence** than dispatch, so they neither pace dispatch nor add to
the read pressure that causes the lag in the first place.

### The projection is the control room's read model

The [control room](../control-room.md)'s live views (board, DAG, dead-letter, status
bar, epic roll-up) read the **work-graph projection**, not direct `bd` polling — so they
never disagree with the single writer's own view, and they place no `bd list` load on
the store. The sync is **snapshot-then-stream**: on connect the control room takes a
*snapshot* of the projection, then applies the [`issue-state` events](../messaging.md)
the same choke point already emits, gap-free (the snapshot is the baseline; subsequent
transitions stream as deltas). Because the emit is already part of every transition,
this is the *same* additive observability path, now also feeding a consistent read
surface — beads remains the durable log and the cold-start hydration source, never the
hot read path.

This binds the projection's two consumers to the deployment topology:

- **Co-located** (`harness run` with the control room in-process — the default): the
  control room reads the in-process projection directly and streams transitions over
  SSE. This is the live, consistent surface.
- **Standalone** (`harness serve` with no attached orchestrator): there is no live
  projection, so the control room **degrades to a beads snapshot** — static, no live
  updates — exactly as `/events` already 503s with no in-process NATS. Distributed
  (cross-host) live reads are a later concern (see OPEN questions); v1 live reads are
  co-located.

The **[`integrated`](../integration.md) marker** rides the projection so the epic
roll-up counts genuinely-integrated children, not any `closed` bead — see
[integration.md](../integration.md) "Atomic feature integration".

---

## Crash safety

- The orchestrator holds **no authoritative in-memory state**. Its only in-memory
  state is the work-graph projection, which is **derived from beads and rebuilt from it
  on restart** before the first dispatch — so a crash loses nothing. On restart it reads
  beads to find ready and in-flight work and resumes.
- In-flight work is protected by a **lease/TTL** on the `in_progress` status plus
  JetStream `AckWait`: if the runner that owned an issue dies, its work message is
  redelivered and the orchestrator's sweep resets the stranded issue.
- Because beads is single-writer, there is never a write race to resolve on
  recovery — the orchestrator's view *is* the truth. The projection never overrides
  beads; it only caches what the single writer already wrote.

**Single writer means a single _process_, not a single loop.** Within the
orchestrator several concurrent loops write beads — the event-driven Result and
approval consumers, and the tick loop that dispatches and sweeps. This is safe not
because writes are globally serialised behind a lock (there is none), but because (a)
for an issue the projection **already knows**, two loops never touch it at once — the
work-graph projection gates
same-issue contention: a Result is acted on only while the projection shows its issue
`in_progress`, a dispatch skips an issue the projection shows in-flight or settled, and
serial Result handling transitions an issue out of `in_progress` before the next is
processed — and (b) the work store tolerates concurrent writes to
*different* issues. The constraint this puts on future change: anything that adds
another beads-writing loop must preserve (a) — it cannot assume a global write lock,
because the coordination is the projection plus serial Result handling, not a mutex.
This is why splitting dispatch's fast cadence from the slow full-table sweep keeps
them on one loop's serialisation rather than introducing an independent writer.

**The creation window — where the projection does not yet gate.** The gate in (a)
holds only *once the projection knows an issue*. A freshly **created** issue is the
exception, and a sharp one: a planner's decomposition children, a re-derivation plan's
children, or a routed fix are written to beads by the **creating loop** (a Result
consumer running `bd.Apply`) *before* that loop records them in the projection, and the
dispatch loop's candidate oracle (`bd.ready()`) can observe them in that gap. Worse,
`bd.Apply` creates the issue and adds its blocking edges as **separate** writes, so
there is a sub-window in which the child exists with **no blockers** — `bd.ready()`
returns it as dispatchable even though its decomposition is not yet committed and its
inter-sibling ordering does not yet exist. In that window the dispatch loop can *claim*
a child the creating loop is mid-way through recording — exactly the same-issue
contention (a) is supposed to forbid. Two invariants close it, and **every creation
path must honour both**:

1. **A child is not dispatchable until both its blocking edges and its projection record
   exist.** Creation must be atomic with respect to the dispatch oracle — e.g. the child
   is created already-blocked (so `bd.ready()` never returns a half-built child), or
   dispatch is held off a decomposition until its edges are committed. Without this the
   parent gate the planner relies on (children become ready only when the plan closes)
   *and* the inter-sibling order are both silently bypassed — every child dispatches at
   once, in no order.
2. **A creation (or reopen) projection write must never *downgrade* a live claim.** If
   the dispatch loop records `in_progress` before the creating loop's record runs, the
   creation record — which would set `open` — must **preserve** the `in_progress`
   status, exactly as the settle path preserves the monotonic
   [`integrated`](../integration.md) marker. A claim is a real state advance and wins.

Violating either **wedges the issue permanently**: the projection reads `open` while
beads reads `in_progress`, so the returning Result is discarded as a stale/duplicate
(see *Result gating* above) **and** `bd.ready()` — which correctly sees `in_progress` —
never re-surfaces it, so it is never retried or dead-lettered. This is not theoretical:
it stalled the **2026-06-23 vault-demo run** (the creation-tracking added in T8.4 met
the non-atomic `bd.Apply` window). The remediation is **Phase 10**.

**Durable-write loss — the backend must actually serialise the single writer.** The
single-writer guarantee above ("there is never a write race to resolve") has a hidden
precondition: the beads **backend** must durably serialise the writes the one writer
issues. The orchestrator issues them as separate `bd` invocations, and a stage advance on
the produce-next-stage path is **two** of them — create the successor issue, then close
the predecessor. Against a single serialised engine (a warm `dolt sql-server`) those
commit in order. Against a backend that is *not* serialised — e.g. every `bd` process
round-trips the whole `.beads/issues.jsonl` (import-into-empty-DB → mutate → export)
instead of hitting the server — the two round-trips **race**, and the successor-create's
export can clobber the close (a lost update). The lost close still lands in the projection
(so the pipeline advances and the successor runs) but never reaches the durable store —
and because both the readiness oracle (`bd.ready()`) and the epic-drain oracle
([`sweepEpicCompletion`](../integration.md)) read the **durable** store, not the
projection, a lost close does not merely lag: it **permanently strands** the dependent
sibling (never `ready`) and blocks the epic terminal merge (never `drained`) — a silent
stall the projection cannot self-heal, because the projection believes the work is done.
This bit the **2026-07-06 vault-demo run** five times across a two-child epic; only the
terminal integrate steps (no concurrent create racing the close) survived. Two
requirements close it: the backend must serialise the writer's writes (run beads against
the warm server, never a per-call jsonl round-trip), **and** create-successor +
close-predecessor must be atomic with respect to that store (one transaction, or serialised
through the creation choke point) so a close can never be clobbered by an adjacent create.

---

## What it must never do

- **Never execute untrusted code in its own process or trust zone.** All
  execution — including gate checks — happens inside sandboxes.
- **Never let a sandboxed agent write beads directly.** Proposals only.
- **Never advance a stage on an agent's self-report.** Only a passing gate
  advances the graph.
- **Never let a freshly-created child reach the dispatch oracle before its blocking
  edges and its projection record exist**, and never let a creation/reopen write
  downgrade a live claim. Creation must be atomic with respect to `bd.ready()` (see
  *The creation window*) — the alternative wedges the issue silently.
- **Never treat a status write that reached only the projection as durable.** The
  readiness and epic-drain oracles read the durable store, so a stage-transition close
  that lands in the projection but not the store strands the dependent work silently (see
  *Durable-write loss*). The backend must serialise the single writer's writes, and
  create-successor + close-predecessor must be atomic against it.

---

## OPEN questions

- **HA / multiple orchestrators.** One logical writer is required for consistency.
  Whether that is a single process with leader election (e.g. via a NATS KV lock)
  or a single instance is undecided. Single instance is fine for v1.
- **Scheduling fairness** across epics (avoid one epic starving others) — policy
  TBD; budgets bound damage in the meantime.
