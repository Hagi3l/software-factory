# Observability

In a system where humans never read code and rarely intervene, the control room
is the human's *primary* relationship with the work. Observability is therefore a
first-class concern, not operational garnish.

See also: [control-room.md](control-room.md),
[components/runner.md](components/runner.md),
[components/artifact-store.md](components/artifact-store.md),
[messaging.md](messaging.md).

---

## The broker is already the collector

The key realization: because every [agent](components/agent.md) is zero-network and
*all* its I/O is brokered through the [runner](components/runner.md), the runner
sees everything an agent does — every LLM request/response, every tool call, every
git op, every package fetch. **The security chokepoint and the telemetry chokepoint
are the same point.**

So observability falls out of the architecture: emit structured events and
[OpenTelemetry](#opentelemetry) spans at the broker, and you have a complete,
tamper-evident record of agent behaviour with nowhere for an agent to act
unobserved. No separate instrumentation pass is required.

---

## Three durable stores, cleanly split

Observability data has different shapes; they live in different places.

| Store | Holds | Spec |
|-------|-------|------|
| **beads** | work *state* + outcomes (status, transitions, retries, which gate failed) | [orchestrator.md](components/orchestrator.md) |
| **git** | code + provenance trailers (issue→soul→model→prompt→evidence) | [security.md](security.md) |
| **artifact store** | the *large* stuff: full transcripts, gate outputs, mutation reports, diffs | [components/artifact-store.md](components/artifact-store.md) |

The artifact store is the new piece. It matters precisely *because* sandboxes are
ephemeral: anything you want to inspect after an agent dies must be **harvested
before teardown**. It is content-addressed and referenced by hash from beads and
from the provenance trailer — never inlined.

---

## An invocation is a trace

The natural data model:

```
epic ─┬─ issue ─── invocation (one trace)
      │              ├─ span: boot
      │              ├─ span: llm-turn  (× N)
      │              ├─ span: tool-call (× M)
      │              └─ span: gate-run
      └─ issue ─── invocation (one trace)
```

This is exactly the OpenTelemetry shape, which gives a clean **build-vs-buy** line:

- **Buy / reuse:** OTel spans → a trace backend (Tempo / Jaeger) and metrics
  (latency, throughput, error rates, cost-over-time). Do **not** rebuild Grafana.
- **Build:** the *domain* views nothing off-the-shelf provides — the board, the
  dependency-graph DAG, the issue/diff/evidence detail, the
  [dead-letter](workflow.md) queue, and provenance. See
  [control-room.md](control-room.md).

### OpenTelemetry
Spans are emitted at the broker (agent I/O), the orchestrator (scheduling, gating,
graph transitions), and the runner (sandbox lifecycle). Exporting OTel keeps the
infra-level timeline in proven tooling and frees the bespoke UI to focus on
factory-specific views.

---

## Live vs. history

The control room must show both "everything going on" and the full past:

- **Live** — NATS events → SSE → the board and activity feed update themselves (no
  polling). See [messaging.md](messaging.md), [control-room.md](control-room.md).
  The activity feed carries two sources side by side: **agent** events brokered from
  inside the sandbox (an assistant `token` stream, a `reasoning`/think stream, and one
  `tool` row per tool call), and **system** events — *what the factory itself is doing*
  (dispatch, sandbox provision, gate pass/fail, merge, dead-letter). The system stream
  is the trusted side's own structured log teed into the feed (a `slog` bridge in the
  co-located run), not a second instrumentation pass — the same single-source-of-truth
  logic as "[the broker is already the collector](#the-broker-is-already-the-collector),"
  applied to the orchestrator/runner. It means a turn that is all tool calls (no
  narration) still reads as live activity, and an operator sees the machine think *and*
  the machine act in one timeline.
- **History** — server-rendered from beads + the artifact store, with the
  structured timeline from the OTel trace backend.

The UI stitches these so a human can go **live event → drill into its trace →
replay** (below), and browse history by epic / issue / soul / time.

---

## Replayability — the differentiator

Because the broker captured every agent action, you can **replay an invocation's
decision trail**: exactly what the LLM saw and did, step by step, live or after the
fact. This is what turns adequate dashboards into *great* observability — when an
autonomous change looks wrong, you can answer *why* with certainty rather than
guessing. It is also the audit mechanism that makes a no-human-review system
accountable.

---

## OPEN questions

- Event schema / span attribute conventions — TBD at implementation.
- Retention tiers (how long live events, traces, and full transcripts are kept) —
  see [components/artifact-store.md](components/artifact-store.md).
- Whether the OTel collector is embedded or a sidecar — deployment choice.
