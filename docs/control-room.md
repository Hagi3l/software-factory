# The control room

The human's window into the factory and their only action surface. It's a web UI built
into the binary — server-rendered with templ, live over SSE, with htmx + Alpine for
interactivity and Tailwind for styling, all embedded via `embed.FS`. No external
runtime, no separate frontend build. The authoritative design is
[`specs/control-room.md`](../specs/control-room.md) and
[`specs/observability.md`](../specs/observability.md).

## Running it

Two ways, with one important difference — **whether there's a live pipeline to read**:

```bash
# Co-located with a run — full live data (recommended):
harness run --serve-addr 127.0.0.1:8080 ...

# Standalone — static + read views only, no live feed:
harness serve --addr 127.0.0.1:8080
```

Standalone `serve` has no NATS, so the live feed (`GET /events`) returns 503 and the
data-backed views show a "not attached" notice. The live SSE feed needs the run's
in-process NATS, so it's served co-located. Open <http://127.0.0.1:8080>.

## The views

Top navigation:

| View | Path | What it shows |
|------|------|---------------|
| **Board** | `/board` | Kanban over all issues, grouped into columns by pipeline stage. Cards show id/title/status/spec/retry generation and two **client-ticked timers** — time in the current state (`working`/`queued`/`blocked`) and total time since creation; click through to the detail page. It refreshes on the orchestrator's typed [issue-state event](../specs/messaging.md), so a card moves *exactly* when the work advances, and the move is **animated** via the browser's View Transitions API (instant where unsupported). |
| **DAG** | `/dag` | The issue dependency graph — what blocks what — rendered server-side to SVG (pure Go, no graphviz, no client graph lib). Hover highlights a node and its neighbours; click drills into the issue. |
| **Activity** | `/activity` | "What the agents *and the factory* are doing right now" — a live feed with two sources, filterable by an **All / Agents / System** toggle. *Agent* rows are brokered from inside the sandbox: streamed model output coalesced into readable rows — `token` (the answer) and `reasoning` (the model's thinking, shown even when a turn is all tool calls) — plus one `tool` row per tool call (e.g. `write_file index.html`). *System* rows are the factory's own log teed in (`info`/`warn`/`error` from the orchestrator/runner/gate — dispatch, sandbox provision, gating, merge, dead-letter), tinted distinctly. So an agent that works purely through tool calls — and the machine driving it — both stay visible. |
| **Dead-letter** | `/dlq` | The escalations awaiting a human — the primary *action* surface. Each row shows the triage signals (cumulative spend, retry generation, spec) and the dead-letter reason, and links to the detail page and Resolve mode. An empty queue reads as reassurance, not an error. |
| **Budgets** | `/budgets` | Per-epic and per-issue burn vs. cap (tokens, USD, wall-clock), tinted by how close to the cap, breaches first. Read off the exact numbers the orchestrator enforces on. |
| **Provenance** | `/provenance` | Recent merged commits, each tracing commit → issue → soul → model → prompt → evidence, with the prompt and each passing gate check linking to its raw artifact. |
| **Create Task** | `/create` | The wizard — author a new spec and seed work. See below. |

Drill-through pages (not in the nav):

- **Issue / invocation detail** (`/issue/{id}`) — the forensic snapshot of one issue:
  the brief, cumulative spend, merge provenance, an evidence list (prompt, traceability
  map, each passing gate check, transcript) linking to raw artifacts, and the candidate
  diff. Blocked issues show the dead-letter reason and a "Resolve →" link.
- **Replay** (`/replay/{id}`) — the reconstructed decision trail of an invocation, turn
  by turn: exactly what the model saw (inbound messages), what it said, its tool calls,
  stop reason, and per-turn token usage. Reconstructed from the broker-captured
  transcript.
- **Raw artifact** (`/artifact/{hash}`) — streams artifact content as `text/plain` with
  `nosniff` (artifact bytes are untrusted agent output and must never be interpreted as
  HTML/script).

## The Create-Task wizard

The wizard is how a human authors intent — the consent boundary past which everything
is autonomous. It's a guided conversation, not a form:

1. **Conversation** (`/create`) — a chat with the trusted requirements planner that
   streams its reply token-by-token over SSE. The persona probes for examples, edge
   cases, what to reject, and what's out of scope, converging on testable acceptance
   criteria.
2. **Alignment ledger** — a live panel beside the conversation showing where things
   stand: each item agreed or open with a one-line rationale, and forks rendered as
   selectable chips with their tradeoff. Clicking a chip steers the conversation
   (it's folded back through the planner, not a separate client-side model).
3. **Draft** — once intent converges, the planner drafts the spec markdown and the
   seed issues. You see the proposed specs and issues before committing.
4. **APPROVE** — an explicit consent gate. Approving commits the **server-side** draft
   (the trusted planner's snapshot, never browser content): it validates the spec
   (safe paths, link integrity, every spec maps to ≥1 issue, seed issues enter at a
   legal entry stage), writes the spec files, stores the transcript, writes a decisions
   sidecar from the agreed ledger items, git-commits, and creates the seed issues. The
   running pipeline picks them up.

If the wizard isn't configured (no `requirements_planner` block, or standalone
`harness serve`), `/create` shows a "wizard disabled" notice.

## Resolve mode

The wizard's second entry mode, launched from a dead-lettered issue (`/resolve/{id}`,
or the "Resolve →" links on the DLQ and detail pages). It's the guided way to apply the
**only** human lever for stuck work — refining the spec.

It pre-loads the escalation, the governing spec slice, and the transcript that raised
it, then runs the same conversation/ledger/draft flow. Before the consent gate it shows
the **blast radius** of your proposed spec edit: which in-flight issues would reissue
and which already-merged `(epic, spec-path)` groups would re-derive. APPROVE writes the
refined spec, commits it, and **reopens** the dead-lettered issue so the next dispatch
re-resolves the edited slice. Resolve creates no new seed issues — new scope goes
through Create.

## A note on liveness

Live views refresh over SSE, each with a slow periodic backstop so a settled board
still converges. The **Board, DAG, and DLQ** refetch on the orchestrator's typed
[issue-state event](../specs/messaging.md) — they care about *transitions*, so they
update precisely when work moves between stages (and the board animates the move). The
**Activity** feed instead refreshes on the finer-grained agent-event stream, since its
job is to show per-turn agent progress as it happens. Budgets, Provenance, and the
wizard refresh on the same substrate. Forensic pages (issue detail, Replay) are
deliberately *not* live — they're
snapshots, not feeds. Authentication is not yet implemented (an open item); session ids
in the wizard are crypto-random but there's no login gate.
</content>
