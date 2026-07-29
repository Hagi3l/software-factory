# Messaging

All inter-process communication is **NATS**. This is what makes the factory
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
factory.work.<role>          JetStream work queue — assignments (pull consumers)
factory.result.<role>        JetStream — agent Result envelopes
factory.agent.<id>.events    core NATS — progress/log events (fire-and-forget)
factory.issue.<id>.state     core NATS — issue state transitions (fire-and-forget)
factory.merge.<id>.state     core NATS — merge-queue lifecycle transitions (fire-and-forget)
factory.dlq                  JetStream — dead-lettered work for human triage
factory.approvals            JetStream — human approve/reject of parked integrates
factory.control.*            core NATS — orchestrator control/health
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
  a *proposal* validated and applied exactly like an agent Result. The embedded in-process
  NATS (empty `nats.url`) is the single-process default; a separate `software-factory approve`
  process reaches it via the opt-in local TCP listener `software-factory run --nats-addr
  <host:port>` opens. Pointing `nats.url` at an external cluster instead is the
  distributed deployment — the orchestrator and runners take the same connection
  unchanged (location transparency), and `software-factory approve` connects to that cluster
  directly.
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
  attempt, integrated, state_entered_at, ts}`. They serve two consumers off the one
  emit: they let the [board / DAG / dead-letter views](control-room.md) refresh
  *crisply on the actual transition* — a card moves columns the moment the
  orchestrator advances it — rather than polling around agent activity; and they are
  the **delta stream** that keeps the control room's
  [live read model](observability.md) (a snapshot of the orchestrator's work-graph
  projection, then these events applied gap-free) consistent without a `bd` poll. The
  payload carries the fields those views render — including the
  [`integrated`](integration.md) marker and `state_entered_at` — so a consumer updates
  its projected card from the event alone. They are best-effort core NATS like agent
  events: a dropped one is harmless because the views keep a slow periodic backstop
  that reconverges them (and the snapshot re-baselines on reconnect). They are an
  *additive observability emit* (publish-only, no behaviour change) — beads stays the
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
- A model call carries a **sub-context selector** so one sandbox can drive more than one
  model — the parent invocation and the [`explore` tool](components/agent.md#explore--distilled-comprehension)'s
  nested read-only sub-loop each tag their calls. The runner routes each tag to its adapter
  and meters the explorer's tag against the fixed `policy.explore_budget`. The tag→soul→model
  binding is **pinned by the trusted dispatch and enforced by the runner**, never chosen by
  the sandbox — an agent that renamed its tag to a stronger tier is refused. See
  [models.md](models.md).

This keeps the [single-writer](security.md) and [zero-egress](security.md)
properties intact: the only NATS citizens are trusted (orchestrator, runners).

---

## Stream definitions

The four JetStream streams. Their **subjects and retention *policy*** are fixed by the
factory's semantics; the **replication factor** and the **result retention window** are
the only knobs that vary by deployment, surfaced on the infra overlay as
`nats.jetstream.replicas` / `nats.jetstream.max_age` (see
[configuration.md](configuration.md)). Every component that ensures the streams threads
the *same* knobs, since `CreateOrUpdateStream` reconciles an existing stream to whatever
config it is handed.

| Stream | Subjects | Retention | Age bound |
|--------|----------|-----------|-----------|
| `SOFTWARE_FACTORY_WORK` | `factory.work.>` | **WorkQueue** — each assignment consumed exactly once; the consumer ack is the lease | none (consume-once) |
| `SOFTWARE_FACTORY_RESULT` | `factory.result.>` | **Limits** — durable for the orchestrator to consume + replay | `max_age` (default 7d) |
| `SOFTWARE_FACTORY_DLQ` | `factory.dlq` | **Limits** — durable until a human triages | **none** (must survive) |
| `SOFTWARE_FACTORY_APPROVALS` | `factory.approvals` | **Limits** — durable until the orchestrator consumes | **none** (must survive) |

- **Replicas** apply uniformly to every stream: 1 on the single in-process embedded
  server (the dev/bootstrap default), `>1` only on an external cluster of at least that
  size (config validation rejects `>1` with no `nats.url`). More replicas trade write
  latency for surviving a node loss.
- **Only the result stream is age-bounded.** Work is consume-once (WorkQueue retention
  reclaims an acked message), and the dead-letter and approval streams must survive until
  a human acts, so neither is given a `max_age`.
- **Consumers** are durable pull/push consumers, all `AckExplicit` (the ack is the lease,
  so an at-least-once redelivery on a dead consumer is recovered): one shared `work-<role>`
  per role that runners compete on, and the orchestrator's single `orchestrator-results`
  and `orchestrator-approvals`.

## OPEN questions

- Whether to use a **NATS KV** bucket for orchestrator leader-election / locks if
  HA is pursued — see [components/orchestrator.md](components/orchestrator.md).
- Subject-level multi-tenancy if multiple projects share a NATS cluster — TBD.
