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
   [../integration.md](../integration.md).
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
  for issue in candidates and not in inflight: # inflight = the in-flight projection (below)
      bd.set(issue, in_progress, lease=ttl)    # single-writer transition; also records into inflight
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
restart re-runs it. "Already dispatched" is read from the **in-flight projection**
(below), *not* from a fresh beads query — see why next.

Every status write above (`in_progress`, `advance`, `on_failure`, `dead_letter`,
`reset`) routes through the single transition choke point that stamps
`state_entered_at`, publishes the [issue-state event](../messaging.md), **and updates
the in-flight projection** — so the counter anchor, the live nudge, and the live
read surface are all maintained at the same place, exactly once per real transition,
and a redelivered result that lands on an already-settled issue neither re-stamps nor
re-announces nor re-dispatches (idempotent, like every other step).

---

## Live state vs. durable state — the in-flight projection

beads is the **durable** source of truth. It is **not** a strongly read-your-writes
consistent **read surface** under load: the orchestrator's own reconcile loop (plus
the [control room](../control-room.md)) drives enough concurrent traffic that a fresh
`bd.ready()` issued moments after a `set(in_progress)` write may not yet observe that
write. Treating `bd.ready()` as the authority for "already dispatched" therefore
**re-dispatches in-flight work** — the same issue is claimed and published every tick
until the write becomes visible, multiplying agent invocations and corrupting the
graph with duplicate proposals. Polling cadence faster than write-visibility makes a
storm, not progress.

The orchestrator is the **single writer**, so it already knows the live status of
every issue at the instant it writes it. It therefore keeps a small **in-flight
projection**: a volatile in-memory record of the issues it currently considers
`in_progress` (with their leases). Two rules make it correct and cheap:

- **It is derived, never authoritative.** It holds nothing that is not recoverable
  from beads. On restart it is rebuilt from the `in_progress` set (and their leases)
  before the first dispatch, so the crash-safety guarantees are unchanged — beads
  remains the truth; the projection is a consistency *cache* over it.
- **It is maintained at the one transition choke point.** Every status write updates
  it in the same place it writes beads: a transition *to* `in_progress` adds the
  issue; any transition away from it (`open`, `closed`, `blocked`) removes it. Because
  every status mutation already routes through that choke point, the projection cannot
  silently drift from beads.

How the two hot paths use it:

- **Dispatch.** `bd.ready()` remains the **candidate oracle** — it still computes
  "no open blockers + precondition holds", so the dependency graph is *not*
  re-implemented in memory. The orchestrator then **skips any candidate already in
  the projection.** A stale `bd.ready()` that returns a just-claimed issue is now
  harmless: the projection knows it is in flight.
- **Result gating.** A returning Result is processed only if its issue is in the
  projection; otherwise it is a stale/duplicate redelivery and is ignored. This is
  read from the projection, not a (possibly lagging) beads status read, so a valid
  result is never discarded as "not in progress" because beads had not caught up, and
  a duplicate cannot be applied twice (the first result removes the issue from the
  projection before the next is processed — result handling is serial).

Because the projection (not beads) answers "in flight?", the **lease sweep** scans
it in memory, and the **in-flight spec-drift check** iterates it and re-resolves
specs from the worktree — neither needs a beads query. The slow, full-table sweeps
that *do* read beads (e.g. re-deriving already-merged work on a spec edit) run on a
**separate, slower cadence** than dispatch, so they neither pace dispatch nor add to
the read pressure that causes the lag in the first place.

---

## Crash safety

- The orchestrator holds **no authoritative in-memory state**. Its only in-memory
  state is the in-flight projection, which is **derived from beads and rebuilt from
  the `in_progress` set on restart** before the first dispatch — so a crash loses
  nothing. On restart it reads beads to find ready and in-flight work and resumes.
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
two loops never touch the *same* issue at once — the in-flight projection gates
same-issue contention: a Result is acted on only while its issue is in the projection,
a dispatch skips a projected issue, and serial Result handling removes an issue before
the next is processed — and (b) the work store tolerates concurrent writes to
*different* issues. The constraint this puts on future change: anything that adds
another beads-writing loop must preserve (a) — it cannot assume a global write lock,
because the coordination is the projection plus serial Result handling, not a mutex.
This is why splitting dispatch's fast cadence from the slow full-table sweep keeps
them on one loop's serialisation rather than introducing an independent writer.

---

## What it must never do

- **Never execute untrusted code in its own process or trust zone.** All
  execution — including gate checks — happens inside sandboxes.
- **Never let a sandboxed agent write beads directly.** Proposals only.
- **Never advance a stage on an agent's self-report.** Only a passing gate
  advances the graph.

---

## OPEN questions

- **HA / multiple orchestrators.** One logical writer is required for consistency.
  Whether that is a single process with leader election (e.g. via a NATS KV lock)
  or a single instance is undecided. Single instance is fine for v1.
- **Scheduling fairness** across epics (avoid one epic starving others) — policy
  TBD; budgets bound damage in the meantime.
