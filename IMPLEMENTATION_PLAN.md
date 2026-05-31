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
  Phases 2–3 (T2.11/T2.12).
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
- **Known flake (infra, not a code bug):** `internal/runner.TestTeardownRunsEvenWhenInvokeErrors` (and
  occasionally its NATS-backed siblings) can stall under the *full-suite* `go test ./...` parallel load —
  many packages each spin an embedded NATS server, and the redelivery-loop teardown becomes timing-sensitive
  under contention — surfacing as a 10-minute package timeout. It passes deterministically in isolation
  (`go test ./internal/runner/` ≈ 0.25s). If `make check` times out here, re-run; a real fix would cap test
  parallelism (`go test -p`) or give the embedded-NATS runner tests a tighter per-test deadline. Not caused by
  any feature work; flagged so a future loop does not chase it as a regression.

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
- [ ] **T2.11** *(optional)* **N-version model diversity — resolved as configuration, not a built-in mechanism.**
  Soul independence (`producer ≠ verifier`) is already enforced; running the verifier on a *different model
  family* than the producer is a **config capability** the harness already provides (a role maps to a set of
  souls via `selector` (T3.3), each soul names its own model/tier (T3.4)), consistent with the config-is-the-
  pipeline principle — the harness *enables and recommends* diversity but the model assignment is the user's.
  No bespoke "second reviewer soul" mechanism is needed. The one piece of buildable work this leaves — a
  non-fatal validation warning on producer/verifier model-family overlap — is tracked as **T2.13**. Decision
  recorded in verification.md ("Model diversity is configured, not mandated").
  ([verification.md](specs/verification.md), [configuration.md](specs/configuration.md))
