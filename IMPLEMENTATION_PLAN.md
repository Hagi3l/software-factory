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
it proceeds the same way the kernel was built. **Nothing below is *blocked* by the
missing key** — only the autonomous self-hosting milestone and the requirements
wizard (T4.12) need a capable model *at runtime*; everything builds and tests
offline.

## How to read this

- **Phases 0–1** are **complete** — see Status. Their per-task findings were pruned
  from this plan; that history lives in git, the code, and the specs they informed.
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

- [x] **TE.1 Deterministic end-to-end spine test (fast, no Docker) + Docker variant** —
  *done.* `spec → implement → gate → merge` now runs end-to-end in one process against a
  fixture repo, the first test to exercise a full agent turn (run_test.go still only
  proves wiring). Both variants verified against real infra (local: ~2.7s in `make
  check`; Docker: against a built `go-toolchain` image). Learnings for downstream tasks:
  - **Fake model** lives in `internal/model/modeltest` (`NewServer(t, []Turn)`): an
    `httptest` SSE server speaking the OpenAI streaming wire format, scripted by request
    count. It drives the **real** `openai` adapter (resolved through the real registry) —
    `TestServerDrivesRealAdapter` pins that wire contract in isolation. **No production
    model-layer change**; the server is selected purely by an `openai-compat` model
    entry whose endpoint is patched to `srv.URL()` at runtime. Reusable by any future
    test needing a deterministic model (e.g. T2.4 author-tests, T3.1 planner).
  - **Local backend** is defined in `cmd/harness/spine_e2e_test.go` as **test-only**
    (compiled only under test, so the non-isolating host-exec backend can never ship). It
    is wired through the new `runOptions.backend` seam in `buildRunComponents` (nil →
    `NewDockerBackend()`, unchanged prod). The **same injected backend serves both the
    runner and the gate**, so the local path verifies candidates too.
  - **Fixture** is generated in a tempdir at runtime: a real non-bare git repo (`main` +
    initial commit), a `bd init --prefix harness` store seeded via `beads.Apply` (role
    `implementor`), and a config tree with `checks: { tests-pass: "true" }`. The scripted
    turns are `run` (commit the candidate branch) then `submit` — that two-turn script is
    what pins the run/submit tool contract. Gotcha: a soul's `persona` path resolves
    against the **config root**, so it needs the `souls/` prefix.
  - **Targets:** the fast e2e (`TestSpineE2ELocal`) is plain `go test`, in `make check`.
    The Docker variant (`TestSpineE2EDocker`) is behind `//go:build docker_e2e`, run via
    `make test-e2e-docker`; it skips cleanly unless Docker + the `go-toolchain` image are
    present (image is an operator prerequisite, overridable via `HARNESS_E2E_IMAGE`).
  - Confirmed still-latent: `buildRunComponents` ignores `cfg.Infra.Sandbox.Backend`
    (hardcodes Docker on the prod path). The injection seam is the *only* non-Docker
    route, so `"local"` is never config-reachable. Honoring the config value for real
    backends stays deferred to T5.2.

---

## Phase 2 — Independent verification

Turns the kernel's "implementor writes + grades its own tests" into a genuinely
independent, strong gate. This is what *earns* no-human-review — its full payoff
only lands once a capable model drives autonomous runs, but every gate here also
strengthens the human-reviewed loop. ([verification.md](specs/verification.md))

- [x] **T2.1 Postcondition-driven gate checks** — *done.* The gate now grades a
  candidate against the **stage's declared postconditions**, resolved through a config
  **check registry**. Learnings for downstream tasks:
  - The registry is `checks:` in `harness.yaml` (`config.Harness.Checks`,
    `map[name]command`); `gate.Registry.Resolve([]postconditions) → []Check` is the
    bridge, called inside `gate.Runner.Run` *before* provisioning (so a config fault or
    an empty-postcondition stage fails fast with no sandbox spent). `gate.Candidate`
    carries `Postconditions`; the orchestrator sets it from `stage.Postcondition` in
    `runGate`. Command + CLI flags `--gate-build/--gate-test` are gone — config is the
    single source of truth.
  - **Validate vs. gate gap to mind for T2.3/T2.7:** `harness validate` accepts
    reserved proofs (`tests-red-then-green`) and metric comparisons (`mutation>=0.8`)
    as known, but the gate has **no check kind for them yet** and `Resolve` errors
    loudly if asked to run one. No live gap — bootstrap `config/harness.yaml` only
    declares `tests-pass` — but T2.3 (red→green) and T2.7 (mutation) must add their
    check kinds to `gate` *and* keep them resolvable, not just validatable.
  - `internal/config/validate.go`: `reservedPostconditions` replaced the old
    `knownPostconditions`; `knownPostcondition` consults `Checks`; `isMetricComparison`
    is now reusable. Empty check commands are a validation error.
