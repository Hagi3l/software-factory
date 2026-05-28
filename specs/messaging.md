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
harness.dlq                  JetStream — dead-lettered work for human triage
harness.control.*            core NATS — orchestrator control/health
```

- **Work queues** (`work.<role>`) are JetStream pull consumers: runners across
  hosts **compete to pull**, giving load balancing and horizontal scale by adding
  runners. No component needs to know which runners exist.
- **Results** flow back on `result.<role>` as durable messages the orchestrator
  consumes and validates.
- **Events** are best-effort observability: the [control room](control-room.md)
  tails them and pushes to browsers over SSE, and they are also emitted as
  [OpenTelemetry](observability.md) spans. Losing one is harmless.

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
