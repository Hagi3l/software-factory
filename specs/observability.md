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
[OpenTelemetry](#opentelemetry--three-signals-one-endpoint) spans at the broker, and you have a complete,
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

- **Buy / reuse:** OTel signals → an off-the-shelf backend for the infra-level
  timeline — a trace backend (Tempo / Jaeger), metrics (latency, throughput, error
  rates, cost-over-time), and logs. A single-binary, multi-signal sink (e.g.
  OpenObserve) can take all three at one endpoint; a trace-only viewer (Jaeger) takes
  spans and refuses the rest. Do **not** rebuild Grafana.
- **Build:** the *domain* views nothing off-the-shelf provides — the board, the
  dependency-graph DAG, the issue/diff/evidence detail, the
  [dead-letter](workflow.md) queue, and provenance. See
  [control-room.md](control-room.md).

### OpenTelemetry — three signals, one endpoint

The harness emits all three OTel signals, and they share a single OTLP exporter
endpoint so one backend can ingest the whole picture:

- **Traces** — the invocation trace above. Spans at the broker (agent I/O), the
  orchestrator (scheduling, gating, graph transitions), and the runner (sandbox
  lifecycle).
- **Metrics** — token `Usage`, USD spend against [budgets](workflow.md), gate
  pass/fail, invocations by stage, retry/dead-letter counts, and context discipline
  (tool results elided / bytes saved by
  [tool-result aging](components/agent.md), by role — the counter that makes the
  aging's effect measurable rather than assumed).
- **Logs** — the trusted side's structured `slog`, exported as OTel log records and
  **trace-correlated**: every log call carries the active context, so a record lands
  with the trace/span id of the work that emitted it. The same single source feeds two
  sinks — the live activity feed (below) and the OTel logs backend — never a second
  instrumentation pass.

Exporting OTel keeps the infra-level timeline in proven tooling and frees the bespoke
UI to focus on factory-specific views. Export is off by default; a configured
[`otel.endpoint`](configuration.md) turns it on, and an authenticated backend is
reached with export **headers whose credential comes from the environment, never from
config** (the same key-handling discipline as the model registry).

### Logs are trusted-side only — by construction

A log record is trusted telemetry, so only **trusted host-side code** writes one: the
broker, orchestrator, runner, and the host-side [agent loop](components/agent.md)
itself (which *observes* an untrusted model and drives an isolated sandbox — its
`slog` is instrumentation, not the agent's voice). The genuinely untrusted material —
model-generated text and sandbox command output — is **never** a log record. It is
captured as span attributes and harvested to the [artifact
store](components/artifact-store.md) as *evidence*. So "all logs" is the complete
trusted-side picture with nothing to filter, and the property that makes it
trustworthy is the same one behind [producer ≠
verifier](verification.md): **the watched does not write the trusted log stream.** An
agent cannot forge or suppress its own audit trail because it does not author it —
the boundary that contains it is the boundary that records it.

### Correlation: one schema across all three signals

Three signals in one backend are only worth having if they *join* — "show me the
spans **and** the logs **and** the metrics for this issue / soul / stage." That
requires every signal to carry the **same attribute keys with the same meaning**, so
the schema is defined **once** (the `telemetry` package is its single source of truth)
and emitted from named constants, never inline string literals, at every site —
spans, metric dimensions, and log records alike. The join columns are the harness's
domain dimensions: issue id, epic id, soul, role, model, invocation id, attempt, and
emitting component. A log record carries them by enriching **one per-invocation
logger** at the invocation boundary (where the work's identity is in hand), so a log
line lands with the same `issue`/`soul`/`model` as the span on its trace and the
metric it contributed to.

One rule is binding because violating it is silent and expensive: **the
high-cardinality identifiers (issue, epic, invocation id) are trace/log attributes
only — never metric dimensions.** A metric labelled by an unbounded id spawns a new
time series per value and melts the backend. Metric dimensions stay **bounded**
(role, stage, soul, model, gate-check, pass/fail, and the small-integer attempt
count); the unbounded ids live on traces and logs, where high cardinality is exactly
what you want for drill-down.

---

## Live vs. history

The control room must show both "everything going on" and the full past:

- **Live** — NATS events → SSE → the board and activity feed update themselves (no
  polling). See [messaging.md](messaging.md), [control-room.md](control-room.md).
  Two event families feed this: the single-writer orchestrator's typed
  **`issue-state`** transitions (which the board / DAG / dead-letter views refresh off,
  for crisp animated card moves) and the broker's **`agent-event`** stream (which the
  activity feed shows). The activity feed carries two sources side by side: **agent**
  events brokered from
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

### The live read model

The control room's *live* views (board, DAG, dead-letter, status bar, epic roll-up) do
**not** read work state by polling beads. beads is the **durable log**; the live **read
model** is the orchestrator's
[work-graph projection](components/orchestrator.md#live-state-vs-durable-state--the-work-graph-projection)
— the single writer's own in-memory view of every issue's current state, which is
consistent the instant a status is written. The control room consumes it
**snapshot-then-stream**: a snapshot of the projection at connect, then the
[`issue-state` events](messaging.md) applied as gap-free deltas. This is why the board
agrees with the single writer's reality (no "card shows `open` while its agent runs")
and why it adds no `bd list` load to the store. The read model is the consistent
*surface*; beads stays the durable *truth* and the cold-start hydration source.

The model binds to topology: **co-located** (`software-factory run`, the default) the control
room reads the in-process projection live; **standalone** (`software-factory serve`, no attached
orchestrator) there is no projection, so it degrades to a static **beads snapshot** — the
same way [`/events`](control-room.md) has no live feed without in-process NATS.

---

## Replayability — the differentiator

Because the broker captured every agent action, you can **replay an invocation's
decision trail**: exactly what the LLM saw and did, step by step, live or after the
fact. This is what turns adequate dashboards into *great* observability — when an
autonomous change looks wrong, you can answer *why* with certainty rather than
guessing. It is also the audit mechanism that makes a no-human-review system
accountable.

---

## Decisions

- **Signal schema / attribute conventions — decided.** The `telemetry` package owns the
  span names, metric names, and `harness.*` attribute keys as one definition; every
  emitter uses the constants, and all three signals share the join columns so they
  correlate (see [Correlation: one schema across all three signals](#correlation-one-schema-across-all-three-signals)).
  The binding rule: unbounded ids on traces/logs only, metric dimensions stay bounded.
- **Collector is embedded — decided.** Export is the in-process OTel SDK exporter
  straight to the backend; there is no standalone collector by default. A sidecar
  collector is an *optional* overlay for distributed/multi-host deployments
  — fan-out to several backends or
  tail-sampling at scale — and is invisible to the harness, which always just speaks
  OTLP to [`otel.endpoint`](configuration.md). Export is a **trusted-layer egress**:
  the sandbox never exports; trusted host components do, with an env-injected
  credential — the same posture as the post-merge git push in [security.md](security.md).

## OPEN questions

- Event schema / span attribute *values* beyond the join columns (per-tool, per-check
  detail) — extended as emitters are added; the keys and the cardinality rule are fixed.
- **Production retention tiers** (how long live events, traces, and full transcripts are
  kept) — still open; it is a production concern, not yet designed. The durable
  record is the artifact store + git + beads (replay reads the *transcript*, not the
  trace backend), so telemetry retention is operational, not forensic. The demo sidesteps
  it entirely: its OTel backend is **ephemeral** (`docker run --rm`, no volume, wiped each
  run). The precise GC rule for the durable evidence lives in
  [components/artifact-store.md](components/artifact-store.md).