- [x] **T2.2 Gate evidence persistence** — *done.* The gate now harvests each check's
  evidence to the artifact store before returning a verdict, and the orchestrator cites
  it by hash in the `Verified:` trailer. Learnings for downstream tasks:
  - **The gate (not the orchestrator proper) gained the `artifact.Store`** — it owns the
    verification sandbox, so it harvests evidence the same way `runner.harvest` harvests
    prompt/transcript. `gate.New(backend, registry, store, socketDir, log)`;
    `buildRunComponents` passes the **same store the runner uses**. The captured bytes are
    already in memory (Exec copied them out), so persistence survives the deferred
    teardown regardless of ordering.
  - **One combined `gate-evidence` artifact per check** (`formatEvidence`: a header —
    name/command/exit/status — then `--- stdout ---`/`--- stderr ---` sections), so each
    check maps to exactly one hash on `CheckResult.Evidence`; `gate.Report` carries the
    refs. Deterministic format → content-addresses stably. This doc is the artifact the
    control-room gate-evidence view (T4.7) will render.
  - **Best-effort, mirrors harvest:** a nil store or a failed `Put` logs loudly and leaves
    the ref empty (degraded provenance) but never changes the verdict. **Both passing and
    failing checks persist** — a rejected gate's output is exactly what a human triages
    (feeds the T4.8 DLQ view).
  - **Trailer citation:** `verifiedChecks` renders each passed check as
    `name@<evidence-hash>`, degrading to bare `name` when no hash. This is a *superstring*
    of the old bare-name list, so `strings.Contains(msg, "Verified: tests-pass")`-style
    assertions (spine e2e) stayed green, and merge/orchestrator tests that build
    `Provenance` by hand or use a fake gate with no evidence refs were unaffected. Specs
    updated: `security.md`/`integration.md` trailer examples now show the `name@<hash>` form.
  - **For T2.3/T2.6/T2.7:** the persistence + citation path is now generic over checks —
    a new check kind only needs to populate `CheckResult` (and may write richer structured
    evidence, e.g. a mutation report, through the same `store.Put`); it is cited and
    auditable for free.
