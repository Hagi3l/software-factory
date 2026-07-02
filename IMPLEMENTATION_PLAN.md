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

**Phases 2, 3, 4, 6, 7, 8, 9, 10, 11, and 13 are complete; open build work is Phase 5 (all optional
— T5.11 warm pools + HA — or hardware-blocked — T5.2 Firecracker, needs KVM the dev box lacks)
and the newly-specced Phase 12 (distilled explore tool: **COMPLETE — T12.1–T12.6 all landed: the
`explore` tool + nested read-only sub-loop, the broker sub-context selector + runner-pinned explorer
model + per-stream sub-budget metering, the `policy.explore_budget` / reserved-`explorer`-role config +
validation, explore evidence/provenance/observability nesting, per-role enablement + verify-path
diversity advisory, and the vault demo wired (planner/implementor explore on a cheap Haiku explorer;
verify path reads raw). Remaining is a live-run cost/context validation, not a build item**).** Phase 11
closed with T11.2 (prompt caching) landing — the Anthropic
adapter now caches unconditionally and the openai-compat adapter caches opt-in, with cache
read/write tokens normalized into the canonical Usage for accurate USD accounting.
Phase 13 (live-demo hardening II) is complete: T13.1 gave the openai-compat adapter the
second **moving-tail** cache breakpoint T11.2 had only on the first-party path (the fix for
the slow, token-heavy deep runs), T13.2 added the wizard `requirements_planner.prefill`
insert-a-prepared-requirement button for a scripted stage kickoff, and T13.3 tuned the vault
demo's walls, gave the test-author `explore`, and added a running-plan persona convention.
Phase 2 (independent verification) is complete — only
the optional T2.11 decision-note remains (resolved as configuration, not a build item).
Phase 3 (full DAG, decomposition, merge queue) is complete (T3.13 and T3.14 landed). Phase 4
(control room + Create/Resolve wizard) is complete. Phase 6 (agent semantic LSP tooling)
is complete (T6.1–T6.3). Phase 7 (atomic feature integration / epic mode) is complete
(T7.1–T7.8; the vault demo now runs `integration.mode: epic`). Phase 8 (demo-hardening:
authoritative read model + decomposition granularity), Phase 9 (structured check findings
& agent context discipline), and Phase 10 (read-model concurrency correctness) are complete.
The only remaining *engineering* of new substrate is Phase 5 (production isolation &
distribution), and within it every still-open item is either **optional** (T5.11 warm pools +
HA orchestrator) or **hardware-blocked** (T5.2 Firecracker, needs KVM the dev box lacks). T5.5
(gVisor backend) is now **done** — and landing it wired up the previously-missing config→backend
selection, so `sandbox.backend` is finally honored at startup (firecracker fails closed rather
than silently degrading to Docker).

**Outstanding runtime validation:** a clean `./demo/vault` re-run (the exercising use case — Go,
a `qa` gate running gosec/govulncheck/license-scan, an inner loop running `go test`) to confirm
the read-model concurrency fixes and the structured-findings context/cost reduction; that needs a
live run (Docker + a capable model), not a build item.

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
  0–1, 2, 3, 4, 6, 7, 8, 9, and 10 are done; the verbose per-task findings were pruned once
  complete — that history lives in git, the code, and the specs they informed (each task updated
  its `(spec)` as it landed).
- **Open tasks (`- [ ]`) keep their full detail.** **Phase 5** (production isolation) has
  open lines — the Firecracker backend T5.2 is hardware-blocked and deliberately last; the one
  remaining optional item is T5.11 warm pools + HA (T5.5 gVisor is now done) — and **Phase 12**
  (distilled explore tool) is **COMPLETE (T12.1–T12.6)**: the tool + nested sub-loop; the broker
  sub-context selector + runner-pinned explorer model + per-stream sub-budget metering; the
  `explore_budget`/reserved-`explorer`-role config + validation; explore evidence/provenance/observability
  nesting; per-role enablement + verify-path diversity advisory; and the vault demo wired. The only
  open item is a live-run cost/context validation (not a build item).
  Everything else (Phases 0–4, 6, 7, 8, 9, 10, 11) is complete and collapsed.
- **Phases 2–5 and Phase 12 are atomic tasks** (`T<phase>.<n>`), each a single
  self-contained, verifiable unit of work, listed in dependency order — the same granularity Phase
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

- [x] **T8.1 Full work-graph projection + cold-start rebuild** — *done.*
- [x] **T8.2 Scheduler dispatches off projection status** — *done.*
- [x] **T8.3 `integrated` as a durable state + correct epic rollup** — *done.*
- [x] **T8.4 Control-room projection-backed read model** — *done.*
- [x] **T8.5 Planner decomposition granularity** — *done.*
- [x] **T8.6 Name the real merge target in the log** — *done.*
- [x] **T8.7 Wizard one-root-in-epic hardening** — *done.*

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

- [x] **T9.1 `core.Finding` + findings on `CheckResult`/`GateVerdict`** — *done.*
- [x] **T9.2 `internal/gotest` parser** — *done.*
- [x] **T9.3 `run_tests` agent tool (first consumer)** — *done.*
- [x] **T9.4 Build precondition + tri-state in the gate** — *done.*
- [x] **T9.5 Gate verdict carries findings** — *done.*
- [x] **T9.6 Scanner finding adapters** — *done.*
- [x] **T9.7 `run_gate` full self-check tool** — *done.*
- [x] **T9.8 Failure-aware retry Brief** — *done.*

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

## Phase 10 — Read-model concurrency correctness (demo-hardening II)

Opened from the **2026-06-23 live vault-demo run**. The feature *"one-time share links for
secrets"* planned and decomposed cleanly into four children (`tbs`→`0sp`→{`8wi`,`vzr`}), and the
test-author stage ran — but **every harvested result was dropped** (`orchestrator: result for issue
not in flight, ignoring as stale/duplicate`) and the run wedged at `author-tests`: no child advanced
to `implement`, no failure retried or dead-lettered. Root cause is a **three-part concurrency race**
that Phase 8's read-model work introduced the last ingredient of:

