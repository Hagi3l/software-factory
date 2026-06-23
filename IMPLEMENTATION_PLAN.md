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

**Phases 2, 3, 4, 6, and 7 are complete; only Phase 5 has live work, and all of it is
optional or hardware-blocked.** Phase 2 (independent verification) is complete — only
the optional T2.11 decision-note remains (resolved as configuration, not a build item).
Phase 3 (full DAG, decomposition, merge queue) is complete (T3.13 and T3.14 landed). Phase 4
(control room + Create/Resolve wizard) is complete. Phase 6 (agent semantic LSP tooling)
is complete (T6.1–T6.3). Phase 7 (atomic feature integration / epic mode) is complete
(T7.1–T7.8; the vault demo now runs `integration.mode: epic`). The only remaining
*engineering* of new substrate is Phase 5 (production isolation & distribution), and within
it every still-open item is either **optional** (T5.11 warm pools + HA orchestrator) or
**hardware-blocked** (T5.2 Firecracker, needs KVM the dev box lacks). T5.5 (gVisor backend)
is now **done** — and landing it wired up the previously-missing config→backend selection,
so `sandbox.backend` is finally honored at startup (firecracker fails closed rather than
silently degrading to Docker).

**Phase 8 (demo-hardening) is complete** (T8.1–T8.7 all landed). The 2026-06-18 live vault-demo run surfaced read-model
consistency bugs (scheduler + control room read beads directly) and a planner over-bundling cost
issue. The **read-model spine is complete** (T8.1–T8.4, T8.6): the orchestrator's work-graph
projection is now the authoritative live read model — the scheduler dispatches off it (T8.2), it is
durable-stamped + correctly rolled up (T8.3), and the control room's live work-state views read it
instead of polling beads (T8.4). The cost-driver fix has also landed: **T8.5** hardened both
decomposition-planner personas to require single-concern children (closing the spec↔persona gap —
the spec already mandated it). **T8.7** (wizard one-root-in-epic hardening) has landed — Phase 8
is now complete. See Phase 8 below,
[`demo-run-issues.md`](demo-run-issues.md), and [`REMEDIATION_PLAN.md`](REMEDIATION_PLAN.md).

**Phase 9 (structured check findings) is complete** (opened + built 2026-06-22) — from a context-management
design pass: every gate check becomes a thin per-tool adapter emitting compact, structured
`Finding`s, so raw `go test`/scanner dumps stop blowing the agent's context window and burying
the signal — the same failure mode (a flailing agent against a growing, noise-filled history)
that drove ~80% of the 2026-06-18 demo-run cost. The **spec landed first** (commit 653824a); the
code is landing incrementally: **T9.1 (core.Finding + tri-state), T9.2 (`internal/gotest` parser),
T9.3 (`run_tests` tool), T9.4 (build precondition + tri-state cascade), and T9.6 (scanner adapters)
and T9.5 (gate verdict carries findings + verification view renders them) are done** — the parsers
built in parallel worktree subagent waves (T9.2+T9.6, then T9.3+T9.4), T9.5 wired sequentially;
and T9.7 (`run_gate` full pre-submit self-check, sharing the gate's adapters via the new
`internal/checkfindings` leaf) are done. **Phase 9 is complete** — T9.8 (the failure-aware retry
Brief threading the parsed findings into the `on_failure` fix issue) landed, closing the loop the
phase was opened for: structured findings now flow from the gate into the agent's context (run_tests/
run_gate), the verdict, the verification view, and the retry. **Next: validate the context/cost
reduction on a `./demo/vault` re-run** (the exercising use case), and consider the deferred
refinements below (severity-threshold grading, progress-based termination, finding-type routing).
**`./demo/vault` is the exercising use case** — it is Go (the shipped
adapters' language), its `qa` gate already runs gosec/govulncheck/license-scan on agent output,
and its inner loop runs `go test`, which is exactly where context blows up today. See Phase 9.

**Development mode for Phases 2–6: built by hand with Claude Code, human-reviewed —
not self-hosted.** Bootstrap's threshold (a) ("the harness builds itself as a
trusted dev tool, human reviews diffs") assumes a capable model drives the
harness's *own* sandboxed agents; without a hosted key for one, that autonomous
path is deferred. The remaining work is ordinary Go / config / web development, so
it proceeds the same way the kernel was built. **This project is about the *machinery*,
not model quality — nothing below is blocked for lack of a capable model.** A model is
already available in dev (the deterministic `modeltest` server; local Ollama via
`openai-compat`). The wizard and the autonomous self-hosting loop are buildable and
testable *offline*; the only thing a *capable* model gates is the subjective *quality*
of their outputs (good requirements elicitation, trustworthy auto-drafted specs, good
autonomous implementation) — a later validation concern, never an engineering blocker.

## How to read this

- **Completed tasks are collapsed to a one-line `— *done.*` checklist entry.** Phases
  0–1, 2, 3, 4, 6, and 7 are done; the verbose per-task findings were pruned once complete —
  that history lives in git, the code, and the specs they informed (each task updated its
  `(spec)` as it landed).
- **Open tasks (`- [ ]`) keep their full detail.** Only **Phase 5** (production
  isolation) has open lines — the Firecracker backend T5.2 is hardware-blocked and deliberately
  last; the one remaining optional item is T5.11 warm pools + HA (T5.5 gVisor is now done).
  Everything else (Phases 0–4, 6, 7) is complete and collapsed.
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
- [x] **T2.12 Run-all independent scanners** — *done.*
- [x] **T2.10 Trusted-dev policy profile** — *done.*
- [ ] **T2.11** *(optional)* **N-version model diversity — resolved as configuration, not a built-in mechanism.**
  Soul independence (`producer ≠ verifier`) is already enforced; running the verifier on a *different model
  family* than the producer is a **config capability** the harness already provides (a role maps to a set of
  souls via `selector` (T3.3), each soul names its own model/tier (T3.4)), consistent with the config-is-the-
  pipeline principle — the harness *enables and recommends* diversity but the model assignment is the user's.
  No bespoke "second reviewer soul" mechanism is needed. The one piece of buildable work this leaves — a
  non-fatal validation warning on producer/verifier model-family overlap — is tracked as **T2.13**. Decision
  recorded in verification.md ("Model diversity is configured, not mandated").
  ([verification.md](specs/verification.md), [configuration.md](specs/configuration.md))
- [x] **T2.13 Producer/verifier model-family diversity warning** — *done.*
- [x] **T2.14 `golangci-lint` gate check + the producer-self-check principle** — *done.*
- [x] **T2.15 Tests-red compiled-language complement: a `compiles` companion check** — *done.* The spec was already landed (verification.md "Tests-red proof" — overclaim fixed + the complement documented); this task did the **vault-demo-only wiring**, no harness Go change (kernel stays language-neutral — `compiles` is an ordinary exit-0-pass registry check, postconditions already AND). Added a `compile` target to `demo/vault/app/Makefile` (`go build ./... && go test -run='^$' ./...` — builds the test binaries, runs none); registered `compiles: make compile` in the demo's `checks:` and paired it onto the `author-tests` postcondition as `[tests-red, compiles]` (**fail-fast — deliberately NOT in `independent_checks`**: no point grading a tree that does not build), so "red" now means **compiles ∧ tests-fail** (the genuine *executing* red, closing the vacuous-red gap where a not-yet-defined symbol's compile failure satisfied bare `tests-red`). Reconciled the demo test-author persona (`demo/vault/config/souls/prompts/test-author.md`): "minimal non-test scaffolding … prefer to avoid even that" → **"commit the minimal compiling API skeleton"** (signatures/types/error values, no logic) the implementor inherits as a compiler-checked contract — and fixed its point-1 claim that `tests-red` rejects compile errors (it is the `compiles` check that does). Added a differences-table row to `demo/vault/README.md`; shipped `config/harness.yaml` left `tests-red` unpaired (a compiled target opts in). Verified: `harness validate --config demo/vault/config` = OK (only the pre-existing T2.13 family advisory), `make compile` in the vault app exits 0 building all test binaries, `internal/config` tests green. ([verification.md](specs/verification.md))

## Phase 3 — Full DAG, decomposition & merge queue

Unwinds the kernel's single-stage, single-soul, trivial-merge simplifications.