- [x] **T2.3 Red→green proof postcondition** — *done.* The gate now realizes the reserved
  `tests-red-then-green` proof: the acceptance tests must **fail on the base** (red) and
  **pass on the candidate** (green), proving the tests aren't vacuously green. Verified
  with unit fakes (pass / base-not-red / candidate-not-green / mixed-with-command-check /
  missing-base-ref) and a real docker+git integration subtest. Learnings for downstream:
  - **Two verification sandboxes.** `gate.Runner.Run` provisions the candidate verifier
    always (command checks + the green half) and a **second** verifier seeded at the base
    ref *lazily* — only when a `redGreenProof` check is present (`requiresBase`). Both are
    deny-all and torn down; provisioning was extracted into `provisionVerifier(ctx, c,
    ref) (sb, cleanup, err)`. A gate with no proof still spends exactly one sandbox.
  - **The proof reuses `tests-pass`.** A reserved proof has no `checks` entry of its own;
    `Registry.Resolve` binds `tests-red-then-green` to the **`tests-pass`** acceptance-test
    command (run against both refs). The two shared identifiers live in
    `core/conditions.go` (`core.PostconditionRedGreen`, `core.CheckAcceptanceTests`) so
    config-validation and the gate agree on the spelling (no cycle: `core` is a leaf).
    `internal/config/validate.go` now cross-checks that a stage declaring the proof
    registers a `tests-pass` command (caught at startup, not mid-run). Specs updated:
    `configuration.md` + `verification.md` document the two-ref / reuse-`tests-pass` shape.
  - **Base ref threading.** `gate.Candidate` gained `BaseRef`; the orchestrator's `runGate`
    passes `o.base` (the ref the candidate branched from, the same value `buildBrief`
    seeds the producer's worktree at). A proof with an empty `BaseRef` is a wiring fault
    that fails before any sandbox is spent.
  - **Evidence is generic (T2.2 path).** `CheckResult.Base *RunResult` carries the red
    half; `formatEvidence` renders both runs (`kind: red-green`, base + candidate
    sections) into one `gate-evidence` artifact, cited by hash like any check — no
    provenance change needed.
  - **⚠ Was-critical-for-T2.5 (now mostly resolved by T2.4):** the proof is only
    *meaningful* once `implement` branches from a base that holds the tests but not the
    impl. T2.4 landed `author-tests` **and** the base threading, so `implement` now branches
    from the author-tests candidate (part (b), done). What remains for T2.5 is (a) flip
    `implement`'s postcondition to `tests-red-then-green` and point `runGate`'s `BaseRef`
    at `issue.Base`. `implement` still uses `tests-pass` in the meantime, so nothing is broken.
- [x] **T2.4 `author-tests` soul + persona** — *done.* A `test-author` role/soul (`config/souls/test-author.yaml` + `souls/prompts/test-author.md`) that writes *failing* acceptance tests from the spec and never reads/writes the implementation. **It could not land in isolation** — three validation/runtime invariants couple the soul to a live stage, so this increment also wired `author-tests` into the DAG and pulled the candidate-threading forward (the structural half of T2.5). Learnings for downstream tasks:
  - **Three walls forced a single coherent landing, not a soul-only diff:** (1) *deadwood validation* rejects a soul whose role no stage uses, so the soul needs a stage; (2) the *gate rejects an empty postcondition set* (`gate.go` Run), so the stage needs a runnable check; (3) `advance` *dropped the predecessor candidate* (the "Phase 3" base-threading TODO), so `author-tests → implement` would have branched implement from `main` (no tests) and **regressed the kernel**. The infra to fix #3 was already in place (arbitrary base-ref worktree seeding; candidate branches persist after merge — never deleted; metadata round-trips like `Attempt`), so threading was small.
  - **New reserved proof `tests-red`** (`core.PostconditionTestsRed`) is the `author-tests` gate: like red→green it has no `checks` entry and reuses the `tests-pass` command (`core.CheckAcceptanceTests`), but runs it **once against the candidate and passes iff it FAILS** (nonzero exit) — proving the author wrote real, executing, non-vacuous failing tests. Gate plumbing mirrors red→green exactly: a `redProof` `checkKind`, `Registry.Resolve` binds both proofs to `tests-pass`, `runCheck` inverts the verdict, `formatEvidence` labels the nonzero-exit-is-pass record (`kind: tests-red`). `requiresBase` stays false for it (single ref, no base sandbox). Validation: `tests-red` joins `reservedPostconditions` and the new `reusesAcceptanceTests` set that requires a `tests-pass` command (generalized the old red→green-only check). **Known limitation:** `tests-red` (like red→green's "red" half) can't distinguish an assertion failure from a *compile* failure — a non-compiling suite passes it. It's caught downstream (the implementor can't make non-compiling tests pass without editing them, which its persona forbids → escalate). A compile-then-run split is a possible future refinement.
  - **Base threading landed (was deferred to "Phase 3"):** `core.Issue.Base` (rides in beads metadata via `MetadataKeyBase`, like `Attempt`). `advance` sets a produced agent-stage issue's `Base = res.Branch.Ref` (the predecessor's verified candidate); `route` preserves `issue.Base` across `on_failure` retries; `buildBrief` seeds the worktree from `issue.Base` when set, else the pipeline base (`o.base`/main). So `implement` branches from the `author-tests` candidate holding the failing tests. `runGate` still passes `BaseRef: o.base` — fine while implement uses `tests-pass`; **T2.5 must switch `runGate`'s `BaseRef` to `issue.Base` when it flips implement to red→green** (so the proof's red half runs against the author-tests candidate, not main).
  - **Live DAG is now `author-tests → implement → integrate`;** the seed enters at `author-tests` (entryRole/agentRoles/seed tests updated). `implement` deliberately **kept `tests-pass`** (not red→green) to bound this increment — see T2.5.
  - Specs updated: `configuration.md` (author-tests gets `postcondition: [tests-red]`; `tests-red` documented alongside red→green), `verification.md` (new *Tests-red proof* subsection; the red→green base sentence is now realized, not hypothetical), `workflow.md` (depth transitions seed the produced issue with the predecessor candidate as base), `integration.md` (candidate cleanup must not remove a branch still referenced as a base).