1. **`bd.Apply` is non-atomic** (`internal/beads/transitions.go`): Phase 1 `bd create`s each child
   (no dependency edges yet); Phase 2 adds the `blocked-by` edges as *separate* `bd dep add` calls.
   Between the two, a child is visible to `bd.ready()` as **dispatchable** (no blockers).
2. **Three loops mutate the projection with no shared serialisation** beyond the per-op mutex inside
   `inflightProjection` — the Result consumer, the approval consumer, and the tick loop. The
   "single writer = single process" reasoning in `orchestrator.md` assumed the projection always
   gates same-issue contention; it does **not** for an issue still being *created* (see that spec's
   new "*The creation window*").
3. **`inflight.track()` unconditionally writes `status=open`** (`internal/orchestrator/inflight.go`)
   — the creation-tracking added in **T8.4** — so when the tick loop claims a child (`add`,
   `in_progress`) inside the `bd.Apply` window and the creating loop's `applyTracked` `track()` then
   runs, it **clobbers the live claim back to `open`**. From then on `has()` is false forever, the
   result is dropped, and `bd.ready()` (which sees `in_progress`) never re-surfaces it. Permanent
   stall. The same window also **bypassed the dependency order** — all four children dispatched at
   once because their edges did not yet exist when claimed.

The spec change landed first (per CLAUDE.md): `orchestrator.md` "*The creation window — where the
projection does not yet gate*" (the two invariants any creation path must honour) + a "What it must
never do" bullet; `control-room.md` promotes the inter-child `blocked-by` "waits-for" edge from a
deferred "may later" to a committed board overlay (T10.4). T10.1–T10.3 are the correctness spine
(do first; they are TCB — orchestrator — so stay human-reviewed, land behind tests); T10.4–T10.5
are clarity/discipline. **A clean re-run gates T10's close** (and re-opens the #6 question below).

- [x] **T10.1 `track()` must not downgrade a live claim** — *done.*
- [x] **T10.2 Creation atomic w.r.t. the dispatch oracle** — *done.*
- [x] **T10.3 Regression test — concurrent decomposition under a two-phase `Apply`** — *done.*
- [x] **T10.4 Board "waits-for" dependency edges** — *done.*
- [x] **T10.5 Agent build-command discipline** — *done.*

**Phase 10 build items (T10.1–T10.5) are complete.** The remaining gate on closing the phase is a
**clean `./demo/vault` re-run** (re-opens the #6 turn-budget question and verifies the board no longer
shows `open`/`queued` on active work) — a runtime validation, not a build item, blocked only on a live
run (Docker + a capable model).

