# Implementation Plan

Build order for the harness, derived from [specs/](specs/README.md). The spine is
[bootstrap.md](specs/bootstrap.md): hand-build a minimal kernel that does
`spec → implement → gate → merge` for one issue (the **self-host point**), then
build out the full design.

## Status — built out

**Every engineering phase is complete except the tail of Phase 5 below.** The
verbose per-task findings this file once carried were pruned as work completed —
that history lives in git (this file's own log), the code, and the specs each task
updated as it landed. What shipped, in build order:

- **Phases 0–1 — the kernel.** `spec → implement → gate → merge` for one issue
  end-to-end: `cmd/software-factory` (`validate`/`seed`/`run`), in-process orchestrator +
  runner, Docker sandboxes, provenance trailer on the merge. Verified against a
  real model (local Ollama via `openai-compat`).
- **Phase 2 — independent verification.** Postcondition-driven gates, red→green
  proof, the `author-tests` soul, independent scanners (gosec/govulncheck/license/
  golangci-lint), mutation testing, the `qa` stage, the trusted-dev approval
  profile, and the producer/verifier model-family diversity advisory. N-version
  diversity is **configured, not mandated** (decision recorded in
  [verification.md](specs/verification.md)).
- **Phase 3 — full DAG, decomposition & merge queue.**
- **Phase 4 — control room.** All views (Board, DAG, Activity, Dead-letter,
  Budgets, Provenance, Verification, Config, Replay), the live SSE feed, and the
  Create-Task / Resolve wizard.
- **Phase 5 — production substrate** (all but the two open items below): vsock
  broker transport, base-image composition, gVisor backend + honored
  config→backend selection (firecracker fails closed), package proxy on the broker
  allowlist, scoped short-lived secret minting, distributed NATS, S3/MinIO
  artifact backend, provenance signing + key custody, and multi-signal OTLP
  observability.
- **Phase 6 — agent semantic tooling (LSP).**
- **Phase 7 — atomic feature integration (epic mode).**
- **Phase 8 — demo-hardening: authoritative read model + decomposition granularity.**
- **Phase 9 — structured check findings & agent context discipline.**
- **Phase 10 — read-model concurrency correctness.**
- **Phase 11 — model-layer capability fields, tool-call observability, prompt caching.**
- **Phase 12 — distilled `explore` tool.** Helper souls: a nested, read-only,
  sub-budgeted explore loop with its own evidence/provenance/observability nesting.
- **Phase 13 — live-demo hardening.** Moving-tail cache breakpoint on the
  openai-compat path, wizard prefill kickoff, vault-demo tuning.
- **Phase 14 — trusted-layer hardening.** Least-privilege container boot, bounded
  transient-fault retry at the model relay, reasoning in the recorded transcript.
- **Phase 15 — vault-demo run hardening.** OpenRouter cost accounting, per-model
  idle timeouts, durable stage-close (root cause: bd's JSONL auto-export reverting
  committed writes — fixed by making the Dolt server authoritative,
  `bd config set export.auto false`), feature-level provenance on the epic
  terminal merge, reload-safe wizard sessions, session-aware Refresh.

**Runtime validation: satisfied by the 2026-07-02/06/07 live vault runs**
(Phases 13–15). The 2026-07-06 run carried the exercising use case — Go, a `qa`
gate running gosec/govulncheck/license-scan, an inner loop running `go test` —
through a two-child epic to its terminal merge on `main`; the defects those runs
surfaced became Phase 15 and the Deferred list below.

## How to read this

- **Open tasks (`- [ ]`) keep their full detail**; completed work is summarized
  above, with per-task history in git.
- `(spec)` links point at the authoritative contract for each task. If a task needs
  the design to change, **update the spec first.**
- `*(OPEN)*` marks a task whose shape is still undecided in the specs;
  `*(optional)*` marks a nice-to-have.

## The self-host milestone

The kernel from [bootstrap.md](specs/bootstrap.md) is: config → beads → sandbox →
runner/broker → agent loop → gate runner → orchestrator loop. Bootstrap
simplifications held at the kernel and were unwound across Phases 2–5 — DAG collapses
to `implement → gate → integrate`, merge queue is trivial (single stream, no
rebase/re-gate), NATS is in-process, Docker stands in for Firecracker, no control
room (CLI-driven), the implementor writes its own tests.

## TCB caveat

Per [bootstrap.md](specs/bootstrap.md), the components that *enforce* the guarantees —
orchestrator, runner/broker, sandbox, gate harness — are the Trusted Computing Base.
**TCB-touching changes stay human-reviewed even after self-hosting.** Autonomy is
earned first for non-TCB work (new souls, stages, the control room). While the
harness is built by hand this is moot — everything is human-reviewed — but the
boundary matters the moment a capable model is wired and autonomy is switched on.

## Testing infrastructure (cross-cutting)

The deterministic end-to-end spine test (no Docker) and its Docker variant verify
the kernel's *machinery* — routing, the tool contract, gating, merge, provenance —
independently of a capable runtime model. Specs: [models.md](specs/models.md),
[components/sandbox.md](specs/components/sandbox.md), [bootstrap.md](specs/bootstrap.md).

- **Known flake (infra, not a code bug):** `internal/runner.TestTeardownRunsEvenWhenInvokeErrors` (and
  occasionally its NATS-backed siblings) can stall under the *full-suite* `go test ./...` parallel load —
  many packages each spin an embedded NATS server, and the redelivery-loop teardown becomes timing-sensitive
  under contention — surfacing as a 10-minute package timeout. It passes deterministically in isolation
  (`go test ./internal/runner/` ≈ 0.25s). If `make check` times out here, re-run; a real fix would cap test
  parallelism (`go test -p`) or give the embedded-NATS runner tests a tighter per-test deadline.

---

## Open work — Phase 5 tail (production isolation & distribution)

Everything else in Phase 5 landed (see Status). The Firecracker backend was
deliberately ordered **last** (decided 2026-06-04): every other Phase-5 task was
buildable and verifiable in a Docker-only dev environment, while Firecracker alone
needs KVM — building it without hardware to exercise it would mean shipping an
untested microVM backend. Nothing depends on it; the Docker/gVisor backends satisfy
development and human-reviewed runs.

- [ ] **T5.11** *(optional)* Warm sandbox pools + HA orchestrator via NATS-KV leader election. *(OPEN.)* ([components/runner.md](specs/components/runner.md), [components/orchestrator.md](specs/components/orchestrator.md))
- [ ] **T5.2 Firecracker sandbox backend** — a KVM-microVM backend implementing the
  `Backend`/`Sandbox` interface: rootfs seeding, vsock I/O (already landed), resource
  limits incl. disk, deterministic teardown. The production isolation target.
  **Blocked on hardware, not on code:** needs KVM (bare-metal or nested virt) that the
  dev environment lacks, so it cannot be built-and-verified here — do it only once such
  hardware is available. The config plumbing is ready: `sandbox.backend: firecracker`
  validates and **fails closed** at startup rather than silently degrading to Docker.
  ([components/sandbox.md](specs/components/sandbox.md))

## Deferred & follow-ups (filed, not blocking)

- **`run.sh` leaks its `dolt sql-server` on exit** *(2026-07-07 live demo run)*. Each
  `demo/vault/run.sh` invocation `bd init --server`-starts a `dolt sql-server` for the scratch
  repo but never tears it down, so repeated runs leave **orphaned dolt servers** accumulating
  (4 observed live). They hold RAM + open DBs and worsen the memory pressure below. **Fix:** trap
  EXIT in run.sh to stop the per-run dolt server (it prints its pid/socket under
  `$SITE/.beads/`), alongside the existing scratch-repo cleanup.
- **Durable stage-close lost under memory pressure (OOM `signal: killed`)** *(2026-07-07
  live demo run — the run wedged with generate done but one child stuck)*. Serializing `bd`
  invocations killed the lost-update *race*, but a stage-close is
  still lost if the OS **SIGKILLs the `bd`/`dolt` write process** mid-operation — which happens
  when the host is out of RAM (observed 176 MB free of 32 GB; the log showed `bd … signal:
  killed`, a harvested result "ignored as stale/duplicate", then a 6-min stall). Predecessor
  beads stayed `in_progress` in the durable store though the projection had
  advanced through security, so the dependent sibling's readiness oracle never cleared. This is
  an OOM/robustness gap, not a concurrency one. **Fix directions:** (a) relieve pressure — the
  demo needs materially more free RAM (drop `OPENOBSERVE=1`, kill orphaned dolt servers, cap
  Docker sandbox memory); (b) make the trusted layer **detect a killed `bd` write and retry**
  (a non-zero/killed exit on a write is currently not reconciled); (c) a projection→durable
  **reconciler** that re-asserts a stage-close the durable store is missing (deferred as
  unnecessary once the race was closed — an OS kill re-opens the
  need). Interacts with the "In-flight projection growth" note below.
- **Wizard: `announcesDraft()` matcher too literal** *(same run)*. The draft-nudge backstop only fires when concluding prose matches a fixed phrase list (`"draft the spec"`, `"seed issues"`, …) — it missed the model's actual `"Drafting the spec and seed issue now"` (gerund `drafting` ≠ `draft the`; singular `issue` ≠ `issues`), so the nudge never fired and the promised draft never came. **Fix:** broaden to stems/keywords (`drafting`, `seed issue`, `propose_draft`, `proposing`) in `internal/controlroom/wizard/wizard.go`.
- Live-streaming replay (reconstruct the decision trail as the invocation runs) — needs the broker to emit structured per-turn events; overlaps the activity feed.
- Consolidate the status bar's 2–3 per-page SSE connections (page content + status bar + alerts.js) onto one connection or h2c.
- Client-side live wall/token ticker on the invocation budget meter (mid-invocation spend isn't persisted to beads).
- Decomposition-preview dry-run before APPROVE (control-room.md OPEN, "leaning defer"; seed issues stay coarse and the autonomous planner decomposes).
- **First-party thinking-block preservation** *(from the 2026-07-02 demo-prep pass)*. Through the openai-compat/OpenRouter path, Claude's interleaved thinking blocks are dropped between tool turns, so a deep-reasoning role (the Opus test-author) re-derives its plan from scratch each turn — a quality *and* token cost. The first-party `anthropic` adapter (already built) can preserve them across a tool loop. Strategic framing: the harness stays provider-unaware, but the frontier-Claude path should be **first-party by default** with compat as the portability fallback — this, native `effort`, and native cache-TTL control all being first-class there. A deployment/config choice plus verifying the anthropic adapter round-trips thinking blocks through the loop; no architecture change. Weigh against OpenRouter's single-key convenience for the demo.
- **Package-proxy redirect pinning** *(from the 2026-07-03 review)*. The fetcher's
  `http.Client` sets no `CheckRedirect` (`internal/packageproxy/packageproxy.go:50-55`):
  `ValidatePath` confines the *first* hop to the configured proxy host, but a 3xx is followed
  to any host — an agent-reachable SSRF from the runner (which holds the API key and full
  network). The module-proxy protocol doesn't need redirects. **Fix:** refuse redirects, or
  re-pin `host == base` on every hop.
- **Broker-socket I/O deadlines + handler drain** *(2026-07-03 review)*. No `SetDeadline`
  anywhere in `internal/broker` and no in-flight connection cap: a sandbox can open
  connections that never send (or stall mid-frame), each wedging a goroutine + FD on the
  trusted runner; `Serve`'s per-connection goroutines also aren't awaited at invocation end
  (`internal/broker/server.go:98-128`, `internal/runner/runner.go:468-474`). **Fix:** per-frame
  read/write deadlines (one request per connection makes a short deadline safe), a bounded
  in-flight count, and a `WaitGroup` drain in `Serve`.
- **Wizard session lifecycle** *(2026-07-03 review; partially resolved)*. The
  **LRU-touch-on-use** part is **done**: `Get`/reopen mark a session
  most-recently-used, so an actively-used session is no longer evicted out from under a human
  drafting in it. **Still open:** an idle-timeout sweeper that tears down an abandoned session
  + its explorer sandbox (today they pin the sandbox until count pressure), and a terminal
  "session expired" SSE event so an evicted session's browser stream isn't silently inert.
- **OpenRouter provider routing / pinning** *(2026-07-06 vault-demo run)*. `ModelProvider` has
  no seam to pin or order the upstream provider OpenRouter routes a slug to — it load-balances a
  slug like `z-ai/glm-5.2` (the current security/qa verifier) across multiple upstream hosts
  each with different throughput
  and reliability, so behaviour varies run-to-run. Injectable via the adapter's existing
  `option.WithJSONSet` escape hatch (a top-level `provider: {order|only|sort}` field) behind a
  new `ModelProvider` field. Value: run-to-run **reproducibility** and the ability to route to a
  high-throughput host; **not** a reliability win on its own (`allow_fallbacks:false` trades
  resilience for determinism, so prefer an ordered preference with fallbacks on, or
  `sort: throughput`). Deferred as an enhancement — it is not the fix for the hung-stream stall
  (the per-model idle timeout is), and does nothing for an upstream model's intrinsic per-turn latency.
- **Control-room pagination** *(2026-07-03 review)*. Provenance history is hard-capped at the
  newest 50 merges with no offset/cursor (`internal/controlroom/server.go:698`) — older
  forensic history is unreachable in the UI; board/DLQ render their full sets unbounded.
  **Fix:** limit/offset params + "showing N of M / load older" affordances.
- **In-flight projection growth — decided 2026-07-03: do nothing for now.** `settle()`
  retains every closed/blocked entry, `reset()` rehydrates the full closed history from
  `ListAll` at startup, and the 2s ticks + board snapshots scan the whole map
  (`internal/orchestrator/inflight.go:83-90,263-270`); `authorizeEpic` separately does a
  full `ListAll` per result when an epic budget is set (`results.go:1021`). Accepted while
  runs are session-length — the retention exists to serve the lagging-`bd.ready()`
  suppression, and nothing today runs long enough for the growth to bite. **Revisit
  trigger:** a long-running deployment or a sluggish board. The sketched fix: TTL-evict
  settled entries (the lag window is seconds; minutes of retention is generous) + a per-epic
  running spend counter updated at settle — which also takes `authorizeEpic` off its
  per-result `ListAll` as a bonus.

## Open decisions affecting the plan

These are still `OPEN:` in the specs and may reshape tasks above. (Decisions once open
here — mutation threshold, gate fail-fast, `integrate` ownership, the condition-expression
language — are now recorded in the specs they informed, not duplicated here.)

- HA orchestrator: single instance (fine for v1) vs. leader election (T5.11).
- **Concurrent epics (epic mode).** Phase 7 v1 admits one active epic at a time (a
  second is refused). Lifting it needs a two-level merge queue (children serialize onto their
  epic branch; epic→`main` merges serialize onto `main`, an in-flight epic rebasing onto a
  moved `main` + re-gating the whole feature at its terminal merge). Deferred; spec'd as OPEN
  in [integration.md](specs/integration.md).
- Exact module set in the TCB boundary — operationally the `policy.tcb_paths` globs;
  the concrete list must still be reviewed and pinned before autonomy is switched on for harness
  work. Now formally tracked as an **OPEN question in configuration.md** (was only prose in
  bootstrap.md + this plan).
