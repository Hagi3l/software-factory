# Control Room

The web UI. It is the human's entire window into the factory and their **only**
place to act. Two faces: a read-only observability surface, and the interactive
**wizard** through which humans author and refine intent.

See also: [observability.md](observability.md),
[specs-process.md](specs-process.md), [workflow.md](workflow.md),
[security.md](security.md).

---

## Stack

A server-driven hypermedia app over the Go backend — no SPA, no heavy client JS.

| Concern | Choice |
|---------|--------|
| Interactions | **htmx** (server-rendered HTML, swaps over the wire) |
| Local interactivity | **Alpine.js** (toggles, filters, small client state) |
| Templating | **templ** (typed Go components; compiled via `templ generate`) |
| CSS | **Tailwind** via the **standalone CLI** (single static binary, no Node) |
| Assets | **`embed.FS`** — htmx, Alpine, and compiled CSS embedded into the Go binary |
| Live updates | **SSE** (htmx SSE extension), fed from NATS |

The result is a single self-contained binary: no runtime asset serving, no
external toolchain at deploy time. The Tailwind CLI and `templ generate` are part
of the build (`go generate`), not the runtime.

---

## The views

| View | Purpose | Source |
|------|---------|--------|
| **Board** | kanban over beads issues by stage; live, with animated card moves, per-card timers, and auto-scroll to the work frontier | beads + NATS (SSE) |
| **DAG** | the [issue dependency graph](glossary.md#issue-dependency-graph); blockers, merge order | beads → server-side SVG |
| **Activity feed** | what the agents *and the factory* are doing right now — agent `token`/`reasoning`/`tool` events plus system lifecycle events, filterable by source | NATS events (SSE) + factory log bridge |
| **Issue / invocation detail** | Brief, transcript, candidate diff, gate evidence, budget, retries | beads + [artifact store](components/artifact-store.md) + [trace](observability.md) |
| **Live invocation** | watch *one* running agent: its `reasoning`/`tool`/`token` stream scoped to a single invocation, with its live budget (token burn broken down by kind) — until it terminates, then hands off to Replay | NATS events (SSE), scoped by issue/agent |
| **Verification** | the trust argument for one issue, forensically: producer≠verifier soul split, red→green proof, mutation score, scanners, the test↔spec map | [artifact store](components/artifact-store.md) (`gate-verdict`) + git |
| **Merge queue** | the [serialized merge train](integration.md) in flight: each `integrate` candidate's step (`queued`/`rebasing`/`re-gating`/`conflicted`) | NATS [`merge-state`](messaging.md) (SSE) + beads |
| **Dead-letter queue** | escalations needing a human — *the action surface* | beads + artifact store |
| **Budgets** | token/$/wall-clock burn vs. caps, per epic/issue — each token total broken down by kind (input/output/cached) | beads + OTel metrics |
| **Provenance** | trace any merged commit back to issue→soul→model→prompt→evidence, with each commit's [signature verdict](security.md) (signed / unsigned / unverified) when signing is configured | git + artifact store |
| **Config** | the declared factory at rest: role-flow pipeline, gate checks, the resolved soul roster, policy, and redacted infra — read-only | validated config (in-process) |

### Rendering
- **Live:** NATS → SSE → htmx swaps; the board and feed update without refresh.
  The work-state views (board, DAG, dead-letter, status bar, epic roll-up) read the
  orchestrator's [work-graph projection](observability.md#the-live-read-model) — the
  consistent live **read model** — not a direct beads poll, so they never lag the single
  writer (no card showing `open` while its agent runs) and add no `bd list` load;
  [`issue-state` events](messaging.md) stream the deltas onto a snapshot taken at
  connect. beads is the durable/forensic source the historical pages render from.
  The live pattern is **server-render-a-fragment + htmx re-fetch**, *not* DOM
  `sse-swap`: a live view wraps its content in an SSE-connected element and
  re-fetches a bare server-rendered fragment on `hx-trigger="sse:<event>
  throttle:Ns, every Ns"` — the SSE event is a *nudge* to refetch, with a slow
  periodic `every Ns` backstop so a settled view still converges when the event
  stream goes idle. `sse-swap` is deliberately avoided because the raw
  `agent-event` payload is JSON (and the runner's per-token stream is a firehose),
  not DOM-ready HTML; rendering stays on the server where the templ components live.
  Views key their nudge to the **most precise event they can:** the board / DAG /
  dead-letter views refetch on the typed [`issue-state` event](messaging.md) (so a
  refresh fires on the actual transition), the activity feed on `agent-event` (the
  thing it *is* showing); the periodic backstop stays on all of them.
  **Exception — interactive/stateful panels carry no periodic backstop.** A fragment
  that holds in-progress human input or client-side disclosure state — the wizard's
  [alignment-ledger](#the-alignment-ledger) batch form, and the draft panel's
  expandable spec diffs — must *not* be re-fetched on an `every Ns` clock: a blind
  periodic re-render would discard a half-filled answer (selected chips, typed
  free-text) or snap shut a spec diff the human is mid-read. These panels refetch
  **only** on their precise tool-channel nudge — the ledger on the planner's
  `update_ledger` (`ledger` event), the draft on `propose_draft` (`draft` event) —
  which by construction fires only at a planner-turn boundary, never while the human
  owns the form; a nudge missed across an SSE drop is recovered by refetching on
  **(re)connect** (`htmx:sseOpen`), not on a clock. The clock backstop is right for
  read-only views that converge to server truth; it is wrong wherever the client DOM
  carries unsubmitted state the server does not yet know.
- **Historical/forensic:** plain server-rendered pages from the stores, with the
  structured timeline from the OTel trace backend. Supports **replay** of an
  invocation's decision trail (see [observability.md](observability.md)), including
  each turn's token usage broken down by kind (input/output/cache read/cache write).
  A detail
  page is a forensic snapshot, not a feed — it is deliberately *not* live. The one
  deliberate exception is the [live invocation view](#the-live-invocation-view),
  which *is* a feed — but only while its agent runs; the moment the invocation
  terminates it hands off to the forensic [Replay](glossary.md#replay) of the very
  same invocation. Live and forensic are two phases of one invocation, not a live
  detail tier: nothing that has *finished* is ever shown as a feed.
- **Graph viz:** render the DAG **server-side to SVG** (Go → DOT/Graphviz or d2)
  and embed it; hover/click-to-drill via Alpine + htmx on the SVG nodes. No
  client-side graph **library**. The board's epic-lineage overlay (see *Epics on the
  board* below) is the one deliberate exception: a small **bespoke** client-side SVG
  layer drawn over the live kanban. It pulls in no graph library, and this rule still
  governs the DAG view. The exception is principled — connectors *between the moving
  kanban cards* are inherently a client-side, layout-dependent job (card positions are
  known only in the browser, and they animate), so the rule's real intent (no
  heavyweight client graph engine) holds while the board still gets its threads.
- **Serving artifacts:** artifact bytes are untrusted agent output, served
  `text/plain` + `nosniff` and never interpreted as markup — see
  [security.md](security.md) Control 7.

### The board, in motion

**Columns are the pipeline, not the data.** The board renders one column per
declared stage of the configured [DAG](workflow.md) — the full pipeline
(`requirements → plan → author-tests → implement → qa → integrate`) in flow order,
left-to-right — **whether or not any issue currently occupies it**. An empty stage
is a column with a count of `0`, not an absent column. This is deliberate: the
operator reads the shape of the whole factory at rest, the layout never reflows as
work flows through it, and a card animating into a stage lands in a column that
already exists. A brand-new run with no issues yet shows the full empty skeleton,
not a blank page. Stages that appear in the data but are *not* declared in the DAG
(an ad-hoc role) and the catch-all **unassigned** column are the exception — they
materialise only when they actually hold work, since the config never promised
them. (When not attached to a running factory there is nothing to read, so the
board shows a notice instead; see [observability.md](observability.md).)

Beyond the static shape, the board is a *live* kanban, so two things make a card
legible at a glance:

- **Animated moves.** Each card carries a stable identity (the issue id). When the
  [`issue-state` event](messaging.md) nudges a refetch and the new fragment places a
  card in a different column, the move is **animated** via the browser's View
  Transitions API (htmx opts the swap in) — no client-side graph/animation library,
  no manual DOM diffing; the server still renders the whole fragment and the browser
  tweens between the two states. Where View Transitions are unsupported the swap is
  instant (graceful degradation). The animation fires off the typed transition, so a
  card slides exactly when the orchestrator advances the work.
- **Per-card timers (client-ticked).** A card shows **time in current state**
  (`working 2m12s`, `queued 30s`, `blocked 1h` — the label keyed off status) and
  **total time** since the issue was created. Both are *anchors* the server emits
  once on the card (the orchestrator-stamped `state_entered_at` for the former,
  beads' `created_at` for the latter); a small Alpine ticker advances them in the
  browser every second, so the clock is live **without** the server re-rendering to
  tick it. The current-state timer resets naturally on the next transition (the
  refetched card carries a fresh `state_entered_at`). Optionally the in-progress
  timer tints toward its `budget.wall` ceiling — a live "about to breach" signal off
  data the orchestrator already enforces.
  A **closed** card is the exception: the work is done, so there is no live clock —
  it shows a single *static* lead time (`took 2h05m` = `state_entered_at − created_at`,
  rendered server-side, no ticker). Only `closed` freezes; a **blocked** card keeps
  ticking by design, since its time-in-state is the "how long has this been awaiting
  triage" signal.

**Follow the frontier (auto-scroll).** The board is as wide as the whole pipeline,
so on any real run it is wider than the viewport. Rather than make the operator chase
work with the scrollbar, the board **auto-scrolls horizontally to the work frontier**:
it keeps in view the **leftmost column that still holds incomplete work** — any card
not yet `closed`, so `open` / `in_progress` / **`blocked`** all count as incomplete
(a blocked card therefore *pulls* focus, since it is exactly the work awaiting a
human). When every card is `closed`, the frontier is the **rightmost** column — the
factory is done, so the view rests at the finish line. The frontier column is
**left-aligned** in the viewport, so the operator sees it *and the road ahead* of it.
"Leftmost" is purely positional over the rendered columns, so column order — including
where the ad-hoc and **unassigned** columns sit — is governed by the configured stage
order, not by this behavior.

The frontier is a property of board *state*, so it is **computed server-side** and
recomputed on every refetch — the same [`issue-state`](messaging.md) nudge that
animates a card's move also re-marks which column is the frontier. As work closes out
left-to-right the frontier walks rightward, and the view follows the work across the
board with no human input. It is **on by default** and persists across the live swaps;
a toggle in the board header turns it off, **remembered across visits**, and any manual
horizontal scroll also pauses it until the operator re-enables it — the human stays in
control of their own viewport. It respects `prefers-reduced-motion` (motion suppressed
for operators who ask for it); the first paint snaps to the frontier, later moves ease
smoothly. This is a *viewport* convenience only — it never moves work, just where the
operator is looking.

**Epics on the board.** The board makes the *feature* legible, not just the work items —
a feature is what lands and deploys. This rides the `epic_id` every card already carries
(the root seed's id, threaded across the epic — [workflow.md](workflow.md)), so it needs **no
new data**. The two **grouping** cues below — the epic badge/tint and the lineage thread — are
pure observability and render **independent of [`integration.mode`](integration.md)**: they
appear whenever an issue belongs to a **multi-issue epic** (a real decomposition fan-out), under
`per-item` and `epic` modes alike. The **hero** roll-up is the exception — it is epic-mode-only,
because its lifecycle is defined by the atomic merge. A lone, directly-seeded issue (its own
single-issue epic) gets none of this — the chrome would only be noise.

- **Shared identity (colour).** Each card shows an **epic badge** (the root id, optionally the
  abbreviated feature title) and a left-border tint whose hue is a **deterministic hash of
  `epic_id`**, computed **server-side** and exposed as a CSS custom property (`--epic`) on the
  card so the badge, the tint, and the lineage thread (below) all read **one** colour source —
  no central registry, stable across restarts. Colour is never the *sole* channel (projector
  wash-out, colour-blindness): the badge text is the robust identifier. With v1's single active
  epic the live payoff is **history/audit legibility** (completed and abandoned epics stay
  grouped and distinctly coloured); the disambiguation value compounds once concurrency lands.
- **Lineage thread (curved connectors).** A bespoke client-side SVG layer over the kanban draws a
  **curved connector from each card to the card that produced it**, so a feature reads as a tree
  threading left-to-right through the pipeline: root → its author-tests children → each child's
  implement → qa. The producer link is recoverable from data the cards already carry (the
  predecessor's verified candidate is the next stage's `base`; the decomposition's children point
  back at the root), so it needs **no new edge**. The thread terminates at the **qa** card —
  `integrate` is an inline trusted-merge with no card of its own (under epic mode the landing
  instead shows as the root reaching `done` and the hero completing). Each connector is drawn in
  its epic colour (`var(--epic)`) and is **faint by default**; hovering or focusing a card
  **highlights the whole path through it** (its ancestors *and* descendants) and **dims** the
  rest, so relationships are explorable without the board ever looking busy. Connectors **settle
  after** a card's View-Transitions move rather than chasing it mid-tween, and the layer redraws
  from the cards' stable ids on each live refetch; it draws statically under
  `prefers-reduced-motion`. Sibling-ordering edges (a planner's inter-child `blocked-by`) are
  **not** drawn as lineage — the thread stays a clean producer tree; they may later surface as a
  distinct (dashed) "waits-for" edge. This overlay is the one client-side-graph exception noted
  under *Graph viz* above.
- **The root card is the hero (epic mode).** Under [`integration.mode: epic`](integration.md) the
  epic root renders distinctly from its children, with a **progress indicator** (children
  **integrated** / total — counting the durable [`integrated`](integration.md) marker, and
  excluding the epic root and any superseded retry attempt, so it reads *true* progress
  rather than any closed bead; see [integration.md](integration.md) "Integrated vs.
  closed") and the live `budget` against the [`epic_budget`](workflow.md) cap, so
  "bounded autonomy" is visible as the feature builds. It carries the feature through an
  **`integrating`** state while children finish and flips to **`done`** as the single terminal
  merge lands — the board's read of the atomic feature landing, so the operator watches the
  *feature* complete without reading commits. The hero is epic-mode-only by design: in `per-item`
  mode the root closes at decomposition and never tracks the live feature, so its
  `integrating → done` state (read from git, not issue status) would be meaningless — there the
  grouping cues above carry the feature's legibility on their own.

Note what stays absent **by design**: there is no drag-to-move. Humans never move
work — the orchestrator is the single writer and the human's only levers are the
spec (the wizard) and escalations (the dead-letter queue). The board is read-only;
cards move because the *factory* advanced them, never because a human dragged one.

---

## The live invocation view

The [activity feed](#the-views) shows *everything at once*; the board shows where work
*is*. Neither lets you **watch one worker think**. The live invocation view does: given
a single running agent, it streams that invocation's `reasoning` / `tool` / `token`
events — the same firehose, filtered to one issue — with a header carrying its role and
stage and a live budget meter ticking toward the wall/token ceiling the orchestrator
enforces. You reach it by drilling from a board card (the agent currently working it) or
an activity-feed row.

Scoping is cheap by construction: the [`agent-event` envelope carries the issue id and
role](messaging.md), so the server filters the live buffer to one invocation without a
second beads read. It renders with the same **fragment-refetch-on-SSE-nudge** pattern as
the feed (nudge on `agent-event`, periodic backstop), not `sse-swap` — the per-token
stream is a JSON firehose, and rendering stays server-side where the templ components are.

**It is live only while the invocation runs.** This is the one deliberate exception to
"a detail page is not a feed" (see [Rendering](#rendering)), and it is bounded: when the
agent terminates, the view hands off to the forensic [Replay](glossary.md#replay) of the
*same* invocation — the turn-by-turn decision trail reconstructed from the
[artifact store](components/artifact-store.md). Live is the in-flight phase; Replay is the
settled one. Nothing finished is ever a feed, so the forensic guarantee holds.

---

## The verification view

This is the factory's **trust argument, made legible**. [Verification](verification.md)
is how the factory merges with no human reading code — yet its proof was historically
computed by the gate and thrown away, leaving only a green checkmark. This view renders
the whole argument for one issue, forensically, from the persisted
[`gate-verdict` record](components/artifact-store.md):

- **Producer ≠ verifier** — the `author-tests` soul and the `implement` soul shown side
  by side (from the souls [recorded on advance](verification.md), also on the merge
  trailer), with the `qa` gate marked as running independently in a clean
  [verification sandbox](glossary.md#verification-sandbox) — there is no verifier *soul*
  to show, and that is the point.
- **Red→green proof** — tests fail on the base, pass on the candidate, per check.
- **Mutation score** vs. its threshold; **scanners** with their parsed
  [findings](verification.md) (`file:line` + message, not a raw log), each check shown
  passed / failed / **not-run** (a check the build precondition short-circuited).
- **The [test↔spec traceability map](verification.md)** — each test against the spec
  heading and sentence it claims to encode: the only window into how the author read the
  prose.
- **The transformation log** — when the issue ran semantic write tools
  ([components/agent.md](components/agent.md) "Mechanism is recorded"), each `rename` /
  `code_action` with the mechanism it ran through: **semantic** (the language server's own
  WorkspaceEdit) or **text fallback** (the degraded word-boundary floor, which can rewrite
  comments and string literals). The count of text fallbacks and each fallback's precision
  note are surfaced so the imprecise edits — the ones that warrant a closer look — read at a
  glance. It is the verification-side payoff of recording the mechanism: the gate weighs a
  text-fallback rename more suspiciously, and so can a human. Omitted when the issue ran no
  semantic write tools.

It is a **forensic snapshot**, not a feed (a settled proof, like Replay), and it is
rendered for *rejected* candidates too — a failed verdict is exactly what a human triaging
the dead-letter queue needs to see. Artifact bytes it links (raw gate output) are served
untrusted per [security.md](security.md) Control 7.

---

## The merge-queue view

[Integration](integration.md) is serialized and otherwise invisible: between "a branch's
gate passed" and "a commit appeared on `main`" lies the rebase-and-re-gate interval where
combinations actually break, and nothing showed it. This view makes the merge train
observable, fed by the typed [`merge-state` events](messaging.md): an ordered list of
`integrate` candidates with each one's current step — `queued → rebasing → re-gating →
landed`, or the terminal `conflicted` / `regate-failed`. The interesting rows are the
terminal failures, which correlate with the [dead-letter](#the-views) entry or fix issue
the same transition routes; a landed row links onward to [Provenance](#the-views). It
refetches on the `merge-state` nudge with the usual periodic backstop. Like the board it
is **read-only** — the human never reorders the queue; the orchestrator is the single
writer and integration is its function, not a lever.

---

## The config view

The factory is **config-driven** ([configuration.md](configuration.md)) — the pipeline,
the souls, the gates, and the infrastructure are all declarative — yet none of it was
*readable* anywhere. The board renders the stage columns, the budgets view the policy
caps, and verification/provenance the souls *per issue*; but the **declared factory as a
whole** — the thing in `harness.yaml` + `souls/` — could be run and not inspected. This
view is that missing window: **the declarative pipeline at rest**, read-only.

It is the natural complement to the [DAG view](#the-views): the two are the *two graphs*
the [architecture](architecture.md) distinguishes — the DAG view shows the **issue**
dependency graph (the data flowing through), this view the **role-flow** (the declared
pipeline the data flows through). They stay separate pages — the DAG view is about work,
the config view about shape — cross-linked one line each way, never duplicated.

**Read-only is the principle, not a limitation.** The control room has exactly two write
surfaces — the [wizard](#the-wizard--the-only-human-in-the-loop-surface) (spec) and the
[dead-letter queue](#the-views) (approve/reject) — because the human's only levers are
intent and escalation. Config is neither: it is the substrate, changed by editing files
and restarting (`configuration.md` decides restart over hot-reload for v1). Letting the UI
edit it would invent a third write surface the architecture deliberately omits. The config
view *shows*, never mutates.

**It reflects the running factory by construction.** The control room is
[co-located](observability.md) in the `harness run` process, so the config it renders is
the very validated object that process is running — not a re-read of files that may have
since moved. There is therefore no staleness and no "reload" affordance, and the page is a
plain server-rendered snapshot: config is restart-static, so it is deliberately *not* a
feed (see [Rendering](#rendering) — nothing that cannot change while you watch is rendered
live). Under a standalone `harness serve` with no attached factory there is no config to
show, so the view shows the same not-attached notice as the other live views.

What it renders, in flow order:

- **Identity strip** — the config root, the active infra overlay (the `infra.<env>.yaml`
  in force), the `policy.profile` (`trusted-dev`/`autonomous`), and that the config passed
  startup validation. *Which* factory is this.
- **Advisories** — the non-fatal warnings `harness validate` surfaces (the
  [`Warnings`](configuration.md) channel, distinct from validation faults that fail startup):
  chiefly producer/verifier **model-family overlap** (a same-family producer and verifier share
  correlated blind spots, weakening the N-version independence
  [verification.md](verification.md) recommends — model choice is the operator's, so it is
  *advised*, never forced), plus a package-proxy or git-remote named but not allowlisted (dead
  config the broker would deny). Surfacing them here puts the same safety signal where the
  operator inspects the running factory, not only in the launch logs. Shown only when the config
  trips one — a clean config renders no section.
- **Pipeline graph** — the role-flow rendered **server-side to SVG** (the same renderer the
  [DAG view](#rendering) uses, fed the declared stages instead of issues), with `produces`
  and `on_failure` edges styled distinctly so the happy path and the retry/branch edges
  read apart.
- **Stages** — the textual detail behind the graph: each stage's `role`/`kind`, pre/post
  conditions, `on_failure` and `produces`. A stage links to its
  [board](#the-board-in-motion) column, tying the declared pipeline to the work currently
  in it.
- **Checks** — the [check registry](configuration.md): each postcondition name and the
  shell command that realizes it in the verification sandbox.
- **Souls** — the roster, shown **resolved**, not as raw soul files: each soul's `model`
  joined to its provider and cost, its `sandbox` profile joined to the concrete
  digest-pinned artifact, and — for a role fulfilled by several souls — the souls ordered
  by **selection specificity**, the empty-selector one marked the catch-all default. This
  is the orchestrator's own [selection resolution](configuration.md) made legible, so "why
  did this issue route to that soul" is answerable by reading, not tracing. Each soul's
  **persona** — its verbatim system prompt ([components/agent.md](components/agent.md)) — sits
  behind a per-soul lazy fold: the path is always shown, and expanding it fetches the file's
  bytes on demand (so the long markdown never bloats the page) and shows them as **inert escaped
  text**, the literal prompt the model receives, never rendered markup. The trusted,
  non-sandboxed **requirements planner** is shown here too — set apart from the sandboxed souls —
  because it carries a persona the same way.
- **Policy** — budgets, retry caps, and the `tcb_paths` globs that define the
  permanently-human-reviewed [TCB boundary](bootstrap.md).
- **Infra** — the environment overlay, **redacted** (below).

**Rendered and raw are one model, two projections.** Every section renders structured, with
a per-section "raw" fold (Alpine) exposing the underlying YAML. Crucially the raw fold is
**not the file bytes**: it is the *effective, post-overlay-merge config re-serialized with
the same redactions applied* — labelled "effective config (redacted)" — so it shows what is
actually running and can never leak what the rendered view masks. One redacted view model
feeds both faces; they cannot disagree.

**Redaction is by allowlist.** Secrets are never in config to begin with (`configuration.md`
— keys come from the environment), so redaction targets *topology*, not credentials: the
`nats.url`, model `endpoint`s, the `otel` endpoint, and the artifact store path/bucket are
masked. The egress `broker.allowlist` and the digest-pinned image/rootfs identifiers are
kept visible — they are operational policy and build provenance, not secrets. The allowlist
masks the named sensitive fields and shows everything else, so a field added to infra later
cannot silently leak. This is belt-and-suspenders pending the open question of **who may
operate the control room** ([OPEN](#open-questions)); when that lands, redaction may follow
the viewer's role.

---

## The status bar and escalation alerts

A thin **status bar** rides every page (part of the layout chrome, not a destination):
queue depth · active agents · open escalations · budget-health dot · last merge. It is
the "is the factory healthy?" glance, assembled from the same reads that back the board,
dead-letter, and budget views, and nudged live off the existing `issue-state` /
`agent-event` streams rather than a new one. *Active agents* is derived from the distinct
agent ids seen on the live event buffer within a recent window — no new registry.

The escalation count is also a **push**: the control room tails the durable
[`harness.dlq`](messaging.md) subject and fires a browser notification when a new
dead-letter arrives. The [dead-letter queue is the human's only action surface](#create-and-resolve-are-the-same-component),
so an arrival is the one factory event that should reach an operator who isn't looking —
everything else is pull. The durable queue remains the source of truth; the alert is only
the nudge to come look.

---

## The wizard — the only human-in-the-loop surface

Launched from **"Create Task"** on the board. It is *not* a form and *not* an
open-ended chat — it is a **steered conversation with a live alignment ledger**:
the [requirements stage](workflow.md) realised with the trusted (non-sandboxed)
requirements planner driving toward aligned, testable intent, exactly like a
collaborative design discussion that converges and *then* authors specs. Its job is
threefold:

1. **Elicit testable intent.** Because [specs are pure prose](specs-process.md) and
   the acceptance criteria are the human's only correctness lever, the wizard
   actively probes for examples, edge cases, what-to-reject, and out-of-scope —
   converging on crisp criteria rather than wandering.
2. **Author and maintain `specs/`.** Output is markdown in the spec tree — and it
   *maintains* the tree, not just grows it: when intent fits a domain an existing spec
   already owns, the wizard **edits that file in place** (additively, preserving what's
   there) rather than spawning a near-duplicate, and creates a new spec only for a genuinely
   new domain. When the *set* of spec files changes it updates the README index in the same
   draft, keeping the cross-link graph navigable. The wizard owns spec link-integrity, not
   just issue creation — and editing an existing spec seeds no work, so the
   every-spec-maps-to-an-issue rule binds only *new* specs (see
   [specs-process.md](specs-process.md)).
3. **Ground in the existing code (when configured).** Against an established codebase the
   planner gets **read-only exploration tools** — the agent's `read_file`/`list_dir`/`search`
   plus the LSP comprehension tools (`find_symbol`/`references`/…) — so its specs and seed
   issues fit the real structure and its link-integrity is checked against real files, not
   assumed. The reads run in a **read-only, zero-network sandbox** seeded from the repo (the
   same construction the [gate's verification sandbox](verification.md) uses, behind a deny-all
   broker); the sandbox is provisioned **lazily** on the first tool call and torn down when the
   session ends or is evicted, so a conversation that never explores boots nothing. Exploration
   is read-only — the planner's only outputs remain the spec + seed issues.
4. **Gate on explicit human approval.** The human reviews the drafted spec + the
   seed issues and approves *before* anything is written. **That approval is the
   consent boundary** — everything past it is autonomous. Approval is itself gated
   on a converged [alignment ledger](#the-alignment-ledger) (no fork left `open` or
   `discussing`). The commit it triggers is **one-shot**: a spec is committed and its
   seed issues created at most once per draft, so a double-click, a resubmit, or a
   second tab re-renders the original outcome rather than seeding the feature twice.

Data flow:

```
human ⇄ requirements planner (LLM, trusted; conversation runs host-side; streams over SSE)
      ↳ may read the codebase via read-only tools, executed in a read-only zero-network
        sandbox (lazily provisioned); a `tool` SSE event surfaces each read as a status line
      → drafts spec markdown + proposed seed issues
human → APPROVE
      → spec committed to git;  seed issues created via the orchestrator's
        single-writer path (validated, never written directly)
```

Under [`integration.mode: epic`](integration.md) the consent gate has three extra
behaviours: the draft must seed **exactly one** root issue — the epic is keyed on that
single root's id and the [decomposition planner](workflow.md) fans it into children, so a
draft proposing two or more roots is refused with a prompt to consolidate (see
[integration.md](integration.md)); the drafted spec is committed onto the feature's
**`epic/<epic_id>` branch** (its first commit), not `main`, so `main` moves only at the
atomic terminal merge; and the gate **refuses a second approval** while an epic is in
flight (v1 runs one epic at a time), reporting the in-flight feature instead of seeding a
second.

The planner's conversation and its spec/issue authoring run host-side and write no untrusted
code; its only *read* of untrusted code is sandbox-confined and network-isolated, so a
model-directed command can never reach the host or the network. Its conversation is itself an
LLM interaction and is therefore observable/replayable like any other.

---

## The alignment ledger

Alongside the conversation, a live **ledger** shows where you are — a lightly
structured list the planner maintains and you steer. It is the shared "where are
we" view a plain chat lacks — a *working aid*, not a durable object model.

**The planner emits structured state as tool calls, not parsed prose.** Each turn the
planner emits the complete ledger as the schema-validated arguments of an `update_ledger`
tool call, and — once intent converges — proposes the [draft](#the-wizard--the-only-human-in-the-loop-surface)
(spec + seed issues) via a `propose_draft` call. This is the same tool-calling mechanism the
[model layer](models.md) is built on, and that the test author uses to record the
[trace map](verification.md): the schema is enforced at the model boundary, so a malformed
payload is rejected there rather than silently mis-parsed downstream. The control room renders
the validated arguments; the planner's *text* reply is the prose the human reads, and the
ledger/draft ride the **tool** channel — so the conversation stays clean and the structured
output stays robust across models, with no free-text block for the server to scrape. These are
**output** tool calls (pure structured state) and are distinct from the read-only exploration
tools (genuine actions): emitting one never triggers another exploration round-trip.

**A draft is recorded only by the `propose_draft` call — and the engine holds the planner to
that.** Because a prose-only turn concludes the conversation, a planner that *narrates* the draft
("let me propose the draft") without emitting the call would otherwise leave the human staring at a
promise that never materializes, re-asking while the model re-narrates. So when a turn's concluding
prose announces a draft yet carried no `propose_draft` call, the engine injects a single corrective
nudge — *prose does not record a draft; emit the call now if intent has converged* — and lets the
model act. The nudge fires at most once per human turn (a termination guarantee) and only reminds,
never forces: a planner that is genuinely not ready simply keeps to questions and the ledger.

**Forks become chips.** When the planner surfaces a decision, it renders the
options as selectable chips (with the tradeoff). Each fork offers the same three
moves: **pick a chip** (it moves to *agreed*); **type free text** (the planner
folds the nuance in — the canned options are only its *guess* at the answer space,
and catching where that guess missed is the whole job of this stage, so freeform is
first-class on *every* fork, never just a fallback); or **flag "let's discuss"**
(the human isn't ready to decide and wants the planner to go deeper on this one,
optionally with a note on *what* gives them pause).

**Forks are surfaced and answered in batches, not one at a time.** The planner
posts a coherent set of currently-independent open forks at once, and the human
resolves them in any combination in a single submit — far fewer round-trips than a
linear question-at-a-time chat. Attribution stays unambiguous because **every
answer carries its fork's identity — the fork's question, not its position.** The
planner re-emits the *whole* ledger every turn and may reorder or drop forks, so a
position captured when the form rendered can point at the wrong fork by the time the
batch posts; keying each answer to its question re-resolves it against the latest
ledger, mapping every answer to the right fork or dropping it cleanly when that fork
is gone — never silently mis-applying it to a stale index. (The fork *number* shown
to the human is just its position in the current ledger.) The planner reconciles the
whole batch on its next turn — *including* noticing that one answer made another fork
moot ("given your answer to Q1, Q3 falls away"). The division of labour is deliberate: the **ledger stays dumb and the
planner stays smart** — the UI guarantees clean attribution, the planner owns all
dependency reasoning, so there is no dependency graph to encode in the ledger.
Dependent forks simply appear in the next batch once their prerequisites are agreed.

**The form is the human's until they submit it.** The planner re-emits the whole
ledger every turn, but only at a *turn boundary* — after the human sends answers or a
message — never while they are mid-selection (the planner is idle then, awaiting
them). The ledger panel relies on exactly this: it re-renders **only** on the
planner's `update_ledger` nudge, with no periodic poll (see [Rendering](#rendering)),
so a human resolving a batch of chips and free-text across several forks is never
re-rendered out from under them and can take their time and submit once. This is the
in-progress complement to the question-keyed attribution above: keying each answer to
its question keeps a *submitted* batch correct across a re-emit; suppressing the
periodic re-render keeps an *unsubmitted* batch from being discarded before it is
sent. (A genuinely new ledger the planner emits in response to the human's own turn
does replace the form — that is the planner's new truth, and the human has just
submitted to get it.)

**Each item is `open`, `agreed`, `discussing`, or `deferred`**, with a one-line
rationale. `open` is the start state; it resolves to `agreed` (decided),
`discussing` (the human flagged it — **non-terminal**: the planner keeps going), or
`deferred` ("we agree *not* to decide this now" — terminal, and counts as
resolved). `discussing` is the only non-terminal resolution, which is exactly what
gates approval.

**Approval is gated on a converged ledger.** Because everything past APPROVE is
autonomous (the [consent boundary](#the-wizard--the-only-human-in-the-loop-surface)),
the human cannot commit a spec with a fork still `open` or `discussing`. The gate is
**soft, not a lock**: the human may press Approve with plain `open` forks remaining,
which **auto-converts them to `deferred` and records them** — nothing vanishes
silently and the human stays sovereign. The one exception is `discussing`: an item
the human *actively* flagged is never auto-deferred out from under them; to approve,
they must consciously downgrade it to `agreed` or `deferred` themselves. This keeps
the human's own loop terminating — a stuck discussion can't hold the spec hostage —
without ever silently dropping a decision.

**Deferring is not free, and that is the point.** A `deferred` fork means the spec
stays silent on that point, so an implementing agent may later hit it as a
[spec gap](specs-process.md) and return `needs-spec-clarification` — routing
straight back into [Resolve mode](#create-and-resolve-are-the-same-component) of
this same wizard. The discuss/defer mechanic and the dead-letter escalation loop are
the *same loop at two different times*: decide it now in the conversation, or decide
it later when an agent forces the issue. So a `deferred` fork is recorded in the
decisions sidecar ("deliberately left open: X"), and that future escalation arrives
with its own pre-written context instead of as a surprise.

**What gets stored is deliberately minimal.** The specs are the source of truth;
the ledger and conversation are *provenance*:

- the **conversation transcript** → the [artifact store](components/artifact-store.md),
  linked from the seed epic (replayable, the "why");
- the **finalized decisions** → a simple markdown sidecar in git (a short bulleted
  list, one line of rationale each, per epic/spec area). `agreed` forks land as
  decisions; `deferred` forks land as explicitly-recorded open items ("deliberately
  left open: X"), so the sidecar carries both what was decided *and* what was
  knowingly left for later — pre-context for the escalation a defer may later raise.

Git history of that sidecar *is* the decision-evolution log — there is no
status/supersession machinery. Changing your mind later just means re-running the
wizard and editing the spec (and the decisions file along with it). Spec drift is
handled where it always is, by [spec-version pinning](specs-process.md) — not by a
parallel decision-recompile path.

---

## Create and Resolve are the same component

The [human re-entry invariant](specs-process.md) says stuck work is resolved by
*refining the spec* — which is the same operation as creating one. So **"Create
Task" and "Resolve" (from the DLQ) are one wizard, two entry modes:**

- **Create** → blank; elicit new intent.
- **Resolve** → pre-loaded with the escalation (`needs-spec-clarification`), the
  relevant [spec slice](glossary.md#spec-slice), and the agent transcript that
  raised it; the human edits the spec to resolve the ambiguity.

On the Resolve path the wizard shows the **spec diff and its blast radius** before
commit — "this change re-pins and reissues these 3 in-flight items" (the
recompile-the-delta mechanism in [specs-process.md](specs-process.md)) — so the
consequence of an edit is visible at the moment of consent.

This is the whole human interface: **guided spec authoring**, whether starting
fresh or unsticking dead-lettered work.

---

## OPEN questions

- **Decomposition preview:** dry-run the planner's breakdown ("this becomes ~5
  implement tasks") inside the wizard before approval. (Leaning defer.) — *under
  discussion.*
- ~~**In-session ledger shape:** plain agreed/open checklist vs. the
  lightly-structured items.~~ **Decided:** the lightly-structured shape — forks
  surfaced and answered in **batches** (every answer carrying its fork's question
  as its identity), a
  first-class free-text move on every fork, and four item states
  (`open`/`agreed`/`discussing`/`deferred`) with approval **soft-gated** on a
  converged ledger. See [The alignment ledger](#the-alignment-ledger).
- ~~**Coarse live trigger → precise issue-state event.**~~ **Decided:** the
  single-writer orchestrator emits a typed [`issue-state` event](messaging.md) on
  every status transition, and the board / DAG / dead-letter views refresh off it
  (the activity feed stays on `agent-event` — the thing it shows). This makes card
  moves crisp and animated and gives the per-card timers their `state_entered_at`
  anchor (see [The board, in motion](#the-board-in-motion),
  [orchestrator.md](components/orchestrator.md)). The periodic backstop is retained
  as the convergence safety net for a dropped best-effort event.
- Auth / who may operate the control room — TBD.
