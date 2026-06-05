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
- **Open tasks (`- [ ]`) keep their full detail** — the remaining Phase 5 items, plus the handful of
  optional items left in Phase 2 (T2.11/T2.12). T4.26 (Config view) closed the original Phase-4 view
  roster and **T4.27 is done — Phase 4's original scope is fully complete** (one open follow-up,
**T4.29**, migrates the wizard's structured output from parsed fenced blocks to tool calls);
**Phase 6 (agent semantic tooling — the
  LSP-backed tool surface) is now fully complete (T6.1–T6.3)**. The only remaining *engineering* of new
  substrate is Phase 5 (production isolation & distribution) — and within it the Firecracker backend
  (T5.2) is hardware-blocked and deliberately last; the live Phase-5 work is T5.5/T5.6/T5.7/T5.11 (T5.10
  provenance signing is now done).
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
- [x] **T2.13 Producer/verifier model-family diversity warning** — *done.* Closes the last non-optional Phase 2
  gap. **(1) Non-fatal warning channel:** new **`Config.Warnings() []string`** (sibling of `Validate`, in
  `internal/config/warnings.go`) — same `add`-closure + sort pattern as `Validate`, plus de-dup; `cmdValidate`
  prints each to **stderr** (`harness validate: warning: …`) after a clean `Validate` and still exits 0
  (stdout keeps the OK line). The split is the point: `Validate` gates startup on run-time-breaking faults,
  a warning is the operator's call (config-is-the-pipeline). **(2) Diversity check (`warnModelDiversity`):**
  derives the **producer** stage as the one gated by the **red→green proof** (`core.PostconditionRedGreen` — the
  principled signal for "implement", per verification.md), and its **verifiers** as the **gate stages downstream
  of it via produces edges** (`isGateStage` = a postcondition that is a command check in the `checks` registry or
  a metric comparison — excludes the reserved proofs and orchestrator-evaluated `human-approved`). Scoping
  verifiers to produces-descendants (not "every gate stage") means (a) a non-gate stage inserted between
  implement and qa can't hide the overlap and (b) the **conflict-spawned `resolve` stage — gated but not produced
  by implement, and not an independent reviewer of the implementor — is correctly NOT treated as its verifier**
  (regression-tested). Each role → soul(s) → `core.Soul.Model` → `config.ModelProvider.Provider`; a role resolves
  to a **provider set** (selector-based multi-soul), and an intersection between producer and verifier sets warns
  per shared provider. Keyed on **`Provider`** as the family proxy; when the shared provider is `openai-compat`
  the message names the known imperfection (distinct endpoints read as one family). **The shipped config trips
  exactly one advisory** (implementor + security both `anthropic`, different tiers — anthropic is the only family
  wired in dev); documented as an accepted tradeoff in `config/souls/security.yaml` and pinned by
  `TestWarnShippedConfig` (and `TestValidateShippedConfig` confirms it stays non-fatal). Tests (config +
  cmd/harness): same-provider warn, differing-provider no-warn, no-gate-stage / no-producer no-warn, never-fatal,
  openai-compat note, provider-set intersection, resolve-is-not-a-verifier, shipped-config advisory. `make check`
  green (lint 0, 656 pass / 2 skip). **Deferred (filed, not blocking):** the complementary control-room tooltip
  still needs a souls/config view that does not exist yet. ([verification.md](specs/verification.md), [configuration.md](specs/configuration.md))
- [x] **T2.14 `golangci-lint` gate check + the producer-self-check principle** — *done.* Adds static
  lint to the `qa`/`resolve` gates and records the self-check-vs-trust boundary in the spec.
  **Config-only on the gate side (no Go change):** `golangci-lint: make lint` joins the `checks`
  registry and the check is appended to the `qa` and `resolve` postconditions — placed after
  `tests-pass`, before the expensive `mutation` pass, so the fail-fast gate surfaces a cheap lint
  failure first. Verified by tracing the gate that it is **fully generic** — postcondition
  classification (`gate.Registry.Resolve`), execution (`runCheck`→`execCheck`, graded on exit code),
  validation (`config.knownPostcondition`), and the provenance trailer / `gate-verdict` records all
  treat `golangci-lint` identically to `gosec`; there is no hardcoded check-name enum anywhere, so the
  design's "adding a check is a config edit" holds and **no implementation change was needed**.
  **Feedback half:** the `implementor-go` persona now runs `make lint`/`make check` and fixes before
  `submit`, so a lint nit is caught at the keyboard rather than bouncing a whole fresh qa attempt — the
  *same* `make lint` the gate re-runs (one command, run by the agent for speed and by the gate for
  trust; a producer self-check earns zero trust — only the gate's independent re-run in the clean
  sandbox advances the transition). New spec section [verification.md](specs/verification.md) "Producer
  self-checks are feedback, not grades"; docs updated (configuration.md, pipeline.md, getting-started.md).
  `harness validate` passes (golangci-lint resolves to `make lint`). **Live-green awaits T5.3** — like
  the other scanners, the check fails closed in a real run until `golangci-lint` is baked into the
  `security` role's sandbox image (a pure static analyser, binary-only, no offline DB).
  ([verification.md](specs/verification.md), [configuration.md](specs/configuration.md))

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

### Live transition events + board-in-motion (T4.16–T4.18)

Implements the now-**decided** control-room refinement (was the "coarse live trigger →
precise issue-state event" OPEN): the single-writer orchestrator emits a typed
transition event the board/DAG/DLQ refresh off, giving crisp **animated card moves**
and the anchor for **per-card timers** (time-in-state + total). The three tasks split
backend-emit / transport / frontend-consume, the same way T4.3 (SSE plumbing) and T4.4
(board) were split. Resolves [control-room.md](specs/control-room.md) "The board, in
motion"; specs already updated (orchestrator.md §9, messaging.md `issue.<id>.state`,
control-room.md, observability.md).

