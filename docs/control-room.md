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
| **Board** | `/board` | Kanban over all issues, grouped into columns by pipeline stage. There is one column for **every declared stage** of the configured DAG, in flow order, whether or not it currently holds work — an empty stage is a count-0 column, not a missing one, so the board shows the shape of the whole pipeline even on an idle/empty run (an ad-hoc role not in the DAG, and the catch-all *unassigned* column, appear only when they hold cards). Cards show id/title/status/spec/retry generation and two **client-ticked timers** — time in the current state (`working`/`queued`/`blocked`) and total time since creation; click through to the **live invocation view** (which hands off to the forensic detail/Replay on termination). It refreshes on the orchestrator's typed [issue-state event](../specs/messaging.md), so a card moves *exactly* when the work advances, and the move is **animated** via the browser's View Transitions API (instant where unsupported). |
| **DAG** | `/dag` | The issue dependency graph — what blocks what — rendered server-side to SVG (pure Go, no graphviz, no client graph lib). Hover highlights a node and its neighbours; click drills into the issue. |
| **Activity** | `/activity` | "What the agents *and the factory* are doing right now" — a live feed with two sources, filterable by an **All / Agents / System** toggle. *Agent* rows are brokered from inside the sandbox: streamed model output coalesced into readable rows — `token` (the answer) and `reasoning` (the model's thinking, shown even when a turn is all tool calls) — plus one `tool` row per tool call (e.g. `write_file index.html`). *System* rows are the factory's own log teed in (`info`/`warn`/`error` from the orchestrator/runner/gate — dispatch, sandbox provision, gating, merge, dead-letter), tinted distinctly. So an agent that works purely through tool calls — and the machine driving it — both stay visible. |
| **Merge Queue** | `/merge` | The serialized merge train in flight — each `integrate` candidate's current step (`queued` → `rebasing` → `re-gating` → `landed`, or the terminal `conflicted` / `regate-failed`), tinted so the eye lands on the failures. A landed row shows its main commit and links onward to **Provenance**; a failed row links to the issue for its dead-letter / fix routing. Fed by the typed [`merge-state` events](../specs/messaging.md), so it surfaces the rebase-and-re-gate interval beads alone can't show. **Read-only** — the human never reorders the queue; integration is the orchestrator's function, not a lever. |
| **Dead-letter** | `/dlq` | The escalations awaiting a human — the primary *action* surface. Each row shows the triage signals (cumulative spend, retry generation, spec) and the dead-letter reason, and links to the detail page, the verification view, and Resolve mode. An empty queue reads as reassurance, not an error. |
| **Budgets** | `/budgets` | Per-epic and per-issue burn vs. cap (tokens, USD, wall-clock), tinted by how close to the cap, breaches first. Read off the exact numbers the orchestrator enforces on. |
| **Provenance** | `/provenance` | Recent merged commits, each tracing commit → issue → soul → model → prompt → evidence, with the prompt and each passing gate check linking to its raw artifact. When [signing](../specs/security.md) is configured, each commit also carries a **signature verdict** badge — `signed` (verified against the allowed-signers file), `unsigned`, or `unverified` (signed by an unrecognized key) — so the chain's cryptographic integrity is visible at a glance, not just its content. |
| **Config** | `/config` | The declared factory at rest, **read-only**: the identity strip (config root · active infra overlay · autonomy profile · validated), the **role-flow pipeline** rendered server-side to SVG (stages as nodes — `produces` solid, `on_failure` dashed amber — the *shape* work flows through, distinct from the issue DAG which is the work itself; cross-linked each way), the stages table, the check registry, the **resolved** soul roster (model→provider+cost, sandbox→concrete digest, ordered by selector specificity with the catch-all marked, plus the trusted non-sandboxed requirements planner), policy (budgets/retries/TCB paths), and **redacted** infra. Each soul (and the requirements planner) carries a lazy **persona** fold — expand it to fetch and view that soul's verbatim system prompt as inert escaped text (the literal prompt the model receives), served from `GET /config/souls/{name}/persona`. Each section also has a "raw" fold showing the *effective config re-serialized with the same redactions applied* (never the file bytes, so raw can't leak what rendered masks). It is the in-process validated config the running factory holds (zero staleness), so it is a plain snapshot — **not** a live feed. A standalone `harness serve` shows the not-attached notice. |
| **Create Task** | `/create` | The wizard — author a new spec and seed work. See below. |

Drill-through pages (not in the nav):

- **Issue / invocation detail** (`/issue/{id}`) — the forensic snapshot of one issue:
  the brief, cumulative spend, merge provenance, an evidence list (prompt, traceability
  map, each passing gate check, transcript) linking to raw artifacts, and the candidate
  diff. Blocked issues show the dead-letter reason and a "Resolve →" link.
- **Live invocation** (`/invocation/{id}`) — watch *one* worker think: the agent's
  `reasoning`/`tool`/`token` stream scoped to a single invocation (filtered server-side
  by the issue id the runner stamps on each event — no second beads read), with a header
  carrying its role/stage and a budget meter advancing toward the wall/token ceiling. It
  is the one deliberate live *detail* surface, and it is bounded: **live only while the
  agent runs** — on termination it stops refreshing and hands off to the forensic Replay
  (when merged) or issue detail of the same invocation. Reached by drilling from a board
  card or an activity-feed row.
