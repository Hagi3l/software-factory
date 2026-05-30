# Implementation Plan

Build order for the harness, derived from [specs/](specs/README.md). The spine is
[bootstrap.md](specs/bootstrap.md): hand-build a minimal kernel that does
`spec → implement → gate → merge` for one issue (the **self-host point**), then
build out the full design.

## Status — self-host point reached; building by hand

Phases 0–1 are **complete**. The kernel does `spec → implement → gate → merge` for
one issue end-to-end: `cmd/harness` exposes `validate`/`seed`/`run`, and the
in-process orchestrator + runner carry a seed issue through implement → gate →
merge to `main` with a provenance trailer. Verified end-to-end against a real model
(local Ollama via `openai-compat`) and real Docker sandboxes.

**Development mode for Phases 2–5: built by hand with Claude Code, human-reviewed —
not self-hosted.** Bootstrap's threshold (a) ("the harness builds itself as a
trusted dev tool, human reviews diffs") assumes a capable model drives the
harness's *own* sandboxed agents; without a hosted key for one, that autonomous
path is deferred. The remaining work is ordinary Go / config / web development, so
it proceeds the same way the kernel was built. **This project is about the *machinery*,
not model quality — nothing below is blocked for lack of a capable model.** A model is
already available in dev (the deterministic `modeltest` server; local Ollama via
`openai-compat`). The wizard (T4.12–T4.15) and the autonomous self-hosting loop are
buildable and testable *offline*; the only thing a *capable* model gates is the subjective
*quality* of their outputs (good requirements elicitation, trustworthy auto-drafted specs,
good autonomous implementation) — a later validation concern, never an engineering blocker.

## How to read this

- **Completed tasks are collapsed to a one-line `— *done.*` checklist entry.** Phases
  0–1 are done (see Status); Phases 2–3 are done bar a few open items kept in full below.
  The verbose per-task findings were pruned once complete — that history lives in git,
  the code, and the specs they informed (each task updated its `(spec)` as it landed).
- **Open tasks (`- [ ]`) keep their full detail** — Phases 4–5, plus the handful left in
  Phases 2–3 (T2.11/T2.12, T3.7b).
- **Phases 2–5 are atomic tasks** (`T<phase>.<n>`), each a single self-contained,
  verifiable unit of work, listed in dependency order — the same granularity Phase
  0–1 used and the natural unit for one Claude Code session. Cross-task deps are
  noted `(needs Tx.y)` where they aren't the obvious linear predecessor.
- `(spec)` links point at the authoritative contract for each task. If a task needs
  the design to change, **update the spec first.**
- `*(OPEN)*` marks a task whose shape is still undecided in the specs;
  `*(optional)*` marks a nice-to-have.

## The self-host milestone

The kernel from [bootstrap.md](specs/bootstrap.md) is: config → beads → sandbox →
runner/broker → agent loop → gate runner → orchestrator loop. Bootstrap
simplifications hold at the kernel and are unwound across Phases 2–5 — DAG collapses
to `implement → gate → integrate`, merge queue is trivial (single stream, no
rebase/re-gate), NATS is in-process, Docker stands in for Firecracker, no control
room (CLI-driven), the implementor writes its own tests.

## TCB caveat

Per [bootstrap.md](specs/bootstrap.md), the components that *enforce* the guarantees —
orchestrator, runner/broker, sandbox, gate harness — are the Trusted Computing Base.
**TCB-touching changes stay human-reviewed even after self-hosting.** Autonomy is
earned first for non-TCB work (new souls, stages, the control room). While Phases
2–5 are built by hand this is moot — everything is human-reviewed — but the boundary
matters the moment a capable model is wired and autonomy is switched on.

---

## Testing infrastructure (cross-cutting)

Verifies the kernel's *machinery* — routing, the tool contract, gating, merge,
provenance — deterministically and fast, independently of a capable runtime model.
Specs: [models.md](specs/models.md) (deterministically-fakeable), [components/sandbox.md](specs/components/sandbox.md)
(non-isolating local backend), [bootstrap.md](specs/bootstrap.md) (testing the spine).

- [x] **TE.1 Deterministic end-to-end spine test (fast, no Docker) + Docker variant** — *done.*

---

## Phase 2 — Independent verification

Turns the kernel's "implementor writes + grades its own tests" into a genuinely
independent, strong gate. This is what *earns* no-human-review — its full payoff
only lands once a capable model drives autonomous runs, but every gate here also
strengthens the human-reviewed loop. ([verification.md](specs/verification.md))

- [x] **T2.1 Postcondition-driven gate checks** — *done.*
- [x] **T2.2 Gate evidence persistence** — *done.*
- [x] **T2.3 Red→green proof postcondition** — *done.*
- [x] **T2.4 `author-tests` soul + persona** — *done.*
- [x] **T2.5 Flip `implement` to the red→green proof** — *done.*
- [x] **T2.6 Independent scanners as checks** — *done.*
- [x] **T2.7 Mutation-testing postcondition** — *done.*
- [x] **T2.8 Test↔spec traceability map** — *done.*
- [x] **T2.9 `qa` stage + soul** — *done.*
- [ ] **T2.12** *(optional)* Run-all independent scanners — aggregate every independent-scanner
  finding in one `qa` pass (better DLQ triage) instead of fail-fast, via a per-check
  "independent" config signal the gate honors (keeps proof/measurement checks fail-fast). See T2.9. ([verification.md](specs/verification.md))