- [x] **T4.16 Issue-state transition events + `state_entered_at` stamp** — *done.* The
  single-writer choke point. **TCB-touching (orchestrator), human-reviewed.** **(1) Durable stamp:**
  `core.Issue.StateEnteredAt time.Time` (beads `state_entered_at`, new `MetadataKeyStateEntered`)
  is stamped **atomically inside every status-changing bd write** — `setStatus` (Close/Block/
  AwaitApproval/Release/Reissue) and `Claim` both append a `stateEnteredNow()` `--set-metadata`
  pair, mirroring how `Claim` already stamps `lease_until` — so the anchor is set exactly once
  per real transition in a single write (no second write that could fail independently). Decoded
  back via a new **`metaTime`** sibling of `metaString`/`metaInt`/`metaFloat`/`metaDuration`
  (RFC3339→UTC, lenient: absent/malformed reads as zero). A **metadata-only** write
  (PinSpecHash/StampClosingSpend/StampTranscript/**RecordApproval**) deliberately does **not**
  stamp it — it records the *entry* into a status, not a later annotation. **(2) Live nudge:** new
  `core.IssueStateEvent{ID,Status,Role,Epic,TS}` (in `core`, single-source like `ApprovalRequest`)
  published on **`harness.issue.<id>.state`** (new `messaging.IssueStateSubject`/`IssueStateWildcard`/
  `IssueIDFromStateSubject`, mirroring `AgentEventsSubject` exactly — embedded-separator/wildcard/
  empty rejected). **One transition helper (`internal/orchestrator/transition.go`):**
  `o.transition(ctx, issue, to, write)` runs the beads write (which stamps), then on success
  `announceState` publishes the event best-effort over **core NATS** (`o.nc`, the conn under `js` —
  issue-state has no stream); a marshal/publish failure is logged, never propagated (callers keep
  Nak-on-error). **`Epic` = `core.EpicOf`** (root falls back to own id). Every status write now
  funnels through it: `scheduleReady` Claim (+the failed-publish Release reversal), `accept`/
  `acceptPlan`/`route`/`resolveConflict` Close, `deadLetter` Block, `parkAwaitingApproval`
  AwaitApproval, `resumeApproved` Close, `recompileSpecDelta` Reissue, `sweepLeases` Release (best-effort
  `Get` to populate role/epic, minimal id-only event on a read miss). **Idempotency is provided
  upstream, not by a stale-status guard:** `handleResult`/`handleApproval` act only while the issue
  is in its expected transient status, so a redelivery onto an already-settled issue returns before
  any write — no re-stamp, no re-announce — which is also why `transition` announces unconditionally
  after a successful write (a guard would wrongly suppress the claim→release reversal). **Constructor
  change:** `orchestrator.New(...)` gains an `nc *nats.Conn` param (validated, before `js`); wired in
  `run.go` (`nc` already in scope) and `newOrch` test helper. Core NATS, publish-only, additive — beads
  stays authoritative. `make check` green (lint 0, **685 pass / 2 skip**). Tests: beads stamp on each
  transition kind + `metaTime` round-trip/degrade (transitions_test); messaging subject round-trip +
  malformed-reject; orchestrator transition announces each kind with full payload + non-zero TS,
  EpicOf fallback, write-failure-no-announce, and settled-issue no-reannounce (via `handleResult`).
  **Unblocks T4.17** (the SSE pump tailing `harness.issue.*.state`) **→ T4.18** (crisp board refresh,
  animated moves, per-card timers off `StateEnteredAt`). ([components/orchestrator.md](specs/components/orchestrator.md), [messaging.md](specs/messaging.md))
- [x] **T4.17 Issue-state SSE pump** — *done.* Non-TCB (controlroom). New
  **`StartIssueStatePump(nc, hub)`** in `internal/controlroom/live/pump.go`, a sibling of
  `StartAgentEventPump`: one wildcard subscribe to **`messaging.IssueStateWildcard`**
  (`harness.issue.*.state`), each message broadcast into the `Hub` as an **`issue-state`** SSE
  event the board/DAG/DLQ views will consume as an `hx-trigger` nudge (server-render-a-fragment,
  not `sse-swap` — T4.18 swaps their triggers from `agent-event` over to this). **Two differences
  from the agent-event pump, both deliberate:** (1) **no `Activity` buffer** — issue-state is a
  view-refresh nudge, not feed content, so the pump only broadcasts; (2) **no envelope** — the
  payload is already a complete `core.IssueStateEvent` (the id rides in the body, not only the
  subject), so the pump **relays the original bytes** after validating them, rather than wrapping
  like `AgentEvent`. **Best-effort guards** matching the fire-and-forget core-NATS transitions it
  tails: a subject that doesn't parse to an id (`IssueIDFromStateSubject == ""`, defensive — NATS
  only delivers concrete subjects matching the wildcard) and a body that isn't a well-formed event
  (or is missing its `ID`) are dropped; a stalled browser is dropped by the hub. Losing a
  transition is harmless — beads stays authoritative and the views keep a periodic backstop. The
  pump unmarshals into `core.IssueStateEvent` as the core-struct comment prescribes (the read side
  of the single-source schema the orchestrator writes). **Wiring:** `buildRunComponents` starts it
  behind `--serve-addr` alongside the agent-event pump, **sharing the same hub**; its unsubscribe
  joins the teardown stack (`releases`). Standalone `harness serve` has no NATS, unchanged. No new
  route/flag/view → no doc change (the `/events` endpoint already multiplexes hub events by name).
  Tests (`pump_test.go`, real embedded NATS): happy-path round-trip (marshaled `IssueStateEvent` →
  `issue-state` SSE event, full struct equality), malformed-drop proven by **ordering** (non-JSON
  and id-less events published ahead of a valid one; receiving the valid one first proves the bad
  ones never reached the hub — NATS preserves single-subscription publish order), and stop-func
  teardown. `make check`-adjacent green: `go build ./...`, `go vet`, `golangci-lint` (0 issues),
  and `go test -race ./internal/controlroom/live/` all pass. **Unblocks T4.18** (board crisp
  refresh, animated moves, per-card timers). (needs T4.16) ([messaging.md](specs/messaging.md), [control-room.md](specs/control-room.md))
- [x] **T4.18 Board in motion — crisp refresh, animated moves, per-card timers** — *done.* Non-TCB
  (controlroom views + query + beads read path). The frontend-consume half of the live-transition
  refinement (T4.16 emit, T4.17 transport). **(a) Crisp refresh:** the board / DAG / DLQ `hx-trigger`
  is swapped from `sse:agent-event` to **`sse:issue-state`** (the `every 15s` backstop kept), so each
  refetches precisely when the orchestrator advances work, not on every per-token agent nudge. **The
  activity feed deliberately stays on `agent-event`** — its job is per-turn progress, not transitions.
  **(b) Animated moves:** each `boardCard` gains a stable **`id="card-<id>"`** + **`view-transition-name:
  card-<id>`** keyed on the issue id, and the board swap opts into the **View Transitions API** via
  htmx **`hx-swap="innerHTML transition:true"`** (per-swap modifier — no global config needed), so a card
  landing in a new column tweens its move; instant-swap fallback where unsupported, no animation lib. The
  view-transition-name is emitted through **`templ.SafeCSS`** (templ's style sanitizer drops the unknown
  `view-transition-name` property otherwise) via a new `views.cardVTName`; because SafeCSS is verbatim, the
  id is reduced to the CSS custom-ident charset by `cssIdent` (defense in depth — beads ids already are).
  **(c) Timers:** beads' own top-level **`created_at`** (RFC3339, not harness metadata) is decoded into a
  new **`core.Issue.CreatedAt`** (`issueJSON.CreatedAt` + shared `parseRFC3339`, extracted from `metaTime`),
  the total-time anchor; `core.Issue.StateEnteredAt` (T4.16) is the time-in-state anchor. Both ride onto
  **`query.IssueCard`** and the card emits them as **Unix-epoch `data-state-since` / `data-created`**
  attributes (`views.epochAttr`, empty for the zero time → ticker shows a dash). A new
  **`assets/static/ticker.js`** (Alpine `x-data="cardTicker()"`) rewrites two `x-ref` spans every second
  client-side (`fmtDuration` → `45s`/`3m12s`/`2h05m`/`1d04h`); the **status→label** mapping
  (`working`/`queued`/`blocked`/`closed`) lives server-side in `views.stateLabel` (one home) so the JS only
  advances durations. The server **never re-renders to tick**; Alpine's lifecycle clears the interval on
  htmx swap (destroy) and re-inits new cards. **(d) `budget.wall` tint — deferred** (filed): the board cards
  don't currently receive the threaded caps (T4.10 passed them only to the Budgets view), and the spec marks
  this optional; a follow-up can thread `BudgetCaps` to the board. Ran `make generate` (templ + Tailwind).
  Tests: beads `created_at` decode (valid/absent/malformed → zero), query card carries both anchors,
  board renders the two epoch data attrs + stable id + `view-transition-name` + `transition:true` +
  `sse:issue-state` (and no longer `sse:agent-event`) + the `working` label + the ticker `<script>`,
  ticker.js served from `/static`, and the `sse:issue-state` trigger swap asserted on the DAG and DLQ
  pages too. `make check` green (lint 0, **692 pass / 2 skip**). Docs: `docs/control-room.md` Board row +
  liveness note updated (typed event, animated moves, client-ticked timers; board/DAG/DLQ on issue-state,
  activity on agent-event). (needs T4.16, T4.17) ([control-room.md](specs/control-room.md))

### Observability surfaces (T4.19–T4.25)

Four new control-room surfaces decided in the post-T4.18 spec pass — the
[live invocation view](specs/control-room.md), the [verification view](specs/control-room.md),
the [merge-queue view](specs/control-room.md), and the [status bar + escalation
alerts](specs/control-room.md). Each is backed by a **named, persisted or emitted**
thing (an enriched event envelope, a `gate-verdict` artifact + recorded souls,
`merge-state` events) rather than a live scrape — so every surface stays inside the
existing invariants (single-writer, producer≠verifier, forensic-unless-running). The
tasks follow the same **emit / transport / consume** split as T4.16–T4.18 and are
listed in dependency order: the cheap status/DLQ surface first, then live invocation
(after the event envelope carries issue/role), then verification (after its gate-verdict
+ soul plumbing), then the merge queue (after merge-state events). Specs already updated
(control-room.md, messaging.md, verification.md, integration.md, security.md,
components/artifact-store.md, glossary.md).

- [x] **T4.19 Status bar + DLQ escalation alerts** — *done.* Non-TCB (controlroom). The cheap
  status/DLQ surface, the first of the T4.19–T4.25 batch. **(1) Status bar in the layout chrome:**
  a thin bar rides every page (queue depth · active agents · open escalations · budget-health dot ·
  last merge). It is a **self-loading live fragment**, not threaded data — `views.StatusBarShell`
  (added to `Layout`, after `</header>`) is an SSE-connected element that lazy-loads `GET /status/bar`
  on `hx-trigger="load, sse:issue-state, sse:dlq-arrival, sse:agent-event throttle:5s, every 30s"`,
  so the chrome stays data-free (`Layout(title, active)` unchanged) and every page picks up the bar for
  free. New **`query.Reader.Status(ctx, caps)`** assembles it from a **single `Budgets` read** (one
  `ListAll`, reusing the exact burn/breach math the budget view enforces on) plus the newest provenance
  commit — so the bar agrees with the board/DLQ/budget views by construction and adds no new beads
  query. Queue depth = non-terminal, non-blocked issues; escalations = blocked count; budget-health =
  worst per-dimension level across issues+epics (`ok`/`warn ≥80%`/`breach`, mirroring the Budgets
  tints); last merge = newest integration commit's issue id (best-effort, nil/faulting prov ⇒ empty).
  **Active agents** is NOT in `query.Status` (the read model has no buffer access): new
  **`live.Activity.ActiveAgents(window)`** counts distinct agent-sourced ids seen in a trailing window
  (90s), filled in by the handler — "no new registry" per spec. **(2) DLQ escalation alerts:** new
  **`live.StartDLQPump(nc, hub)`** (sibling of `StartIssueStatePump`) tails the durable
  `messaging.SubjectDLQ` (`harness.dlq`) with a plain core sub (a JetStream publish is an ordinary
  publish core subscribers also receive — the stream stays the source of truth, the tail is only the
  nudge) and broadcasts a **`dlq-arrival`** SSE event; a new **`assets/static/alerts.js`** opens its own
  `EventSource('/events')` purely to fire a **browser `Notification`** on it (the DLQ is the human's only
  action surface, so an arrival is the one push-worthy event; everything else is pull). The status bar's
  own SSE connection bumps the escalation count on the same event. **Single-source type:** the
  orchestrator's private `dlqAlert` was promoted to **`core.DLQAlert`** (the discipline
  `core.IssueStateEvent`/`ApprovalRequest` use) so the write side (`deadLetter`/`parkAwaitingApproval`)
  and the read side (the pump) share one schema. **Server:** `GET /status/bar` handler (503 no reader ⇒
  htmx leaves the neutral `StatusBarLoading` placeholder = the spec's "degrades to a static bar"; 500 on
  read error ⇒ keeps last good bar). **Wiring:** `StartDLQPump` joins the pump teardown stack in
  `buildRunComponents` behind `--serve-addr`, sharing the hub. Docs: `docs/control-room.md` "The status
  bar" section added. Tests: `live.ActiveAgents` (distinct/system-excluded/windowed/empty), `StartDLQPump`
  (round-trip + malformed-drop-by-ordering + stop-unsubscribe over real embedded NATS), `query.Status`
  (counts/health ok-warn-breach/epic-breach/no-merge/ListAll-error), server (`/status/bar` fragment +
  active-agents fill + 503 no-reader + layout-includes-bar). Also fixed `TestBoardInMotion`'s now-overbroad
  whole-page `sse:agent-event` negative assertion — scoped it to the `#board` element, since the status bar
  legitimately uses `agent-event` for the active-agents count. `make check` green (lint 0, 685 pass / 2
  skip in `make test-unit`). **Deferred (filed, not blocking):** the bar opens 2–3 HTTP/1.1 SSE
  connections per page (page content + status bar + alerts.js notification listener) — fine for a
  single-operator control room, but a future consolidation onto one connection (or h2c) would tidy it.
  (unblocks T4.20+) ([control-room.md](specs/control-room.md), [messaging.md](specs/messaging.md))
- [x] **T4.20 Agent-event envelope: issue id + role** — *done.* TCB-adjacent (runner/broker emit),
  human-reviewed. The originating **issue id + role** now ride on every `agent.<id>.events` payload so a
  consumer scopes a feed to one *live invocation* without a second beads read. **Single-source wire
  envelope:** new **`core.AgentEventEnvelope{IssueID, Role, Payload}`** (`internal/core/agent_event.go`,
  the discipline `core.IssueStateEvent`/`core.DLQAlert` use) wraps the opaque inner event (the existing
  `runner.tokenEvent` or `broker.PublishRequest`) — the runner is the write side, the control-room pump the
  read side. **The agent (invocation) id is deliberately NOT in the envelope** — it is the subject's final
  token (`AgentEventsSubject`), recovered by the consumer, so the payload only adds what the subject does not
  already say. **Runner:** the `relay` gains `issueID`/`role` (threaded via `relayConfig` from
  `brief.Issue.{ID,Role}` in `runner.go`); **both** publish paths funnel through one new
  **`relay.publishEnveloped(payload)`** helper (`publishEvent` for token/reasoning/tool deltas;
  `PublishEvent` for agent progress/log) so stamping happens in exactly one place. **Pump:**
  `StartAgentEventPump` now unmarshals the envelope, drops a subject that doesn't parse to an id or a body
  that isn't a well-formed envelope (best-effort guards matching the other pumps), labels the broadcast with
  the subject-derived agent id + the envelope's issue/role, and broadcasts the **unwrapped inner event** as the
  `agent-event` payload. `live.AgentEvent` gained `IssueID`/`Role` (the SSE-broadcast struct; agent id added by
  the pump). **Buffer:** `live.Activity.Entry` gained `IssueID`/`Role` and `Record(agentID, issueID, role,
  payload)` records them — so a downstream view (T4.21) filters the feed to one invocation server-side with no
  beads read. Spec wording corrected: messaging.md now says the **runner** (not orchestrator) stamps the
  binding at publish time. Publish-only, additive, no new route/flag/config/view → no `docs/` change. Tests:
  runner `decodeEvent` helper asserts the issue/role stamping on every published event (Complete token + the
  reasoning/tool batch + PublishEvent); pump round-trips the envelope (agent id from subject, issue/role from
  body, inner payload unwrapped) into both hub + buffer; new `TestActivity_CarriesIssueBinding` proves the
  binding lands on both a coalesced token run and a discrete row. `make check` green (lint 0). (unblocks T4.21)
  ([messaging.md](specs/messaging.md), [control-room.md](specs/control-room.md))
- [x] **T4.21 Live invocation view** — *done.* Non-TCB (controlroom). `GET /invocation/{id}` — a scoped
  activity feed filtered **server-side** to one invocation plus a live budget meter, the one deliberate
  live-*detail* surface. **Query:** new **`query.Reader.Invocation(ctx, id, caps)`** → `query.Invocation`
  (header id/title/role/status/spec/body + `Terminal` + `ReplayAvailable` + an `IssueBudgetRow`). The
  per-issue budget meter is built by a new **`buildIssueBudgetRow`** extracted from `Budgets` and shared by
  both — so the invocation meter and the Budgets table can never disagree on an issue's burn. `Terminal` =
  closed|blocked; `ReplayAvailable` is gated **merged-with-transcript** exactly like the issue-detail Replay
  link (T4.7b), best-effort (a git fault leaves it false). **Scoped feed:** new
  **`live.Activity.RecentForIssue(issueID)`** filters the buffer on the issue id the runner stamps on every
  event (T4.20) — newest-first, system rows excluded — so the server scopes the feed to one invocation with
  no beads read. **Server:** `handleInvocation` (page) + `handleInvocationItems` (the live body fragment) +
  an `invocation` helper that degrades to a bare-id projection when no Reader is attached; with no Activity
  buffer (standalone `harness serve`) the page shows the not-attached notice (200) and the fragment 503s.
  **Views (`invocation.templ`):** static header (id/role/status/spec + drill-back to issue detail + Replay
  when available) outside the SSE region; `InvocationBody` (budget meter via the reused `budgetPct` tints +
  terminal handoff banner + scoped feed) is the htmx swap target, re-fetched on **`sse:agent-event throttle:1s,
  sse:issue-state, every 10s`** — issue-state so the terminal handoff appears the instant the orchestrator
  advances the issue, agent-event for per-turn progress, server-render-a-fragment (not `sse-swap`, the token
  stream is a firehose). **Drill-in:** board cards now link to `/invocation/{id}` (control-room.md "drill from
  a board card") and agent activity rows link their invocation id to `/invocation/{issueID}`; issue detail
  stays reachable from the invocation header / DLQ / provenance / budgets. Ran `make generate` (templ +
  Tailwind). Docs: `docs/control-room.md` drill-through list + Board row updated. Tests: query
  (in-flight/terminal-merged-offers-replay/blocked-no-replay/get-error), `live.RecentForIssue` (scopes +
  excludes system + unknown-issue empty + newest-first), server (page header+meter+scoped-feed+SSE wiring +
  no-leak, bare fragment, terminal handoff, no-activity notice/503), and the board-card link flipped to
  `/invocation/`. `make check` green (lint 0). **Deferred (filed, not blocking):** sub-result *live* wall/token
  ticking — the meter reflects spend stamped at result boundaries (re-fetched on nudge), since mid-invocation
  spend isn't persisted to beads; a client-side wall ticker (like the board's T4.18 timer) could smooth it.
  (needs T4.20) ([control-room.md](specs/control-room.md))
- [x] **T4.22 Gate-verdict record + producing-soul attribution** — *done.* **TCB-touching** (gate harvest + orchestrator advance + provenance trailer), human-reviewed. Two coupled pieces. **(1) Gate-verdict record:** new **`core.GateVerdict`** (`internal/core/gateverdict.go`) — the assembled, serializable per-check result of one gate run (`Passed` + `[]GateCheckOutcome{Name,Kind,Passed,ExitCode,Evidence,Base?,Metric?}`, with `GateRunOutcome`/`GateMetricOutcome` for the red→green base exit and the metric score/op/threshold) and the stable kind spelling (`GateCheckCommand/RedGreen/TestsRed/Metric`, the serialized mirror of the gate's unexported `checkKind`). New artifact kind **`core.ArtifactKindGateVerdict = "gate-verdict"`**. The gate **harvests it after `persistEvidence`** (new `Runner.persistVerdict` + `verdictRecord` mapper) so each check cites its own gate-evidence hash — the record is the *index* over the per-check output, not a copy (artifact-store.md). New **`gate.Report.Verdict core.ArtifactRef`** carries the hash back; best-effort like evidence (a nil/erroring store leaves it empty, never changes the verdict). Recorded for **every** run, pass or fail. **(2) Producing-soul attribution:** **`core.Issue` gains `TestsSoul`/`ImplementSoul`** (threaded forward like `TraceMap`) **+ `GateVerdict`** (the issue's gate-run record hash, stamped post-hoc per gate run, not threaded). beads: new metadata keys `tests_soul`/`implement_soul`/`gate_verdict`, `create()` threads the souls, `toCore` decodes all three, new `StampSouls`/`StampGateVerdict` (non-empty-only / empty-is-no-op, idempotent sets). The orchestrator **stamps the issue's own producing soul in `handleResult`** (new `stampProducingSoul`, keyed off the stage's reserved proof via `stageProves` — `tests-red`⇒TestsSoul, `tests-red-then-green`⇒ImplementSoul) before the disposition switch, **mutating the in-memory issue** so the just-stamped soul threads forward, and threads `issue.TestsSoul`/`issue.ImplementSoul` onto every produced/route/conflict child. It **stamps `report.Verdict.Hash`** onto the issue after the gate runs (StatusDone), for accept *and* route, so a rejected candidate's verdict stays reachable. **Trailer:** `core.Provenance` gains `TestsSoul`, rendered as `Tests-Soul:` on line 1 (`Soul: … | Model: … | Tests-Soul: …`) and parsed back (`ParseCommitMessage`/`parseTrailerLine`); `provenanceFor` sets `prov.TestsSoul = issue.TestsSoul` (Soul stays `selectSoul`). All stamps non-fatal (audit, not a correctness gate). **Read side:** new **`query.Reader.GateVerdict(id) → GateVerdictView`** (Issue, Merged, TestsSoul, ImplementSoul, Hash, Available, `core.GateVerdict`) — resolves the verdict record from `issue.GateVerdict` (reachable for merged *and* rejected work) and reads the souls **from the issue stamps, not the trailer** (on the shipped pipeline the integrate-producing stage is `qa`, so the trailer's `Soul` is the qa/security soul, not the implementor — the threaded `issue.ImplementSoul` is the principled source; the trailer's `Tests-Soul` is itself derived from it). Best-effort: a missing/unfetchable/corrupt record → `Available=false` with the hash retained for the raw-bytes link, never a blank page (mirrors Replay). **Specs were pre-updated** (security.md/integration.md trailer, verification.md "The gate verdict is recorded", artifact-store.md/glossary.md gate-verdict kind) — no spec edit needed; no CLI/config/route change ⇒ no `docs/` change. Tests: core trailer round-trip with `TestsSoul` + first-line layout; gate harvests command/red→green/metric/failed verdicts + best-effort empty-on-store-outage; beads souls threading + `StampSouls`/`StampGateVerdict` round-trips; orchestrator stamps TestsSoul (author-tests) + threads it, stamps ImplementSoul (red→green implement) + cites both on the trailer, gate-verdict stamped on accepted *and* rejected issues; query `GateVerdict` (rejected reads issue stamps, merged reads issue stamps not trailer, no-record/unresolvable degrade, issue-error fatal). `make check` green (lint 0, **738 pass / 2 skip**). **Unblocks T4.23** (verification view renders `GateVerdictView`). (needs T2.8, T2.9) ([verification.md](specs/verification.md), [components/artifact-store.md](specs/components/artifact-store.md), [security.md](specs/security.md))
- [x] **T4.23 Verification view** — *done.* Non-TCB (controlroom). `GET /verification/{id}` — the factory's
  **trust argument, made legible**: a forensic snapshot rendering T4.22's `gate-verdict` record + the
  producing-soul stamps. **Mirrors the Replay/issue-detail forensic pattern exactly** (plain server-render,
  no SSE, best-effort never-blank-500). **View:** `views/verification.templ` (`VerificationPage` +
  `soulCard`/`verdictCheck`/`verdictBadge`/`checkPassBadge`/`verdictNotice`/`VerificationNotAttached`) +
  `views/verification.go` text helpers (`checkKindLabel`, `metricSummary`/`fmtScore` — the kind→label and
  mutation-score-vs-threshold formatting). Renders: **(1) producer≠verifier** — the `author-tests` and
  `implement` souls side by side (from the issue's own threaded `TestsSoul`/`ImplementSoul` stamps, so it
  shows for a *rejected* candidate with no merge trailer), with the `qa` gate explicitly marked as having
  **no verifier soul** (it runs in the orchestrator-controlled clean verification sandbox — that structural
  separation *is* the verification); **(2)** the **test↔spec traceability map** evidence link; **(3)** when a
  verdict record resolved, the per-check list — **red→green** proof (base exit must fail → candidate exit must
  pass, tinted red✓/green✓), **mutation metric** vs threshold, **scanners** — each check linking its own
  captured-output evidence via `/artifact/{hash}` (nosniff, untrusted bytes). Degrades to a notice (no
  verdict stamped, or unfetchable record with the raw-bytes link still offered) while still showing the soul
  split — exactly what a DLQ triager needs. **Query:** added a `Trace ArtifactLink` field to the existing
  `query.GateVerdictView` (T4.22), populated in `GateVerdict` from the issue's threaded `TraceMap` via the
  shared `r.link` (resolves store availability) — the only query-layer change. **Server:** `handleVerification`
  (registered ahead of the placeholder loop, after `/replay/{id}`) mirrors `handleReplay`: nil-reader →
  not-attached notice (200), unknown-id/read-fault → in-chrome notice. **No nav item** (a drill-through like
  `/issue` and `/replay`, not a top-level view). **Drill-in:** the issue-detail header gains a "▸ Verification"
  link (shown whenever `issue.GateVerdict` is set — gated work, rejected *or* merged) alongside the Replay link,
  and each DLQ row gains a "Verification" triage link (always offered — the page degrades gracefully when an
  issue dead-lettered before its candidate was gated). Ran `make generate` (templ + Tailwind; emerald/rose/amber
  tints compiled). **Specs pre-updated** in the T4.19–T4.25 spec pass (control-room.md "The verification view",
  verification.md "The gate verdict is recorded") — no spec edit; `docs/control-room.md` updated (new
  drill-through entry + forensic-pages list + DLQ row). Tests: query `Trace` link (present→available+labeled,
  absent→empty/unavailable); server — full render (verdict badge, soul split, red→green base/candidate exits,
  mutation `0.86 >= 0.80`, scanner, traceability + raw-verdict + check-evidence links, drill-back), no-verdict
  notice (in-flight issue still shows soul split, no check list), not-attached, unknown-id, and the
  issue→verification + DLQ→verification drill links. Enriched the shared `detailServer` fixture (harness-1
  carries soul stamps + a passing gate-verdict record + an available trace map; harness-2's map stays
  unavailable). `make check` green (lint 0, **744 pass / 2 skip**). **Unblocks T4.24/T4.25** (merge-queue
  surface, the last of the T4.19–T4.25 batch). (needs T4.22)
  ([control-room.md](specs/control-room.md), [verification.md](specs/verification.md))
- [x] **T4.24 Merge-state transition events** — *done.* **TCB-touching** (merge path / orchestrator),
  human-reviewed. The emit half of the merge-queue surface: the orchestrator publishes a typed
  **`core.MergeStateEvent`** on **`harness.merge.<id>.state`** at each step the
  [serialized queue](specs/integration.md) passes through — `queued → (rebasing → re-gating →)? landed`,
  or terminal `conflicted` / `regate-failed` — additive, best-effort core NATS exactly like `issue-state`.
  **Single-source schema:** new `core.MergeStateEvent{ID,State,Role,Epic,Commit?,TS}` + `MergeState*`
  state constants (in `core/state_event.go`, the discipline `IssueStateEvent`/`DLQAlert` use); `Commit`
  is set **only on landed** (the new main tip a landed row links to provenance). **Subjects:** new
  `messaging.MergeStateSubject`/`MergeStateWildcard`/`IssueIDFromMergeSubject` mirroring the issue-state
  trio exactly (embedded-separator/wildcard/empty rejected) — a **distinct subject tree** because the
  merge queue is a lifecycle layered over the integrate stage. **Announce:** new
  `o.announceMergeState(issue, state, commit)` (sibling of `announceState` in `transition.go`) — fire-and-
  forget core NATS over `o.nc`, marshal/publish failure logged-not-propagated so the emit can't wedge the
  merge path. **The split between who emits which state is the design point:** the orchestrator-observable
  queue steps (`queued` on `mergeCandidate` entry, `landed`/`conflicted`/`regate-failed` off the `Merge`
  return) are announced by `mergeCandidate`; the merger's *internal* steps (`rebasing`, `re-gating`) are
  announced via a new **`MergeProgress func(state string)`** callback threaded into the `Merger` interface +
  `gitMerger.Merge` (a nil-guarded no-op), which the orchestrator passes as a closure that just publishes —
  so the **git merger stays NATS-unaware**, calling `progress(...)` at the precise rebase/re-gate boundaries
  exactly as it already calls `ReGate`. A fast-forward (main didn't move) correctly emits only `queued →
  landed`. beads/git stay authoritative; these events are never the source of truth (the view's periodic
  backstop reconverges a dropped one). Specs were pre-updated (messaging.md `merge.<id>.state` row + §,
  integration.md "The queue announces itself"); no CLI/config/route/view change ⇒ **no `docs/` change**
  (the merge-queue *view* doc lands with T4.25). Tests: messaging subject round-trip + malformed-reject;
  gitMerger emits `rebasing` (nil-regate, no `re-gating`) and `rebasing→re-gating` (with regate) at the
  unit level; orchestrator end-to-end via `handleResult` — `queued→landed` (fast-forward, landed carries
  commit+role+epic, queued carries none), `queued→rebasing→re-gating→landed` (rebase+passing re-gate),
  `queued→conflicted` (rebase conflict), `queued→rebasing→re-gating→regate-failed` (two-green-branches),
  each asserting subject-id == body-id (the invariant the T4.25 pump relies on). Updated the `fakeMerger`
  + all `gitMerger.Merge` test call sites for the new param. `make check` green (lint 0, **749 pass / 2
  skip**). **Unblocks T4.25** (merge-state SSE pump + merge-queue view, the last of the T4.19–T4.25 batch).
  (needs T3.9, T3.10, T3.11) ([integration.md](specs/integration.md), [messaging.md](specs/messaging.md), [components/orchestrator.md](specs/components/orchestrator.md))
- [x] **T4.25 Merge-state SSE pump + merge-queue view** — *done.* Non-TCB (controlroom). The
  transport+consume half of the merge-queue surface, **the last of T4.19–T4.25 and the final open Phase
  4 task**. **(1) Pump:** `live.StartMergeStatePump(nc, h, *MergeQueue)` (sibling of `StartIssueStatePump`)
  tails `messaging.MergeStateWildcard` (`harness.merge.*.state`), drops a subject that doesn't parse to an
  id or a body that isn't a well-formed `core.MergeStateEvent` (best-effort guards matching the other
  pumps), **records the latest step per candidate into a buffer** and broadcasts the original bytes as a
  `merge-state` SSE event — the same buffer+broadcast shape `StartAgentEventPump` uses for the activity
  feed. **(2) Live buffer (`internal/controlroom/live/mergequeue.go`):** `MergeQueue` — bounded,
  mutex-guarded, **latest-wins per issue id keeping the candidate in its train (insertion) position** (so
  queued→rebasing→re-gating→landed updates in place, never reorders), oldest-evicted when over `max` (the
  earliest-queued/landed rows age out first). It is the read model for the live step because **beads holds
  no per-step state** — the rebase-and-re-gate interval only exists in the typed events (this is *why* a
  buffer, not a beads read, sources the step); in-memory + best-effort like `live.Activity` (a restart
  losing the live shape is harmless — git refs + beads stay authoritative). **(3) Query enrichment:**
  `query.Reader.MergeQueue(ctx, []core.MergeStateEvent) ([]MergeRow, error)` joins each buffered event to
  its beads issue (title/role/spec) via one `ListAll`, preserving event (train) order; a candidate raced
  past/out of beads still renders from the event alone (the step is the point, the title is enrichment).
  `MergeRow` carries `Terminal` (landed|conflicted|regate-failed) + `Failed` (the two interesting terminal
  failures that correlate to a dead-letter/fix issue). **(4) View (`views/merge.templ`):**
  `MergeQueuePage`/`MergeQueueList`/`mergeRow`/`MergeQueueMessage` mirror the DLQ two-handler page+fragment
  pattern; the list sits in `<div hx-ext="sse" sse-connect="/events">` refetching `GET /merge/items` on
  **`sse:merge-state throttle:2s, every 15s`** (the dedicated event, not issue-state — board cadence). The
  state badge is tinted per merge step (sky/amber in-flight, emerald landed, rose failed) and the whole row
  too, so the eye lands on failures; a landed row shows its short-hashed commit + a "Provenance →" link, a
  failed row a "Dead-letter / fix →" link to `/issue/{id}`. Tint class literals live in `.templ` (incl. the
  `mergeRowClass` Go helper, following the `chipClass`/`ledgerStatusClass` precedent) so the Tailwind
  `@source` scanner compiles them. **(5) Server:** `Options.MergeQueue *live.MergeQueue` + `mergeQueue`
  field; `handleMerge`/`handleMergeItems` registered ahead of the placeholder loop, `"merge"` added to the
  `implemented` set; **both endpoints need the buffer *and* the reader** (the step + the title), so nil
  either ⇒ not-attached notice (200) / 503 fragment, mirroring every Reader-backed view. New `merge` nav
  item (`views.NavItems`, between activity and dlq). **(6) Wiring:** `buildRunComponents` builds
  `live.NewMergeQueue(100)`, starts the pump behind `--serve-addr` sharing the hub (unsubscribe joins the
  teardown stack), and threads the buffer into `controlroom.Options`. Ran `make generate` (templ +
  Tailwind). Docs: `docs/control-room.md` views table + liveness note (Merge Queue on `merge-state`).
  Tests: buffer (latest-wins-keeps-position, evict-oldest + re-add, id-less drop, snapshot-is-copy,
  concurrent `Record` under `-race`); pump (round-trip into hub+buffer, malformed-drop-by-ordering,
  stop-unsubscribe over real embedded NATS); query (enrich+order+terminal/failed flags, missing-issue
  renders, empty, ListAll-error); server (full train render incl. steps/commit/provenance+issue links/SSE
  wiring, bare fragment, empty-is-calm, not-attached notice/503). `make check` green (lint 0, **764 pass /
  2 skip**); `-race` clean on the live package. **Completes Phase 4.** (needs T4.24)
  ([control-room.md](specs/control-room.md), [messaging.md](specs/messaging.md))
- [x] **T4.26 Config view** — *done.* The declared factory at rest, made readable (control-room.md "The config
  view"). **Non-TCB** (controlroom read-only; no orchestrator/runner/broker/gate change), human-reviewed.
  `GET /config` — a plain server-rendered page (config is restart-static, so deliberately **not** a feed: no
  SSE, no fragment/refetch — the one Reader-independent data view). **New leaf package
  `internal/controlroom/configview`** owns the projection: it imports `internal/config` + `core` + the T4.6
  `dag` renderer and returns a `ConfigView` of presentation structs (views import it, like `query`/`dag`). It is
  **NOT** a `query.Reader` method — config doesn't come from the beads/git/artifact stores; the validated
  `*config.Config` + env name are threaded into **`controlroom.Options{Config,Env}`** the way
  `BudgetCaps`/`StageOrder`/`SpecDepth` already are (server gains `cfg`/`env`; `run.go` passes `cfg` + a new
  `runOptions.env` from the `--env` flag). `configview.Build(cfg, env)` applies redaction **once** so the
  structured sections and the per-section raw folds agree by construction. **Rendered + raw are one model:**
  each section has a native `<details>` "raw" fold whose body is the **effective config re-serialized +
  redacted** (`yaml.Marshal` of a redacted copy, labeled "effective config (redacted)"), never the file bytes
  — proven by a test that the masked secrets (`nats.url`, artifact path, otel/model endpoints) never appear in
  the raw YAML while kept values (allowlist, image digest, provider) do. **Redaction by allowlist**
  (`redactInfra` masks `nats.url` / model+otel endpoints / artifact path; **keeps** `broker.allowlist` + the
  image/rootfs digests + provider/cost; copies the Models map so the running config is never mutated — pinned
  by a test). **Sections, in flow order:** identity strip (root · `infra.<env>.yaml` · profile · validated✓);
  **pipeline graph** server-side SVG by **reusing the `dag` renderer** — extended minimally with
  **`Edge.Kind`** + **`RenderSVGWith(g, RenderOptions{NodeHref,NodeFill,Label})`** so `produces` (solid) and
  `on_failure` (dashed amber, own marker) edges style apart and stage nodes render anchor-less with a stage-kind
  palette; **`RenderSVG` is exactly `RenderSVGWith(zero)`** so the issue-DAG output is byte-identical (the
  failure marker is added to `<defs>` only when an on_failure edge exists). `roleFlowGraph` drops self-loops
  (`plan→plan`) but keeps cross-stage back-edges (`qa→implement`). Cross-linked each way with the DAG view.
  **stages** table (name/kind/pre/post/`on_failure`/`produces`); **checks** registry (name → command);
  **souls** roster shown **resolved** — `model`→provider+cost, `sandbox`→concrete digest via `ResolveImage`,
  souls ordered by **selector specificity** (mirrors the orchestrator's `selectSoul`: most-specific first,
  empty-selector marked catch-all, stable name tie-break) with personas **linked** (path relative to root), not
  inlined; **policy** (budgets/retries/`tcb_paths`, uncapped shown ∞); **infra** redacted. **Wiring:** `config`
  entry in `views.NavItems`, `/config` handler + `implemented` set ahead of the nav-placeholder loop, nil config
  → not-attached notice (200). `make generate` ran (templ + Tailwind). **Docs:** `docs/control-room.md` views
  table updated. Tests: configview projection (identity/stage-kind labels), soul specificity + resolution,
  redaction (structured + raw-fold no-leak + source-not-mutated), role-flow SVG (produces/on_failure styling,
  no `/issue/` anchor, self-loop drop, back-edge keep, custom fill/label), policy ∞-formatting, nil-config; dag
  (`RenderSVG`==`RenderSVGWith(zero)`, kind-less graph emits no failure styling, role-flow options); server
  (nil-config notice, full render incl. masked-secret-no-leak + nav highlight + not-a-feed). `make check`-adjacent
  green: `go build ./...`, `go vet ./...`, `golangci-lint` (0 issues), `go test ./...` all pass.
  ([control-room.md](specs/control-room.md), [configuration.md](specs/configuration.md))
- [x] **T4.27 Ledger: batched forks, free-text, discuss/defer states + soft approval gate** — *done.*
  **Non-TCB** (controlroom + the trusted planner persona). Extends T4.13/T4.14 without replacing them — the
  planner stays the single source of truth (it re-emits the COMPLETE ```ledger block each turn; the engine keeps
  a latest-wins snapshot). **(1) Four item states:** `normalizeStatus` (`ledger.go`) now maps
  `open`/`agreed`/`discussing`/`deferred` (unknown→open, never errors); `LedgerItem.Answerable()` = open||discussing
  drives which forks invite input and the gate. Views tint each state (agreed→ok, open→warn, discussing→
  `st-progress` "needs you", deferred→`st-idle` muted). **(2) Batched forks:** `Session.Choose` (one canned turn
  per chip) replaced by **`Session.Answer([]ForkAnswer)`** — one user turn enumerating each answered fork by its
  1-based number + question (chip pick / free text / "let's discuss"+note, precedence in that order), so the
  planner attributes unambiguously and reconciles the batch (dropping moot forks). The ledger panel is now ONE
  batch `<form>` POSTing `/create/ledger/answer` (replaces `/create/ledger/select`); each answerable fork renders
  option chips (radios `opt-<i>`), a first-class free-text box (`text-<i>`), and a discuss checkbox+note
  (`discuss-<i>`/`note-<i>`); `parseForkAnswers` (server) collects them against the latest ledger. "Dumb ledger,
  smart planner" — no dependency graph in the engine. **(3) Soft approval gate:** new
  **`wizard.ApprovalDecisions(items) (decisions, blocked)`** — a `discussing` item BLOCKS (returned in `blocked`,
  decisions nil; never auto-deferred — the human's own flag); otherwise plain `open` forks **auto-defer**
  (`autoDeferOpen`) and the converged ledger yields the decisions. Both `handleCreateApprove` and
  `handleResolveApprove` enforce it server-side before committing (`ledgerBlockedMessage` names the flagged
  questions). **(4) Deferred in the sidecar:** `DecisionRecord` gained `Deferred bool`; `FinalizedDecisions` now
  emits agreed forks as decisions AND deferred forks as recorded open items (question only, no option folded); the
  cmd-side `decisionsSidecar` renders them as "Deliberately left open: X" — pre-context for a later
  needs-spec-clarification escalation. **Persona:** the requirements-planner alignment-ledger section rewritten
  for the four states, batched independent forks, the `Here are my answers to the open forks:` enumerated-answer
  format, marking `discussing` when flagged, and dropping moot forks. **Docs:** `docs/control-room.md` wizard
  section updated (four states, batched/free-text/discuss, soft gate, deferred sidecar). `make generate` ran
  (st-progress/st-idle tints compiled). Tests: `normalizeStatus` 4 states + unknown→open, `Answerable`,
  `ApprovalDecisions` (discussing blocks / open auto-defers + no source mutation), `FinalizedDecisions` with
  deferred, `Answer` batch (enumerated turn + chip/text/discuss + out-of-range skip), server
  `/create/ledger/answer` round-trip + panel renders the new inputs, and the soft-gate approve (discussing blocks
  Seeder, open auto-defers into the Seeder's decisions). lint 0, **785 pass / 2 skip** (the runner full-suite
  timeout is the documented NATS-teardown flake; runner passes in isolation in 0.08s). **Completes Phase 4.**
  (needs T4.13, T4.14; touches T4.15's sidecar consumer)
  ([control-room.md](specs/control-room.md), [specs-process.md](specs/specs-process.md))
- [x] **T4.28b Configurable exploration depth + live "it's working" progress** — *done.* Two follow-ons to
  T4.28's read-only codebase exploration, both needed before the wizard is usable for a *deep* exploration on a
  real demo (the vault). **(1) Config knobs on `requirements_planner`:** the previously-hardcoded
  `maxToolTurns` (read-only exploration round-trips per human turn) and the per-turn wall-clock timeout are now
  config — new `RequirementsPlanner.MaxToolTurns int` + `TurnTimeout Duration` (`max_tool_turns`/`turn_timeout`
  YAML), wired through `cmd/harness` via new `wizard.WithMaxToolTurns`/`WithTurnTimeout` options; `0` falls back
  to the wizard defaults (`defaultMaxToolTurns=16`, 5m). **They must be raised together** — a high tool-turn cap
  is moot if the clock cuts the turn short — documented on both the struct and in `docs/configuration.md`.
  Validation (`validateRequirementsPlanner`) rejects negative values at the `harness validate` gate. **(2) Live
  progress:** the activity line now shows a client-ticked elapsed `mm:ss` clock (new `Session.TurnElapsed()`
  anchored off `turnStarted`, ticked browser-side by `wizardElapsed()` in the new `assets/static/wizard.js` so a
  slow turn visibly counts up — the "working, not hung" signal — without per-second server renders) and a running
  per-turn read counter (`Session.readCount`, surfaced in the `tool` status line as "… · N read"). The shared
  `wizard.js` also carries the sticky auto-scroll + Enter-to-send composer ergonomics. **(3) Demo tuning:** the
  vault `requirements_planner` is pointed at `deepseek-v4-flash` (1M context) with `max_tokens: 16384`,
  `max_tool_turns: 40`, `turn_timeout: 10m` so it can read the vault codebase deeply; the persona gains a
  "Grounding in the existing codebase" section (read-before-asserting, verify-links-by-reading) and stricter
  JSON-emission rules for the ledger/draft blocks. **(4) Spec lead for T4.29:** `control-room.md` "The alignment
  ledger" now asserts the planner emits structured state as **tool calls, not parsed prose** — a deliberate
  spec-ahead-of-code assertion that **T4.29** (next) brings the implementation in line with. Tests: config
  validation (valid tuning, negative `max_tool_turns`/`turn_timeout` rejected), `WithMaxToolTurns` caps the
  exploration loop end to end (a tool-call-looping model is dispatched exactly N times then concludes with the
  cap-exceeded error), and `TurnElapsed` is 0-when-idle / nonzero-while-running. `make check` green (lint 0,
  **916 pass / 2 skip**). Docs: `docs/configuration.md` planner field reference. (needs T4.28)
  ([control-room.md](specs/control-room.md), [configuration.md](specs/configuration.md))
- [ ] **T4.29 Wizard structured output via tool calls (replace fenced blocks)** — **Non-TCB** (controlroom +
  the trusted planner persona). Migrate the requirements planner's structured output — the alignment ledger
  (T4.13/T4.27) and the draft (T4.14) — from **parsed fenced ` ```ledger `/` ```draft ` blocks** to
  **schema-validated tool calls** (`update_ledger`, `propose_draft`), the mechanism the rest of the model layer
  already uses (and that the test author uses for the `trace_test` map, verification.md). **Spec leads code:**
  `control-room.md` "The alignment ledger" already asserts this design ("The planner emits structured state as
  tool calls, not parsed prose"); this task brings the implementation in line, closing a deliberate
  spec-ahead-of-code gap. **Why:** robustness — the schema is enforced at the model boundary, so the
  "`fence present but did not parse`" failure class (real, and worse on a small model like deepseek-flash) is
  rejected there instead of silently mis-parsed; and simplification — it **deletes** the bespoke parser
  (`cutFencedBlock`/`cutLedgerBlock`/`displayProse` fence-stripping, `parseLedger`'s three-way lenient fallback,
  the smart-quote normalization) and ~80 lines of JSON-protocol prose from the persona. **Design fulcrum —
  output tools vs action tools:** `update_ledger`/`propose_draft` are *output* tools (pure structured state),
  distinct from the read-only exploration *action* tools (T4.28). The `converse` loop continues to iterate **only**
  on an exploration call; output-tool calls are harvested latest-wins (matching today's snapshot semantics) and
  ride the terminal message, so they **never add a model round-trip** — the only bookkeeping is synthesizing
  `ToolResult` acks so the next human turn's history is well-formed. **Streaming simplifies:** text content
  becomes pure prose, so the `delta` broadcast no longer needs `displayProse()` to scrub fences. **Scope:** two
  `model.ToolDef` schemas (encode the four-state enum; keep `normalizeStatus` as cheap belt-and-suspenders since
  the schema enforces shape, not that a weak model respects the enum); the `converse` action-vs-output branch +
  ack bookkeeping; `json.Unmarshal(call.Args, …)` into the existing `LedgerItem`/`Draft` structs; delete the dead
  parser; rewrite the **`modeltest`** fixtures (the bulk of the effort) to script `tool_calls` in the OpenAI
  streaming wire format instead of fenced-text replies. `tool_choice` stays **auto** (you cannot force a specific
  tool *and* allow prose + exploration in one turn — the robustness win is schema-validated args, not forcing the
  call). **De-risk first:** spike provider tool-calling reliability on **deepseek-flash via openai-compat
  (OpenRouter)** before deleting the parser — if flash can't reliably call tools, that is itself evidence to keep
  the guard-railed prompt path until back on a stronger model. **Docs:** no CLI/config/route change; the mechanism
  is internal, so `docs/` likely needs no change (confirm the `docs/control-room.md` wizard section doesn't
  describe the fenced blocks). (needs T4.13, T4.14, T4.27, T4.28) ([control-room.md](specs/control-room.md),
  [models.md](specs/models.md), [verification.md](specs/verification.md))

## Phase 5 — Production isolation & distribution

Replaces the bootstrap stand-ins (Docker, in-process NATS, local-repo push, files
store) with the production stack.

**Prioritization (decided 2026-06-04): the Firecracker backend (T5.2) is the *lowest*
priority of Phase 5 — moved to the end of the list below.** Every other Phase-5 task
(and all of Phase 6) is buildable and testable in this dev environment: the vsock
transport landed against a real loopback (T5.1), the base images, package mirror,
scoped secrets, distributed NATS, S3 store, and signing are ordinary Go/infra work, and
Docker remains a satisfactory `Backend` for development and human-reviewed runs.
Firecracker alone needs **KVM / bare-metal-or-nested-virt hardware that this environment
does not provide**, so it can be neither exercised nor verified here — building it now
would mean shipping an untested microVM backend ahead of the substrate that *is*
verifiable. Nothing depends on it: T5.1 is the only piece Firecracker *needed*, the
remaining tasks target the transport/security/distribution layer the Docker backend also
uses, and Phase 6 is explicitly Firecracker-independent. So the build order is now
**T5.1 (done) → the rest of Phase 5 + Phase 6 → T5.2 last**, switched on when capable
isolation hardware is available. The tasks keep their original IDs (other tasks reference
them); only the *order of attention* changes.

- [x] **T5.1 vsock broker transport** — *done.* The broker now speaks its one-request-per-connection
  protocol over **AF_VSOCK** as well as unix — the transport a Firecracker microVM needs (a microVM has no
  shared filesystem to bind-mount a unix socket into, so vsock is its only route in/out). **The seam was
  already clean** (`sandbox.Endpoint{Network,Address}` threaded runner→sandbox→agent; the agent's broker
  `Client` dials generically), so this is purely additive at two switch points plus a new transport file —
  no interface, runner, or agent-loop change. **New `internal/broker/transport.go`** owns the transport
  interpretation: `parseVsockAddr` splits the `"<cid>:<port>"` convention the `Endpoint` documents (both
  halves required, valid uint32 — unlike a unix path there's no lenient fallback, so a malformed address is
  a hard error before any syscall); `listenVsock` binds via `vsock.Listen(port)` (the listener binds its own
  machine's context id, so the cid half — which only the *dialer* needs, `Host=2` for a guest reaching its
  host runner — is validated but unused on the listen side); `dialContext(network,address)` is the single
  dial-side switch mirroring `Listen`'s, so adding a transport touches exactly these two functions;
  `dialVsock` runs `vsock.Dial` (which has no ctx-aware form) in a goroutine and races the caller's ctx, so a
  canceled invocation abandons a dial to a wedged/gone microVM instead of hanging (a late-landing conn is
  closed, not leaked). **`broker.Listen`** gains the `vsock` case (the unix stale-socket cleanup stays
  unix-only); **`broker.NewClient`** routes its default dial through `dialContext`. **Library:**
  `github.com/mdlayher/vsock v1.3.0` (compiles on all OSes — `!linux` stubs return runtime errors — so the
  package stays portable; the Docker dev/macOS builds are unaffected). **Tested with a real vsock loopback**
  (this Linux dev host has `/dev/vsock` + the `vsock_loopback` module): `TestVsockIntegration` drives the
  *actual* `Server`+`Client` over an AF_VSOCK connection at the `Local` cid (auto-assigned port read back from
  `ln.Addr()`), proving completion/git-push/publish round-trip identically to unix — it `Skip`s where vsock is
  unavailable, so it stays portable. Plus `parseVsockAddr` table (valid + 7 malformed forms, overflow,
  negative), `dialContext` rejects non-unix/vsock, `dialVsock` returns promptly (no hang) on a canceled ctx,
  and the `"<cid>:<port>"` Endpoint string round-trips. Updated `TestListenRejectsUnsupportedNetwork` (vsock is
  no longer rejected; a *malformed* vsock address still is). The Docker backend deliberately **still rejects
  vsock** (`docker.go` requires unix) — vsock is wired by the Firecracker backend (**T5.2**), which constructs
  the `Endpoint{Network:"vsock"}` the runner currently hardcodes as unix. `go vet`/`golangci-lint` clean,
  `go test -race ./internal/broker/` green. (unblocks T5.2) ([messaging.md](specs/messaging.md), [components/runner.md](specs/components/runner.md))
- [x] **T5.3 Rootfs / base-image composition** — *done.* The `go-toolchain` image
  (`deploy/go-toolchain.Dockerfile`) now bakes everything the zero-network gate and the Phase-6
  semantic tools need, **built and verified offline here** (Docker daemon present; `docker build` →
  `docker run --network none`). **(a) Gate tooling:** `golangci-lint` v2.5.0 (T2.14), `gosec` v2.22.9,
  `govulncheck` v1.1.4, `go-licenses` v1.6.0, `gremlins` v0.5.0 — all `go install`ed at build time
  (network at build, never at run), version-pinned for reproducibility, landing on `/go/bin`. The
  **offline Go vuln DB** is mirrored into `/opt/harness/vulndb` (v1 layout: 3 index files + all **3147**
  `ID/*.json`, parallelized via `xargs -P 16`); the image sets `GOVULNDB=file:///opt/harness/vulndb`,
  which the **`make govulncheck`** target now passes via `-db` (`$(if $(strip $(GOVULNDB)),-db
  $(GOVULNDB),)` — falls back to the online default off-image). **Proven:** `make govulncheck` runs
  `--network none` against the baked DB → "No vulnerabilities found" (and correctly counts 13 vulns in
  required-but-uncalled modules); `make lint` → 0 issues; `make license-scan` → exit 0. **(b) Language
  server + manifest:** `gopls` v0.20.0 on PATH; the **`languageId`→server manifest** at the fixed launch
  convention `/etc/harness/language-servers.json`. **Single source of truth:** new leaf package
  **`internal/sandbox/lsmanifest`** (stdlib-only) defines the format (`Manifest`/`Server`,
  `Parse`+validate with `DisallowUnknownFields`, `ResolveExtension`/`ResolveLanguageID`, `ManifestPath`
  const) and **embeds** `language-servers.json`; the Dockerfile `COPY`s the *same* file into the image,
  so the format the Phase-6 tools resolve and the file the image carries cannot drift. Demo scope ships
  the `go`→`gopls` entry only (`.templ`/`.css` ride the text floor, per Phase-6 note). **This unblocks
  T6.1** (the in-sandbox session manager resolves servers from `lsmanifest.ManifestPath`). **Real bug
  found & fixed:** with the tooling now actually running, `go-licenses check ./...` failed on the
  harness's *own* packages ("Unknown license type" — the repo has no LICENSE file); the `license-scan`
  target now `--ignore github.com/Loxstomper/harness` so it enforces the policy that matters (third-party
  dependency licences) rather than failing on the internal module. Specs/docs updated: sandbox.md
  ("Per-language language server" gets the concrete manifest convention; the OPEN narrows from "build &
  publish" to **publish & digest-pinning** — composition is now defined, only registry push + `@sha256`
  pinning remains, which needs registry infra dev lacks), `config/harness.yaml` offline note,
  `docs/getting-started.md` troubleshooting, Makefile qa-gate comments. No image rebuild needed for the
  Makefile/manifest-consumer changes (the Makefile travels in the seeded worktree). The e2e Docker test
  (`HARNESS_E2E_IMAGE`/`go-toolchain` tag) is unaffected. `go vet`/`golangci-lint` clean on the new
  package; `lsmanifest` tests green (embedded-valid, resolve-by-ext/id, parse-rejects table).
  ([components/sandbox.md](specs/components/sandbox.md))
- [x] **T5.3a Harness kernel passes its own `gosec` gate (self-host readiness)** — *done.* The kernel now
  passes `gosec ./...` clean (**0 findings**, 13 justified `#nosec`), so switching on self-hosting no longer
  trips the kernel on the same SAST gate it runs on candidates. The 26 latents T5.3 surfaced were triaged by
  cause, not blanket-suppressed: **real fixes** — 8×G301 dirs `0o755`→`0o750` and 5×G306 files `0o644`→`0o600`
  (artifact store root/shard dirs, jetstream store dir, seed/wizard spec + decisions-sidecar writes; least-
  privilege, and git stores no perms beyond the exec bit so the committed result is unaffected); **justified
  inline `#nosec`** — 6×G204 (the git/bd subprocess helpers in beads/orchestrator/runner/query/wizard: fixed
  binary, `-C`-scoped, trusted harness-built arg lists, never agent input) and 6×G304 (the trusted-path readers:
  artifact `os.Open` off a content-address, operator-supplied config/infra/souls/persona paths, and the
  `spec.go` reader already confined under `rootAbs` by the `filepath.Rel` check above), plus 1×G115 in
  `broker/protocol.go` (the `uint32(len(b))` frame-length cast is bounded by the `maxFrameSize` 64 MiB check
  immediately above, far below `math.MaxUint32`). **Inline annotations, not a config-wide exclusion, by design**
  — they keep the gate live for *new* G204/G304/G115 occurrences and document the per-site reasoning (gosec
  reports `nosec: 13`, all rule-scoped). No test asserted the old modes (the fixtures that write `0o644`/`0o755`
  create their own scratch files); `make check` green (lint 0, **816 pass / 2 skip**). No CLI/config/route/view
  change ⇒ no `docs/` change. **gosec install note:** the host needs `gosec` on PATH to run the gate locally —
  `go install github.com/securego/gosec/v2/cmd/gosec@v2.22.9` (the version T5.3 bakes into the role image);
  in a real run the gate runs it from the baked image, offline. ([verification.md](specs/verification.md), [security.md](specs/security.md))
- [x] **T5.4 Sandbox seeded-worktree ownership** *(carried from Phase 1)* — *done.* Dropped the
  `git config --global --add safe.directory '*'` crutch by fixing the ownership mismatch at its cause.
  **Backend change (`internal/sandbox/docker.go`):** after the `docker cp host/. container:/workspace`
  seed — which preserves the **host** uid/gid on every copied file while the container's exec user is
  root — `Provision` now runs one `docker exec <id> sh -c chownWorktreeCmd`, where
  **`chownWorktreeCmd = chown -R "$(id -u):$(id -g)" /workspace`**. The `$(id -u)/$(id -g)` form chowns the
  tree to **whoever Exec runs as** ("the container user" literally, no uid hard-coded — robust if the image
  later declares a non-root `USER`); the default exec user is root (CAP_CHOWN), so the chown succeeds and the
  worktree owner then matches the process that runs git/`go build`, so git's dubious-ownership guard (exit
  128, which silently broke VCS stamping + the in-sandbox candidate commit) never fires. A failed chown tears
  the container down like a failed seed (fails closed). **Dockerfile (`deploy/go-toolchain.Dockerfile`):**
  removed the `&& git config --global --add safe.directory '*'` line + rewrote the comment block to explain the
  backend now owns this. **Transition-safe:** the chown is in the harness binary (runs regardless of image),
  so it's harmless against the old image (chowning to root is redundant there) and the *fix* against the rebuilt
  one — no lockstep deploy needed. **Verified end-to-end against real Docker:** the busybox integration test
  (`TestDockerSandboxIntegration`) now asserts `.git` is owned by the container's exec user post-provision; a
  manual go-toolchain reproduction with the global `safe.directory` stripped proved `git status` fails (exit
  128) *before* the chown and `git status` + `git rev-parse HEAD` succeed *after* it (`.git` 1000→0). Unit
  tests: `TestProvisionArgShapes` now asserts the 3rd call is the chown `exec`; new
  `TestProvisionChownFailureTearsDown` (run, cp, exec-fail, rm). `go vet`/`golangci-lint` clean (0 issues),
  `go build ./...` green, `go test ./internal/sandbox/` green (incl. real-Docker integration). The Firecracker
  half (seed correct ownership) lands with **T5.2** (hardware-blocked). No CLI/config/route/view change ⇒ no
  `docs/` change. ([components/sandbox.md](specs/components/sandbox.md))
- [ ] **T5.5** *(optional)* gVisor backend (medium-trust). ([components/sandbox.md](specs/components/sandbox.md))
- [ ] **T5.6 Vetted package mirror/proxy** — route package fetches through a pinning/scanning/logging proxy on the broker allowlist; a read-through cache amortizes downloads without weakening egress control. ([security.md](specs/security.md), [components/runner.md](specs/components/runner.md))
- [ ] **T5.7 Scoped short-lived secret minting** — the runner mints a per-task git token scoped to push *only* the task branch, injected for the invocation lifetime and dying with the sandbox (replaces the bootstrap local-repo push). ([components/runner.md](specs/components/runner.md), [security.md](specs/security.md))
- [x] **T5.8 Distributed NATS** — *done.* The code seam for an external NATS cluster, plus the concrete
  JetStream stream definitions that were the **messaging.md OPEN**. **Latent bug closed:** the infra overlay
  already declared `nats.url`, `nats.jetstream.replicas`, and `nats.jetstream.max_age`, but the code **ignored
  all three** — it always started the embedded in-process server (`DontListen`) and hardcoded `streamConfigs`
  (replicas absent ⇒ 1, result max-age 7d), so the dev `url: nats://localhost:4222` was a no-op lie and the
  knobs did nothing. **(1) Concrete, config-driven stream defs:** new **`messaging.StreamOptions{Replicas,
  ResultMaxAge}`** (the only deployment-varying knobs; subjects + retention *policy* stay fixed by harness
  semantics). `streamConfigs(opts)` now applies `Replicas` **uniformly to all four streams** (normalizing <1→1)
  and `ResultMaxAge` to the **result stream only** (work is consume-once; dlq/approvals must survive until a
  human acts, so they stay unbounded; 0→`defaultResultMaxAge` 7d). `SetupStreams(ctx, js, opts)` gained the
  options arg — **every caller in one deployment must pass the SAME options** because `CreateOrUpdateStream`
  reconciles a stream to whatever config it's handed, so the orchestrator's idempotent every-startup re-call
  threads the SAME infra-derived options (new `Orchestrator.streamOptions()`, nil-safe off `opts.Config.Infra`)
  rather than silently resetting replicas/max-age back to defaults. **(2) External-cluster connection:** new
  **`messaging.Connect(url, opts...)`** dials an external server (the location-transparency swap-in for the
  embedded one). `buildRunComponents` now **branches on `cfg.Infra.NATS.URL`**: empty ⇒ embedded in-process
  (the dev/bootstrap default, optionally exposed via `--nats-addr`); set ⇒ `Connect` to that cluster, **no
  embedded server started** and `--nats-addr` ignored (warned). Either path yields the same `*nats.Conn` the
  orchestrator/runner take unchanged. **(3) Semantics decision** — `nats.url` empty = embedded, set = external
  (honors the dev overlay's own documented intent: "the same code distributes later by pointing `url` at an
  external cluster"); **no new config field**. Fixed the dev overlay's misleading `url: nats://localhost:4222`
  → `url: ""` (it was always embedded/`DontListen`). **(4) Validation** — new `validateNATS`: each
  comma-separated `nats.url` endpoint must be a `nats://`/`tls://`/`ws[s]://` URL or bare `host:port`
  (`validNATSEndpoint`); `replicas ≥ 0`; **`replicas > 1` requires a `url`** (the single embedded server is
  single-replica — a guaranteed boot failure otherwise); `max_age ≥ 0`. **Verifiable in dev; the only remainder
  is ops** (standing up a real multi-host cluster + per-host runners is deployment, like T5.3's registry-push /
  T5.2's KVM remainders — not code). Tests: `streamConfigs` option mapping + zero-default fallback;
  `SetupStreams` applies the result max-age override + idempotent re-apply; `Connect` to an external server
  (full stream + work round-trip over TCP) + unreachable-dial error; `validateNATS` valid/reject table +
  `validNATSEndpoint` grammar; `buildRunComponents` external-NATS path (connects to a separate server, both
  loops run + shut down clean). `make check` green (lint 0, **837 pass / 2 skip**). Specs/docs: messaging.md
  OPEN replaced with a concrete "Stream definitions" section (table + replicas/max-age knobs + consumer
  configs); configuration.md NATS field reference; dev overlay comments. ([messaging.md](specs/messaging.md), [configuration.md](specs/configuration.md))
- [x] **T5.9 S3/MinIO artifact backend** — *done.* The distributed-deployment artifact store: a second
  `artifact.Store` implementation over plain S3 (`internal/artifact/s3.go`, `S3Store`) so runners on many
  hosts and the control room share one bucket, the same Store contract the files backend serves on one host.
  Speaks plain S3 via **minio-go v7.2.0**, so it serves AWS S3 and any S3-compatible service (MinIO is the
  dev test target) identically. **Single-source address layout:** extracted the files backend's hash
  validation + sharding into a shared **`storeKey(hash)`** (`store.go`) returning `sha256/<ab>/<rest>` — both
  backends now derive object paths/keys through it, so their content-address layout *and* their rejection of
  a malformed (untrusted) hash can never drift; `FilesStore.pathFor` delegates to it. **S3Store:** `Put`
  buffers content to a temp file while hashing (the key IS the hash, unknown until fully read, and minio needs
  the exact size up front — a temp file also keeps a multi-MB transcript off the heap, mirroring the files
  backend), StatObjects for idempotent dedup, then PutObject; `Get` StatObjects first so a missing key is
  `ErrNotFound` *here* (minio's GetObject defers the request, which would otherwise hide not-found behind the
  reader); `Has` maps NoSuchKey→false. `isNotFound` maps S3 NoSuchKey/NoSuchBucket/404 to the `os.ErrNotExist`
  analog. **Network-free constructor** (`NewS3Store`): `minio.New` only builds a client (dials lazily), safe
  in the network-free composition root like the OTLP exporter — a missing bucket/unreachable endpoint surfaces
  on the first best-effort harvest, not at boot. Endpoint accepts an optional `http://`/`https://` scheme
  (http = plaintext dev MinIO; bare host = TLS); empty endpoint derives `s3.<region>.amazonaws.com`.
  **Credentials from the environment** (`credentials.NewEnvAWS`: `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/
  `AWS_SESSION_TOKEN`), never config — the model-API-key posture. **Config:** `ArtifactsConfig` gains
  `Bucket`/`Endpoint`/`Region`; `artifact.Open`'s s3 case now constructs the store (was a "not implemented"
  stub); new **`config.validateArtifacts`** (wired into `Validate`) catches an s3 backend with no
  bucket or no endpoint/region at the `harness validate` gate before a store is built (the files path stays an
  Open-time check). **Verified end-to-end against real MinIO in Docker:** `TestS3StoreIntegration` boots a
  throwaway `minio/minio` container on a free loopback port, creates the bucket, builds the store via the
  production `Open` path (creds from env), and drives the full contract — content round-trip, dedup
  (same bytes→same key, no re-upload), distinct content, `ErrNotFound`, malformed-hash rejection (the
  traversal guard fires before any S3 call), and a 5 MiB streamed upload — skipping cleanly when Docker/the
  image is unavailable. Plus `Open`-path unit tests (s3 opens with endpoint or region; fails with neither / no
  bucket) and `validateArtifacts` table tests. Deps: minio-go + transitive (go.mod/go.sum, `go mod tidy`).
  `go build`/`go vet`/`golangci-lint`/`gosec` clean; full unit suite green. Docs: `docs/configuration.md`
  (artifacts bullet + commented s3 example) + spec `artifact-store.md` (config example, plain-S3/shared-layout/
  env-creds/bucket-prerequisite notes). The store is pluggable by config — dev runs `files`, production runs
  `s3`, no code change. ([components/artifact-store.md](specs/components/artifact-store.md))
- [x] **T5.10 Provenance signing + key custody** — *done.* Closes the security.md OPEN ("Signing scheme /
  key custody for provenance trailers — TBD"). The harness-authored provenance commit (the trusted commit
  on main, the *only* place a trailer the trusted layer vouches for can live) is now **SSH-signed with the
  harness identity and verified on read**, so "the audit trail is the accountability" is cryptographic, not a
  forgeable plaintext trailer. **Scheme decision: git-native SSH signing** (`gpg.format=ssh`) — no GPG
  keyring/daemon, verification is a public-key check against an **allowed-signers file** anyone can hold;
  chosen over GPG/Sigstore for zero new runtime deps (git+ssh-keygen, already present) and offline
  verifiability. **Sign side (single chokepoint):** the merger's lone `commit-tree` call
  (`internal/orchestrator/merge.go`) gains `-c gpg.format=ssh -c user.signingkey=<key>` + `-S` when a key is
  configured; `gitMerger.signingKey` is set via new **`NewGitMerger(bin, ...MergerOption)` / `WithSigningKey`**
  (variadic, so the existing `NewGitMerger("")` test/seam call sites are unchanged, and an empty key is a no-op
  so callers pass it unconditionally). Only the trusted top commit is signed — the agent's candidate commits
  beneath it are never signed (untrusted by construction); the rebase replay is left unsigned. **Verify side:**
  `query.GitProvenance` gains **`WithAllowedSigners(path)`**; when set, `Recent` folds git's **`%G?`** verdict
  into the SAME `git log` call (the `-c` overrides + an extra `%G?` field — no per-commit git invocation) and
  maps the code to a new **`query.SignatureStatus`** (`G`→verified, `N`→unsigned, `U`/`B`/`E`/…→untrusted) on
  `MergedCommit.Signature`. The provenance view renders a tinted badge (signed / unsigned / unverified;
  **unverified** = signed by an unrecognized key, flagged distinctly as the one alarming state); unchecked (no
  allowed-signers configured) renders no badge, so an unsigned deployment's view is unchanged. **Config:** new
  **`config.SigningConfig{Enabled,Key,AllowedSigners}`** on `Infra` + `Active()` (enabled && key); `validateSigning`
  gates only the run-breaking shape (`enabled` with no `key`) — the key is a **runtime-provisioned secret**
  (API-key posture: referenced by path, never committed/baked, existence NOT stat-checked at validate time; a
  missing key fails loudly on first merge). **Key custody:** dev = an operator-supplied key file (disabled by
  default in `infra.dev.yaml`, commented example); production = secret-manager/ssh-agent delivery to the
  orchestrator host — the deployment remainder, like T5.8's cluster / T5.9's bucket. **Artifacts need no separate
  signing:** every artifact the trailer cites is content-addressed and the hashes live in the signed commit, so
  the signature transitively authenticates them (single source of truth, no parallel mechanism). **Wiring:**
  `cmd/harness` threads `signingKey(cfg)` into the merger and `cfg.Infra.Signing.AllowedSigners` into the reader.
  **Verified end-to-end against real git + ssh-keygen** (`TestGitMergerSignsProvenanceCommitIntegration`: signed
  merge → `%G?`=G + `git verify-commit` passes; unsigned merge → `%G?`=N), plus fast unit tests (the exact
  signing argv the merger builds; `%G?`→status mapping; `Recent` folds verification in only when configured;
  validateSigning/Active table; the view badge across all four states). `make check` green (lint 0, **904 pass /
  2 skip**). Specs/docs: security.md OPEN resolved + new "Signing the provenance commit" section, integration.md
  (signed provenance commit), configuration.md + control-room.md + specs/control-room.md (signing block + signature
  badge). TCB-touching (orchestrator merge path), human-reviewed. ([security.md](specs/security.md), [integration.md](specs/integration.md), [configuration.md](specs/configuration.md), [control-room.md](specs/control-room.md))
- [ ] **T5.11** *(optional)* Warm sandbox pools + HA orchestrator via NATS-KV leader election. *(OPEN.)* ([components/runner.md](specs/components/runner.md), [components/orchestrator.md](specs/components/orchestrator.md))
- [ ] **T5.2 Firecracker sandbox backend** ***(lowest priority — see the Phase 5 prioritization note above; deliberately last)*** — a KVM-microVM backend implementing the `Backend`/`Sandbox` interface: rootfs seeding, vsock I/O (T5.1, done), resource limits incl. disk, deterministic teardown. The production isolation target. **Blocked on hardware, not on code:** needs KVM (bare-metal or nested virt) that the dev environment lacks, so it cannot be built-and-verified here — do it only once such hardware is available, after the rest of Phase 5 + Phase 6 (all of which the Docker backend supports for dev/human-reviewed runs). Kept as ID T5.2 (referenced elsewhere) despite its end-of-list position. (needs T5.1) ([components/sandbox.md](specs/components/sandbox.md))

## Phase 6 — Agent semantic tooling (LSP)

Replaces the agent's text-only `search`/`edit_file` floor with **intent-first, LSP-backed**
comprehension and transformation tools: the agent states *what* it wants (find this symbol,
rename this) and the trusted tool layer resolves it **LSP-first with a text fallback** — it
never picks the mechanism, so "prefer semantic, fall back to grep/sed" is a structural property,
not a persona nudge. The surface is **language-neutral**; the backing server is per-language,
resolved from the sandbox image (T5.3) — the same canonical-interface / thin-adapter split the
model layer uses (*provider adapter : model :: language server : semantic tool*). Buildable and
testable in dev against a `go-toolchain` image carrying `gopls`; **does not depend on Firecracker**
(T5.2). New spec contract: [components/agent.md](specs/components/agent.md) "Semantic tools
(LSP-backed)" + [components/sandbox.md](specs/components/sandbox.md) "Per-language language server".
**Demo scope (Go+htmx+templ+tailwind):** ship the `go` (gopls) `languageId` entry only — `.templ`
and `.css` ride the text floor (templ compiles to `_templ.go`, which gopls already sees; tailwind
is a build step, not a navigable language).

- [x] **T6.1 Per-language LSP session manager** — *done.* The warm, per-invocation language-server
  session manager that the comprehension/transformation tools (T6.2/T6.3) will query, **verified end-to-end
  against real gopls** in the `go-toolchain` image. Three layers, cleanly split:
  **(1) Streaming sandbox primitive** — a new **optional** capability `sandbox.SessionOpener`
  (`OpenSession(ctx, Command) (SessionStream, error)` + `Workdir()`), the streamed sibling of `Exec`: a
  long-lived in-sandbox process with stdin/stdout attached, so a stdio LSP server runs for the whole
  invocation instead of cold-starting per call. It is *not* a new boundary hole — the server still runs
  **inside** the sandbox, host never reaches in (the property `Exec` already has). Docker implements it via
  `docker exec -i` with pipes (`realDockerSession`/`dockerSession`, stderr drained to a capped buffer; the
  process is bound to its own cancel, NOT the launch ctx, so it lives until `Close` or the wall-clock
  watchdog reaps the container; `OpenSession` refuses past the wall budget like `Exec`). It is **optional**
  by design (a separate interface, not a `Sandbox` method) so no test fake or future backend is forced to
  implement it — a sandbox without it leaves semantic tools degrading to the text floor. **(2) `internal/lsp`**
  — a stdlib-only JSON-RPC 2.0 client over LSP `Content-Length` framing: one reader goroutine demuxes
  responses (by id) from notifications (`publishDiagnostics` cached + waiter-signaled) and **answers
  server→client requests with defaults** (`workspace/configuration` → array of nulls, else null) so gopls
  never stalls. Typed methods: initialize/initialized, didOpen/didChange/didSave, definition, references,
  implementation, hover, documentSymbol, workspace/symbol, rename, codeAction, diagnostics, shutdown/exit —
  each normalizing the wire's shape variants (Location|Location[]|LocationLink[]; hierarchical
  DocumentSymbol[] vs flat SymbolInformation[]; the three Hover content forms; `changes` vs
  `documentChanges`). Generic and sandbox-unaware — *provider adapter : model :: language server : semantic
  tool*. **(3) `agent.Sessions`** — the manager owning the sandbox/worktree specifics: reads the **image's**
  baked manifest via `Exec cat /etc/harness/language-servers.json` (`lsmanifest.Parse`, so the file the image
  carries drives resolution, not an embedded copy), **lazy-launches** one server per `languageId` on the
  first semantic call (`gopls serve`), `initialize`s with `rootUri`/`file://` URIs built from `Workdir`,
  `didOpen`s a file with current disk content before a query, and exposes the full op set (Definition/
  References/Implementation/Hover/DocumentSymbol/WorkspaceSymbol/Diagnostics/Rename/CodeAction) the T6.2/T6.3
  tools wrap. **Edit coupling (by design, not a bolt-on):** `WorkspaceTools(sb, notifier)` now threads an
  `editNotifier`; `write_file`/`edit_file` call `Sessions.NotifyEdit` after a successful write, which
  `didChange`s a *running* session (best-effort, never fails an edit). It deliberately does **not** launch on
  an edit — lazy launch is the first *semantic* call; a server launched later reads fresh disk at `didOpen`,
  so a pre-launch edit needs no notification. `ErrNoSemanticSession` is the uniform degrade signal (no opener
  / no manifest / no entry). **Lifecycle:** `ToolSource` now returns a `cleanup func()` the loop defers;
  `run.go` builds a `Sessions` per invocation and returns `sessions.Close` (orderly `shutdown`/`exit` then
  stream close), the sandbox teardown being the backstop. **Tests:** `internal/lsp` (handshake, every query
  shape, async diagnostics, server-request reply, transport-death fail-fast; `-race`); `agent` sessions
  (degrade-without-opener, no-manifest, unknown-ext, lazy-launch-once, didOpen-on-query, edit→didChange +
  new-file→didOpen, notify-before-launch-is-noop, diagnostics, idempotent Close — all over an in-memory
  scripted server, `-race`); sandbox (`OpenSession` arg-shape, post-wall rejection, real `docker exec -i cat`
  round-trip); and **`TestSessionsRealGopls`** (skips unless docker+git+`go-toolchain:latest`): launches real
  gopls, asserts documentSymbol finds `greet`+`main`, diagnostics round-trips, and **after `NotifyEdit` a
  fresh documentSymbol sees the newly-added `extra` — proving the sync coupling against a real server**.
  `make check` green (lint 0, **859 pass / 2 skip**). No CLI/config/route/view change ⇒ no `docs/` change.
  **Unblocks T6.2/T6.3** (the tools are now thin wrappers over `Sessions` + the text-floor fallback policy).
  ([components/agent.md](specs/components/agent.md), [components/sandbox.md](specs/components/sandbox.md))
- [x] **T6.2 Comprehension (read) semantic tools** — *done.* Six canonical, intent-first read tools —
  `find_symbol`, `references`, `definition`, `implementation`, `hover`, `diagnostics` — as **thin wrappers
  over the T6.1 `Sessions`** (`internal/agent/semantic.go`, new `SemanticReadTools(*Sessions) []Tool`),
  appended to the per-invocation tool set in `run.go` between `WorkspaceTools` and `LifecycleTools` (they
  capture the same `*Sessions` that already backs the edit tools, so semantic queries see edits via the T6.1
  didChange coupling). Non-TCB (agent tool surface), human-reviewed. **Reads degrade *silently* to the text
  floor (the structural property, not a persona nudge):** every tool calls the semantic engine first; on
  **any** error (no opener / no manifest / no language entry — i.e. `ErrNoSemanticSession` — *or* a server-side
  failure) it falls back to a `grep -rnE` over the worktree, prefixing the output with an explicit
  **`[unverified: no language server available; showing text matches]`** banner so the model never mistakes a
  text match for a semantic result (worst case = today's `search`). `find_symbol` greps the whole symbol name
  (`\b<name>\b`, `regexp.QuoteMeta`); the **position-anchored** tools (`references`/`definition`/`implementation`/
  `hover`) read the file, extract the **identifier at the position** (`identifierAt`, rune-aware, treats a caret
  one-past-the-token as inside it) and grep *that* — an honest best-effort when only a position is known; a
  position not on an identifier returns a recoverable error rather than guessing. `diagnostics` has **no** grep
  equivalent (type-checking isn't a text search), so its degrade is an `[unverified]` note steering the model to
  `run` the build/tests — deliberately annotated `//nolint:nilerr` (a missing server is a degrade, not a fatal
  fault). A grep that *cannot run at all* (sandbox broken) stays **fatal** (the runner redelivers), distinct
  from "no matches". **Positions are 1-based line+column on the tool surface** (matching what `find_symbol` and
  `search`/`grep -n` print, so a location from one tool feeds straight into the next); the layer translates to
  the session's 0-based LSP positions and back (`line0`/`char0`, `+1` on render). `find_symbol` defaults
  `language` to `"go"` (demo scope — ship the `go`/gopls entry only). **Formatting:** locations as
  `path:line:column` (worktree-relative via `relForURI`, which inverts the session's `file://<root>/<rel>` URI),
  symbols as `Kind Name — path:line:col (detail)` (`symbolKindName` maps the LSP SymbolKind 1..26 enum, unknown
  ⇒ `Symbol`), diagnostics as `path:line:col: severity: message [source]` (`severityName`). **Text-floor
  honesty:** `search`'s description now states it is a plain-text floor and points at `find_symbol`/`references`/
  `definition` for code symbols (the spec's "honest descriptions steering toward the semantic tools"). The spec
  contract (agent.md "Semantic tools (LSP-backed)") was pre-written and matched as-is; no CLI/config/route/view
  change ⇒ no spec/docs edit. Tests (`semantic_test.go`): tool-set + valid JSON schemas; **semantic success**
  for each tool against the in-memory scripted LSP server (extended the shared `fakeLangServer` with
  `workspace/symbol`/`references`/`hover` cases) asserting 0→1-based translation, relative paths, kind labels,
  and *no* unverified banner; empty-result-is-not-an-error (`implementation`); arg validation (missing
  name/path/line/character, 1-based floor); **silent degrade** (no-opener sandbox) for `find_symbol` (bounded
  name grep) and `references` (identifier-at-position grep), non-identifier-position recoverable error,
  diagnostics-points-at-`run`, and grep-exec-error-is-fatal; plus unit tables for `identifierAt`/`relForURI`/
  `symbolKindName`. `make check` green (lint 0, **875 pass / 2 skip**). **Unblocks T6.3** (the write tools —
  `rename`/`code_action` — apply a `WorkspaceEdit` and degrade *loudly*; they reuse `relForURI`/the position
  decoding here). (needs T6.1) ([components/agent.md](specs/components/agent.md))
- [x] **T6.3 Transformation (write) semantic tools** — *done.* Two intent-first write tools —
  **`rename`** (project-wide) and **`code_action`** (the server's own fixes — organize imports,
  quickfix, extract) — as thin wrappers over the T6.1 `Sessions` (`internal/agent/semantic_write.go`,
  new `SemanticWriteTools(*Sessions, *TransformLedger)`), appended to the per-invocation tool set in
  `run.go` between `SemanticReadTools` and `LifecycleTools`. Non-TCB (agent tool surface), human-reviewed.
  **Writes degrade LOUDLY (the structural inverse of the reads' silent degrade):** `rename` calls the
  language server first, applies the precise **`WorkspaceEdit`** it returns, and records a **semantic**
  mechanism; with no server (or a server refusal) it falls back to a **word-boundary text rename** —
  *performed* (word boundaries avoid the substring-corruption class structurally) but flagged with an
  explicit `[unverified: … TEXT rename …]` warning carrying match count, files, and a **heuristic count of
  hits inside comments/string literals** (`riskyMatch` — a single-line quote/`//` scan, reported as
  heuristic) and recorded as a **text** mechanism — never a silent `sed`. `code_action` has **no text
  floor** (no grep equivalent for "organize imports"), so with no server it **refuses loudly** (`IsError`).
  **WorkspaceEdit application (the shared core):** `(*Sessions).applyWorkspaceEdit` writes each changed
  file via the existing `writeFile` and re-syncs it into the running session via `NotifyEdit` (so a
  follow-up `diagnostics`/`references` reads the new text — the T6.1 coupling extended to writes);
  `applyTextEdits` splices a document's `TextEdit`s in **descending start order** (non-overlap invariant ⇒
  earlier offsets stay valid), and `positionToOffset` does **UTF-16-aware** column→byte translation (LSP
  columns are UTF-16 code units). A read/write fault mid-apply is **fatal** (broken sandbox, runner
  redelivers), matching the text-floor edit tools. **Mechanism recorded in evidence (the spec's "Mechanism
  is recorded"):** new **`core.Result.Transforms []TransformRecord`** (tool/target/mechanism/files/edits/
  note) + mechanism constants `TransformMechanism{Semantic,Text}` + artifact kind
  **`ArtifactKindTransformLog`**. A shared per-invocation **`agent.TransformLedger`** (nil-safe, the
  write-side analog of the lifecycle's proposal/trace accumulators) is built in `run.go`'s `toolSource` and
  handed to **both** `SemanticWriteTools` and `LifecycleTools(brief, brk, ledger)` (signature extended) —
  the write tools `Record` into it, the terminal `submit`/`submit_plan`/`escalate` fold `ledger.take()`
  into the Result. The runner **harvests** `res.Transforms` to the artifact store as a transform-log
  (new `formatTransformLog`, stable/deterministic like the traceability map) and **clears the structured
  form** so it travels by hash — same pattern as `Result.Trace`. **code_action selection:** omit
  title/kind to **list** the offered actions; pass `title` (exact/substring) or `kind` (exact/prefix) to
  apply the single match; a command-only action (no inline edit) is reported, not executed (the protocol
  keeps server `Command`s opaque); range defaults to the whole file (for organize-imports) or the given
  position. **Verified against REAL gopls** — `TestSessionsRealGopls` gained a step 4 that writes an edited
  body to disk, renames `greet → welcome` via gopls, applies the WorkspaceEdit, and asserts every reference
  (decl + call) became `welcome` on disk (proving `applyWorkspaceEdit` consumes real server output). Unit
  tests (`semantic_write_test.go`, scripted in-memory LSP server extended with `rename`/`codeAction`):
  tool-set + schemas, semantic rename (applies edit + re-syncs didChange + records semantic), arg
  validation (incl. invalid identifier), **loud text degrade** (rewrites code+string+comment, 3 edits / 2
  risky, text mechanism), non-identifier-position recoverable error, grep-fatal, code_action apply-by-kind,
  list-when-no-selector, command-only refused, no-server loud refusal, and `submit` folds the ledger; plus
  unit tables for `applyTextEdits`/`positionToOffset`/`riskyMatch`/`isIdentifier`/`selectAction`/`utf16Len`;
  runner `formatTransformLog` + harvest-stores-transform-log. The spec contract (agent.md "Semantic tools",
  "Mechanism is recorded") was **pre-written and matched as-is**; no CLI/config/route/view change ⇒ no
  spec/docs edit. `make check` green (lint 0, **895 pass / 2 skip**). **Completes Phase 6.** **Deferred
  (filed, not blocking):** a control-room surface for the transform log (a verification-view row weighing
  text-fallback renames) — the record is now harvested and reachable on Evidence; only the read/render is a
  follow-up, like T4.7b surfaced the transcript. (needs T6.1, T6.2)
  ([components/agent.md](specs/components/agent.md))

---

## Open decisions affecting the plan

These are still `OPEN:` in the specs and may reshape tasks above. (Decisions once open
here — mutation threshold, gate fail-fast, `integrate` ownership, the condition-expression
language — are now recorded in the specs they informed, not duplicated here.)

- HA orchestrator: single instance (fine for v1) vs. leader election (T5.11).
- Rootfs / base-image composition per role (T5.3).
- Exact module set in the TCB boundary — operationally the `policy.tcb_paths` globs (T2.10);
  the concrete list must still be reviewed and pinned before autonomy is switched on for harness work.