- **Replay** (`/replay/{id}`) — the reconstructed decision trail of an invocation, turn
  by turn: exactly what the model saw (inbound messages), what it said, its tool calls,
  stop reason, and per-turn token usage. Reconstructed from the broker-captured
  transcript.
- **Verification** (`/verification/{id}`) — the factory's *trust argument* for one issue,
  made legible: the producer≠verifier soul split (the `author-tests` and `implement`
  souls side by side, with the `qa` gate marked as running independently in the clean
  [verification sandbox](../specs/glossary.md#verification-sandbox) — no verifier soul,
  and that is the point), the red→green proof per check, the mutation score vs threshold,
  the scanners, and the test↔spec traceability map. Reconstructed from the persisted
  [`gate-verdict` record](../specs/components/artifact-store.md) (recorded for *every* gate
  run), so it renders for **rejected** candidates too — exactly what a dead-letter triager
  needs. A forensic snapshot (no live refresh). Drilled into from issue detail and the DLQ.
- **Raw artifact** (`/artifact/{hash}`) — streams artifact content as `text/plain` with
  `nosniff` (artifact bytes are untrusted agent output and must never be interpreted as
  HTML/script).

## The status bar

A thin status bar rides every page (it's part of the layout chrome, not a destination) —
the "is the factory healthy?" glance: **queue depth** (work still in flight) · **active
agents** · **open escalations** (the dead-letter count, tinted when non-zero) · a
**budget-health dot** (emerald healthy / amber ≥80% of a cap / rose breach, matching the
Budgets view) · **last merge**. It's assembled from the same beads/provenance reads that
back the board, dead-letter, and budget views — so it agrees with them by construction —
and refreshes live off the existing `issue-state` and `agent-event` streams (no new
stream), with a periodic backstop. *Active agents* is derived from the distinct agent ids
seen on the live activity buffer within a recent window — no new registry. With no read
model wired (standalone `harness serve`) it degrades to a neutral static bar.

The open-escalation count is also a **push**: the control room tails the durable
[`harness.dlq`](../specs/messaging.md) subject and fires a **browser notification** when a
new dead-letter arrives. The dead-letter queue is the human's only action surface, so an
arrival is the one factory event that should reach an operator who isn't looking —
everything else is pull. The durable queue stays the source of truth; the alert is only
the nudge to come look (browser notifications require granting permission; if denied, the
bar's count still updates live).

## The Create-Task wizard

The wizard is how a human authors intent — the consent boundary past which everything
is autonomous. It's a guided conversation, not a form:

1. **Conversation** (`/create`) — a chat with the trusted requirements planner that
   streams its reply token-by-token over SSE. The persona probes for examples, edge
   cases, what to reject, and what's out of scope, converging on testable acceptance
   criteria. When the repo has a `specs/README.md`, a grounded session **opens with an
   orientation message**: the planner has read the project's spec index host-side, so
   its first reply is grounded in what already exists rather than a blank slate. If
   `requirements_planner.sandbox_profile` is configured (see
   [configuration.md](configuration.md)), the planner can also **explore the existing
   codebase** read-only to ground its specs — a small status strip ("🔍 read_file …")
   shows each read while it looks. The reads run in a read-only, zero-network sandbox
   over the repo, provisioned lazily on first use; the planner still writes nothing.
2. **Alignment ledger** — a live panel beside the conversation showing where things
   stand: each fork in one of four states (`open`, `agreed`, `discussing`, `deferred`)
   with a one-line rationale. Forks are surfaced and answered **in batches**: each
   answerable fork offers selectable option chips, a first-class **free-text** box (for
   the nuance the canned options missed), and a **"let's discuss"** flag (with an
   optional note), and you resolve any combination in a single **Submit answers**. Every
   answer is folded back through the planner (not a separate client-side model), which
   reconciles the batch on its next turn — including dropping forks one answer made moot.
3. **Draft** — once intent converges, the planner drafts the spec markdown and the
   seed issues. You see the proposed specs and issues before committing.
4. **APPROVE** — an explicit consent gate, **soft-gated on a converged ledger**: you
   cannot commit with a fork still `discussing` (the planner names which to resolve
   first), but plain `open` forks are **auto-deferred and recorded** rather than blocking
   you — nothing is silently dropped. Approving commits the **server-side** draft (the
   trusted planner's snapshot, never browser content): it validates the spec (safe paths,
   link integrity, every spec maps to ≥1 issue, seed issues enter at a legal entry
   stage), writes the spec files, stores the transcript, writes a decisions sidecar (the
   `agreed` forks as decisions and the `deferred` forks as "deliberately left open: X"
   pre-context for a later escalation), git-commits, and creates the seed issues. The
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
job is to show per-turn agent progress as it happens. The **Merge Queue** refreshes on
the dedicated [`merge-state` event](../specs/messaging.md), since the steps it shows
(`rebasing`/`re-gating`) are merge-queue transitions, not beads-status transitions.
Budgets, Provenance, and the wizard refresh on the same substrate. Forensic pages (issue detail, Replay, Verification) are
deliberately *not* live — they're
snapshots, not feeds. Authentication is not yet implemented (an open item); session ids
in the wizard are crypto-random but there's no login gate.
</content>