- [x] **T2.10 Trusted-dev policy profile** — *done.* The **`human-approved`** postcondition is
  orchestrator-evaluated (not a `checks` command): on a passing integrate the orchestrator **parks**
  the candidate (blocked, recording its candidate ref + the gate-verified provenance) and publishes an
  escalation, burning no retry — `advance` returns a new `errAwaitingApproval` sentinel `accept` treats
  like the merge sentinels. **`harness approve`/`harness reject <issue>`** read the parked candidate ref
  and publish a `core.ApprovalRequest` over a new JetStream **`harness.approvals`** stream
  (`EnsureApprovalConsumer`); the single-writer orchestrator's third consumer (`consumeApprovals`)
  applies it idempotently (status-gated on blocked + a parked candidate ref; a sha mismatch is a stale
  approval, ignored). **Approve** replays the preserved provenance onto the merge via the shared
  `mergeCandidate` (re-gated by the merge queue only on a rebase) then closes; **reject** routes a fix
  through `route` (→ DLQ when caps spent). **`policy.profile`** (`trusted-dev` = every integrate;
  `autonomous`/unset = only TCB-touching diffs) + **`policy.tcb_paths`** globs (matched by a new
  `config` doublestar matcher; `Policy.ApprovalRequired`) drive the decision; the orchestrator's
  `diffFiles` seam (default `git diff base...ref`) supplies the changed files. Validation: profile
  enum, trusted-dev ⇒ `human-approved` on every trusted-merge, `human-approved` only on trusted-merge,
  no command checks on trusted-merge, well-formed/non-empty globs. The shipped `config/harness.yaml` is
  now **trusted-dev** with `human-approved` on integrate + the TCB glob set. **Cross-process reach:**
  `harness run --nats-addr <host:port>` opens an opt-in TCP listener on the embedded NATS
  (`ServerConfig.ClientAddr`) so a separate `harness approve` reaches it (single-host; full distributed
  NATS is T5.8). New metadata keys `candidate_ref`/`parked_prov`/`approved_ref`/`approver` +
  `AwaitApproval`/`RecordApproval` transitions; `core.Issue` gains `CandidateRef`/`ParkedProvenance`.
  Resolves the bootstrap.md TCB-boundary OPEN (the `tcb_paths` list is the operational definition).
  ([bootstrap.md](specs/bootstrap.md), [configuration.md](specs/configuration.md), [messaging.md](specs/messaging.md))
- [ ] **T2.11** *(OPEN)* Second, different-model reviewer soul in `qa` (N-version diversity). ([verification.md](specs/verification.md))

## Phase 3 — Full DAG, decomposition & merge queue

Unwinds the kernel's single-stage, single-soul, trivial-merge simplifications.