- [ ] **T2.5 Flip `implement` to the red→green proof** — *reduced to its remaining half* (the structural wiring, seed-entry shift, base threading, and implementor persona update all landed in T2.4). Switch `implement`'s postcondition to `tests-red-then-green` in `config/harness.yaml`, and change `runGate`'s `BaseRef` from `o.base` to the candidate's `issue.Base` (its author-tests base, where the tests are present but the impl is absent → red), falling back to `o.base`. Add an orchestrator test that the red half runs against the produced base. (needs T2.3 ✓, T2.4 ✓) ([workflow.md](specs/workflow.md), [verification.md](specs/verification.md))
- [ ] **T2.6 Independent scanners as checks** — `gosec` (SAST), `govulncheck` (vuln), dependency/license scan, each a gate check emitting evidence. (needs T2.1, T2.2) ([verification.md](specs/verification.md), [security.md](specs/security.md))
- [ ] **T2.7 Mutation-testing postcondition** — integrate a Go mutation tool (e.g. `gremlins`), run as a gate check, gate on a minimum score. *(OPEN: score + operators — pick a default.)* (needs T2.1) ([verification.md](specs/verification.md))
- [ ] **T2.8 Test↔spec traceability map** — the test author emits, per test, the spec heading + sentence it claims to encode; harvested to the artifact store and surfaced in provenance. (needs T2.4) ([verification.md](specs/verification.md), [specs-process.md](specs/specs-process.md))
- [ ] **T2.9 `qa` stage + soul** — a `security`/QA role/soul (distinct from the implementor) whose gate runs the mutation + scanner postconditions in the clean verification sandbox; `on_failure: implement`, `produces: integrate`. (needs T2.6, T2.7) ([workflow.md](specs/workflow.md), [verification.md](specs/verification.md))
- [ ] **T2.10 Trusted-dev policy profile** — a lighter policy profile with a human-approval postcondition for the self-hosting transition (a CLI `approve` command satisfies it). *(OPEN, configuration.md.)* ([bootstrap.md](specs/bootstrap.md), [configuration.md](specs/configuration.md))
- [ ] **T2.11** *(OPEN)* Second, different-model reviewer soul in `qa` (N-version diversity). ([verification.md](specs/verification.md))

## Phase 3 — Full DAG, decomposition & merge queue

Unwinds the kernel's single-stage, single-soul, trivial-merge simplifications.