- [x] **T3.1 Decomposition planner soul + `plan` stage** — *done.*
- [x] **T3.2 `beads.Apply` self-validates `DependsOn` existence** — *done.*
- [x] **T3.3 Stage ≠ role + selector matching** — *done.*
- [x] **T3.4 Per-role model tiers** — *done.*
- [x] **T3.5 Spec-slice resolution** — *done.*
- [x] **T3.6 Spec-version pinning** — *done.*
- [x] **T3.7 Recompile-the-delta** — *done.*
- [x] **T3.7b Re-derive already-merged work on a spec edit** — *done.*
- [x] **T3.8 Cumulative per-issue budget** — *done.*
- [x] **T3.8b Cumulative epic budget + cross-loop wall-clock** — *done.*
- [x] **T3.9 Merge queue: serialized rebase onto `main`** — *done.*
- [x] **T3.10 Re-gate the merged result** — *done.*
- [x] **T3.11 Conflict-resolution issue** — *done.*
- [x] **T3.12 Orchestrator in-flight projection (consistency under eventually-consistent beads reads)** — *done.*
- [x] **T3.13 Split the sweep cadence off the dispatch tick** — *done.*
- [x] **T3.14 Ambient specs: always-injected conventions + index** — *done.* New opt-in `ambient_specs []string` on `config.Harness` (validated by `validateAmbientSpecs`: repo-relative, no absolute/`../` escape, no dup; existence is best-effort at dispatch, not validated — paths are repo-relative but `harness validate` need not run from the repo root). Resolution centralized in `internal/spec`: new `ResolveWithAmbient(root, ref, depth, ambient)` prepends the ambient files (rendered via a new shared `render` helper, confined under root, NOT cross-link-followed — they are leaves) AHEAD of the issue's bounded slice, de-duplicated against its `Members`, returning the combined slice + a `missing` list; `ResolveAmbient` is the prefix builder. `buildBrief` (schedule.go) now calls it for EVERY issue (even spec-less seeds get the prefix + a pin) and logs missing ambient files loudly but non-fatally. **Load-bearing consistency fix:** the pinned hash now covers the ambient prefix, so BOTH recompile sweeps (`recompileSpecDelta`, `recompileMergedDelta`) re-resolve through `ResolveWithAmbient` — re-hashing with plain `Resolve` would mismatch every tick and reissue all live work spuriously; conversely an ambient (conventions) edit now correctly drifts/re-derives every pinned issue like a contract edit. The in-flight sweep guard changed from `Spec=="" || SpecHash==""` to just `SpecHash==""` (the pin is the signal — a spec-less issue can now be ambient-pinned). Control-room `BlastRadius` gained an `ambient` param + short-circuits membership to all-pinned when an edited path is ambient, so the consent preview cannot diverge from the sweep. **Vault demo wired:** authored `demo/vault/app/specs/conventions.md` (extracted from the README + the new no-new-modules/zero-network rule), slimmed `app/specs/README.md` to a thin index, set `ambient_specs: [specs/README.md, specs/conventions.md]` in `demo/vault/config/harness.yaml`. No spec change (specs-process.md "Ambient specs", configuration.md, components/agent.md, glossary.md were written ahead). Shipped `config/harness.yaml` leaves it empty (commented example) — the harness has no `specs/conventions.md` to self-host yet. docs/configuration.md gained an `ambient_specs` subsection. Tests: spec `TestResolveWithAmbient*`/`TestResolveAmbientConfinesToRoot`; config `TestValidateAmbientSpecs*`; orchestrator `TestBuildBriefInjectsAmbientSpecs`/`TestBuildBriefAmbientReachesSpeclessIssue`/`TestRecompileSpecDeltaAmbientConsistency`; query `TestBlastRadiusAmbientEditTouchesAllPinned`. ([specs-process.md](specs/specs-process.md), [configuration.md](specs/configuration.md), [components/agent.md](specs/components/agent.md), [glossary.md](specs/glossary.md))

## Phase 4 — Control room

The human's read-only window + the wizard (their only action surface). Stack: templ
+ Tailwind standalone CLI + `embed.FS` + htmx/Alpine + SSE.
([control-room.md](specs/control-room.md), [observability.md](specs/observability.md))

- [x] **T4.1 Web server scaffold + asset pipeline** — *done.*
- [x] **T4.2 Read/query layer** — *done.*
- [x] **T4.3 SSE plumbing** — *done.*
- [x] **T4.4 Board view** — *done.*
- [x] **T4.5 Activity feed** — *done.*
- [x] **T4.6 DAG view** — *done.*
- [x] **T4.7 Issue / invocation detail** — *done.*
- [x] **T4.7b Surface transcript + candidate diff on the detail page** — *done.*
- [x] **T4.8 Dead-letter queue view** — *done.*
- [x] **T4.9 OTel spans + export** — *done.*
- [x] **T4.10 Budgets + Provenance views** — *done.*
- [x] **T4.11 Replay** — *done.*
- [x] **T4.12 Requirements-planner conversation loop** — *done.*
- [x] **T4.13 Alignment ledger** — *done.*
- [x] **T4.14 Spec authoring + consent gate** — *done.*
- [x] **T4.15 Resolve mode** — *done.*

### Live transition events + board-in-motion (T4.16–T4.18)

Implements the now-**decided** control-room refinement (was the "coarse live trigger →
precise issue-state event" OPEN): the single-writer orchestrator emits a typed
transition event the board/DAG/DLQ refresh off, giving crisp **animated card moves**
and the anchor for **per-card timers** (time-in-state + total). The three tasks split
backend-emit / transport / frontend-consume, the same way T4.3 (SSE plumbing) and T4.4
(board) were split. Resolves [control-room.md](specs/control-room.md) "The board, in
motion"; specs already updated (orchestrator.md §9, messaging.md `issue.<id>.state`,
control-room.md, observability.md).

- [x] **T4.16 Issue-state transition events + `state_entered_at` stamp** — *done.*
- [x] **T4.17 Issue-state SSE pump** — *done.*
- [x] **T4.18 Board in motion — crisp refresh, animated moves, per-card timers** — *done.*

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

