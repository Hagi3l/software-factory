# Messaging

All inter-process communication is **NATS**. This is what makes the harness
distributable: agents and runners may be co-located in one process or spread across
hosts with no code change (location transparency).

See also: [architecture.md](architecture.md),
[components/runner.md](components/runner.md),
[components/orchestrator.md](components/orchestrator.md).

---

## Why NATS, always

Even when components are co-located, they speak NATS rather than in-process
channels. The point is that "a runner is a goroutine in the orchestrator process"
is a *deployment topology*, not a code constraint — the same runner can become a
separate binary on another host (or a git-worktree-per-agent setup on the same
host) with zero code change. Location transparency is a first principle.

---

## Subject taxonomy

```
harness.work.<role>          JetStream work queue — assignments (pull consumers)
harness.result.<role>        JetStream — agent Result envelopes
harness.agent.<id>.events    core NATS — progress/log events (fire-and-forget)
harness.issue.<id>.state     core NATS — issue state transitions (fire-and-forget)
harness.merge.<id>.state     core NATS — merge-queue lifecycle transitions (fire-and-forget)
harness.dlq                  JetStream — dead-lettered work for human triage
harness.approvals            JetStream — human approve/reject of parked integrates
harness.control.*            core NATS — orchestrator control/health
```

- **Work queues** (`work.<role>`) are JetStream pull consumers: runners across
  hosts **compete to pull**, giving load balancing and horizontal scale by adding
  runners. No component needs to know which runners exist.
- **Results** flow back on `result.<role>` as durable messages the orchestrator
  consumes and validates.
- **Approvals** (`approvals`) carry a human's approve/reject of a parked integrate
  candidate (the trusted-dev / TCB-review gate, [verification.md](verification.md),
  [bootstrap.md](bootstrap.md)). Like results they are JetStream-durable and consumed
  only by the single-writer orchestrator, which records the decision against the issue's
  current candidate — a human never writes beads directly during a run, so an approval is
  a *proposal* validated and applied exactly like an agent Result. In the bootstrap the
  embedded NATS is in-process only; to let a separate `harness approve` process reach it,
  `harness run --nats-addr <host:port>` opens an opt-in local TCP listener (a single-host
  convenience, not the distributed cluster — that is T5.8).
- **Agent events** (`agent.<id>.events`) are best-effort observability: the
  [control room](control-room.md) tails them and pushes to browsers over SSE, and
  they are also emitted as [OpenTelemetry](observability.md) spans. Losing one is
  harmless. The envelope carries the originating **issue id and role** alongside the
  agent id, so a consumer can scope a feed to a *single live invocation*
  (the [control room](control-room.md)'s invocation view) without a second beads
  read — the runner already holds the binding (it is in the Brief the orchestrator
  dispatched), so it stamps it on the event at publish time rather than making every
  viewer reconstruct it.
- **Issue-state events** (`issue.<id>.state`) are the single-writer orchestrator's
  typed announcement of an issue *state transition* — it publishes one whenever it
  changes an issue's status (and the stamped `state_entered_at`; see
  [orchestrator.md](components/orchestrator.md)), carrying `{id, status, role, epic,
  ts}`. They let the [board / DAG / dead-letter views](control-room.md) refresh
  *crisply on the actual transition* — a card moves columns the moment the
  orchestrator advances it — rather than polling around agent activity. They are
  best-effort core NATS like agent events: a dropped one is harmless because the
  views keep a slow periodic backstop that reconverges them. They are an *additive
  observability emit* (publish-only, no behaviour change) — beads stays the
  authoritative state, never reconstructed from these events.
- **Merge-state events** (`merge.<id>.state`) are the same pattern applied to the
  [serialized merge queue](integration.md): the orchestrator publishes one whenever
  an `integrate` candidate changes its position in the train — `queued → rebasing →
  re-gating → landed`, or the terminal `conflicted` / `regate-failed`. They make the
  integration pipeline observable in flight (the [merge-queue view](control-room.md))
  rather than only after a commit lands on `main`. Like issue-state they are an
  *additive observability emit* — publish-only, best-effort core NATS, with the same
  periodic backstop reconverging a dropped one; the queue's authoritative state stays
  the git refs and beads, never these events.
- **Dead-letter** (`dlq`) is JetStream-durable (a human must not miss an escalation),
  but the [control room](control-room.md) also *tails* it to push an immediate
  **escalation alert** to the operator's browser — the dead-letter queue is the human's
  [only action surface](control-room.md), so a new arrival is the one event worth a
  push, not a poll. Durability is the queue; the SSE tail is just the nudge.

---

## JetStream as the lease

A runner **acks** a work message only after it has harvested the agent's result.
JetStream `AckWait` therefore *is* the lease: if the runner dies mid-task, the
message is redelivered to another runner, and the orchestrator's reconciliation
sweep resets the stranded beads issue. We get crash recovery from the transport —
no separate lease/heartbeat system to build. See
[components/orchestrator.md](components/orchestrator.md).

Delivery is **at-least-once**, so every consumer must be idempotent (the
orchestrator's single-writer model makes this natural).

---

## The broker protocol (sandbox ↔ runner)

A sandboxed agent has **no NATS connection of its own** — it has zero network. It
talks to its [runner](components/runner.md) over a local channel (vsock / unix
socket), and the runner relays allowed calls onto NATS (and to the model API,
package mirror, git). So:

- Agents never hold NATS credentials.
- All agent-originated NATS traffic passes through, and is logged by, the runner.
- Model calls travel this channel too, as provider-agnostic
  [canonical requests](models.md); the runner attaches the key and the right
  provider adapter.

This keeps the [single-writer](security.md) and [zero-egress](security.md)
properties intact: the only NATS citizens are trusted (orchestrator, runners).

---

## OPEN questions

- Concrete stream definitions (retention, replicas, max-age) and consumer configs
  — TBD at implementation.
- Whether to use a **NATS KV** bucket for orchestrator leader-election / locks if
  HA is pursued — see [components/orchestrator.md](components/orchestrator.md).
- Subject-level multi-tenancy if multiple projects share a NATS cluster — TBD.