- [ ] **T3.1 Decomposition planner soul + `plan` stage** — a `planner` soul/persona that reads a seed issue + its spec slice and proposes child issues (with dependency edges) via `request_subtask`; `plan` produces `author-tests`; the seed enters at `plan`. ([workflow.md](specs/workflow.md))
- [ ] **T3.2 `beads.Apply` self-validates `DependsOn` existence** *(carried from Phase 1)* — a prefix-independent referential-integrity check on each proposed dep target, closing the bd-1.0.4 foreign-prefix gap (a hostile proposal naming a foreign-prefix dep is currently accepted silently). TCB beads code. ([architecture.md](specs/architecture.md), [workflow.md](specs/workflow.md))
- [ ] **T3.3 Stage ≠ role + selector matching** — a role maps to a *set* of souls; the orchestrator picks one per issue by matching the issue's tags against each soul's `selector`. Generalize the kernel's `stage==role` assumption (`stageForRole`). ([configuration.md](specs/configuration.md))
- [ ] **T3.4 Per-role model tiers** — resolve the model per issue from the selected soul, so cheap models serve easy roles and frontier models the hard ones. (builds on T3.3) ([models.md](specs/models.md), [configuration.md](specs/configuration.md))
- [ ] **T3.5 Spec-slice resolution** — a new `internal/spec` package that builds the bounded slice (referenced file + linked neighbours to a configured depth) and populates `Brief.Spec`, so the agent no longer reads the whole `specs/` tree from the worktree. ([specs-process.md](specs/specs-process.md), [components/agent.md](specs/components/agent.md))
- [ ] **T3.6 Spec-version pinning** — the Brief pins the content hash of its spec slice; the hash is stored on the issue. (needs T3.5) ([specs-process.md](specs/specs-process.md))
- [ ] **T3.7 Recompile-the-delta** — on a spec-file edit, the orchestrator diffs which issues referenced it and invalidates / re-derives the affected in-flight issues; already-merged work may spawn new issues for the diff. (needs T3.6) ([specs-process.md](specs/specs-process.md))
- [ ] **T3.8 Cumulative per-issue / epic budget** *(carried from Phase 1)* — surface `Usage` on the Result envelope (the runner already tallies it); the orchestrator accumulates spend across the `on_failure` loop per issue/epic and dead-letters on breach; a per-model cost table converts tokens → USD. ([workflow.md](specs/workflow.md))
- [ ] **T3.9 Merge queue: serialized rebase onto `main`** — pop `integrate` issues in issue-graph topological order and rebase each candidate onto the *current* `main` tip in a sandbox (replaces the kernel's bare provenance-commit advance). ([integration.md](specs/integration.md))
- [ ] **T3.10 Re-gate the merged result** — after rebase, re-run the full gate suite in a clean verification sandbox against the *rebased* result before advancing `main` (catches two-green-branches breakage). (needs T3.9) ([integration.md](specs/integration.md))
- [ ] **T3.11 Conflict-resolution issue** — on a rebase conflict, spawn a sandboxed resolution issue (proposes a rebase), block, loop. *(OPEN: `integrate` as its own role vs. orchestrator function.)* (needs T3.9) ([integration.md](specs/integration.md))

## Phase 4 — Control room

The human's read-only window + the wizard (their only action surface). Stack: templ
+ Tailwind standalone CLI + `embed.FS` + htmx/Alpine + SSE.
([control-room.md](specs/control-room.md), [observability.md](specs/observability.md))

- [ ] **T4.1 Web server scaffold + asset pipeline** — `internal/controlroom` + a `harness serve` command; `go generate` running `templ generate` + the Tailwind standalone CLI; `embed.FS` for htmx, Alpine, and compiled CSS; a base templ layout. Single self-contained binary, no runtime toolchain. ([control-room.md](specs/control-room.md))
- [ ] **T4.2 Read/query layer** — render-ready reads over beads + the artifact store + git provenance, decoupled from the views. ([control-room.md](specs/control-room.md), [observability.md](specs/observability.md))
- [ ] **T4.3 SSE plumbing** — NATS events → an SSE endpoint consumed by the htmx SSE extension; the live-update substrate for the board and feed. ([messaging.md](specs/messaging.md), [control-room.md](specs/control-room.md))
- [ ] **T4.4 Board view** — kanban over beads issues by stage, live via T4.3. (needs T4.2, T4.3) ([control-room.md](specs/control-room.md))
- [ ] **T4.5 Activity feed** — `harness.agent.<id>.events` streamed to the browser (what agents are doing right now). (needs T4.3) ([control-room.md](specs/control-room.md))
- [ ] **T4.6 DAG view** — the issue dependency graph rendered server-side to SVG (Go → DOT/d2), hover/drill via Alpine + htmx. No client-side graph lib. (needs T4.2) ([control-room.md](specs/control-room.md))
- [ ] **T4.7 Issue / invocation detail** — Brief, transcript, candidate diff, gate evidence, budget, retries, from beads + the artifact store. (needs T4.2) ([control-room.md](specs/control-room.md))
- [ ] **T4.8 Dead-letter queue view** — the escalations needing a human; the primary action surface; links into Resolve (T4.15). (needs T4.2) ([control-room.md](specs/control-room.md), [workflow.md](specs/workflow.md))
- [ ] **T4.9 OTel spans + export** — emit spans at the broker, orchestrator, and runner (boot, llm-turn, tool-call, gate-run) and metrics (latency, throughput, cost); export to a trace backend (Tempo/Jaeger). ([observability.md](specs/observability.md))
- [ ] **T4.10 Budgets + Provenance views** — budgets (token/$/wall burn vs. caps) from OTel metrics; provenance (trace a merged commit → issue → soul → model → prompt → evidence). (needs T4.2, T4.9) ([control-room.md](specs/control-room.md))
- [ ] **T4.11 Replay** — reconstruct an invocation's full decision trail from the broker-captured transcript + the artifact store, live or after the fact. (needs T4.7) ([observability.md](specs/observability.md))
- [ ] **T4.12 Requirements-planner conversation loop** — the trusted, **non-sandboxed** LLM that drives toward aligned, testable intent, streaming over SSE; reuses the canonical model layer. *(Needs a capable model at runtime.)* ([control-room.md](specs/control-room.md), [specs-process.md](specs/specs-process.md))
- [ ] **T4.13 Alignment ledger** — forks rendered as selectable chips (with tradeoffs); each item agreed/open with a one-line rationale; freeform typing always available. (needs T4.12) ([control-room.md](specs/control-room.md))
- [ ] **T4.14 Spec authoring + consent gate** — the planner drafts spec markdown + seed issues (keeping link integrity + the README index); on explicit human **APPROVE**, the spec is committed to git, the decisions sidecar written, the conversation transcript stored, and the seed issues created through the single-writer path. (needs T4.12) ([specs-process.md](specs/specs-process.md), [control-room.md](specs/control-room.md))
- [ ] **T4.15 Resolve mode** — Create and Resolve are one component; Resolve pre-loads the escalation + spec slice + the agent transcript that raised it and shows the spec diff + blast radius before commit. (needs T4.14) ([control-room.md](specs/control-room.md), [specs-process.md](specs/specs-process.md))

## Phase 5 — Production isolation & distribution

Replaces the bootstrap stand-ins (Docker, in-process NATS, local-repo push, files
store) with the production stack.

- [ ] **T5.1 vsock broker transport** — `broker.Listen`/`Serve` over vsock and a `sandbox.Endpoint` of `vsock`+`cid:port` (currently unix-only); the transport Firecracker needs. ([messaging.md](specs/messaging.md), [components/runner.md](specs/components/runner.md))
- [ ] **T5.2 Firecracker sandbox backend** — a KVM-microVM backend implementing the `Backend`/`Sandbox` interface: rootfs seeding, vsock I/O (T5.1), resource limits incl. disk, deterministic teardown. The production isolation target. (needs T5.1) ([components/sandbox.md](specs/components/sandbox.md))
- [ ] **T5.3 Rootfs / base-image composition** — per-role toolchain images with the module/package cache baked in for offline (zero-network) builds. *(OPEN in sandbox.md.)* ([components/sandbox.md](specs/components/sandbox.md))
- [ ] **T5.4 Sandbox seeded-worktree ownership** *(carried from Phase 1)* — the Docker backend `chown`s the seeded worktree to the container user (and the Firecracker backend seeds correct ownership), dropping the `safe.directory` / VCS-stamping workaround the bootstrap profile image relies on. ([components/sandbox.md](specs/components/sandbox.md))
- [ ] **T5.5** *(optional)* gVisor backend (medium-trust). ([components/sandbox.md](specs/components/sandbox.md))
- [ ] **T5.6 Vetted package mirror/proxy** — route package fetches through a pinning/scanning/logging proxy on the broker allowlist; a read-through cache amortizes downloads without weakening egress control. ([security.md](specs/security.md), [components/runner.md](specs/components/runner.md))
- [ ] **T5.7 Scoped short-lived secret minting** — the runner mints a per-task git token scoped to push *only* the task branch, injected for the invocation lifetime and dying with the sandbox (replaces the bootstrap local-repo push). ([components/runner.md](specs/components/runner.md), [security.md](specs/security.md))
- [ ] **T5.8 Distributed NATS** — an external cluster with concrete JetStream stream defs (retention / replicas / max-age — the messaging.md OPEN) and runners across hosts, swapped in for the embedded in-process server. ([messaging.md](specs/messaging.md))
- [ ] **T5.9 S3/MinIO artifact backend** — an `artifact.Store` implementation for distributed deployments (config `bucket`), shared across hosts and the control room. ([components/artifact-store.md](specs/components/artifact-store.md))
- [ ] **T5.10 Provenance signing + key custody** — sign commits/artifacts with the harness identity and verify on read. *(OPEN, security.md.)* ([security.md](specs/security.md))
- [ ] **T5.11** *(optional)* Warm sandbox pools + HA orchestrator via NATS-KV leader election. *(OPEN.)* ([components/runner.md](specs/components/runner.md), [components/orchestrator.md](specs/components/orchestrator.md))

---

## Open decisions affecting the plan

These are `OPEN:` in the specs and may reshape tasks above:

- Mutation score threshold + operators (T2.7).
- `integrate` as its own role/soul vs. orchestrator-owned with sandboxed conflict help (T3.11).
- HA orchestrator: single instance (fine for v1) vs. leader election (T5.11).
- Condition-expression language for pre/postconditions (shell exit-code vs. CEL) — affects
  config validation + the gate runner. T2.1 landed the shell-exit-code form: command-check
  postconditions resolve to commands via the `checks:` registry in `harness.yaml`; the gate
  runs them via `sh -c` (exit 0 = pass). `harness validate` still gates *bare* identifiers
  (reserved proofs, known metrics) against explicit registries that must be extended as new
  built-in check kinds are added. T2.3 added the red→green kind (reuses the `tests-pass`
  command, run against two refs); T2.7 (mutation `>=` comparisons) still has no gate kind.
- Rootfs / base-image composition per role (T5.3).
- Exact module set drawn into the TCB boundary — must be pinned before autonomy is switched on for harness work.