- [x] **T3.1 Decomposition planner soul + `plan` stage** — *done.*
- [x] **T3.2 `beads.Apply` self-validates `DependsOn` existence** — *done.*
- [x] **T3.3 Stage ≠ role + selector matching** — *done.*
- [x] **T3.4 Per-role model tiers** — *done.*
- [x] **T3.5 Spec-slice resolution** — *done.*
- [x] **T3.6 Spec-version pinning** — *done.*
- [x] **T3.7 Recompile-the-delta** — *done.*
- [ ] **T3.7b Re-derive already-merged work on a spec edit** — *designed; spec landed (specs-process.md).*
  Extend the reconcile sweep to the closed/merged case, keyed by **(epic, spec-path)** not per-issue:
  group closed issues by `EpicID`+`Spec` (via `ListAll` + in-process filter — no new bd query),
  re-resolve+re-hash the slice, and on a mismatch against the pinned `SpecHash` spawn **one fresh `plan`
  issue** for that epic+path (carrying `EpicID`/`Tags`, branched from the epic's merged tip) so the planner
  decomposes only the delta against merged code. Re-entry at **planning, not author-tests** (a spec edit
  can add/remove/alter work items, which only the planner expresses). Then **re-pin the closed issues'
  `SpecHash`** to the new slice (idempotency latch) and **skip the spawn when an open re-derivation `plan`
  issue for that (epic, spec-path) already exists**. Known coarseness: a localized single-criterion edit
  still triggers a full planning pass. TCB-touching (orchestrator). (needs T3.7; **`EpicID`** + `epicOf`
  now available from T3.8b — group closed issues by `epicOf`, not a stored field, so a root seed is included)
  ([specs-process.md](specs/specs-process.md))
- [x] **T3.8 Cumulative per-issue budget** — *done.*
- [x] **T3.8b Cumulative epic budget + cross-loop wall-clock** — *done.* Two enforcement gaps closed.
  **(1) Epic budget** — **`core.Issue.EpicID`** (beads `epic_id`) threads forward onto every produced child,
  `on_failure` fix, conflict-resolver, and planner child, exactly like `Base`. A root seed carries **none**:
  the orchestrator's **`epicOf(issue) = EpicID || ID`** supplies the root's own id as the fallback (mirroring
  how `Base` falls back to the pipeline base), so descendants all share the root id with **no extra
  root-stamping write** and the aggregate naturally includes the root. Each result's **own-invocation marginal**
  is stamped via the new **`StampClosingSpend`** (beads `closing_tokens`/`closing_usd`) in `handleResult` —
  whatever the disposition, so a not-yet-terminal (transient) attempt's spend counts as in-flight accrual and
  the just-finished invocation counts before the check; it's a *set* so redelivery is idempotent. `epic_budget`
  (tokens/USD) is enforced as an **aggregate read** — `authorizeEpic` sums `closing_*` over all issues with the
  same `epicOf` via **`ListAll`** + filter — at every "launch more agent work" point (`route`, `resolveConflict`,
  `advance` to an agent stage via the new `errEpicBudgetDeadLettered` sentinel, `acceptPlan` before decomposing);
  the terminal trusted merge is **not** gated (it burns no agent spend). Breach → dead-letter with an
  `epic ... budget exhausted` reason; single-writer serial evaluation means siblings can't race, and an in-flight
  sibling's threaded `Spent*` is **deliberately not** re-added (its closed ancestors already stamped their
  marginals — re-adding would double-count). Stamping + the `ListAll` are **skipped entirely** unless an epic
  budget is configured (`epicBudgetConfigured`), so configs that don't use it pay nothing. **(2) Cumulative
  wall** — runner stamps **`core.Result.Elapsed`** (measured around the agent loop, trusted side like `Usage`);
  **`core.Issue.SpentWall`** (beads `spent_wall`, a duration string via new `metaDuration`) threads across the
  `on_failure` chain like `Spent*`; `chargeAndAuthorize` enforces cumulative `budget.wall` (distinct from the
  per-invocation sandbox ceiling). Config: bootstrap `harness.yaml` gains `epic_budget: { usd: 200 }`; the
  `EpicBudget` doc updated (was "NOT yet enforced"). Tests: wall thread + breach, epic aggregate breach +
  under-budget proceed, closing-spend stamped only when configured, advance/acceptPlan epic gating, EpicID
  root-fallback threading, and a beads round-trip for all new metadata. (needs T3.8) ([workflow.md](specs/workflow.md))
- [x] **T3.9 Merge queue: serialized rebase onto `main`** — *done.*
- [x] **T3.10 Re-gate the merged result** — *done.*
- [x] **T3.11 Conflict-resolution issue** — *done.*

## Phase 4 — Control room

The human's read-only window + the wizard (their only action surface). Stack: templ
+ Tailwind standalone CLI + `embed.FS` + htmx/Alpine + SSE.
([control-room.md](specs/control-room.md), [observability.md](specs/observability.md))

- [x] **T4.1 Web server scaffold + asset pipeline** — *done.* `internal/controlroom`
  (`Server`: `http.ServeMux`, exact-match routes via Go 1.22+ method patterns, `Handler()`
  for httptest, `ListenAndServe(ctx, addr)` with ctx-driven graceful drain) + `harness serve`
  (`--addr`, SIGTERM = clean stop). Views are **templ** in `internal/controlroom/views`
  (base `Layout` + `Home`/`Placeholder`); nav is a single source of truth (`views.NavItems`)
  the server iterates to register placeholder routes. Assets embedded via
  `internal/controlroom/assets` (`//go:embed static`): vendored htmx 2.0.4 + htmx-ext-sse
  2.2.2 + Alpine 3.14.9 (committed) and Tailwind-compiled `app.css`. Build pipeline is
  `make generate` → `go generate` (`generate.go`): `templ generate` first (so `*_templ.go`
  carry the class strings), then the pinned Tailwind v4 standalone CLI (`make tailwind`
  fetches it into gitignored `bin/`; `app.tw.css` uses `@source` globs at the views dir).
  Generated `*_templ.go` + compiled CSS are committed so a plain `go build` needs no
  toolchain — the binary is self-contained. Later views (T4.2+) are thin templ panels
  rendered into `Layout`; SSE attaches via the already-loaded htmx-ext-sse (T4.3).
  ([control-room.md](specs/control-room.md))