- [ ] **T2.13 Producer/verifier model-family diversity warning** — a *non-fatal* advisory at `harness validate`
  when a **verifier** role (a role that runs a gate — `qa`/`security`) is configured with the same model
  **family/provider** as the **producer** role (`implement`), since same-family producer+verifier share
  correlated blind spots and weaken the N-version independence verification.md recommends. **The warning is
  advisory, never fatal** — model assignment is the user's call (config-is-the-pipeline); validate still exits 0.
  Two pieces: **(1)** introduce a **non-fatal warning channel** in config validation — today `Config.Validate()
  returns only a fatal `error` (all problems aggregated), with no advisory class; add a sibling (e.g.
  `Config.Warnings() []string`) that `cmdValidate` prints without failing. **(2)** the diversity check: derive the
  producer role (the `implement`-stage role) and verifier role(s) (the gate/`qa`-stage role(s)) from the DAG,
  resolve each role's soul(s) → `core.Soul.Model` → `config.ModelProvider.Provider`, and warn when a verifier's
  provider matches the producer's. Key on **`Provider`** as the family proxy (known imperfection: two
  `openai-compat` entries at *different endpoints* are distinct families but read as the same provider — note it
  in the message, don't over-engineer). **Deferred / optional:** the complementary control-room **tooltip** needs
  a souls/config view that does not exist yet (no such Phase 4 view was built) — file it as a follow-up, not part
  of this task. Tests: warn on same-provider producer+verifier, no warn on differing providers, no warn when no
  gate stage / no producer, and that the warning never turns `Validate()` fatal. ([verification.md](specs/verification.md), [configuration.md](specs/configuration.md))

## Phase 3 — Full DAG, decomposition & merge queue

Unwinds the kernel's single-stage, single-soul, trivial-merge simplifications.

- [x] **T3.1 Decomposition planner soul + `plan` stage** — *done.*
- [x] **T3.2 `beads.Apply` self-validates `DependsOn` existence** — *done.*
- [x] **T3.3 Stage ≠ role + selector matching** — *done.*
- [x] **T3.4 Per-role model tiers** — *done.*
- [x] **T3.5 Spec-slice resolution** — *done.*
- [x] **T3.6 Spec-version pinning** — *done.*
- [x] **T3.7 Recompile-the-delta** — *done.*
- [x] **T3.7b Re-derive already-merged work on a spec edit** — *done.* New companion sweep
  **`recompileMergedDelta`** (in `recompile.go`, run from `tickLoop` after `recompileSpecDelta`) handles the
  closed/merged case keyed by **(epic, spec-path)**: it `ListAll`s once, groups closed issues by
  **`epicOf(issue)`+`Spec`** in-process (so a root seed folds into its own epic via the `epicOf` fallback — no
  new bd query), re-resolves+re-hashes each group's slice, and on a mismatch against any closed member's pinned
  `SpecHash` spawns **one fresh `plan` issue** for that (epic, path) via `beads.Apply` — carrying `EpicID` +
  the epic's `Tags`, empty `Base` (the merged work is on main, so it branches from the epic's merged tip like a
  seed) — so the planner re-enters at **planning** and decomposes only the delta against merged code. Two
  idempotency mechanisms: **(1)** skip the spawn when a planning pass for the (epic, path) is already open (any
  non-closed plan-role issue — a prior re-derivation or the epic's initial plan); **(2)** after spawning,
  **re-pin every closed member's `SpecHash`** to the new slice (the latch) so once the plan settles the group
  reads settled and does not respawn. Plan-stage role(s) come from a new **`planRoles()`** helper (reads
  `config.StageKindPlan` from the DAG; `spawn=""` when no plan stage ⇒ sweep no-ops, skipping the `ListAll`).
  Best-effort throughout (unresolvable slice / unpinned member left untouched), like the in-flight sweep; added
  `statusClosed` const. Known coarseness (per spec): a localized single-criterion edit still triggers a full
  planning pass. TCB-touching (orchestrator), human-reviewed. Tests: drift→one deduped plan spawn + re-pin,
  no-drift no-op, open-plan idempotency skip (no spawn, no re-pin), and best-effort skips (no plan stage / no
  pin / settled / unresolvable). (needs T3.7, T3.8b) ([specs-process.md](specs/specs-process.md))
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
- [x] **T4.7b Surface transcript + candidate diff on the detail page** — *done.* Both hashes are now reachable
  from the read stores. **(a) Transcript:** `core.Provenance` gains a **`Transcript`** field (artifact-store hash
  of the full broker-captured conversation — the replayable decision trail), rendered as a new pipe-segment on
  trailer line 2 (`| Transcript: <hash>`, `(none)` when unharvested) and parsed back by `ParseCommitMessage` —
  one format, both sides, **security.md + integration.md updated first**. The orchestrator's `provenanceFor` threads
  it via a new **`transcriptHash(res)`** helper (mirrors `traceMapHash`, scans `Result.Evidence.Artifacts` for
  `ArtifactKindTranscript`) — the runner already harvests the transcript and stamps the ref, so no runner change.
  The query layer's `evidenceFromProvenance` emits a **"Transcript"** evidence link (right after the prompt) →
  click-through to `/artifact/{hash}`. Transcript surfaces only for **merged** work (the trailer is the only place
  the hash is retained; the orchestrator otherwise discards `Result.Evidence`). **(b) Candidate diff:** new
  **`ProvenanceReader.DiffByIssue`** on `GitProvenance` — a shared **`commitForIssue`** helper (greps the trailer's
  `Issue: <id> |` with `--format=%H<US>%B`, so `ByIssue` and `DiffByIssue` never drift on "which commit landed this
  issue") resolves the integration commit hash, then `git show --no-color --format= <hash>` yields the candidate
  diff (the integration commit is a single-parent provenance commit on the candidate tree, so its patch *is* the
  candidate diff; the leading blank `--format=` emits is trimmed). `IssueDetail` gains a **`Diff`** field, fetched
  best-effort for merged issues (a git fault leaves it empty rather than blanking the page), and the detail view
  renders a **"Candidate diff"** `<pre>` — agent-authored content, but templ-escaped as inert text (the raw-bytes
  nosniff contract is for the `/artifact` endpoint, not this server-rendered page). TCB-adjacent (trailer format),
  human-reviewed. Tests: provenance round-trip with `Transcript`, orchestrator trailer cites the harvested
  transcript hash, `DiffByIssue` (stub patch-trim + not-found + real-git integration), query stitches transcript
  link + diff, and the detail page renders both (and omits the diff section in-flight). This unblocks **T4.11
  Replay** (the transcript hash is now reachable). (needs T4.7)
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
- [x] **T4.9 OTel spans + export** — *done.* OpenTelemetry tracing + metrics across the kernel, exported
  over **OTLP/gRPC** to a configurable endpoint that defaults to **off** (`""`) with **`stdout`** for offline
  dev — preserving the self-contained-binary / zero-network property (an external Tempo/Jaeger backend is a
  Phase 5 deployment step, T5.8; the exporter wiring is complete so the knob is real, not stubbed). **New leaf
  package `internal/telemetry`** is the single source of truth for the **schema** — the contract T4.10/T4.11
  read — split into `conventions.go` (span names `invocation`/`boot`/`llm-turn`/`tool-call`/`gate-run`; the
  `harness.*` attribute namespace; token-kind/component values; the three metric families latency/throughput/cost,
  durations in seconds) and `telemetry.go` (the `Provider`). **`Provider` is nil-safe by construction:** `Noop()`
  (inert tracer + no-op instruments) is what an unset endpoint and a nil Provider both resolve to, so every call
  site instruments **unconditionally** with zero overhead when export is off (mirrors the nil-logger→discard
  pattern). `Setup(ctx, Config)` is **network-free** (the OTLP exporter dials lazily on first export, so a missing
  collector degrades to dropped exports + logged errors, never a boot failure) — safe in the network-free
  composition root; `Shutdown` flushes the final batch and joins the run's teardown stack. `NewWith(tp, mp)` is a
  legitimate seam (build a Provider over caller-supplied OTel pipelines) so cross-package tests assert call sites
  emit against in-memory recorders. **Emit sites (one shared Provider threaded through `runner.Options`,
  `orchestrator.Options`, and `gate.New`):** the **runner** opens the root **`invocation`** span (an invocation =
  one trace) with issue/role/soul/model/base/invocation-id attrs, wraps **`boot`** around `backend.Provision`, and
  records `RecordInvocation` (throughput + the already-measured `Elapsed`) for **every** disposition — an infra
  error records under a distinct `"error"` status. The **broker relay** opens **`llm-turn`** per model turn
  (timed around `adapter.Complete`, carrying stop-reason + per-kind token attrs) and `RecordLLMTurn`, and
  **`tool-call`** around the brokered git-push (opened **before** the branch guard so a denied push is traced too)
  — the relay needed a new `model string` field (`model.Request` omits the model name) and a `parentCtx` so its
  per-turn spans **parent off the invocation span** (the broker serves on a separate connection-scoped context, so
  without re-parenting via `trace.ContextWithSpan` the spans would be orphan roots). The **orchestrator** records
  `RecordCost` in `handleResult` (it alone holds the per-model price table), once per result, keyed by model —
  documented as a monotonic observability counter that may modestly over-count under at-least-once redelivery
  (budget enforcement stays exact via the idempotent beads closing-spend stamp). The **gate** opens **`gate-run`**
  in its own trace (producer ≠ verifier — no inherited context; issue id is recovered from the candidate ref via
  the new `core.IssueIDFromCandidateBranch`, the inverse of `CandidateBranch`) and records `RecordGateRun`
  **only on a reached verdict** — infra errors (no verdict) are deliberately unrecorded so the pass/fail split
  counts real outcomes only. **Config:** `config.OTelConfig.Endpoint` (already existed) is now validated at the
  startup gate by a new `validateOTel` — `""`/`"stdout"`/`host:port` (via `net.SplitHostPort`, both halves
  required), turning a typo into a loud error rather than silently-dropped exports (the contract telemetry.go
  documents). `config` deliberately does **not** import `telemetry` (would drag the OTel SDK into a foundational
  package) — the `"stdout"` sentinel is a documented literal cross-referencing `telemetry.EndpointStdout`. **Deps:**
  added the OTel v1.44.0 tree (SDK + OTLP/stdout exporters) to go.mod/go.sum via `go mod tidy`. Non-TCB
  observability, human-reviewed. Tests: telemetry schema/provider (Noop safety, off/stdout/OTLP-lazy Setup,
  Record* emit + per-kind token split + zero-skip), `validateOTel` (valid forms incl. IPv6 / malformed rejection),
  `IssueIDFromCandidateBranch` round-trip + rejection, and the gate-run span+metric end-to-end (verdict recorded,
  infra error **not** recorded) through the real `Run` via `NewWith`. `make check` green (lint 0, 548 pass / 2 skip).
  ([observability.md](specs/observability.md))
- [x] **T4.10 Budgets + Provenance views** — *done.* The two remaining data-backed nav views, both
  server-rendered (templ) and **live** over the T4.3 SSE substrate (two-handler page+fragment pattern, board
  cadence `sse:agent-event throttle:2s, every 15s`) — mirroring the Board/DLQ exactly. The `budgets`/`provenance`
  `NavItems` placeholders are replaced with real handlers (added to the server's `implemented` set).
  **Sourcing decision (resolves the plan's "from OTel metrics" ambiguity):** the in-app budget view reads
  **beads' stamped cumulative + marginal spend** — the *exact same numbers the orchestrator enforces budgets on* —
  not a query against the OTel metric backend. Per observability.md's build-vs-buy line, generic cost-over-time is
  the *buy* side (Tempo/Grafana); the bespoke in-app view is self-contained and exact off beads, keeping the
  zero-dependency / self-contained-binary property (control-room.md lists the source as "beads + OTel metrics" —
  beads supplies the burn). **Budgets:** new `query.Reader.Budgets(ctx, caps)` returns per-**epic** aggregates and
  per-**issue** burn-vs-cap. Epic burn sums each member's **marginal** `ClosingTokens/ClosingUSD` grouped by
  **`core.EpicOf`** — exactly what the orchestrator's `authorizeEpic` sums (marginal, never chain-cumulative
  `Spent*`, so a fan-out doesn't double-count shared ancestry). Per-issue burn is `Spent*+Closing*` (the
  chain-cumulative the per-issue budget bounds) for tokens/USD plus `SpentWall`, with `Attempt` vs `MaxRetries`.
  Each dimension carries a clamped 0..100 `Pct` and an `Over` breach flag computed in the query layer (testable),
  tinted in the view (rose breach / amber ≥80% / emerald) — **tinting lives in templ switches because the Tailwind
  `@source` scanner only reads `*.templ` + generated `*_templ.go`, never hand-written `.go`**, so class literals in
  a Go helper would silently never compile (text-only helpers — number/duration/∞-cap formatting — stay in
  `budgets.go`). Rows sort by USD-burn desc (heaviest/breaches first). **`core.EpicOf` extracted as the single
  source** for epic grouping; the orchestrator's `epicOf` now delegates to it (no logic duplication across the
  enforcement and view sides). **Provenance:** `RecentProvenance` already existed (T4.2) — this is just the view +
  handler: a live list of recent merged commits, each tracing **commit → issue → soul → model → prompt → evidence**
  with the prompt and each passing gate check (`name@<hash>`, split in a view helper) linking to their raw
  `/artifact/{hash}` (the colon in a content address survives Go 1.22 path routing), and the issue linking into its
  T4.7 detail page. **Caps wiring:** `cmd/harness budgetCaps(cfg)` projects `config.Harness.Policy` (per-issue
  `Budget`, `EpicBudget`, `MaxRetries`) into a new `query.BudgetCaps`, passed via `controlroom.Options.BudgetCaps`
  — so the read model (`query`) stays free of a `config` import, mirroring how `StageOrder` is threaded. nil-reader
  (standalone `harness serve`) renders the not-attached notice (200) and 503s the fragment, like every other
  Reader-backed view. Ran `make generate` (templ + Tailwind; verified the new emerald/amber/rose/table utilities
  compiled into `app.css`). Tests: `query.Budgets` (epic marginal aggregation + grouping, per-issue Spent+Closing
  burn + breach + 100% clamp, uncapped-never-breaches, ListAll-error), `core.EpicOf` (root fallback + descendant),
  and server handlers for both views (not-attached notice/503, full render incl. issue/artifact links + breach
  meter, bare-fragment shape). `make check` green (lint 0, 557 pass / 2 skip). Non-TCB, human-reviewed.
  (needs T4.2, T4.9) ([control-room.md](specs/control-room.md), [observability.md](specs/observability.md))
- [x] **T4.11 Replay** — *done.* The reconstructed decision trail of one invocation — exactly what the
  LLM saw and did, turn by turn — parsed from the broker-captured transcript in the artifact store
  (observability.md's "differentiator"). A forensic drill-through (`/replay/{id}`) from the issue-detail
  page, not a nav view: keyed by issue id like `/issue/{id}`, plainly server-rendered with **no SSE**
  (a snapshot, not a feed). **Single-source transcript format:** the runner's private `transcriptTurn`
  was promoted to **`model.TranscriptTurn`** (`internal/model/transcript.go`, json tags `request`/`response`)
  so the write side (the relay's `turns`/`record`/`Transcript`) and the read side share **one** wire
  format — the same single-source posture `core.Provenance` takes for the trailer (no second decode struct
  to drift). **Query layer:** `Reader.Replay(ctx, id)` resolves the transcript hash off the merge
  provenance (`prov.Transcript` — the **only** place it's retained, so replay is **merged-only**, exactly
  like the T4.7b transcript evidence link), streams the JSON `[]model.TranscriptTurn` from the store, and
  `buildReplay` folds it into per-turn presentation structs (`Replay`/`ReplayTurn`/`ReplayMessage`/
  `ReplayToolCall`/`ReplayToolResult`, all flattened to strings/ints so **views never import `model`**).
  Per turn: **Inbound** = the messages new to that turn's request vs the previous turn's (the append-only
  agent loop means the suffix beyond the prior length is exactly what the model newly saw — the brief on
  turn 0, prior-turn tool results after), with the **leading assistant echo dropped** (already rendered as
  the prior response); plus the response text, tool calls (args pretty-printed via `json.Indent`, raw
  fallback), stop reason, and per-turn token usage. **Best-effort spine:** only an unreadable issue/
  provenance is fatal; a missing/unmerged/unharvested transcript → `Available=false`, empty `Hash` →
  "none captured" notice; a cited-but-unresolvable or **corrupt** transcript → `Available=false` but `Hash`
  retained so the view offers the raw-bytes `/artifact/{hash}` link — never a blank 500, mirroring the
  detail page. **Server:** `GET /replay/{id}` → `handleReplay` (mirrors `handleIssue`: not-attached notice
  with no reader, in-chrome notice on read fault). **Views:** `replay.templ` (`ReplayPage` + `replayTurn`/
  `replayInbound`/`replayToolCall`/`replayStopBadge` + `replayNotice`/`ReplayNotAttached`) reuses
  `statusBadge`/`shortHash`/`orDash`; stop/error tints are templ switches (Tailwind scanner). The
  issue-detail header gains a **"▸ Replay decision trail"** drill-link, shown only when `Provenance.Transcript`
  is set (merged work). Non-TCB, human-reviewed; `make generate` ran (templ + Tailwind). Tests: query —
  trail reconstruction (inbound delta + assistant-echo skip + pretty args + usage totals), no-transcript/
  not-merged/unresolvable/**malformed**/issue-error paths; server — page render, no-transcript notice,
  not-attached, unknown-id, and the conditional detail→replay link. `make check` green (lint 0, 571 pass /
  2 skip). **Deferred (filed, not blocking):** *(a)* **live-streaming** replay (reconstructing the trail as
  the invocation runs) overlaps the activity feed and needs the broker to emit structured per-turn events —
  the landed differentiator is the after-the-fact forensic trail; *(b)* replaying **dead-lettered/in-flight**
  work would need the orchestrator to thread the transcript hash onto the issue (a TCB-adjacent beads-field
  change) rather than only onto the merge trailer — valuable for DLQ triage, but out of scope for this
  read/render task. (needs T4.7) ([observability.md](specs/observability.md), [control-room.md](specs/control-room.md))
- [x] **T4.12 Requirements-planner conversation loop** — *done.* The trusted, **non-sandboxed**
  requirements planner behind the control-room **Create-Task wizard**, streaming over SSE, reusing the
  canonical model layer **directly** (no broker/sandbox/NATS — it runs no untrusted code, so it is correctly
  outside the sandbox; this is the *requirements* planner, kept distinct from the autonomous, sandboxed
  *decomposition* `plan` stage). **New leaf package `internal/controlroom/wizard`** owns only the conversation:
  a `Planner` (resolved `model.Adapter` + persona + per-turn `MaxTokens`) managing in-memory `Session`s; each
  `Session` holds the running `[]model.Message` and **its own `live.Hub`** (so one human's stream never leaks to
  another). `Session.Send(text)` records the user turn and launches one background `adapter.Complete` whose
  growing reply is broadcast as coalesced, **HTML-escaped** `delta` SSE events (cumulative, so a dropped frame
  self-heals; coalesced by rune-count to tame the token firehose) and finalized with a `turn` nudge; it guards
  **one turn at a time** (a second Send while busy records nothing), always clears `busy` + emits `turn` even on
  error (a failed turn appends an assistant error note rather than wedging), and runs on a `context.Background()`
  + per-turn timeout (it outlives the POST). Sessions are bounded (oldest-evicted, `defaultMaxSessions=64`) —
  best-effort working state, not the durable record (that lands on APPROVE, T4.14). Crypto-random session ids
  (unguessable before auth, which is OPEN). The package depends only on `model` + `live` — **no config/filesystem
  import**: the composition root resolves the adapter and reads the persona, keeping the engine a self-contained,
  testable unit. **Config:** new optional `config.Harness.RequirementsPlanner{Model,Persona,MaxTokens}`
  (`requirements_planner:` in harness.yaml), validated like a soul (model must resolve in the infra registry;
  persona file must exist; non-negative max_tokens) but **NOT** cross-checked against the DAG — it fulfills no
  role (the `requirements` stage stays `kind: human`). New `Config.RequirementsPlannerPersonaPath()` mirrors
  `PersonaPath`. Bootstrap `config/harness.yaml` now sets it (`claude-opus-4-8`,
  `souls/prompts/requirements-planner.md`, 4096) with a new elicitation persona prompt that probes for
  examples/edge-cases/what-to-reject/out-of-scope and converges on testable acceptance criteria. **Control room:**
  `Options.Planner *wizard.Planner` + a new **"Create Task"** nav item; routes `GET /create` (mints a fresh
  session, renders the page), `GET /create/stream/{session}` (the per-session SSE stream — 503 no planner, 404
  unknown session), `GET /create/messages/{session}` (the `turn`-nudge transcript re-fetch fragment), `POST
  /create/message` (records the turn, returns the fragment so the prompt shows at once while the reply streams).
  **views/wizard.templ** (`CreatePage`/`WizardTranscript`/`wizardBubble`/`CreateMessage`): the transcript sits in
  one SSE-connected element; it re-fetches on `sse:turn` (board-style server-render-a-fragment) **and** a live
  `sse-swap="delta"` target streams the partial reply token-by-token — the two-channel pattern a streaming chat
  needs (the `turn` refetch + `every 8s` backstop make it functional even if a `delta` frame is missed). nil
  planner (standalone `harness serve`, or a config omitting the block) renders a "wizard disabled" notice (200),
  mirroring how a nil Reader degrades the data views. **Wiring:** `buildRunComponents` builds the registry before
  the control-room block, resolves `rp.Model` to an adapter, reads the persona, and constructs the `Planner`
  (only when `requirements_planner` is set). Non-TCB, human-reviewed; `make generate` ran (templ + Tailwind).
  Tests: engine (real OpenAI adapter via `modeltest` — stream+escape+record, busy-guard, blank-ignored,
  error-doesn't-wedge, unique+bounded sessions; `-race`) and server (not-configured notice/503, page+session
  render, POST round-trip with a live per-session SSE stream delivering `delta`+`turn`, unknown-session 404s,
  finalized escaped transcript). `make check` green (lint 0, 579 pass / 2 skip). **Deferred to T4.13/T4.14:** the
  conversation is the *only* deliverable here — the alignment ledger (chips/agreed-open, T4.13), and
  spec-authoring + seed-issue creation + the APPROVE consent gate with spec link-integrity (T4.14) build on this
  session loop. *(Machinery builds offline; only the subjective elicitation quality awaits a capable model.)*
  ([control-room.md](specs/control-room.md), [specs-process.md](specs/specs-process.md), [workflow.md](specs/workflow.md))
- [x] **T4.13 Alignment ledger** — *done.* The live alignment ledger beside the wizard conversation (control-room.md "The alignment ledger"): a lightly-structured, latest-wins snapshot of where the requirements conversation stands — each item agreed/open with a one-line rationale, forks rendered as selectable chips with their tradeoff. **Single source of truth = the planner:** it re-emits the COMPLETE ledger each turn as a trailing fenced ```ledger JSON block after its prose; the engine parses it (new `internal/controlroom/wizard/ledger.go` — `parseLedger`/`cutLedgerBlock`/`displayProse`/`normalizeStatus`, exported `LedgerItem`/`LedgerOption`), stores a latest-wins snapshot on the `Session` (`ledger` field + `Ledger()` accessor), strips the block from the displayed prose, and the view renders it. **Streaming-clean:** the `delta` SSE broadcast streams `displayProse(reply)` (text before the fence) so the accumulating JSON never flashes in the live token stream; the finalized transcript message stores the stripped prose. Degrades gracefully — no/malformed/empty block → `(nil, prose)`, never errors, never clobbers a prior ledger (caller overwrites only when items != nil). **Steering funnels through the planner (no parallel client-side model):** a chip click POSTs `/create/ledger/select`; `Session.Choose(itemIdx, optIdx)` reads the option, synthesizes a `For %q, I choose: %s.` user turn and calls `Send`, so the planner folds it in and re-emits the ledger with that point agreed/selected; freeform typing stays the message box. **Live refresh:** a new `ledger` SSE event (broadcast alongside `turn` when a turn emitted a ledger) nudges a dedicated `#wizard-ledger` panel to re-fetch `GET /create/ledger/{session}` (also on `sse:turn` + an `every 8s` backstop), mirroring the board's server-render-a-fragment pattern. **views:** `LedgerPanel`/`ledgerRow`/`ledgerChip` + `chipClass`/`ledgerStatusClass`/`ledgerVals` inside `wizard.templ` (class literals stay in the .templ so the Tailwind `@source` scanner picks them up); chip `hx-vals` carries session+indices (htmx serializes as form fields), `title` shows the tradeoff. **Server:** `handleCreateLedger` (panel fragment; 404 unknown session) + `handleCreateLedgerSelect` (records the choice, returns the transcript fragment; 503 no planner, 404 unknown session) added to `routes()`. **Config:** the requirements-planner persona gains an alignment-ledger section specifying the exact emission format + rules (prose always precedes the block; re-emit the whole ledger each turn; mark agreed + selected once settled; one-line rationales; block is last). Non-TCB, human-reviewed; `make generate` ran (emerald/amber tints compiled into app.css). Tests: ledger parse/strip/displayProse/cut + malformed/empty/no-block/normalize; a ledger-bearing turn populates `Ledger()` with clean transcript prose; `Choose` valid → user turn + planner dispatch, out-of-range → no-op; server panel render + chips, select records a turn + returns transcript, unknown-session 404 / no-planner 503. `make check` green. **Deferred to T4.14:** persisting the finalized decisions sidecar + transcript on APPROVE. (needs T4.12) ([control-room.md](specs/control-room.md), [specs-process.md](specs/specs-process.md))
- [x] **T4.14 Spec authoring + consent gate** — *done.* The planner now drafts spec markdown + seed issues
  and an explicit human **APPROVE** commits them — the consent boundary past which everything is autonomous.
  **Draft model (mirrors the ledger, T4.13):** the planner re-emits a latest-wins ` ```draft ` JSON block
  after its prose (and the ` ```ledger ` block) carrying `summary` + `specs[]` (path+content) + `issues[]`
  (title/body/role/spec/key/depends_on). New `wizard/draft.go` (`Draft`/`DraftSpec`/`DraftIssue`, `parseDraft`,
  `Empty`/`clone`) parses it exactly like the ledger — degrades to (zero,false) on no/malformed/empty block,
  never clobbering a prior draft. **Independent block extraction:** `cutLedgerBlock` generalized to
  `cutFencedBlock(reply, fence)`; `displayProse` now cuts at the **earliest** of the ledger/draft fences, so
  neither JSON block ever flashes in the live `delta` stream or lands in the stored transcript regardless of
  their order. `Session` gains a `draft` field + `Draft()`/`Transcript()` accessors (transcript = the
  user/assistant turns as JSON — the replayable "why"); `run()` parses both blocks independently and broadcasts
  a new `draft` SSE nudge alongside `turn`/`ledger`. **Consent gate seam:** new `wizard.Seeder` interface +
  `SeedRequest`/`SeedResult`/`DecisionRecord`/`SeededIssue` (co-located with the draft); `FinalizedDecisions`
  derives the decisions sidecar from the **agreed ledger items** (the ledger is the single source of the "why",
  not a parallel block). The wizard package stays pure (model+live only) — the Seeder is the one boundary the
  composition root implements. **Server (T4.14):** `Options.Seeder`; routes `GET /create/draft/{session}` (panel
  fragment, re-fetched on `sse:draft,turn`) + `POST /create/approve` (the gate). Approve commits the
  **server-side** draft (the trusted planner's latest snapshot), never browser content — the browser only sends
  "approve session X". Degrades: no planner→503, no seeder→"approval unavailable" notice, empty draft→"nothing to
  approve", seeder error→surfaced in-fragment, unknown session→404. **views:** `DraftPanel` (proposed specs in
  `<details>`/`<pre>`, seed issues, the emerald Approve form with the consent warning; read-only note when no
  seeder) + `CreateApproveResult` (created issues linking to `/issue/{id}`, or the error) in `wizard.templ`;
  wired into `CreatePage` with a `#wizard-result` region. **cmd-side `wizardSeeder`** (`cmd/harness/wizard_seed.go`)
  implements the Seeder: validate → write specs → store transcript → write decisions sidecar → **git commit** →
  `beads.Apply`. Validation enforces the spec contract (specs-process.md "every link resolves; every spec maps to
  ≥1 issue"): safe paths under `specs/` + `.md`, **link integrity reusing the new exported `spec.Links`** (the
  same links the orchestrator traverses — single source, refactored out of `spec.Resolve`), issue→spec coverage,
  and **produces-legality** via a new `seedRoles`/`resolveSeedRole` (`cmd/harness/config.go`; `entryRole`
  refactored onto `seedRoles`) — a seed issue may only enter at a pipeline **entry** stage (the human-seed analog
  of `acceptPlan` rejecting an illegal planner child; never mid-pipeline). Seed issues are created as epic roots
  (no `EpicID` — `epicOf` falls back to own id), each carrying its spec ref + a provenance footer linking the
  sidecar + transcript hash. Git commit message records the same provenance. **Wiring:** `run.go` builds the
  seeder (over the run repo + a beads client + the artifact store) only when the requirements planner is
  configured; a standalone `harness serve` wires neither and APPROVE shows disabled. **Persona** updated to emit
  the ` ```draft ` block only once intent has converged, owning link integrity + spec coverage and keeping seed
  issues coarse (the autonomous `plan` stage decomposes). Non-TCB, human-reviewed; `make generate` ran. Tests:
  wizard draft parse/degrade/drop-incomplete + displayProse-cuts-both-fences + FinalizedDecisions; a draft-bearing
  turn populates `Draft()` with clean transcript prose + fires the `draft` nudge; server draft-panel render +
  approve success (server-side draft handed to the Seeder verbatim) + the four guard paths; cmd-side
  `validate` (valid + 9 rejection cases) + a real git+bd `Seed` integration (spec written+committed, transcript
  stored, seed issue created with role+spec+footer). `make check` green (lint 0, 624 pass / 2 skip).
  **Deferred:** the decomposition-preview dry-run (control-room.md OPEN, "leaning defer") stays deferred — seed
  issues are coarse and the autonomous planner decomposes. (needs T4.12) ([specs-process.md](specs/specs-process.md), [control-room.md](specs/control-room.md))
- [x] **T4.15 Resolve mode** — *done.* The wizard's second entry mode (`GET /resolve/{id}`), pre-loaded from a
  dead-lettered issue: the escalation, the governing spec slice, and the transcript that raised it, with the
  spec edit's **blast radius** shown before the consent gate (control-room.md "Create and Resolve are the same
  component"). **Reachability (the piece T4.11 deferred):** the orchestrator now stamps the most-recent
  invocation transcript onto the **issue** (`core.Issue.Transcript`, `beads.StampTranscript`, new
  `transcript` metadata key) in `handleResult` for **every** disposition — best-effort, idempotent, skipped on
  an empty hash — so the decision trail is reachable for in-flight/dead-lettered work, not only from a merge
  trailer; and the dead-letter **reason** is stamped in the same transition that blocks the issue
  (`Block(ctx, id, reason)` → `core.Issue.DeadLetterReason`, new `dlq_reason` key), surfaced on the DLQ row and
  the detail page (closes the long-standing "reason not on the issue" gap). **Blast radius:** new read-only
  `spec.Members(root, ref, depth)` (shares `Resolve`'s traversal) answers "does this slice include the edited
  path"; `query.Reader.BlastRadius(repo, depth, editedPaths)` runs the recompile-the-delta predicate as a
  preview — in-flight issues whose slice includes an edit (would reissue) + closed `(epic, spec-path)` groups
  (would re-derive), the same membership the sweep acts on. `query.ResolveContext` assembles the escalation +
  current slice + transcript ref. **Wizard:** `Planner.NewResolve(ResolveSeed)` grounds a session in the
  escalation (folded into the system prompt, not the visible transcript), records the issue id it commits
  against (`Session.issueID` / `ResolveIssue()`), and auto-opens one turn; the conversation/ledger/transcript
  reuse the per-session `/create/*` endpoints (a Resolve session is just a `Session`). New `wizard.Resolver`
  seam (sibling of `Seeder`) + `ResolveRequest`/`ResolveResult`; the cmd-side `wizardSeeder` implements both
  (shared `validateSpecFiles`/`commit`/`decisionsSidecar`), with `Resolve` writing the refined spec, storing
  provenance, committing, and **reopening the dead-lettered issue** via `Reissue` (a blocked issue is neither
  in_progress nor closed, so the recompile sweep does not touch it — Resolve reopens it explicitly, clearing
  the stale pin so the next dispatch re-resolves the edited slice). Resolve creates **no** new seed issues (new
  scope → Create). **Server:** `/resolve/{id}` (page), `/resolve/blast/{session}` (combined draft + blast +
  approve panel, refreshed on `sse:draft`), `POST /resolve/approve` (commits the server-side draft against the
  server-bound issue id — never browser content); `Options.{Resolver,Repo,SpecDepth}` threaded from the
  composition root (repo/depth supplied by the caller, keeping the read model config-free). **Views:**
  `resolve.templ` (ResolvePage, ResolvePanel, blastRadius, ResolveApproveResult, ResolveMessage); the
  spec-files block extracted to `draftSpecFiles` (shared with Create); DLQ rows and the blocked issue-detail
  header gained "Resolve →" launch links + the inline reason. **Deferred (filed, not blocking):** wiring
  `Reader.Replay` to read `issue.Transcript` so non-merged invocations replay too — the data is now reachable,
  only the read is merge-trailer-bound; a small follow-up. Tests: `spec.Members`; beads `StampTranscript` +
  `Block`-with-reason round-trips; orchestrator stamps transcript + reason on a dead-letter; query
  `BlastRadius` (in-flight membership, merged grouping, empty/error) + `ResolveContext` (+ degrade) + DLQ
  reason; wizard `NewResolve` grounds+opens; cmd `Resolve` integration (commit + transcript + reopen) +
  validation; server handlers (page render, blast panel, approve success + guards). `make check` green.
  (needs T4.14) ([control-room.md](specs/control-room.md), [specs-process.md](specs/specs-process.md), [observability.md](specs/observability.md))

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