**Carried forward / not separate tasks:** `trace_test` (list item #1) is **not a bug** — it is the
test-author's test↔spec traceability tool (`verification.md`, `components/agent.md`); no change. The
board showing `open`/`queued` on active work (#2) is a faithful render of the projection corrupted by
this race — **fixed by T10.1**, verify on the re-run. The `vault-8wi` 50-turn exhaustion (#6) was
worsened by out-of-order dispatch (it was implemented-against a `0sp` store layer that did not yet
exist) — **re-measure after T10.2**; only then decide the deferred test-author turn-budget backstop
(left open by T8.5).

---

## Phase 11 — model-layer capability fields & tool-call observability

Cost/quality dials and a traceability gap surfaced while tuning the `./demo/vault` run on
hosted models. The spec change landed first (per CLAUDE.md): [models.md](specs/models.md)
"*Optional capability fields*" (per-model config the adapter emits, **not** canonical-`Request`
fields, so the agent stays provider-unaware) and the two new model-entry fields in
[configuration.md](specs/configuration.md). None of this is TCB; all land behind unit tests.

- [x] **T11.1 Reasoning-effort field** — *done.* `effort: low|medium|high|xhigh|max` on a model
  entry; the Anthropic adapter emits `output_config.effort` (a `WithEffort` builder threaded by
  the registry). Config validation checks the level and rejects it on non-Anthropic providers.
  Wire-body + validation tests.
- [x] **T11.2 Prompt caching** — *done.* The agent loop re-sends a large stable prefix every turn
  (persona in `System` + the Brief in `messages[0]`) that grows only at the tail, so without caching
  each turn re-pays full input price for the whole prefix — the single largest cost on the loop.
  **Anthropic adapter caches unconditionally** (`applyCaching` in `toParams`): two ephemeral
  `cache_control` breakpoints via the SDK — the first message's first block pins the stable
  `tools+system+Brief` prefix (a cache read after turn one), and the last message's last block is
  the **moving breakpoint the provider auto-advances** over the growing conversation (each turn reads
  the previous prefix, the ~1.25× write bills only the new tail); a marker below the provider's min
  cacheable size is silently ignored, so marking is always safe. **openai-compat adapter caches
  opt-in** (`WithPromptCaching`, off by default): when on, the first user message (the Brief) goes on
  the wire as a structured content array whose text part carries a `cache_control` breakpoint via the
  SDK's `SetExtraFields` escape hatch — its prefix covers the leading system message too. Opt-in
  because that surface is mixed (OpenAI/DeepSeek auto-cache and need no marker; a strict local server
  rejects it), so the marker is sent only where a backend both needs *and* accepts it — an Anthropic
  model behind a gateway (OpenRouter). **Usage:** `fromMessage` already mapped Anthropic cache tokens;
  `fromCompletion` now also recovers the gateway's **cache-WRITE** count from the non-schema usage
  field (`cache_creation_input_tokens` and siblings, checked on both the usage object and its
  prompt-token details, mirroring `reasoningDelta`) into `CacheCreationTokens`, so the ~1.25× write
  shows in USD accounting, not just `cached_tokens` (reads). **Config:** new `prompt_caching` bool on
  the model entry; `validateModels` restricts it to `provider: openai-compat` (the native anthropic
  caches unconditionally, native openai auto-caches — the flag would silently no-op elsewhere). The
  registry chains `.WithPromptCaching(mp.PromptCaching)` on the compat branch. **Demo wired:** both
  Anthropic-via-OpenRouter entries in `demo/vault/config/infra.dev.yaml` set `prompt_caching: true`
  and gained `cache_write_per_mtok`/`cache_read_per_mtok` (≈1.25×/0.1× input) so a cached run's USD
  figure is accurate. Tests: anthropic `TestToParamsMarksCacheBreakpoints` (breakpoints on first+last,
  interior unmarked); openai `TestToParamsCachingMarksBrief` (array+cache_control when on, plain
  string when off) and `TestFromCompletionMapsCacheWrite` (gateway cache-write → `CacheCreationTokens`);
  config `TestValidatePromptCaching{RejectedOffOpenAICompat,OnOpenAICompatPasses}`. Specs were written
  ahead (models.md "Optional capability fields", configuration.md `prompt_caching`) — no spec change;
  docs/configuration.md gained the `prompt_caching` field doc. **Verify `cache_read_tokens` goes
  nonzero on a live re-run** (a runtime check, not a build item — needs a hosted key). ([models.md](specs/models.md),
  [configuration.md](specs/configuration.md))
- [x] **T11.3 Tool-call spans for in-sandbox tools** — *done.* The agent loop opens a `tool-call`
  span per tool invocation (parented to the invocation), closing the [observability.md](specs/observability.md)
  `tool-call ×M` gap for the unbrokered workspace/lifecycle tools — previously only the broker's
  egress tools (git-push, package-fetch) were traced. Caveat in code: this works while the loop is
  co-located in the trusted runner; once the agent is its own in-sandbox binary (Phase 5) these
  must ride the broker.

---

## Phase 12 — Distilled explore tool (helper souls)

Adds `explore`: a read-only comprehension tool that answers a **broad, multi-step** question
by running a nested agent loop on a **cheap** model — the iterative search→read→refine — and
returning a distilled `{summary, anchors, coverage, leads}` instead of the raw reading, so the
intermediate reading never bloats the parent's window or burns its (frontier) tokens. It is
the first **helper soul**: a soul invoked *as a tool* by the runner (reusing the `Soul` struct
and the one agent loop), **off the DAG**, running in-process in the parent's sandbox so it
reuses the warm LSP session. Motivation is context + cost — the same signal-density goal as
Phase 9, one level up (collapse an iterative multi-turn search into one cheap sub-loop).
Additive and never load-bearing: the parent keeps every raw comprehension tool, and it does
**not** fight prompt caching (T11.2) the way in-loop compaction would — an explore call is one
tool-call/result appended at the tail, leaving the cacheable prefix intact.

**Spec landed first** (per CLAUDE.md, this session): [components/agent.md](specs/components/agent.md)
"Explore — distilled comprehension" (the tool, the five rules, the free-form-in/structured-out
contract), [models.md](specs/models.md) "Helper souls — two models in one sandbox" (sub-context
selector + trusted-pinned model resolution + fixed sub-budget), [configuration.md](specs/configuration.md)
(`policy.explore_budget`, the reserved `explorer` role, the explorer soul shape, verify-path
diversity), [messaging.md](specs/messaging.md) (broker sub-context selector),
[verification.md](specs/verification.md) (explore's correlated-blind-spot risk on the verify
path), [glossary.md](specs/glossary.md) (Explore, Helper soul).

**TCB note:** T12.1/T12.2 touch the agent loop + runner/broker (TCB) — stay human-reviewed;
land behind tests. **Deterministically testable** (per [models.md](specs/models.md)): the
explorer's cheap model is just a soul resolved from config, so `modeltest` scripts the whole
machinery — read-only enforcement, the `answer` contract, no-recursion, sub-budget
harvest-on-breach, and the runner refusing an agent-chosen model — with no key and no Docker.

Build order (each a single, independently testable concern; the two TCB pieces first):

- [x] **T12.1 Canonical `explore` tool + in-process nested read-only sub-loop** — *done.* New
  `internal/agent/explore.go`: the canonical `explore(question)` tool + the explorer's one lifecycle
  tool `answer(summary, anchors, coverage, leads)` (with `ExploreAnswer`/`ExploreAnchor` + the three
  `Coverage*` constants), and `runExplore` — the nested read-only ReAct sub-loop. It mirrors the main
  loop's request→complete→dispatch→append shape but **terminates on a value** (the `ExploreAnswer`
  captured by the `answer` tool into an `exploreSink`) instead of a `core.Result`, and — the
  load-bearing difference — **never surfaces an error to the parent**: a model-call error → a
  `partial-uncertain` answer, a turn/token-budget breach → `partial-budget`, so explore stays
  additive/never-load-bearing (an explore failure just routes the parent back to searching itself).
  **Read-only + no-recursion are structural, not runtime guards:** the sub-loop's toolset is exactly
  `ReadOnlyTools(sb, sessions)` + `answer`, so no `edit/write/run/submit/escalate/request_subtask`
  and no `explore` is ever *built* (a forbidden call hits the loop's unknown-tool path). Reuses the
  parent's warm sandbox + LSP `Sessions` (no reseed); the child's opening turn is the **question +
  ambient specs (project map), NOT the parent conversation** (per spec). **Consolidation (single
  source of truth):** promoted the read-only comprehension subset into exported
  `agent.ReadOnlyTools` (+ `keepTools`) and refactored `internal/controlroom/wizard/explore.go` to
  call it, deleting the wizard's duplicate `readOnlyToolsOver`/`filterTools` so the two can't drift
  into leaking a writer. **Model-call seam:** added `Completer` to `agent.Invocation` (loop.go now
  passes the same brokered `conn`), so a tool can drive its own nested model loop keyless/provider-
  unaware exactly like the main loop. **T12.1/T12.2 boundary (important for the next loop):** the
  sub-loop is model-agnostic — it calls whatever `Completer` it is handed. In the co-located world
  today that is the parent's `conn`, so the child currently runs on the *parent's* model; **T12.2**
  is what pins the cheap explorer model + separate sub-budget metering by wrapping the completer with
  a sub-context selector the runner honors. **Not yet wired into `cmd/harness/run.go`'s toolSource**
  — that needs the explorer soul config (`policy.explore_budget`, the reserved `explorer` role) from
  T12.3 and per-role enablement from T12.5; T12.1 lands the fully-tested component. `agent.Budget`
  reused for the fixed sub-budget; `DefaultExploreMaxTurns = 12` (matches the configuration.md
  `explore_budget` example). Tests (`internal/agent/explore_test.go`):
  `TestExploreReturnsDistilledAnswer` (persona as System, question+project-map opening turn, non-
  terminal rendered answer), `TestExploreReadOnlyToolset` (exact read subset, no writers/lifecycle/
  explore), `TestExploreRejectsForbiddenTools` (write_file + explore → unknown-tool at dispatch),
  `TestExploreTurn/TokenBudgetExhausted` (→ partial-budget, capped call count),
  `TestExploreModelErrorDegrades` (→ partial-uncertain, never fatal),
  `TestExploreInvalidAnswerRetries` (empty summary / bad coverage / complete-without-anchors → IsError
  retry), `TestExploreToolDefAndEmptyQuestion`. Spec was written ahead (components/agent.md "Explore —
  distilled comprehension") — no spec change; no CLI/config/control-room surface yet, so no docs/
  update. ([components/agent.md](specs/components/agent.md))
- [x] **T12.2 Broker sub-context selector + runner-pinned model + fixed sub-budget metering** — *done.*
  **Broker protocol:** `MethodCompletion` now carries `broker.CompletionParams{SubContext, Stream, Request}`
  (the canonical `model.Request` stays clean — the selector is a completion-only wire concern). `SubContext`
  is `""` (parent) or `"explorer"`; the client's `Complete` sends parent, and the new `Client.ExploreCompleter(stream)`
  returns a `*SubCompleter` tagging every call `explorer`+stream. `Handler.Complete(ctx, CompletionParams)` (the
  interface changed — updated the runner relay + the gate/wizard deny handlers + `fakeHandler`). New
  `CodeSubBudgetExhausted` error code; `server.dispatch` now routes handler errors through `handlerErrorResponse`,
  which **preserves a typed `*broker.Error` code** across the boundary (previously every handler error flattened to
  `CodeHandlerError`) so the sub-loop can distinguish a budget breach. **Runner (the TCB piece):** the relay holds a
  SECOND pinned adapter (`exploreAdapter`/`exploreModel`/`exploreBudget`); `Complete` routes by sub-context —
  `completeParent` is the old path, `completeExplore` routes to the pinned explorer adapter, **fails closed** if no
  explorer adapter is pinned (never silently answers on the parent's frontier model — the tier-escape guard), and
  meters **per-call by stream** against `policy.explore_budget.Tokens`: a breach returns `CodeSubBudgetExhausted`
  (→ `partial-budget`), a fresh stream resets (so multiple explore calls each get the full fixed budget — "behaves the
  same wherever in an invocation it is made"). Explorer tokens also feed `Usage()` (combined with the parent tally) so
  the explorer's spend **draws the parent-task ceiling** the orchestrator enforces. `runner.go` resolves the explorer
  adapter from `brief.Explorer.Model` (non-fatal on failure — explore is additive; a bad explorer model disables the
  helper, not the invocation). **Dispatch pins the model, not the agent:** `core.Brief` gained `Explorer *core.Soul` +
  `ExploreBudget core.ExploreBudget`; the orchestrator's `attachExplorer` (in `buildBrief`) selects the `explorer`-role
  soul by the **same selector algorithm** (`selectSoulForRole`, factored out of `selectSoul`) keyed on the issue's
  tags — so a `verify=1`-tagged issue routes to a diverse verify-path explorer — and pins it + the budget onto the
  Brief only when `policy.explore_budget` is enabled. **Single source of truth:** `ExploreBudget` moved to
  `core` (with yaml+json tags + `Enabled()`); `config.ExploreBudget` is now a **type alias**, so the config surface and
  the wire type are one thing. **Agent seam:** new `agent.ExplorerCompleterSource` interface (`ExploreCompleter(stream) Completer`);
  `ExploreTool` takes it and mints a fresh per-call stream (atomic counter) so each explore call gets its own metered
  stream; `runExplore` maps a `CodeSubBudgetExhausted` broker error → `partial-budget` (any other error stays
  `partial-uncertain`), never failing the parent. New telemetry `AttrSubContext` labels the explorer llm-turn.
  **Not yet wired into `cmd/harness/run.go`'s toolSource** — that (building the source adapter over `*broker.Client`
  and offering `explore` to producer/implementor personas) is T12.5; T12.6 wires the vault demo. Tests: broker
  `TestExploreCompleterTagsSubContext`, `TestHandlerErrorCodePreserved` (+ parent-tag assertions on the round-trip);
  runner `TestRelayExploreRoutesToPinnedAdapterAndDrawsCeiling`, `TestRelayExploreSubBudgetRefusesPerStream`,
  `TestRelayExploreDisabledFailsClosed`; agent `TestExploreRunnerSubBudgetDegradesToPartialBudget`; orchestrator
  `TestAttachExplorer`. `harness validate` on both shipped configs still OK (neither enables explore). Specs were
  written ahead (messaging.md "sub-context selector", models.md "Helper souls", agent.md "Explore" rules 3–4) — no
  spec change; no CLI/config/control-room surface change (the wire + Brief are internal; `explore_budget` config
  shape is unchanged), so no docs/ update. ([messaging.md](specs/messaging.md), [models.md](specs/models.md))
- [x] **T12.3 `policy.explore_budget` config + reserved `explorer` role + validation** — *done.*
  New `config.ExploreBudget{Tokens, Turns}` (a distinct struct — the per-issue `Budget` is
  tokens/USD/wall; explore is dimensioned in tokens+turns, a sub-loop concept) on
  `config.Policy.ExploreBudget yaml:"explore_budget"`, with an `Enabled()` predicate (any positive
  dimension = on) so **"omitting the block disables explore" lives in exactly one place**. Reserved
  `RoleExplorer = "explorer"` constant. **Validation:** `validateSouls` now exempts the explorer
  role from the "role which no dag stage uses" check (it is invoked as a tool, never scheduled);
  new `validateExplore` enforces the rest — budget dimensions ≥0; if `explore_budget` is set, ≥1
  `explorer` soul must exist; every explorer soul's declared `tools` must be a subset of the
  read-only comprehension names and must **never** include `explore` (structural no-recursion,
  caught at startup, not as a silent inert soul). The existing selector-duplicate rule already
  covers two catch-all explorers (a verify-path explorer must differ by selector, e.g. `verify=1`).
  **Single-source-of-truth for the tool names:** `config.ReadOnlyToolNames` is the canonical
  comprehension subset the validation checks against; `config` cannot import `agent` (agent imports
  config), so the anti-drift guard is an agent-side test `TestReadOnlyToolsMatchConfigNames`
  asserting `agent.ReadOnlyTools(...)`'s names equal that list. Tests
  (`internal/config/validate_test.go`): `TestValidateExplorerRoleExemptFromDAGCheck` (explorer soul
  + explore disabled → clean), `TestValidateExploreHappyPath` (enabled + valid explorer → clean),
  `TestValidateExploreBudgetRequiresExplorerSoul`, `TestValidateExplorerToolsMustBeReadOnly`,
  `TestValidateExplorerNoRecursion`, `TestValidateExploreBudgetNegative`,
  `TestValidateExplorerDuplicateSelector`. Both shipped configs still `harness validate` OK (neither
  enables explore). Specs written ahead (configuration.md "Validation is a safety feature" + the
  reserved-role/explorer-soul shape) — no spec change; **docs/configuration.md** updated (the
  `explore_budget` policy field + the reserved `explorer` role subsection) per the doc-tracking rule.
  ([configuration.md](specs/configuration.md))
- [x] **T12.4 Explore evidence + provenance + observability nesting** — *done.* The explore
  sub-loop is now first-class, auditable evidence. **Separate transcript capture:** the relay
  (`internal/runner/broker_handler.go`) gained `exploreTurns []model.TranscriptTurn`; `completeExplore`
  calls a new `recordExplore(req,resp)` that appends to it and **never** touches `firstReq`/`turns` —
  the explorer's question is not the invocation's prompt, so it must not contaminate the Prompt-SHA /
  parent transcript. New `ExploreTranscript() ([]byte,bool)` (mirrors `Transcript`) and `ExploreModel()
  (string,bool)` (returns the pinned model only when the sub-loop actually ran — the "explore happened"
  gate). **Harvest:** `runner.go`'s `harvest` stores the explore transcript as its own
  `core.ArtifactKindExploreTranscript` artifact alongside the main transcript (a store failure degrades
  provenance, never fails the candidate — same discipline as the main transcript); `invoke` stamps
  `res.ExploreModel` from `rel.ExploreModel()` (authoritative, from the trusted relay). **Provenance:**
  `core.Result` gained `ExploreModel`; `core.Provenance` gained `ExploreModel`+`ExploreTranscript`, and
  the trailer renders an **optional third line** `Explorer-Model: … | Explore-Transcript: …` only when
  set — pre-explore commits stay byte-for-byte identical and round-trip exactly (parse recognizes the
  two new keys). `orchestrator/provenance.go` populates both. **Observability nesting:** `tokenEvent`
  gained `SubContext string omitempty`; explorer token/reasoning/tool events are tagged
  `broker.SubContextExplorer` (parent events stay unlabelled → byte-identical), threaded through
  `controlroom/live` `wireEvent`/`Entry` (coalesce now requires matching SubContext, so explorer
  reasoning never folds into a parent turn) and the activity/invocation templ views (indented, `explorer`
  badge). docs/control-room.md updated. Tests: `TestRelayExploreCapturedSeparatelyFromParentTranscript`,
  `TestRelayExploreEventsCarrySubContext`, `TestHarvestStampsExploreEvidence`, provenance round-trip +
  `TestTrailerExploreLineOptional`, `TestActivity_ExplorerSubContextNestsSeparately`. No spec change
  (written ahead). Built in an isolated worktree (parallel with T12.5), `make check` green, reviewed on
  merge. ([components/agent.md](specs/components/agent.md), [observability.md](specs/observability.md))
- [x] **T12.5 Per-role explore enablement + verify-path diversity advisory** — *done.* Wires the
  explore tool into the production toolset, gated per-role, and extends the T2.13 advisory.
  **Enablement gate** (`cmd/harness/config.go` `exploreToolFor`, called from `run.go`'s `toolSource`):
  `explore` is offered only when **both** — the parent soul opted in via its `tools` allowlist
  (`soulEnablesTool`; the per-role surface that resolves the agent.md OPEN — planner/implementor opt
  in, verify path only via a diverse explorer) **and** the trusted dispatch pinned an explorer for the
  issue (`inv.Brief.Explorer != nil`, set by the orchestrator's `attachExplorer` from T12.2).
  `buildExploreTool` reads the explorer persona off the host (absolute by dispatch time, like `bootSoul`),
  maps `Brief.ExploreBudget{Turns,Tokens}`→`agent.Budget{MaxTurns,MaxTokens}`, reuses the invocation's
  **warm LSP sessions**, and hands the explorer `inv.Brief.Spec` (the ambient prefix + slice) as its
  project map — deliberately not the parent's conversation. `exploreCompleterSource` adapts
  `*broker.Client` (via `ExploreCompleter(stream)`) to `agent.ExplorerCompleterSource` (broker can't
  import agent) at the composition root; a guarded assertion + build-failure path **degrade to no explore**
  (additive, never load-bearing). **Advisory** (`internal/config/warnings.go` `warnExploreDiversity`,
  wired into `Warnings()`): extends the T2.13 producer/verifier model-family overlap advisory to explore —
  when explore is on and a producer path and its downstream verifier gate both enable `explore` and the
  whole `explorer` pool resolves to a single model family (so the verify path can't be routed to a
  diverse explorer), it emits the same **non-fatal** advisory recommending a diverse verify-path explorer
  (or none); reuses `roleFamilies`/`isProducerStage`/`downstreamStages`/`isGateStage`. docs/configuration.md
  documents the per-role opt-in + the advisory. Neither shipped config enables explore, so both still
  `harness validate` clean (that's T12.6). Tests: `cmd/harness/explore_test.go` (offered when soul opts in
  + explorer pinned; absent when soul omits it or Explorer nil; degrades on a non-broker completer);
  `internal/config/explore_diversity_test.go` (fires same-family; silent when diverse / disabled / verifier
  opts out; never fails Validate). No spec change (written ahead). Built in an isolated worktree (parallel
  with T12.4), `make check` green, reviewed on merge. ([verification.md](specs/verification.md),
  [configuration.md](specs/configuration.md))
- [x] **T12.6 Wire the vault demo (the exercising use case)** — *done.* Added the cheap
  `anthropic/claude-haiku-4.5` entry to `demo/vault/config/infra.dev.yaml` (OpenRouter openai-compat
  + prompt caching + cost, like the others); added the `vault-explorer` soul
  (`souls/explorer.yaml`, role `explorer`, that model, the read-only comprehension `tools` subset,
  catch-all selector, `vault-toolchain` sandbox) reusing the already-drafted `prompts/explorer.md`
  (removed its "not yet wired" banner); set `policy.explore_budget: { tokens: 100_000, turns: 12 }`
  in `harness.yaml`; and enabled `explore` on the **planner + implementor** souls (added `explore`
  to their `tools` allowlist — the T12.5 per-role opt-in). Differences row added to
  `demo/vault/README.md`; `docs/configuration.md` already covers the surface (T12.5). `harness
  validate --config demo/vault/config` = **OK (6 souls, 3 models)**; the only advisory is the
  pre-existing T2.13 producer/verifier same-family one, and — as designed — the T12.5
  explore-diversity advisory does **not** fire.
  **Deliberate deviation from the task's literal "diverse-family verify-path explorer for
  security" (with rationale):** the verify path (`qa`, role `security`) enables **no** explore.
  Two reasons. (1) On the merits, `explore` is a producer-side *broad-localization* accelerant
  (planner finds decomposition seams; implementor finds where layers hook up); the qa gate
  re-grades independently and should read raw for full producer≠verifier independence — the
  spec's explicitly-endorsed stricter alternative (verification.md "verify-path diversity").
  (2) **Machinery gap found this session:** an issue's `Tags` thread forward *unchanged* across
  all of a child's stages (`internal/orchestrator/results.go` — author-tests/implement/qa share
  one tag set) and there is no per-stage/role tag, so a `verify=1`-selectored explorer soul
  cannot be routed to *only* the qa stage of a child — tagging the child `verify=1` would route
  its producer stages to the "verify" explorer too. So a *routed* diverse verify-path explorer is
  not honestly wireable today; a two-family explorer pool would silence the static advisory
  without actually diversifying at runtime. Filed as a follow-up below. **Validate the
  context/cost win on a live vault re-run** (a runtime check, not a build item — broad
  localization on the established Go app, the same loop Phase 9 targets). ([configuration.md](specs/configuration.md))

---

## Phase 13 — Live-demo hardening II (context cost + wizard kickoff)

Opened from a **2026-07-02 pass** preparing the `./demo/vault` run for a live security-meetup
talk. The organizing complaint was the operator's own experience — "implement was slow and burned
tokens" — traced to two shared roots: the OpenAI-compat model path under-caching the growing
conversation, and per-invocation config caps that fought a healthy deep run. Plus one net-new
control-room affordance so a time-boxed stage kickoff doesn't depend on live typing. None of this
is TCB except where noted; all behind unit tests, `make check` green.

- [x] **T13.1 openai-compat moving-tail cache breakpoint** — *done.* Corrects the T11.2 compat gap:
  the openai-compat adapter marked **only** the Brief (stable prefix), so the accumulated tool
  results — which dominate a deep run's input by an order of magnitude — re-billed at full price
  every turn (quadratic input growth; the observed ~1.35M-input runaway). It now marks the **second,
  moving breakpoint on the last message** too, matching the Anthropic adapter's `applyCaching`: a
  `RoleUser` tail reuses `cachedUserMessage`; a `RoleTool` batch marks only its **final** result via
  the SDK's generic `ToolMessage([]ContentPartText…)` + `SetExtraFields` (`cachedToolMessage`); a
  `RoleAssistant` (never last in the loop) falls back to the plain form. Turn 1 the Brief is also
  last, so the Brief branch's `continue` correctly leaves one breakpoint. Tests:
  `TestToParamsCachingMarksTail` (final tool result cached, non-final unmarked, Brief keeps its own
  breakpoint, plain string when caching off). Spec updated ahead: [models.md](specs/models.md)
  "Prompt caching" now describes the two-breakpoint scheme for **both** adapters and the ~5-min
  provider cache-TTL floor (a slow `run_gate`/`run_tests` between turns forfeits the cache — a
  provider property, not an adapter bug). **Verify `cache_read_tokens` goes nonzero and per-turn
  input stops growing on a live vault re-run** (runtime check, needs a hosted key). ([models.md](specs/models.md))
- [x] **T13.2 Wizard `requirements_planner.prefill` — insert-a-prepared-requirement button** — *done.*
  A new optional `prefill` field on the `requirements_planner` config block naming a text/markdown
  file (resolved against the config root like `persona`; `harness validate` checks it exists). When
  set, the Create-Task composer shows an **"Insert prepared requirement"** button that drops the
  file's content into the textarea (`data-prefill` attribute → `wizardChat().insertPrefill`, an
  insert-only move — the operator still reviews and presses Enter, so the send *and* the APPROVE
  consent gate stay deliberate human acts). Read per page load, so the prepared text can be refined
  while the harness runs. Resolve mode passes `""` (a dead-letter conversation has no canned
  opening). Not TCB (control-room surface). Config: `RequirementsPlanner.Prefill` +
  `RequirementsPlannerPrefillPath()` + a `validateRequirementsPlanner` existence check;
  `CreatePage`/`wizardConversation` gained a `prefill` param; `Server.prefillText` reads it
  (nil-cfg- and read-error-safe → no button). Tests: config
  `TestValidateRequirementsPlannerPrefill{Exists,Missing}`, server `TestCreatePrefillButton`
  (rendered with a config, absent without). Docs: `docs/configuration.md`, `docs/control-room.md`.
  **Demo wired:** `demo/vault/config/harness.yaml` sets `prefill: prompts/share-link-request.md`, a
  fully-specified one-time-share-link requirement that pre-resolves every design fork (token-derived
  re-seal, hash-only storage, atomic burn, 1h expiry, audit actions) and instructs the planner to
  ledger them `agreed` and draft in one turn, seeding exactly one epic root. ([configuration.md](specs/configuration.md),
  [control-room.md](specs/control-room.md))
- [x] **T13.3 Demo config + persona tuning for the time-boxed run** — *done.* Config-and-persona
  only (no code): (a) **walls** — per-sandbox `limits.wall` 12m→**18m** (an observed *healthy* deep
  test-author run took ~19m; 12m wall-killed a legitimate attempt) and per-issue `policy.budget.wall`
  20m→**40m** (~2× the sandbox wall, so a healthy attempt + one full retry both fit; the old 20m/12m
  pairing left a wall-killed attempt's retry only ~8m — a guaranteed dead-letter). (b) **test-author
  explore** — added `explore` to `souls/test-author.yaml` `tools` (the demo's proven raw-navigation
  token hog, ~1.3M tokens in one live run; its output is still independently gated, so a weak
  explorer costs at worst a re-search). (c) **running-plan persona convention** — a short paragraph
  in the implementor and test-author personas telling the soul to state a numbered plan before
  editing and re-state remaining steps after a failed self-check; the plan lives in the soul's own
  assistant messages, so it survives context growth (and any future tool-result aging, T-below) and
  directly counters the observed goal-drift/over-build that burned turn budgets. `harness validate
  --config demo/vault/config` OK. **Validate the cost/pass-rate win on a live re-run.**
- [x] **T13.4 Wizard rejection backstop — a rejected `propose_draft`/`update_ledger` is never
  silent** — *done.* Fixes the filed "silently-swallowed `propose_draft` rejection" (observed
  live 2026-06-24: the model emitted Python-style `True` in the draft JSON; `harvestDraft`
  rejected it with only a WARN, and because an output-only reply concludes `converse`
  immediately, the `IsError` ack was **dropped** — the model never learned to correct itself
  and the human saw neither a draft nor an error). Fix in `converse`
  (`internal/controlroom/wizard/wizard.go`): the output-call partition now tracks per-round
  `draftRejected`/`ledgerRejected`; when an output-only turn carries a rejection that actually
  **lost state** (`rejected && !out.draftSet` — a duplicate call rejected after a sibling
  succeeded latest-wins has nothing to recover), the error acks are fed back for **one
  corrective round-trip** (`rejectRetried`, one-shot per human turn like `nudged`, so it can
  never loop) and the ephemeral tool strip broadcasts `rejectionLabel` ("draft rejected
  (malformed args) — retrying") so the retry is visible to the human. Ordered **before** the
  draft-nudge check (a rejected call deserves its specific parse error, not the generic
  "you described a draft without emitting the call" nudge). A rejection riding a round that
  also had exploration calls already fed its ack back (unchanged). Tests (modeltest, real
  compat adapter): `TestDraftRejectionFeedsErrorBackForRetry` (malformed→corrected two-turn
  script; asserts the tool-strip notice, the harvested draft, clean transcript),
  `TestDraftRejectionRetriesAtMostOnce` (both turns malformed; two-turn server over-run is
  the termination proof; turn-2 prose avoids `announcesDraft` phrases so the nudge can't add
  a third call). docs/control-room.md wizard section documents the strip notice. Deliberately
  NOT done here (still filed below): the `announcesDraft` phrase broadening, JSON-slip repair
  in `parseDraftArgs`, and the `max_tokens`-truncation guard.
- [x] **T13.5 Tool-result aging in the agent loop** — *done.* The runtime half of Phase 9's
  context discipline (was the "Tool-result aging" deferred bullet; all its recorded design
  decisions implemented as decided). **Spec landed first**: components/agent.md "Tool-result
  aging" (the five rules: only tool-result content ages; batch cadence not sliding horizon;
  <~1KiB exempt; pure derived view, no LLM summarization; evidence unaffected in substance) +
  observability.md metrics bullet. **Mechanism** (`internal/agent/aging.go`): `agedView(msgs)`
  is a pure function — the model's request carries a copy of the history in which tool-result
  `Content` older than the elision boundary is replaced by a deterministic stub
  (`[read_file {"path":…} — result elided (round N); the worktree is current, re-run the tool
  if you need it]`); the loop's own `messages` stays pristine (the source future views derive
  from). Boundary = `((rounds-K)/B)*B` with K=8 keep, B=8 batch (`elideKeepRounds`/
  `elideBatchRounds`/`elideMinBytes=1024`, non-configurable constants for now) — first elision
  at 16 rounds, view byte-stable between advances so the T13.1/T11.2 cached prefix survives,
  one cache re-write per batch. Brief + assistant messages (the running-plan trail, T13.3)
  never touched; `ToolCallID`/`IsError` survive elision; tool name+args for the stub are
  correlated from the paired assistant message (ToolResult carries only the ID). **Evidence
  verified by recon before building:** the transcript is recorded relay-side from the wire, so
  it faithfully shows what the model saw; every elided result's full content still appears at
  its first appearance (new results always ride the un-aged tail), replay renders first
  appearances, and PromptSHA covers only the first request (no tool results) — nothing lost
  forensically. **Explore sub-loop deliberately un-aged** (12-turn cap on a cheap model, below
  any threshold worth managing; noted in the spec). **Observability:** new counters
  `harness.context.elided.results`/`…bytes` (by role — bounded; bytes not tokens, the loop
  never tokenizes) via `Provider.RecordContextElision`, recorded as a DELTA on boundary
  advance (the view is recomputed per request; totals would double-count); loop wired through
  new `agent.WithMetrics(tel)` (defaults `telemetry.Noop()`, mirroring `WithTracer`) in
  `cmd/harness/run.go`. TCB (agent loop) — human-reviewed, landed behind tests. Tests:
  `aging_test.go` (`TestAgedViewBelowThresholdUntouched`, `…BoundaryQuantized` (8→hold→16),
  `…PurityAndStability` (no mutation; stubs byte-identical across views/advances),
  `…SmallResultsExemptAndIdentityKept`, `TestElideStubShape` (UTF-8-safe truncation));
  `TestToolResultAgingOnTheWire` (18-turn scripted run: turn-16 request unelided, turn-17
  stubs rounds 1-8 keeps 9-16 + Brief + first-appearance tail, turn-18 aged region
  byte-identical, earlier captured requests unmutated); telemetry
  `TestRecordContextElisionByRoleSkipsZero` + Noop-safety. **Validate the token/pass-rate
  effect on a live vault re-run** (the counters make it measurable); interacts with T13.3's
  walls — aging slightly raises turn count (occasional re-reads) while cutting per-turn cost.
  ([components/agent.md](specs/components/agent.md), [observability.md](specs/observability.md))
- [x] **T13.6 Decision: same-family verify-path explorer accepted — advisory removed, demo qa
  gets explore** — *done.* Operator decision (2026-07-02): a shared, same-family explorer on the
  verify path is acceptable — explore is **additive and never load-bearing** (agent.md contract),
  so a correlated explorer blind spot can degrade a verifier's *search*, never its *grade* (the
  gate's checks are deterministic and the candidate is re-graded in a fresh sandbox regardless);
  mandating helper diversity bought a second-order independence gain at the cost of a second
  cheap-model entry + stage-scoped selector routing with no other use. **Spec updated first**:
  verification.md (the explore paragraph now records the decision + reasoning; strict options —
  diverse second explorer, or no explore on verify — remain as pure config), configuration.md
  ("Verify-path explore is a policy choice"; the family advisory explicitly does NOT extend to
  explorers), agent.md OPEN-questions bullet trimmed to the still-open enablement-defaults part.
  **Code**: removed the T12.5 `warnExploreDiversity` advisory + its `roleEnablesTool` helper
  (`internal/config/warnings.go`, only they used each other) + `explore_diversity_test.go`; a
  NOTE in warnings.go records why there is deliberately no explore-family advisory (the T2.13
  main-model advisory stays — there the model IS the grader's judgment). docs/configuration.md
  explore section rewritten to match. **Demo**: `vault-security` now enables `explore` on the
  shared `vault-explorer` (the thing T12.6 could not wire honestly under the old
  recommendation — this decision supersedes T12.6's deviation note and obsoletes the
  stage-specific-selector-tags follow-up, dropped above); README differences row updated.
  `harness validate --config demo/vault/config` = OK, 6 souls, 3 models; only the T2.13
  main-model family advisory fires (as designed). ([verification.md](specs/verification.md),
  [configuration.md](specs/configuration.md))

---

## Deferred & follow-ups (filed, not blocking)

- **Wizard: `announcesDraft()` matcher too literal** *(same run)*. The draft-nudge backstop only fires when concluding prose matches a fixed phrase list (`"draft the spec"`, `"seed issues"`, …) — it missed the model's actual `"Drafting the spec and seed issue now"` (gerund `drafting` ≠ `draft the`; singular `issue` ≠ `issues`), so the nudge never fired and the promised draft never came. **Fix:** broaden to stems/keywords (`drafting`, `seed issue`, `propose_draft`, `proposing`) in `internal/controlroom/wizard/wizard.go`.
- Live-streaming replay (reconstruct the decision trail as the invocation runs) — needs the broker to emit structured per-turn events; overlaps the activity feed *(from T4.11)*.
- Consolidate the status bar's 2–3 per-page SSE connections (page content + status bar + alerts.js) onto one connection or h2c *(from T4.19)*.
- Client-side live wall/token ticker on the invocation budget meter (mid-invocation spend isn't persisted to beads) *(from T4.21)*.
- Decomposition-preview dry-run before APPROVE (control-room.md OPEN, "leaning defer"; seed issues stay coarse and the autonomous planner decomposes) *(from T4.14)*.
- **First-party thinking-block preservation** *(from the 2026-07-02 demo-prep pass)*. Through the openai-compat/OpenRouter path, Claude's interleaved thinking blocks are dropped between tool turns, so a deep-reasoning role (the Opus test-author) re-derives its plan from scratch each turn — a quality *and* token cost. The first-party `anthropic` adapter (already built) can preserve them across a tool loop. Strategic framing: the harness stays provider-unaware, but the frontier-Claude path should be **first-party by default** with compat as the portability fallback — this, native `effort`, and native cache-TTL control all being first-class there. A deployment/config choice plus verifying the anthropic adapter round-trips thinking blocks through the loop; no architecture change. Weigh against OpenRouter's single-key convenience for the demo.
- ~~Stage-specific selector tags for a routed diverse verify-path explorer~~ **Dropped by the
  T13.6 decision** (same-family verify-path explorer accepted): routed explorer diversity was this
  item's only motivation, so the stage/role-scoped selector input is no longer needed by anything.
  If a future feature wants per-stage soul routing for its own reasons, design it fresh then.

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