- [x] **T4.19 Status bar + DLQ escalation alerts** — *done.*
- [x] **T4.20 Agent-event envelope: issue id + role** — *done.*
- [x] **T4.21 Live invocation view** — *done.*
- [x] **T4.22 Gate-verdict record + producing-soul attribution** — *done.*
- [x] **T4.23 Verification view** — *done.*
- [x] **T4.24 Merge-state transition events** — *done.*
- [x] **T4.25 Merge-state SSE pump + merge-queue view** — *done.*
- [x] **T4.26 Config view** — *done.*
- [x] **T4.27 Ledger: batched forks, free-text, discuss/defer states + soft approval gate** — *done.*
- [x] **T4.28b Configurable exploration depth + live "it's working" progress** — *done.*
- [x] **T4.29 Wizard structured output via tool calls (replace fenced blocks)** — *done.*
- [x] **T4.30 Board auto-scroll to the work frontier** — *done.*
- [x] **T4.31 Live public-repo vault demo (GitHub push + deploy) + descriptive commit subjects** — *done.*
- [x] **T4.32 Wizard edits existing specs + maintains the README index (new-specs-only coverage)** — *done.*
- [x] **T4.32a Approve-view diff for spec edits** — *done.*
- [x] **T4.33 Wizard live-refresh discipline: no periodic re-render of stateful panels** — *done.* The alignment-ledger batch form and the draft panel re-fetched on `hx-trigger="sse:ledger, sse:turn, every 8s"` / `"sse:draft, sse:turn, every 8s"`, so the `every 8s` backstop (and the redundant `sse:turn` nudge) blew away a human's in-progress, *unsubmitted* ledger answers — selected chips / free-text reset every few seconds, the reported bug — and collapsed an open spec-diff `<details>` in the draft panel mid-read. Fixed: narrowed both panels to their precise tool-channel nudge plus SSE-reconnect recovery — ledger `hx-trigger="sse:ledger, htmx:sseOpen from:closest [hx-ext='sse']"`, draft `"sse:draft, htmx:sseOpen ..."` — dropping `every 8s` and `sse:turn`. Safe because the planner only re-emits at a turn boundary (never during the human's answer window; `wizard.go` broadcasts `ledger`/`turn` only at turn end), so the precise nudge covers every real change and `htmx:sseOpen` restores the missed-event recovery the poll provided *without* the clobber. Both wizard entry modes (`internal/controlroom/views/wizard.templ` + `views/resolve.templ`; regen'd the committed `*_templ.go` via `templ generate` from the repo root). Read-only views (board/DAG/DLQ/activity/merge-queue and `#wizard-transcript`) keep their periodic backstop. Spec: [control-room.md](specs/control-room.md) "Rendering" exception + "The alignment ledger" ("The form is the human's until they submit it"); operator note in `docs/control-room.md`.

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

- [x] **T5.1 vsock broker transport** — *done.*
- [x] **T5.3 Rootfs / base-image composition** — *done.*
- [x] **T5.3a Harness kernel passes its own `gosec` gate (self-host readiness)** — *done.*
- [x] **T5.4 Sandbox seeded-worktree ownership** *(carried from Phase 1)* — *done.*
- [x] **T5.5** *(optional)* gVisor backend (medium-trust) — *done.* Implemented as the
  Docker provisioning path pinned to the `runsc` OCI runtime (`docker run --runtime=runsc`),
  not a second backend — gVisor boots an ordinary container image with a user-space kernel,
  so reusing `DockerBackend` keeps a single provisioning implementation (zero-network,
  explicit worktree seeding, chown, wall-clock watchdog all unchanged). New `WithRuntime`
  DockerOption + `NewGVisorBackend` (`internal/sandbox/docker.go`, `gvisor.go`→`backend.go`),
  and **the load-bearing fix this exposed**: `cmd/harness/run.go` hardcoded
  `NewDockerBackend()`, so `sandbox.backend: gvisor|firecracker` validated fine but was
  silently ignored — "backend is config, not code" was not actually wired. New
  `sandbox.NewBackend(cfg.Infra.Sandbox)` factory honors the selection: docker/""→Docker,
  gvisor→gVisor, **firecracker→fail-closed error** (hardware-blocked T5.2, must not degrade
  to a weaker backend than asked), unknown→error. The test-injected backend seam
  (`opts.backend`) still takes precedence. runsc isn't installed in this dev box (no network
  to fetch it), so the live boot is unexercised here, but the runtime-arg shaping and factory
  selection are fully unit-tested via the existing `run` seam (no daemon needed). Tests:
  `TestGVisorProvisionPinsRunscRuntime`, `TestDockerProvisionHasNoRuntimeFlag`,
  `TestNewBackendSelectsByConfig`, `TestNewBackendThreadsOptions`. docs/configuration.md
  updated (sandbox.backend now honored, firecracker fails closed); the **fail-closed
  contract is now in the spec too** — components/sandbox.md "Selection is honored, and
  fails closed" + a configuration.md validation note (the learning had landed only in
  docs/ at first). ([components/sandbox.md](specs/components/sandbox.md))
- [x] **T5.6 Package proxy on the broker allowlist** — *done.*
- [x] **T5.6a Gate-verifier package egress** — *done.*
- [x] **T5.7 Scoped short-lived secret minting** — *done.*
- [x] **T5.8 Distributed NATS** — *done.*
- [x] **T5.9 S3/MinIO artifact backend** — *done.*
- [x] **T5.10 Provenance signing + key custody** — *done.*
- [ ] **T5.11** *(optional)* Warm sandbox pools + HA orchestrator via NATS-KV leader election. *(OPEN.)* ([components/runner.md](specs/components/runner.md), [components/orchestrator.md](specs/components/orchestrator.md))
- [ ] **T5.2 Firecracker sandbox backend** ***(lowest priority — see the Phase 5 prioritization note above; deliberately last)*** — a KVM-microVM backend implementing the `Backend`/`Sandbox` interface: rootfs seeding, vsock I/O (T5.1, done), resource limits incl. disk, deterministic teardown. The production isolation target. **Blocked on hardware, not on code:** needs KVM (bare-metal or nested virt) that the dev environment lacks, so it cannot be built-and-verified here — do it only once such hardware is available, after the rest of Phase 5 + Phase 6 (all of which the Docker backend supports for dev/human-reviewed runs). Kept as ID T5.2 (referenced elsewhere) despite its end-of-list position. (needs T5.1) ([components/sandbox.md](specs/components/sandbox.md))

### Multi-signal OTLP observability (T5.12–T5.15)

