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

---

## The reconciliation loop

The orchestrator is a control loop, not an event handler with hidden state. Its
**entire authoritative state lives in beads + JetStream**, so it can crash and
restart at any point and re-derive everything. Conceptually:

```
loop:
  ready  := bd.ready()                         # no open blockers + precondition ok
  for issue in ready and not already dispatched:
      bd.set(issue, in_progress, lease=ttl)    # single-writer transition
      publish work.<role>  { brief(issue) }    # JetStream, at-least-once

  for result in completions:                   # agents' Result envelopes
      validate(result.proposals)               # DAG-legal? acyclic? in budget?
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
restart re-runs it. "Already dispatched" is derived from beads state + JetStream
delivery, never from orchestrator memory.

---

## Crash safety

- The orchestrator holds **no critical in-memory state**. On restart it reads
  beads to find ready and in-flight work and resumes.
- In-flight work is protected by a **lease/TTL** on the `in_progress` status plus
  JetStream `AckWait`: if the runner that owned an issue dies, its work message is
  redelivered and the orchestrator's sweep resets the stranded issue.
- Because beads is single-writer, there is never a write race to resolve on
  recovery — the orchestrator's view *is* the truth.

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