- [x] **T4.2 Read/query layer** — *done.* `internal/controlroom/query`: a `Reader` over
  three ports (`IssueReader`, `ArtifactReader`, `ProvenanceReader`) returning presentation
  structs — `Board(stageOrder)` (issues grouped by stage, columns in pipeline order, empty
  stages skipped, unassigned last), `DeadLetters` (blocked issues — the action surface),
  `IssueDetail` (stitches beads + git provenance + artifact-store availability into named
  evidence links; merged work reads the trailer, in-flight falls back to the issue's
  TraceMap), `RecentProvenance`, and `Artifact` (content stream). Decoupled from views (no
  templ import); fully fake-tested. Supporting single-source moves: **`Provenance` (+ render
  `Trailer`/`CommitMessage` and new inverse `ParseCommitMessage`) relocated to `core`** so
  the orchestrator's write side and the control room's read side share one format —
  `GitProvenance` (shells out to `git log`, `run` seam for tests) parses it back. Added
  beads `List(status)` + `ListAll` (`bd list --all/--status --flat`; closed issues are
  hidden from bd's default list — these surface them). Promoted the artifact-kind taxonomy
  (`prompt`/`transcript`/`traceability-map`/`gate-evidence`) into `core` (was duplicated
  literals in runner/gate). **Not yet wired into the server** — the views consume the Reader
  in T4.4+. **T4.6 (DAG) needed dependency-edge reads** — resolved by decoding the `dependencies` array bd already emits inline on `bd list --json` into a new `core.Issue.DependsOn` (no separate `bd dep` query), done as part of T4.6.
  ([control-room.md](specs/control-room.md), [observability.md](specs/observability.md))
- [x] **T4.3 SSE plumbing** — *done.* `internal/controlroom/live`: a content-agnostic SSE
  substrate — `Hub` (concurrent fan-out broadcaster: `Subscribe` returns a buffered channel +
  idempotent cancel; `Broadcast` is non-blocking and *drops* for a wedged subscriber, mirroring
  the best-effort core-NATS feed so one stalled browser can't stall the pump or other clients),
  `WriteEvent`/`Stream` (the SSE wire format — `event:`/`data:` framing with multi-line data
  split into multiple `data:` lines, periodic `: ping` heartbeats, stops on ctx cancel / channel
  close / write error; requires an `http.Flusher`), and `StartAgentEventPump` (the *only*
  NATS-touching piece: one wildcard subscribe to `harness.agent.*.events`, each message
  broadcast as an `agent-event` SSE event carrying `{agentId, payload}` — `AgentEvent`). Server:
  `Options.Events *live.Hub` turns on `GET /events`; the handler subscribes to the hub and
  streams until disconnect/shutdown. `ListenAndServe` now sets `http.Server.BaseContext` to the
  serve ctx so shutdown promptly cancels long-lived SSE streams instead of waiting out the drain.
  Subject taxonomy stays single-source in `messaging`: added `AgentEventsWildcard` +
  `AgentIDFromEventSubject` (inverse of `AgentEventsSubject`, rejects malformed/wildcard
  subjects). **Wiring:** the control room is **co-located in `harness run` behind `--serve-addr`**
  (empty = off) — it shares the run's in-process NATS so the feed has a live source. A standalone
  `harness serve` has no NATS, so `GET /events` answers **503** there until distributed NATS lands
  (a separate process cannot reach a `DontListen` embedded server — **T5.8**). `buildRunComponents`
  builds the hub+pump+server only when serving is enabled and stays network-free (the server binds
  no socket until `cmdRun` runs it in the errgroup). **Not yet consumed by a view** — the substrate
  delivers named events; the Board (T4.4) attaches via `hx-trigger="sse:..."` → htmx refetch, and
  the Activity feed (T4.5) via `sse-swap`. Fully tested: hub fan-out/drop/cancel (incl. `-race`),
  SSE framing + heartbeat + flusher-required, pump over real embedded NATS, and the `/events`
  endpoint end-to-end (broadcast → frame on the wire; disconnect → unsubscribe). ([messaging.md](specs/messaging.md), [control-room.md](specs/control-room.md))
- [x] **T4.4 Board view** — *done.* Kanban over beads issues, server-rendered (templ) and live via
  the T4.3 SSE substrate. `views.BoardPage`/`BoardColumns`/`boardColumn`/`boardCard` render
  `query.Reader.Board(stageOrder)` — issues grouped by `Role` into columns, each card showing
  id/title/status (tinted `statusBadge`, blocked stands out)/spec/retry generation. **Live refresh**
  is htmx-pure, no client graph lib: the columns sit in `<div hx-ext="sse" sse-connect="/events">`
  and re-fetch the bare `GET /board/cards` fragment on `hx-trigger="sse:agent-event throttle:2s,
  every 15s"` — SSE-responsive with a slow periodic backstop so a *settled* board still converges
  when the event stream goes idle. Initial columns render inline so the page shows data before htmx
  attaches. **Server:** `Options.Reader *query.Reader` + `Options.StageOrder []string`; real
  `/board` + `/board/cards` handlers registered ahead of the nav-placeholder loop (an `implemented`
  set keeps the mux from double-registering); nil reader (standalone `harness serve`) renders a
  "not attached" notice page and 503s the fragment. **Column order:** new `pipelineRoles(cfg)`
  (cmd/harness) walks the DAG `produces` edges **breadth-first** from the entry stage(s) — BFS not
  DFS so a fork/join lays out after *both* branches — yielding `planner, test-author, implementor,
  security` with the out-of-band `merge-resolver` (resolve stage, reached by no edge) appended last;
  passed as `StageOrder`. **Wiring:** `run --serve-addr` builds the Reader from a read-only beads
  client (orchestrator stays the single writer), the shared artifact store, and `query.NewGitProvenance(repo)`.
  Tested: column/card render + pipeline order + empty-column skip + SSE wiring, bare-fragment shape,
  no-reader notice/503, and `pipelineRoles` (linear + diamond + no-harness). **Known coarseness:**
  the live trigger is `agent-event` (per-invocation progress), so a refresh fires on agent activity,
  not precisely on a stage transition — adequate (throttle + backstop converge it), but a dedicated
  orchestrator-emitted issue-state event (the single writer) would be crisper; filed as a future
  refinement, not blocking. (needs T4.2, T4.3) ([control-room.md](specs/control-room.md))
- [x] **T4.5 Activity feed** — *done.* "What agents are doing right now," server-rendered (templ) and
  live via the T4.3 SSE substrate. **Decision (the one T4.5 left open): server-render + htmx re-fetch,
  *not* `sse-swap`.** The raw `agent-event` SSE payload is JSON (`live.AgentEvent`), and the runner's
  per-token stream (`{"type":"token","delta":…}`) is a firehose — neither is swappable into the DOM as
  readable rows. So, mirroring the Board exactly, the feed element re-fetches a server-rendered fragment
  on the SSE nudge: `<div hx-ext="sse" sse-connect="/events">` wraps `#activity` with
  `hx-get="/activity/items" hx-trigger="sse:agent-event throttle:1s, every 10s" hx-swap="innerHTML"`
  (tighter throttle than the Board since this *is* the live view; slow backstop converges a settled
  feed). **New buffer:** `live.Activity` (in `live`, no view/templ coupling) — a bounded, mutex-guarded,
  newest-first ring the existing pump now feeds. It **coalesces consecutive token deltas from the same
  agent into one rolling line** (collapsing the per-token firehose into ~one entry per model turn,
  rune-bounded to ~280) and renders discrete progress/log events as their own rows (summary = a
  top-level `msg`/`message`/`text` field if present, else compact payload JSON, truncated). It is
  in-memory + best-effort by design (durable record is the artifact-store transcript), so a restart
  losing live entries is harmless. **Pump change:** `StartAgentEventPump(nc, hub, *Activity)` — the
  pump records into the buffer (when non-nil) *and* broadcasts to the hub; `run --serve-addr` builds a
  `NewActivity(200)` alongside the hub and passes both. **Server:** `Options.Activity *live.Activity`;
  real `/activity` + `/activity/items` handlers (added to the `implemented` set); nil activity
  (standalone `harness serve`) renders a "not attached" notice page (200, like the Board) and 503s the
  fragment. **views:** `ActivityPage`/`ActivityList`/`activityRow` + `ActivityMessage`, with
  `activityTime`/`activityKindClass` helpers in `views/activity.go`. Tested: token coalescing
  (same-agent folds, cross-agent splits, discrete event breaks a token run), payload summary
  (msg-field vs compact-JSON fallback), newest-first + monotonic Seq ordering, max-bound eviction,
  rolling-text bound, malformed-drop, concurrent `Record` (`-race`); pump integration asserts the event
  lands in both hub and buffer; server handlers (notice/503/render/fragment-has-no-chrome). **Known
  coarseness (shared with the Board):** the live trigger is `agent-event` (per-invocation progress),
  not a precise lifecycle event; throttle + backstop converge it. A future orchestrator-emitted
  issue/turn-lifecycle event would let the feed show crisp stage transitions. (needs T4.3)
  ([control-room.md](specs/control-room.md))
- [x] **T4.6 DAG view** — *done.* The issue dependency graph — what blocks what — rendered **server-side to SVG by a pure-Go layered layout**, no graphviz/d2 runtime binary and no client-side graph library (keeps T4.1's self-contained-binary property). **New leaf package `internal/controlroom/dag`** (imports only `core`+stdlib): `Node{ID,Title,Status}` / `Edge{From,To}` (From=blocker, To=dependent) / `Graph`; `Layout` assigns each node a layer = longest path from a root via topological processing (cycle-defensive), orders within a layer by id for determinism, lays out top→bottom; `RenderSVG` emits a standalone `<svg>` — edges first (`<line class="dag-edge" data-from/data-to marker-end>` with an arrowhead `<marker>`), then nodes as `<a href="/issue/{id}"><g class="dag-node" data-node>` with status-tinted fills mirroring the board palette; **all dynamic text XML-escaped** (issue ids/titles are semi-untrusted) and titles rune-truncated; the graph types live ONLY here (no parallel type, no adapter) — `query` returns `dag.Graph` directly. **Edge source (the piece T4.2 deferred — line 163):** rather than a new `bd dep` read, the edges bd already emits **inline** on `bd list --json` (the `dependencies` array of `{issue_id, depends_on_id, type}`) are now decoded — **`core.Issue` gains a `DependsOn []string` field** and `beads`'s `issueJSON` decoder a `depJSON` type that reads `depends_on_id` into it, the same way Base/TraceMap ride on the issue from beads metadata. `query.DAG(ctx)` reads `ListAll` (all statuses, like the board, so completed work shows), maps each issue's `DependsOn` to edges, drops any edge whose endpoint is outside the issue set (mirroring the orchestrator's prefix-blind existence check), and sorts nodes+edges for a stable render. **views:** `DAGPage`/`DAGGraph`/`DAGMessage` in `views/dag.templ` mirror the board's two-handler SSE pattern — the graph sits in `<div hx-ext="sse" sse-connect="/events">` and re-fetches the bare `GET /dag/svg` fragment on `hx-trigger="sse:agent-event throttle:2s, every 15s"` (board cadence). **Hover/drill (Alpine+htmx):** a small `assets/static/dag.js` global used as `x-data="dagHover()"` dims the graph on hover and highlights the hovered node, its directly-connected neighbor nodes (both directions, via the edges' `data-from`/`data-to`), and the incident edges; clicking a node is the `<a href="/issue/{id}">` drill-through into the T4.7 detail page. CSS for dim/active states is an inline `<style>` in the page (no Tailwind toolchain dependency for the SVG classes). **Server:** real `/dag` + `/dag/svg` handlers added to the `implemented` set ahead of the nav-placeholder loop; nil reader (standalone `harness serve`) renders the not-attached notice (200, like the board) and 503s the fragment; a read error renders in-chrome. No cmd wiring needed — the Reader was already passed in T4.4 and the `dag` nav item already existed. Tested: dag layout (linear chain / diamond / disconnected / dangling-drop / determinism), SVG render (node ids, data attrs, href, arrowhead, XML-escaping, empty-graph no-panic), `query.DAG` (edge mapping, dangling drop, deterministic order, ListAll-error propagation), beads `dependencies` decoding into `DependsOn`, and server handlers (page render, nil-reader notice/503, fragment shape). ([control-room.md](specs/control-room.md))
- [x] **T4.7 Issue / invocation detail** — *done.* The single-issue forensic page, the drill-target the
  board cards, DLQ (T4.8), and provenance view all link into. `views.IssueDetailPage` renders
  `query.Reader.IssueDetail(id)` (already built in T4.2 — zero new query-layer work): a header
  (id/title/status/merged badge), the **brief** (Role, Spec path, pinned Spec version, Base, Attempt,
  cumulative Spend = `SpentTokens`/`SpentUSD`, plus the issue Body), the **merge provenance** (Soul, Model)
  when landed, and an **evidence** list — each cited artifact (Prompt, Traceability map, each passing gate
  check) a click-through to its raw content, degrading to "unavailable"/"no evidence persisted" rather than
  a dead link when the store can't resolve it or the check was cited bare. **New artifact endpoint:**
  `GET /artifact/{hash}` streams content via `query.Reader.Artifact` as **`text/plain` + `X-Content-Type-Options:
  nosniff`** — the security contract, since artifact bytes are *untrusted agent output* and must never be
  interpreted as HTML/script (the content-address colon survives Go 1.22 `{hash}` path routing — `templ.URL`
  passes the leading-slash path through unchanged). **Server:** real `/issue/{id}` + `/artifact/{hash}`
  handlers; nil reader (standalone `harness serve`) renders the not-attached notice (200, like the board) and
  503s the artifact endpoint; an unknown id / read fault renders an in-chrome notice rather than a blank 500.
  **Board wiring:** `boardCard` is now a whole-card `<a href="/issue/{id}">` drill-through. The page is
  deliberately **not live** (no SSE) — a detail page is a forensic snapshot of one issue, not a feed. No cmd
  wiring needed: the Reader (with its artifact-store port) was already passed to the server in T4.4. Tested:
  merged render (brief+budget+provenance+evidence, all three evidence states), in-flight fallback to the
  threaded TraceMap with no provenance section, unknown-id notice, no-reader notice/503, artifact content +
  content-type/nosniff + colon-in-path round-trip, artifact-404, and board-card→detail links. (needs T4.2)
  ([control-room.md](specs/control-room.md))
- [ ] **T4.7b Surface transcript + candidate diff on the detail page** *(carried from T4.7)* — the spec's
  detail view also lists the **transcript** and the **candidate diff**, which T4.7 could not render because
  neither hash is reachable from the read stores today. (a) **Transcript:** the runner harvests it to the
  artifact store (`ArtifactKindTranscript`) and stamps the hash on `Result.Evidence`, but the orchestrator
  consumes the Result without persisting that hash — the provenance trailer carries only PromptSHA / Verified
  / Traceability (security.md's format). Surfacing it needs the trailer (and `core.Provenance`) extended with a
  `Transcript:` field, written by the merger and parsed back by `GitProvenance` — a single-source provenance-
  format change, so **update security.md first**. (b) **Candidate diff:** reachable read-side for a *merged*
  issue (`git show <commit>`), but `ProvenanceReader.ByIssue` returns only the parsed provenance, not the
  commit hash; thread the commit through (or add a `DiffByIssue`) and render it. (a) touches the trailer
  format (mild TCB-adjacent, human-reviewed); (b) is pure read-side git. (needs T4.7)
  ([control-room.md](specs/control-room.md), [observability.md](specs/observability.md), [security.md](specs/security.md))
- [x] **T4.8 Dead-letter queue view** — *done.* The escalations awaiting a human — the control room's
  primary *action* surface — server-rendered (templ) and live over the T4.3 SSE substrate, mirroring the
  Board/Activity two-handler pattern exactly. **Query enrichment:** `DeadLetters` now returns a dedicated
  `query.DeadLetter` projection (was the bare `IssueCard`) carrying the triage signals a human acts on —
  **cumulative spend (`SpentTokens`/`SpentUSD`) and retry generation (`Attempt`)** alongside id/title/role/spec
  — because a budget breach and an exhausted retry cap are the two non-escalation dead-letter causes
  (workflow.md), so a glance at spend+attempt tells the human *why* the work is stuck without opening it. The
  dead-letter **reason is deliberately not synthesized**: it is not a first-class field on the issue (the
  orchestrator only flips status→blocked), so inferring it would mean guessing against policy caps — the honest
  move is to show the evidence (spend/attempt/spec) and let the detail page carry the rest. **views:**
  `DeadLetterPage`/`DeadLetterList`/`deadLetterRow` + `DeadLetterMessage` in `views/dlq.templ`; each row is a
  rose-tinted whole-card `<a href="/issue/{id}">` drill-through into the T4.7 detail view (triage at a glance →
  forensic snapshot); an **empty queue renders as reassurance** ("Nothing needs a human"), the *good* state, not
  an error. Reuses `formatUSD` from `views/issue.go`. **Live refresh:** the list sits in
  `<div hx-ext="sse" sse-connect="/events">` and re-fetches the bare `GET /dlq/items` fragment on
  `hx-trigger="sse:agent-event throttle:2s, every 15s"` (board cadence — the DLQ changes less often than the feed),
  so a settled run that just dead-lettered an issue surfaces it without a manual refresh. **Server:** real
  `/dlq` + `/dlq/items` handlers added to the `implemented` set ahead of the nav-placeholder loop; nil reader
  (standalone `harness serve`) renders the not-attached notice (200, like the board) and 503s the fragment; a
  read error renders in-chrome rather than a blank 500. No cmd wiring needed — the Reader was already passed to
  the server in T4.4; the `dlq` nav item already existed in `views.NavItems`. Tested (query + server):
  blocked-only filtering, triage fields threaded verbatim (spend/attempt/spec), List-error surfaced, page render
  (cards + spend + attempt + spec + detail links + SSE wiring), bare-fragment shape, empty-state reassurance,
  no-reader notice/503. **Resolve (T4.15) is deferred** — it follows the wizard (T4.12/T4.14) in build
  order; until then the DLQ is the read surface that surfaces *what* needs a human, drilling into
  the detail page. (needs T4.2) ([control-room.md](specs/control-room.md), [workflow.md](specs/workflow.md))
- [ ] **T4.9 OTel spans + export** — emit spans at the broker, orchestrator, and runner (boot, llm-turn, tool-call, gate-run) and metrics (latency, throughput, cost). **Decided:** export over **OTLP** to a configurable endpoint that defaults to off / stdout in dev, preserving the offline / self-contained-binary property; wiring an external backend (Tempo/Jaeger) is deferred to Phase 5 where distribution lands. Settle the **span-attribute + event schema first** — it's the contract T4.10/T4.11 read. ([observability.md](specs/observability.md))
- [ ] **T4.10 Budgets + Provenance views** — budgets (token/$/wall burn vs. caps) from OTel metrics; provenance (trace a merged commit → issue → soul → model → prompt → evidence). (needs T4.2, T4.9) ([control-room.md](specs/control-room.md))
- [ ] **T4.11 Replay** — reconstruct an invocation's full decision trail from the broker-captured transcript + the artifact store, live or after the fact. (needs T4.7) ([observability.md](specs/observability.md))
- [ ] **T4.12 Requirements-planner conversation loop** — the trusted, **non-sandboxed** LLM that drives toward aligned, testable intent, streaming over SSE; reuses the canonical model layer. *(Machinery builds offline — drive it with `modeltest` / local Ollama; only the subjective elicitation quality awaits a capable model, never the engineering.)* Scope note: control-room.md gives this planner three jobs — elicit testable intent (this task), author/maintain `specs/` markdown, and gate on human approval. The conversation loop is T4.12; the **spec-authoring persona and its link-integrity ownership** (specs-process.md: "every link resolves; every spec maps to ≥1 issue") land in **T4.14** — keep that validation a first-class postcondition on the planner's output there, not an afterthought. ([control-room.md](specs/control-room.md), [specs-process.md](specs/specs-process.md))
- [ ] **T4.13 Alignment ledger** — forks rendered as selectable chips (with tradeoffs); each item agreed/open with a one-line rationale; freeform typing always available. (needs T4.12) ([control-room.md](specs/control-room.md))
- [ ] **T4.14 Spec authoring + consent gate** — the planner drafts spec markdown + seed issues (keeping link integrity + the README index); on explicit human **APPROVE**, the spec is committed to git, the decisions sidecar written, the conversation transcript stored, and the seed issues created through the single-writer path. The single-writer seam already exists — `beads.Apply` (the validated, referential-integrity-checked write `cmd/harness/seed.go` already uses, "written exactly as the orchestrator would write"); the wizard reuses it, plus a `produces`-legality check on the batch mirroring `acceptPlan`'s validation of planner output. So no separate orchestrator-integration task is needed. (needs T4.12) ([specs-process.md](specs/specs-process.md), [control-room.md](specs/control-room.md))
- [ ] **T4.15 Resolve mode** — Create and Resolve are one component; Resolve pre-loads the escalation + spec slice + the agent transcript that raised it and shows the spec diff + blast radius before commit. (needs T4.14) ([control-room.md](specs/control-room.md), [specs-process.md](specs/specs-process.md))

## Phase 5 — Production isolation & distribution

Replaces the bootstrap stand-ins (Docker, in-process NATS, local-repo push, files
store) with the production stack.

- [ ] **T5.1 vsock broker transport** — `broker.Listen`/`Serve` over vsock and a `sandbox.Endpoint` of `vsock`+`cid:port` (currently unix-only); the transport Firecracker needs. ([messaging.md](specs/messaging.md), [components/runner.md](specs/components/runner.md))
- [ ] **T5.2 Firecracker sandbox backend** — a KVM-microVM backend implementing the `Backend`/`Sandbox` interface: rootfs seeding, vsock I/O (T5.1), resource limits incl. disk, deterministic teardown. The production isolation target. (needs T5.1) ([components/sandbox.md](specs/components/sandbox.md))
- [ ] **T5.3 Rootfs / base-image composition** — per-role toolchain images with the module/package cache baked in for offline (zero-network) builds. *(OPEN in sandbox.md.)* ([components/sandbox.md](specs/components/sandbox.md))
- [ ] **T5.4 Sandbox seeded-worktree ownership** *(carried from Phase 1)* — **drop the current workaround:** the bootstrap `go-toolchain` image relies on `git config --global --add safe.directory '*'` to tolerate the seeded worktree being owned by the host uid (the Docker backend seeds via `docker cp host/. container:workdir`, preserving host ownership; no `chown` today). Replace it by having the Docker backend `chown` the seeded worktree to the container user (and the Firecracker backend seed correct ownership), so the `safe.directory` / VCS-stamping crutch can be removed. ([components/sandbox.md](specs/components/sandbox.md))
- [ ] **T5.5** *(optional)* gVisor backend (medium-trust). ([components/sandbox.md](specs/components/sandbox.md))
- [ ] **T5.6 Vetted package mirror/proxy** — route package fetches through a pinning/scanning/logging proxy on the broker allowlist; a read-through cache amortizes downloads without weakening egress control. ([security.md](specs/security.md), [components/runner.md](specs/components/runner.md))
- [ ] **T5.7 Scoped short-lived secret minting** — the runner mints a per-task git token scoped to push *only* the task branch, injected for the invocation lifetime and dying with the sandbox (replaces the bootstrap local-repo push). ([components/runner.md](specs/components/runner.md), [security.md](specs/security.md))
- [ ] **T5.8 Distributed NATS** — an external cluster with concrete JetStream stream defs (retention / replicas / max-age — the messaging.md OPEN) and runners across hosts, swapped in for the embedded in-process server. ([messaging.md](specs/messaging.md))
- [ ] **T5.9 S3/MinIO artifact backend** — an `artifact.Store` implementation for distributed deployments (config `bucket`), shared across hosts and the control room. ([components/artifact-store.md](specs/components/artifact-store.md))
- [ ] **T5.10 Provenance signing + key custody** — sign commits/artifacts with the harness identity and verify on read. *(OPEN, security.md.)* ([security.md](specs/security.md))
- [ ] **T5.11** *(optional)* Warm sandbox pools + HA orchestrator via NATS-KV leader election. *(OPEN.)* ([components/runner.md](specs/components/runner.md), [components/orchestrator.md](specs/components/orchestrator.md))

---

## Open decisions affecting the plan

These are still `OPEN:` in the specs and may reshape tasks above. (Decisions once open
here — mutation threshold, gate fail-fast, `integrate` ownership, the condition-expression
language — are now recorded in the specs they informed, not duplicated here.)

- HA orchestrator: single instance (fine for v1) vs. leader election (T5.11).
- Rootfs / base-image composition per role (T5.3).
- Exact module set in the TCB boundary — operationally the `policy.tcb_paths` globs (T2.10);
  the concrete list must still be reviewed and pinned before autonomy is switched on for harness work.