Production-grade telemetry export, demo-driven (the vault demo for a security audience wants
to *show the whole record shipped* — traces, metrics, and logs — in one backend). Today the
harness exports OTel **traces + metrics** to a single OTLP/gRPC endpoint dialed insecure with
no headers, so only an anonymous local viewer (Jaeger, which also refuses the metrics) can
receive it. This block adds the **logs** signal, **trace-correlated** from the trusted side's
`slog`, and **authenticated** export so any real OTLP backend (OpenObserve first; equally
Grafana Cloud / Honeycomb) can ingest all three. The unit of work is the signal pipeline, not
OpenObserve — OO is the first consumer and the demo's ephemeral sink. Specs already updated
([observability.md](specs/observability.md) "three signals, one endpoint" + "logs are
trusted-side only", [configuration.md](specs/configuration.md) `otel.headers`). Listed in
dependency order; each carries its own doc update (docs/configuration.md, docs/control-room.md
where relevant, and the vault README's telemetry section) per the doc-tracking rule.

- [x] **T5.12 OTLP logs signal + authenticated multi-signal export** — *done.* Added the third
  OTel pipeline to `internal/telemetry`: `exporters` now returns a `sdklog.Exporter` too
  (`otlploggrpc` for an OTLP endpoint, `stdoutlog` for `endpoint: stdout`), `Setup` builds a
  `sdklog.LoggerProvider` alongside the tracer/meter providers, and the composite `Shutdown`
  joins all three flushes. **Logs batch at Info+** via a new `minSeverityProcessor`
  (`logs.go`) wrapping `sdklog.NewBatchProcessor` — it implements both `sdklog.Processor`
  (drops sub-Info records in `OnEmit`) and the `log.FilterProcessor` `Enabled` hint (so the
  SDK skips *constructing* a dropped record at the logger boundary); undefined-severity
  records are kept (indeterminate → forward). `Provider.LoggerProvider()` exposes the
  `otellog.LoggerProvider` the **T5.13** slog bridge will build over — never nil (Noop/NewWith
  return `lognoop.NewLoggerProvider()`), so bridge wiring stays unconditional like
  Tracer()/the instruments. **Auth + multi-signal export:** `telemetry.Config` gained
  `Headers map[string]string` + `TLS bool`, threaded into all three exporters via
  `WithHeaders` and `WithInsecure`-when-`!TLS` (omitting it → TLS with host roots).
  **Credential discipline:** `OTelConfig` gained `headers`/`tls`; `OTelConfig.ResolveHeaders()`
  expands `${ENV}` refs at the run.go call site (last responsible moment, unset→""), and
  `validateOTel` now rejects a credential-named header (`authorization`/`*-key`/`*-token`/…)
  whose value is a literal rather than an `${ENV_VAR}` ref, plus any malformed `${…}` —
  routing metadata (`organization`) stays literal-OK. **Schema:** `conventions.go` gained
  `AttrAttempt` (`harness.attempt`, bounded retry count — safe as a metric dimension) and a
  **binding cardinality-rule doc** on the attribute block (unbounded ids = trace/log only;
  metric dimensions stay bounded). New deps: `otel/log`, `otel/sdk/log`, `otlploggrpc`,
  `stdoutlog` @ v0.14.0 (resolve cleanly against otel v1.44.0). Tests:
  `TestMinSeverityProcessorDropsBelowInfo`/`…DelegatesLifecycle`,
  `TestSetupOTLPWithHeadersAndTLSBuildsLazily`, `TestLoggerProviderNeverNil`,
  `TestValidateOTelHeadersValid`/`…RejectsLiteralCredential`/`…RejectsMalformedRef`,
  `TestResolveHeadersExpandsEnv`. docs/configuration.md updated (otel.headers/tls + three
  signals). Specs were written ahead (observability.md "three signals, one endpoint" +
  cardinality rule, configuration.md `otel.headers`) — no spec change. ([observability.md](specs/observability.md),
  [configuration.md](specs/configuration.md))
- [x] **T5.13 Trusted-side slog → OTel log bridge** — *done.* The trusted side's `slog` now
  fans out to **three sinks from one source**: console (`TextHandler→stderr`), the live activity
  feed (the existing control-room `LogBridge` tee), and the **OTLP logs backend** (new). New
  `Provider.WrapLogHandler(base, name)` (`internal/telemetry/logs.go`) wraps a base handler in a
  `multiHandler` fan-out that adds an `otelslog`-bridged sink over the LoggerProvider T5.12 built
  — a **no-op passthrough when export is off** (Noop/test provider has `shutdown==nil`), so the
  run wiring calls it unconditionally. `multiHandler` is the host-side analog of the LogBridge
  tee generalized to N terminal handlers (Enabled = OR across sinks, Handle clones the record to
  each, WithAttrs/WithGroup propagate). **Wiring** (`cmd/harness/run.go`): right after
  `telemetry.Setup`, `log = slog.New(tel.WrapLogHandler(log.Handler(), "harness"))` adds the OTel
  sink **regardless of serve mode** (OTLP export is independent of the co-located control room);
  the existing serve-mode `LogBridge` then wraps *that*, so a record flows
  `LogBridge(tee→feed) → multiHandler → {console, otelslog→OTLP}` — one source, three sinks. Only
  trusted host-side code logs through it; untrusted model text + sandbox output stay
  span/artifact evidence (the trusted-side-only invariant). **Per-invocation enrichment**
  (`runner.invoke`): one `ilog := r.log.With(...)` built at the invocation boundary using the
  `telemetry.Attr*` constants (invocation/issue/epic/soul/role/model/attempt) — never inline
  strings — replaces the ad-hoc `r.log.With("invocation",…,"issue",…)` and is threaded through
  the relay, the provisioned-sandbox/usage/teardown/broker logs, and `harvest` (signature now
  takes the enriched `*slog.Logger`, dropping the redundant `issueID` param). The invocation span
  also gained `AttrEpicID`+`AttrAttempt` so the span, its logs, and its metrics share the same
  join columns and correlate in the backend. New dep `contrib/bridges/otelslog v0.10.0` (resolves
  against otel v1.44.0 / log v0.14.0). Tests: `TestMultiHandlerFansOutToEverySink`,
  `TestWrapLogHandlerOffIsPassthrough`, `TestWrapLogHandlerOnFansOutKeepingBase`; existing runner
  + cmd/harness spine suites still green. Specs written ahead (observability.md "Logs are
  trusted-side only", "Correlation: one schema") — no spec change; no CLI/config/control-room
  surface change, so no docs/ update. ([observability.md](specs/observability.md), [components/agent.md](specs/components/agent.md))
- [x] **T5.14 Context-variant log sweep + lint enforcement** — *done.* Enabled **`sloglint`
  with `context: all`** in `.golangci.yml` (new `settings.sloglint.context: all`), so a
  non-context `slog` call now fails `make check`. The linter surfaced ~190 sites across ~20
  files (the initial grep was inflated by non-slog `.Error(`/`.Info(`); sloglint is the
  authoritative oracle). Swept every one in two transforms: **(A)** `Info/Warn/Error/Debug` →
  `…Context(ctx, …)` threading the most-derived span-carrying ctx in scope — the function's
  `ctx` param, the tracer-`Start`-reassigned `ctx`, an HTTP handler's `r.Context()`, or (in
  tests) `t.Context()`; call sites with genuinely no reachable ctx pass `context.Background()`
  (uncorrelated by design, never a signature change). **(B)** `slog.New(slog.NewTextHandler(
  io.Discard, nil))` → `slog.New(slog.DiscardHandler)` at the ~10 discard-logger sites (Go 1.26
  has `slog.DiscardHandler`), dropping the now-unused `io` import. Executed as 6 parallel
  subagents over disjoint packages (agent/gate/orchestrator/runner/controlroom/goproxy+lsp+cmd),
  each driven to **0 sloglint** and a clean package build; the lone `internal/telemetry`
  test site fixed inline. Repo-wide: `golangci-lint run` = 0 issues, full `go test ./...` green.
  No spec/doc change (lint-config + mechanical call-site sweep; observability.md already
  mandates trace-correlated logs). ([observability.md](specs/observability.md))
- [x] **T5.15 OpenObserve demo bootstrap** — *done.* `demo/vault/run.sh` gained an
  `OPENOBSERVE=1` path mirroring `JAEGER=1`: boots OO as an **ephemeral `docker run -d --rm`
  with no volume** (`public.ecr.aws/zinclabs/openobserve`, tag overridable via
  `OPENOBSERVE_IMAGE`), `--memory=1g`-capped so it can't crowd the gate sandboxes' share of the
  ~8Gi VM; exposes the authenticated OTLP/gRPC port `5081` (**all three signals ride that one
  port** — verified live, see below) + UI/REST on `5080`; **health-waits** on
  `GET /healthz`; derives the ingestion **token locally as `base64(email:password)`** (offline,
  no API round-trip — exactly what OO's Ingestion page shows) and exports it as **`OTEL_OTLP_AUTH`**
  (the env var the overlay's `authorization: ${OTEL_OTLP_AUTH}` header references); **best-effort
  POSTs** a three-panel **completeness overview dashboard** (one panel per signal) to
  `POST /api/{org}/dashboards`; and rewrites the temp infra overlay (via `awk`, since one line
  becomes a multi-line block) to point `otel.endpoint` at `127.0.0.1:5081` with `tls: false`
  (plaintext h2c, localhost) + `organization`/`stream-name` (literal routing) + the env-ref
  `authorization` header. **Credential discipline verified:** `harness validate` accepts the
  `${OTEL_OTLP_AUTH}` ref and rejects a literal (T5.12's `validateOTelHeaders` rule). Dashboard
  JSON + provisioning notes live in the **new `demo/vault/observe/`**; the dashboard is pinned to
  OO's **v5** schema and POSTed best-effort (schema drift on an image bump degrades gracefully —
  telemetry still lands, import by hand). The metrics panel queries via **PromQL**
  (`harness_invocations`) since OO stores OTLP metrics as Prometheus-named streams (dotted
  instrument names → underscores; sourced from `internal/telemetry/conventions.go`); traces+logs
  panels count over the `default` stream. `JAEGER`/`OPENOBSERVE` are mutually exclusive (one
  `otel.endpoint`). No Go code changed — the unit is the shell bootstrap + committed assets;
  verified by `bash -n`, a simulated overlay rewrite, and a real `harness validate` (accept +
  reject cases). **Verified live against a booted OpenObserve `v0.14.7`** (network is available
  in this dev box): the pinned image pulls + boots healthy in ~4s; the dashboard JSON POSTs
  **HTTP 200** (OO assigned a dashboardId — the v5 schema is correct); and driving the harness's
  *real* `telemetry.Setup` pipeline at `127.0.0.1:5081` with the demo's
  `authorization`/`organization`/`stream-name` headers landed **all three signals** — confirmed
  by OO's stream list showing `default:traces`, `default:logs`, and `harness_invocations:metrics`
  (the exact PromQL series the metrics panel queries; dots→underscores confirmed). docs:
  `demo/vault/README.md` telemetry section gained the
  `OPENOBSERVE=1` subsection; `demo/vault/observe/README.md` documents the dashboard + auth. No
  spec change — observability.md ("three signals, one endpoint"; "a single-binary multi-signal
  sink e.g. OpenObserve") was written ahead. **The whole T5.12–T5.15 block is now complete.**
  ([observability.md](specs/observability.md))

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

- [x] **T6.1 Per-language LSP session manager** — *done.*
- [x] **T6.2 Comprehension (read) semantic tools** — *done.*
- [x] **T6.3 Transformation (write) semantic tools** — *done.*

---

## Phase 7 — Atomic feature integration (epic mode)

Adds an opt-in **`integration.mode: epic`** that lands a whole feature **atomically**:
children integrate onto an `epic/<epic_id>` branch and `main` advances exactly once, by
the epic's terminal merge, when the epic's subtree drains. Default **`per-item`** is the
existing kernel behaviour, untouched. Motivation: a `main` push triggers a deploy, so the
unit of integration should be the *feature* the human drafted in the wizard, not the
individual work item. Reuses existing primitives — the serialized merge queue, the
`resolve` stage, and the `epic_id` already threaded across every issue (the
[`epic_budget`](specs/workflow.md) key) — so the change is mostly *retargeting* the queue
per epic, not new machinery. Spec contract:
[integration.md](specs/integration.md) "Atomic feature integration (epic mode)",
[configuration.md](specs/configuration.md), [control-room.md](specs/control-room.md)
"Epics on the board", [components/orchestrator.md](specs/components/orchestrator.md). The
**vault demo exercises it** (T7.7).

- [x] **T7.1 Fix the re-gate ref form (unblocks any multi-child rebase)** — *done.*
- [x] **T7.2 `integration.mode` config + validation** — *done.*
- [x] **T7.3 Retarget the merge queue per epic** — *done.*
- [x] **T7.4 Epic-completion detection + terminal merge** — *done.*
- [x] **T7.5 One-active-epic consent gate + wizard creates the epic branch with the spec** — *done.*
- [x] **T7.6 Board epic hero card** — *done.*
- [x] **T7.7 Vault demo exercises epic mode** — *done.*
- [x] **T7.8 Board epic-lineage thread + decoupled grouping chrome** — *done.*

---

## Phase 8 — Demo-hardening: authoritative read model + decomposition granularity

Opened from the **2026-06-18 live vault-demo run** (findings: [`demo-run-issues.md`](demo-run-issues.md);
design: [`REMEDIATION_PLAN.md`](REMEDIATION_PLAN.md)). The run worked end-to-end (one child went
spec→red→implement→qa→integrate onto the epic branch with full provenance) but surfaced two root
causes: **(1)** the scheduler *and* control room read beads/Dolt directly, and those reads are
neither read-your-writes consistent nor scalable under polling — producing a redundant planner
re-dispatch, an "open" card on work-in-progress, a retry that looked like a duplicate, an
integrated-count miscount, and `signal: killed` read overloads; **(2)** the planner bundled four
concerns into one child, so the test-author flailed ~1.35M tokens twice (~80% of run cost).

The fix is **systemic, not surgical** (decided this session): promote the orchestrator's
in-flight projection into the authoritative work-graph read model; beads becomes the durable log
+ cold-start hydration. T8.1–T8.4 are the read-model spine (do their spec work first); T8.5
addresses cost; T8.6–T8.7 are clarity/hardening. **TCB note:** T8.1/T8.2 touch the orchestrator
+ scheduler (TCB) — stay human-reviewed; land behind tests, keep the beads-backed reader
switchable to de-risk rollout.

- [x] **T8.1 Full work-graph projection + cold-start rebuild** — *done.* Generalized the
  in-flight cache (`internal/orchestrator/inflight.go`, `inflightProjection`) into the single
  writer's authoritative view of *every* known issue. Each `projectedEntry` now carries
  `{issue, status, lease}`; the `o.transition` choke point `settle`s an issue AWAY from in_progress
  (retains it under its new status) instead of dropping it; `rebuildInflight` hydrates the **full
  graph** from `bd.ListAll` (one heavy read at cold start, not the hot path) instead of
  `InProgress`. The in-flight accessors (`has`/`issues`/`expired`/`size`) preserve their old meaning
  by filtering to `in_progress`; new readers `statusOf(id)` (the spine **T8.2** reads to skip
  just-settled candidates) and `snapshot()` (the whole-graph read **T8.4** consumes snapshot-then-
  stream) expose settled state. Removed the now-orphaned `InProgress` from the orchestrator `Beads`
  seam (and its fake); `beads.Client.InProgress` stays (own test). **No spec change needed** — the
  specs were already written ahead (orchestrator.md "Live state vs. durable state — the work-graph
  projection", observability.md "The live read model", glossary "Projection"). Tests:
  `TestRebuildHydratesFullWorkGraph` (full-graph cold-start parity + idempotent re-rebuild),
  `TestProjectionRetainsSettledIssue` (settle-not-delete; `statusOf`/`snapshot`).
  ([observability.md](specs/observability.md), [components/orchestrator.md](specs/components/orchestrator.md) "Live state vs. durable state", [glossary.md](specs/glossary.md))
- [x] **T8.2 Scheduler dispatches off projection status** *(fixes #2)* — *done.* `scheduleReady`
  (`internal/orchestrator/schedule.go`) now filters candidates through the work-graph projection via
  `o.inflight.statusOf(issue.ID)` instead of the in_progress-only `o.inflight.has()`: `bd.Ready()`
  stays the candidate oracle (no open blockers + precondition), but a candidate the projection knows
  is `in_progress` **or settled (`closed`/`blocked`)** is skipped (`known && st != statusOpen`) — so a
  just-closed/just-decomposed issue (e.g. a plan closed at decomposition, or a dead-lettered one) is
  not re-dispatched before beads' lagging read catches up (closes demo finding #2's redundant
  re-dispatch). A known-but-`open` candidate (re-derived ready, e.g. released after a failed publish,
  or hydrated open at cold start) still dispatches; a not-known issue is new and dispatches. No spec
  change — orchestrator.md "Live state vs. durable state" already specified the in_progress-*or*-settled
  skip. Tests: `TestScheduleReadySkipsSettledCandidate` (closed + blocked sub-tests),
  `TestScheduleReadyDispatchesKnownOpenCandidate` (inverse guard — open known candidate still dispatches).
  ([components/orchestrator.md](specs/components/orchestrator.md))
- [x] **T8.3 `integrated` as a durable state + correct epic rollup** *(fixes #7)* — *done.* The
  marker is durable **issue metadata** (`MetadataKeyIntegrated = "integrated"`, value JSON `true`),
  consistent with the existing `Stamp*` family — not a selector *label* (Tags are the soul-selector
  namespace; bd only sets labels at create, so a post-hoc label write was the wrong seam). New
  `beads.Client.StampIntegrated(id)` (idempotent set), decoded by `metaBool` into the new
  `core.Issue.Integrated` field; added to the orchestrator `Beads` seam + its fake. The merge path
  (`results.go` `mergeCandidate`) stamps it the instant a candidate lands (before the bead is
  closed; best-effort — a stamp failure logs, never undoes a landed merge), in **both** per-item and
  epic mode (an integration is an integration). `epicSummaries`
  (`internal/controlroom/query/query.go`) now counts the **marker**, not `closed`, and excludes the
  epic root (`i.ID == ep`) and any closed-but-not-integrated bead (a superseded retry or an advanced
  intermediate stage); **spend still aggregates over every bead** so the hero matches the Budgets
  view. The spec's rule collapses each lineage to one frontier bead (its integrated bead or its
  current active stage), so a two-child feature reads `0/2 → 1/2 → 2/2`, never `1/4`. **Cold-start
  re-derivation is automatic** — the projection's `reset()` carries the full `core.Issue` (incl.
  `Integrated`) hydrated from `bd.ListAll`, no git read. Specs were written ahead (integration.md
  "Integrated vs. closed", glossary "Integrated") — no spec change. Tests:
  `TestStampIntegratedRoundTripIntegration` (bd round-trip + idempotent),
  `TestMergeCandidateStampsIntegratedMarker` (write-side: stamped on land, survives close),
  `TestBoardEpicProgressExcludesRootAndSupersededBeads` (1/2 not 4/6; spend unaffected); existing
  epic-hero fixtures updated to the marker. ([integration.md](specs/integration.md), [glossary.md](specs/glossary.md))
- [x] **T8.4 Control-room projection-backed read model** *(fixes #4, #6, #8)* — *done.* The LIVE
  work-state views now read the orchestrator's in-memory work-graph projection; the FORENSIC pages
  keep reading beads (the durable truth) — a deliberate **two-reader split** in `query.Reader`
  (`live` vs `issues`), spec-faithful (observability.md "live read model" vs control-room.md
  "Historical/forensic"). Realized as a **co-located direct in-process read**, not a second
  delta-applied copy (single source of truth, no drift): `orchestrator.Snapshot(ctx)` exposes the
  projection (`inflight.snapshot()`); `query.NewProjectionIssueReader(WorkGraphSnapshot, fallback)`
  is the projection-backed `IssueReader` (ListAll→snapshot, List→status-filter incl. comma sets,
  Get→scan-then-beads-fallback). `NewReaderWithLive` wires it for **Board, DAG, DeadLetters, Status**
  (Status decoupled from Budgets via the shared `aggregateBudgets`); everything else stays beads.
  Standalone `harness serve` keeps `NewReader` (live==issues==beads). The browser still refetches on
  the existing `issue-state` SSE nudge (T4.17/T4.18) + periodic backstop, now rendering from the
  projection — so "snapshot-then-stream" is realized as re-snapshot-on-nudge (strictly gap-free).
  **Projection completeness (the load-bearing write-side work):** the projection was only maintained
  at status `transition()`; it now also captures (a) **creation** — `applyTracked` wraps every
  `bd.Apply` site (accept/acceptPlan/advance/route/resolveConflict) and records created issues as
  `open` (else a freshly created child is invisible until first claimed); (b) **fresh render fields
  on settle** — `transition` stamps `StateEnteredAt` (board timers), `handleResult` mirrors closing
  spend (hero roll-up), `deadLetter` mirrors the DLQ reason, and `mergeCandidate`→`markIntegrated`
  + a **monotonic `settle` preserve** carries the `Integrated` marker through the close (hero
  progress); (c) **external human-approved writes** — public `orchestrator.Track(...)` keeps the
  projection in step with the two wizard write paths that bypass the reconcile loop. **Latent T8.2
  bug fixed here:** the Resolve wizard reopens a dead-letter via `bd.Reissue` (blocked→open) directly,
  so the projection still read it `blocked` and the **T8.2 scheduler would skip it forever** — the
  Resolve path now `Track`s the reopen as open. run.go late-binds the projection into the reader
  (the orchestrator is built after the control room, needing the in-block log bridge) and sets the
  wizard's track callback post-construction. **No spec change** — observability.md/control-room.md
  were written ahead and already say co-located reads the in-process projection live; docs/control-room.md
  updated (status bar + liveness note). Tests: `TestProjectionTrackRecordsCreatedOpenIssue`,
  `TestProjectionMarkIntegratedSurvivesSettle`, `TestApplyTrackedRecordsCreatedIssues`,
  `TestOrchestratorTrackAndSnapshot`, `TestTransitionStampsStateEnteredIntoProjection`,
  `TestProjectionIssueReader`, `TestReaderLiveVsForensicSplit`.
  ([control-room.md](specs/control-room.md), [observability.md](specs/observability.md), [messaging.md](specs/messaging.md))
- [x] **T8.5 Planner decomposition granularity** *(fixes #5 — the cost driver)* — *done.* Both
  decomposition-planner personas (`config/souls/prompts/planner.md` +
  `demo/vault/config/souls/prompts/planner.md`, kept byte-identical) now carry the binding
  granularity principle in their "A good decomposition" list: point 2 was promoted from a soft
  "Is independently testable" to **"Is a single, independently testable concern"** — *one*
  behaviour boundary, **not** several concerns bundled; each child must be carryable **in one
  pass** (one `implement` invocation, one `author-tests` pass); bundling multiple
  subsystems/handlers/features is named as the single most damaging mistake (it drives the
  turn/token-ceiling churn → dead-letter/retry that was ~80% of the demo-run cost); the rule
  closes with **"when in doubt, split"** + the one-sentence-without-"and" heuristic and the
  tradeoff (finer breadth's fixed overhead ≪ one runaway). The **spec was already written ahead**
  — `specs/workflow.md` "Emergent within a stage…" already states granularity-is-a-correctness-
  property as "a binding principle the planner persona carries"; this task is what makes the
  persona actually carry it (closing the spec↔persona gap), so **no spec change needed**. No
  test asserts persona text (only existence/readability is validated — confirmed by repo search);
  both configs still `validate` OK. Optional test-author turn-budget backstop deferred — decide
  after a re-run measures whether finer decomposition alone tames the cost. ([workflow.md](specs/workflow.md))
- [x] **T8.6 Name the real merge target in the log** *(fixes #9)* — *done* (folded into T8.3's
  `results.go` pass). The `mergeCandidate` success log is now `orchestrator: merged candidate` with a
  `target=<integrationBranchName(issue)>` field — `epic/<id>` in epic mode, `main` in per-item — so
  the log never claims a child advanced `main` when it landed on the epic branch. (No spec change.)
- [x] **T8.7 Wizard one-root-in-epic hardening** *(fixes #1)* — *done.* The root cause was that
  the requirements-planner persona is a **static prompt shared across deployments** — it had no way
  to know the run's `integration.mode`, so it split the feature at the *seed* level (two roots) and
  the consent gate refused the APPROVE after the fact. Fix is **systemic, not just persona text**:
  a new `wizard.WithEpicMode()` option (`internal/controlroom/wizard/wizard.go`) folds a one-root
  directive into the Create session's **system prompt** at `New()` (via `epicGrounding()`, mirroring
  `projectGrounding`/T4.28) — it rides the system channel, never the human↔planner transcript;
  `cmd/harness/run.go` sets it only when `cfg.Harness.Mode() == config.IntegrationEpic`, so per-item
  sessions are byte-for-byte unchanged. Both personas (`config/` + `demo/vault/config/`, kept
  in-sync) also gained an explicit one-root-in-epic bullet in "The draft" rules. The consent-gate
  error (`wizard_seed.go`) was clarified to name *why* (epic keyed on a single root id) and the
  action (ask the planner to consolidate; decomposition splits it back). **No spec change** —
  integration.md ("exactly one root") and control-room.md already mandate it; docs already document
  it. Tests: `TestEpicModeFoldsOneRootRuleIntoPrompt` + `TestPerItemModeLeavesPromptUnchanged`
  (grounding is opt-in, never leaks into the transcript); existing `TestValidateEpicRequiresSingleRoot`
  still green. ([specs-process.md](specs/specs-process.md), [control-room.md](specs/control-room.md), [integration.md](specs/integration.md))

---

## Phase 9 — Structured check findings & agent context discipline

Opened from a **2026-06-22 design pass** on reducing agent context-window size and raising
first-pass success (the failure the 2026-06-18 demo run exposed: ~80% of cost was one agent
flailing against a growing, noise-filled history). The organizing principle is **signal
density** — strip noise (raw test/scanner dumps, duplicate reads), preserve signal (the
contract, the failure evidence). The chosen first investment is **structured check output**:
every gate check becomes a *thin per-tool adapter* that parses its tool's machine-readable
output into language-neutral `Finding`s, so the compact findings — not a multi-thousand-line
dump — are what enter the agent's context, the gate verdict, the verification view, and the
retry Brief. The extraction is **infra, not persona**: the model never has to know to run
`-json | jq`, consistent with the "tool layer picks the mechanism, never the agent" stance
(Phase 6 LSP). Complements T8.5: that bounds the failure at *planning* time (decompose smaller);
this bounds it at *runtime* (smaller, signal-dense histories).

**Spec landed first** (per CLAUDE.md, commit 653824a): `verification.md` ("Findings: structured
evidence, not the grade"; tri-state passed/failed/not-run + build precondition; structured
producer self-check), `glossary.md` (`Finding`), `configuration.md` (per-tool adapters; `fail_on`
deferred), `components/agent.md` (`run_tests`/`run_gate`), `components/artifact-store.md`,
`control-room.md`. **Grading is unchanged** — pass/fail stays the exit code / proof / metric;
findings are evidence, not the grade. Severity-threshold grading (`fail_on`) is a deliberate
later refinement.

**`./demo/vault` is the exercising use case.** Go (the shipped adapters' language), a `qa` gate
that already runs gosec + govulncheck + license-scan doing real work on agent output, and an
inner loop that runs `go test` — exactly where raw output blows the window today. A live
wizard-drafted feature taking plan → author-tests → implement → qa is the end-to-end exercise:
the implementor's `run_tests` returns compact findings, the gate verdict carries them, and the
verification view renders them. **Validate the context/cost reduction on a vault re-run.**

Build order (each a single, independently testable concern — T8.5 granularity applied to our own
work; ordered so the inner-loop context win lands first):

- [x] **T9.1 `core.Finding` + findings on `CheckResult`/`GateVerdict`** — *done.* New
  `internal/core/finding.go`: `core.Finding{File, Line, Severity, Rule, Message, Detail}`
  (language-neutral leaf; `Detail` is verbatim free text for the tool-specific essential — a test's
  assertion diff, a vuln's call path — jitter is stripped at *parse* time, T9.2/T9.6, not here) and
  `core.Findings` (a named slice, not bare `[]Finding`, so its `Format()` is the *one* renderer — no
  divergent second path). `Findings.Format()` sorts a copy into canonical order (file→line→rule→
  severity→message) and renders `file:line [severity] rule: message` with empty components dropped +
  Detail indented under it; **cache-stable** — byte-identical regardless of parser emit order (the
  load-bearing property for prefix caching + "findings not shrinking" signals). **Additive fields:**
  `core.GateCheckOutcome` gained `Findings` + a **tri-state `Status`** (passed/failed/not-run) with
  constants `core.CheckStatus{Passed,Failed,NotRun}` + helper `CheckStatusOf(bool)`; `Passed bool`
  kept so older readers still see not-run as `Passed==false`. `gate.CheckResult` gained matching
  `Findings`/`Status` fields; `verdictRecord` derives `Status` from `Passed` for a check that ran but
  **preserves an explicit not-run** (set later by the T9.4 build precondition) and carries `Findings`
  through. Fields are nil/empty until the parsers (T9.2+) populate them, so every existing consumer
  (incl. the verification view) keeps working unchanged. **No spec change** — `verification.md`
  ("Findings: structured evidence…", tri-state) + `glossary.md` (`Finding`) were written ahead
  (commit 653824a); no CLI/config/control-room surface change, so no docs/ update. Tests:
  `TestFindingsFormat{EmptyIsBlank,RendersComponentsCompactly,IndentsMultiLineDetail,IsCacheStable}`,
  `TestCheckStatusOf`, `TestVerdictRecordCarriesStatusAndFindings`.
  ([verification.md](specs/verification.md), [glossary.md](specs/glossary.md))
- [x] **T9.2 `internal/gotest` parser** — *done.* New pure, dependency-free `internal/gotest`
  package: `func Parse(stdout []byte) core.Findings` turns a `go test -json` ndjson stream into
  compact `core.Findings`. Each edge-case branch preserves the one signal a raw dump buries:
  **test failure** → finding anchored at the printed `foo_test.go:NN` (Rule=test name, Message=the
  assertion line, Detail=the failure body); **compile/build failure** → surfaces the *compiler
  error* (handles both structured `build-output`/`build-fail` events **and** a raw non-JSON build
  block printed to stdout — a non-JSON line never crashes the parser, owning CLAUDE.md's "if jq
  fails check .stderr" case); **data race** → `WARNING: DATA RACE` stanza kept verbatim; **panic/
  timeout** → message + triggering test kept, goroutine dump dropped (a package-level sweep recovers
  a timeout that kills the binary before a per-test `fail` fires). Jitter (Elapsed/timestamps) is
  stripped at parse time; output is deterministic (re-run byte-identical, asserted). Real captured
  fixtures under `testdata/` ({pass,fail,compile,race,panic,timeout}.json). Tests:
  `TestParse{PassYieldsNoFindings,TestFailure,CompileFailureStructured,RawNonJSONNeverCrashes,
  DataRace,Panic,Timeout,IsCacheStable,EmptyInput}`. Built in a parallel worktree subagent. No spec
  change (verification.md written ahead). (next consumer: **T9.3** `run_tests`)
- [x] **T9.3 `run_tests` agent tool (first consumer)** — *done.* New `agent.RunTestsTool` (`internal/agent/runtests.go`):
  wraps `go test -json <scope>` (optional `scope` arg, positional — no shell injection — defaults to
  `./...`), parses with `gotest.Parse`, returns the compact `core.Findings.Format()` string instead
  of the raw multi-thousand-line dump (the headline inner-loop context win — the implementor's
  self-check stops dumping noisy logs into the very history that blows its window). `IsError` set when
  findings exist (failure feedback); a non-zero exit / build failure is **not** a tool error (the
  parser turns it into findings). **Zero-trust** (runs in the untrusted producing sandbox; feedback,
  never a grade — only the independent gate re-run grades). **Artifact harvest** mirrors the trace-map/
  transform-log discipline exactly: the agent has no store access (no network), so raw json
  accumulates in a per-invocation `TestEvidenceLedger` keyed by content-address, and the trusted
  `toolSource` cleanup (`harvestTestEvidence` in `cmd/harness/run.go`) Puts each stream under
  `core.ArtifactKindGateEvidence`. The agent computes the address locally as `sha256:`+hex (mirrors
  `artifact.FilesStore.Put` — **verified identical**, so the inline-cited hash resolves once the
  trusted Put lands) without importing `internal/artifact` (keeps the untrusted package off the
  trusted store). Wired for all souls via the shared `toolSource` (no per-soul tool allowlist in
  config — no config change needed). Built in a parallel worktree subagent (disjoint from T9.4).
  Tests: `TestRunTests{ReturnsFindingsNotRawDump,HarvestsRawEvidenceByContentAddress,
  DefaultsScopeToWholeModule,NilLedgerStillCitesHash,BuildFailureBecomesFinding}`. No spec change
  (agent.md/verification.md written ahead). ([components/agent.md](specs/components/agent.md))
- [x] **T9.4 Build precondition + tri-state in the gate** — *done.* `internal/gate/gate.go` `Run` now
  runs a **build precondition first** when a `build` command is registered (`checkBuildName="build"`):
  it reuses the configured `build` registry command (single source of truth — the gate grades a
  command, not a hardcoded `go build`), and on failure **short-circuits the dependent checks**,
  recording each as `core.CheckStatusNotRun` via new `notRunResult` (never re-run, never a misleading
  green or a failure that never executed) instead of letting every downstream tool rediscover the
  broken build in its own format. The build error is captured as a single `core.Finding`
  (`buildFailureFindings`, Detail=compiler output verbatim — the signal the retry Brief needs).
  **not-run never counts as a pass**: `report.Passed` is forced false, so the gate still fails closed;
  the tri-state changes only what the verdict *records*. Independent-scanner aggregation (T2.12) is
  untouched on the build-passes path; a declared `build` postcondition is consumed by the precondition
  (`dropByName`) so the command never runs twice. **Inert for the shipped config** — `config/harness.yaml`
  registers no `build` check (build is folded into `make test-unit`), so this is a pure no-op until a
  deployment adds a `build:` entry (+ optionally declares it first in the qa/resolve postconditions);
  that config wiring is left for whoever activates it. Deliberately did **not** pull in `internal/gotest`
  (T9.5 owns wiring the structured parsers into the gate). TCB-touching (gate ordering) — kept minimal/
  surgical; reviewed before integration. Built in a parallel worktree subagent (disjoint from T9.3).
  Tests: `TestRunBuildPreconditionShortCircuitsDependents`, `TestRunBuildPassesThenIndependentScannersAggregate`,
  `TestRunNoBuildCommandNoPrecondition`, + updated `TestRunStopsAtFirstFailure`/`TestRunVerdictRecordsFailure`.
  No spec change (verification.md "A check is tri-state" written ahead). ([verification.md](specs/verification.md))
- [x] **T9.5 Gate verdict carries findings** — *done.* New `internal/gate/findings.go`:
  `adapterFor(check)` selects the per-tool parser **by the check's identity** (the spec's "registry
  stays a name→command map" — no config field): proofs (red→green/tests-red) + the `tests-pass`
  acceptance check → `gotest.Parse`; `gosec`/`govulncheck`/`golangci-lint`/`license-scan` → the
  matching `internal/scanners` adapter; anything else → nil (graceful fallback — grade on exit code,
  raw output stays evidence). `findingsFor` parses captured **stdout** (stderr only when stdout is
  empty) and populates `CheckResult.Findings` in `runCheck` (command/red-proof path) and `runRedGreen`
  (the candidate/green run); `verdictRecord` (T9.1) already carries them into `core.GateVerdict`, so
  the harvested record holds per-check findings. **Load-bearing guard:** the go-test adapter only fires
  on ndjson-shaped output (`looksLikeNDJSON`) — a human-format test command (the shipped `make
  test-unit`, which routes `-json` to a file) is graded identically on exit code but its plain text
  would otherwise be misread by T9.2's compile-failure path as a bogus "build" finding; a real build
  failure at the gate is T9.4's job, so nothing is lost. **Verification view** (`verification.templ`):
  new tri-state `checkStatusBadge` (pass/fail/**not-run**, falling back to the old Passed badge for
  pre-tri-state records) replaces the two-valued badge, and each check renders its `Findings.Format()`
  (the same cache-stable compact form the retry Brief will carry) in a `<pre>` — raw dump stays the
  linked evidence. Regenerated `verification_templ.go` (templ generate from repo root); reused existing
  Tailwind tokens (no CSS rebuild). **Activation note:** findings populate only when the check command
  emits machine-readable output (`go test -json`, `gosec -fmt=json`, …) — documented in
  docs/configuration.md; the shipped configs are unchanged (graceful-empty until an operator opts in,
  e.g. the vault re-run). TCB-touching (gate) — done sequentially with review, not a worktree. No spec
  change (verification.md "Findings…", configuration.md "per-tool adapters" written ahead); docs updated
  (control-room.md verification view, configuration.md findings activation). Tests:
  `TestAdapterForSelectsByCheckIdentity`, `TestFindingsForFallsBackToStderr`,
  `TestRunPopulatesPerCheckFindings`, `TestRunFailingTestCheckCarriesFindings`.
  ([verification.md](specs/verification.md), [control-room.md](specs/control-room.md))
- [x] **T9.6 Scanner finding adapters** — *done.* New `internal/scanners` package, one pure adapter
  per scanner → `core.Findings`: `ParseGolangciLint` (issues→{File,Line,Rule=linter,Message,Severity};
  streaming decoder reads the first JSON value so the trailing human "N issues." line doesn't break
  the parse), `ParseGosec` (`Issues[]`→{File,Line,Rule=rule_id,Severity lowercased,Message,Detail=code
  excerpt}; `"11-13"` line range→first line), `ParseGovulncheck` (correlates `osv` summaries with
  `finding` traces, emits one finding per **called** symbol-level vuln, dropping module/package-only
  informational ones — the signal-density point; {Rule=GO id,Message=summary,File:Line=call site,
  Detail=aliases+fix+call path}), `ParseLicenseScan` (`go-licenses check` text; drops timestamped glog
  jitter, keys off its stable format strings → {Rule=module,Message=disallowed license}). Every parser
  degrades to "what it could parse, or empty" on a truncated/non-JSON blob without panicking (the
  exit-code verdict still stands — **a command with no adapter still grades on exit code with raw
  output as evidence**). Deterministic (cache-stable, asserted). Fixtures under `testdata/`: golangci/
  gosec captured real from dev-box tools; govuln distilled from a real `golang.org/x/text@v0.3.0`
  scan; license hand-authored from go-licenses' format strings (no JSON mode). 12 tests incl.
  `TestMalformedInputDegradesGracefully` + `TestCacheStability`. Built in a parallel worktree subagent.
  Additive only — **T9.5** wires these into the gate verdict. No spec change. ([configuration.md](specs/configuration.md))
- [x] **T9.7 `run_gate` full self-check tool** — *done.* New `agent.RunGateTool`
  (`internal/agent/rungate.go`): the producer self-check generalized from `run_tests`' single test
  pass to the **whole command-check suite** the gate will run (acceptance tests + scanners). It runs
  each configured check's command once in the untrusted producing sandbox (`sh -c <cmd>`, sorted for
  cache-stability), parses each via the shared adapter, and returns a compact tally + per-check
  findings (never the raw scanner dumps) with each check's raw output harvested as evidence (reusing
  T9.3's `TestEvidenceLedger` + the run.go cleanup harvest). Zero-trust (feedback, not a grade — the
  gate re-runs authoritatively). **Deliberately omits** the red→green proof (needs a base-ref sandbox
  only the gate has) and the **mutation metric** (graded on a score-vs-threshold, not an exit code —
  running it would misread the tool's exit-0 as a pass). **Single source via new `internal/checkfindings`
  leaf:** the per-tool adapter selection (`ByName`/`GoTest` with the ndjson guard) was extracted from
  T9.5's gate code into a neutral package that **both** the gate (`adapterFor` now delegates) and
  `run_gate` import — so "the gate checks it" and "I checked it" share one command *and* one parser.
  New `config.Harness.CommandCheckCommands()` computes the self-check set from the DAG (proofs resolve
  to `tests-pass`; metrics + reserved postconditions excluded by construction since they aren't
  registry keys); `run.go` resolves it once and wires `RunGateTool` into the toolSource. **No spec
  change** — components/agent.md "Verification (self-check)" + verification.md "Producer self-checks"
  written ahead; docs don't enumerate agent tools, so no docs/ change. Tests:
  `TestRunGateRunsEveryCheckAndReturnsFindings`, `TestRunGateAllCleanIsNotError`,
  `TestRunGateNoChecksConfigured`, `TestCommandCheckCommands`, `internal/checkfindings` suite (`ByName`/
  `GoTest` guard/`Parse` fallback); gate suite still green after the refactor. ([verification.md](specs/verification.md))
- [x] **T9.8 Failure-aware retry Brief** — *done.* The `on_failure` route now threads the failed
  gate's parsed findings into the fix issue so the retry sees exactly the N checks it must fix
  instead of re-deriving blind from the spec (the failure mode the whole phase targets).
  `internal/orchestrator/results.go`: new `failingFindings(report)` collects the findings of every
  *failed* check (passing checks and findingless metric failures contribute nothing — the tight
  payload), and `route(...)` gained a `findings core.Findings` param threaded into the fix issue's
  **body** via new `bodyWithGateFeedback` — a delimited (`<!-- harness:gate-feedback -->`),
  **idempotent** section (any prior attempt's section is stripped first, so a multi-retry carries
  only the latest findings, never a growing pile) rendering the compact, cache-stable
  `Findings.Format()`. The body is the existing beads-persisted carrier that already survives the
  create→schedule round-trip and renders into the agent's opening turn via `agent.buildContext`
  (and into the control-room issue view) — no new persistence/Brief/render plumbing. Only the
  gate-rejected callsite (results.go:274) passes findings; the five other `route` callers (agent
  failure, no-candidate, planner-no-children, human-reject, re-gate-failed) pass `nil`. **No spec
  change** — verification.md ("Findings … a failed candidate's retry Brief") + glossary written ahead;
  the fix body shows in the existing issue-view Body render, so no CLI/config/view-surface change and
  no docs/ update. Tests: `TestFailingFindings`, `TestBodyWithGateFeedback` (append / no-findings /
  idempotent-across-retries), `TestHandleResultGateFailThreadsFindingsIntoFixBody`; existing
  `TestHandleResultGateFailRetries` still green. **Deferred:** routing the fix by *finding type*
  (a soul selector keyed on a finding-type tag) — it needs a finding-type taxonomy + a consuming
  soul, a separate decision; the threading this task lands is the prerequisite. (needs T9.5)
  ([verification.md](specs/verification.md), [components/orchestrator.md](specs/components/orchestrator.md))

**Deferred (named, not in this slice):** **finding-type routing** (route the `on_failure` fix to a
specialized soul via a selector keyed on a finding-type tag — T9.8 threads the findings; this needs a
finding-type taxonomy + a consuming soul, a separate decision); **severity-threshold grading**
(`fail_on` — findings-first,
thresholds-second; it makes findings influence the verdict, which the contract currently forbids);
**progress-based termination** (abort a non-progressing invocation early — "findings not shrinking
across attempts" is the strong signal the structured findings unlock, complementing T8.5);
**semantic-first read steering** (the LSP tools return spans not files; the findings' `file:line`
anchors feed them); **a measurement loop** (the OTel/OpenObserve telemetry from T5.12–T5.15 is what
would rank these techniques by measured token / pass-rate impact rather than by guess).

---

## Deferred & follow-ups (filed, not blocking)

- Live-streaming replay (reconstruct the decision trail as the invocation runs) — needs the broker to emit structured per-turn events; overlaps the activity feed *(from T4.11)*.
- Consolidate the status bar's 2–3 per-page SSE connections (page content + status bar + alerts.js) onto one connection or h2c *(from T4.19)*.
- Client-side live wall/token ticker on the invocation budget meter (mid-invocation spend isn't persisted to beads) *(from T4.21)*.
- Decomposition-preview dry-run before APPROVE (control-room.md OPEN, "leaning defer"; seed issues stay coarse and the autonomous planner decomposes) *(from T4.14)*.

## Open decisions affecting the plan

These are still `OPEN:` in the specs and may reshape tasks above. (Decisions once open
here — mutation threshold, gate fail-fast, `integrate` ownership, the condition-expression
language — are now recorded in the specs they informed, not duplicated here.)

- HA orchestrator: single instance (fine for v1) vs. leader election (T5.11).
- **Concurrent epics (epic mode).** Phase 7 v1 admits one active epic at a time (T7.5
  refuses a second). Lifting it needs a two-level merge queue (children serialize onto their
  epic branch; epic→`main` merges serialize onto `main`, an in-flight epic rebasing onto a
  moved `main` + re-gating the whole feature at its terminal merge). Deferred; spec'd as OPEN
  in [integration.md](specs/integration.md).
- Exact module set in the TCB boundary — operationally the `policy.tcb_paths` globs (T2.10);
  the concrete list must still be reviewed and pinned before autonomy is switched on for harness
  work. Now formally tracked as an **OPEN question in configuration.md** (was only prose in
  bootstrap.md + this plan).
