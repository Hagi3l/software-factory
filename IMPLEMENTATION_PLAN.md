# Implementation Plan

Build order for the harness, derived from [specs/](specs/README.md). The spine is
[bootstrap.md](specs/bootstrap.md): hand-build a minimal kernel that does
`spec → implement → gate → merge` for one issue (the **self-host point**), then let
the harness build its own remaining features as beads issues.

## Status — self-host point reached

Phases 0–1 are **complete**. The kernel does `spec → implement → gate → merge` for
one issue end-to-end: `cmd/harness` exposes `validate`/`seed`/`run`, and the
in-process orchestrator + runner carry a seed issue through implement → gate →
merge to `main` with a provenance trailer. Verified end-to-end against a real model
and real sandboxes. A live run needs a Docker daemon + a model the runner can reach
(a keyless local Ollama/vLLM via `openai-compat` works; no hosted key required).
From here the harness can implement Phases 2–5 as beads issues, with a human
reviewing diffs ("trusted-dev mode" per [bootstrap.md](specs/bootstrap.md)).

## How to read this

- **Phases 0–1** (scaffolding + the kernel) are **complete** — see Status. Their
  atomic-task breakdown and the detailed per-task findings have been pruned from
  this plan; that history now lives in git, the code, and the specs they informed.
- **Phases 2–5** are **coarse** on purpose. They are the post-kernel work the harness
  largely builds for itself; detailing them now risks planning against a kernel that
  doesn't exist yet. Expand each into atomic tasks when its phase begins.
- Tasks within a phase are listed in dependency order. Cross-task deps are noted as
  `(needs Tx.y)` only where they aren't the obvious linear predecessor.
- `(spec)` links point at the authoritative contract for that task.

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
earned first for non-TCB work (new souls, stages, the control room).

---

## Carried forward from Phase 1 (deferred / known gaps)

Real obligations surfaced while building the kernel and deliberately deferred.
Logged here so they survive into the phase that closes them — none blocks self-host.

- **Cumulative per-issue budget enforcement** (workflow.md's second budget level):
  not enforced — `core.Result` carries no `Usage`, so the orchestrator can't tally
  spend across the `on_failure` loop. Termination is still guaranteed by retry-cap ×
  per-invocation turn/token cap × sandbox wall; this is cost-control refinement.
  Needs `Usage` surfaced on the Result envelope (the runner already tallies it per
  invocation). USD also needs a per-model cost table. → Phase 3.
- **Gate evidence persistence**: per-check stdout/stderr is logged but not yet
  written to the artifact store by hash / cited in the `Verified:` trailer. → Phase 2.
- **`beads.Apply` self-validating `DependsOn` existence**: bd 1.0.4 skips
  existence-checking a dep id whose prefix differs from the db prefix (treats it as a
  federation ref), so a hostile proposal naming a foreign-prefix dep would be accepted
  silently. Same-prefix ids (the realistic case) are still validated by bd. The
  prefix-independent fix is for `Apply` to check each target itself (touches TCB beads
  code). → Phase 3 (alongside emergent-breadth validation).
- **Docker seeded-worktree ownership**: `docker cp` preserves the host uid on the
  seeded `.git` while the container runs as root, tripping git's dubious-ownership
  guard and Go's VCS stamping (`exit 128 … -buildvcs=false`). The bootstrap profile
  image works around it (`git config --global --add safe.directory '*'`); the proper
  fix is for the docker backend to `chown` the seeded worktree to the container user
  so a profile image needn't opt in. → Phase 5 (with Firecracker / rootfs composition).

---

## Phase 2 — Independent verification *(coarse)*

Earns no-human-review for non-TCB work. ([verification.md](specs/verification.md))

- [ ] Independent `author-tests` stage — a soul distinct from `implement` writes failing acceptance tests (defends against correlated errors).
- [ ] Red→green proof as a first-class postcondition (fail on base, pass on impl).
- [ ] Mutation testing postcondition + minimum-score gate. *(OPEN: score + operators.)*
- [ ] Independent scanners — `gosec` (SAST), dependency/vuln/license scans, policy-as-code.
- [ ] Test↔spec traceability map — per-test spec heading+sentence, harvested as evidence.
- [ ] Gate evidence persistence — per-check stdout/stderr → artifact store by hash, cited in the `Verified:` trailer (carried from Phase 1).
- [ ] Trusted-dev policy profile — human-approval postcondition for the self-hosting transition. *(OPEN, configuration.md.)*
- [ ] *(OPEN)* Second different-model reviewer soul in `qa` (N-version diversity).

## Phase 3 — Full DAG, decomposition & merge queue *(coarse)*

- [ ] Decomposition planner soul (`plan` stage) — reads seed issue + spec, emits `implement` issues with dependency edges.
- [ ] Emergent breadth — child-issue proposals validated for DAG-legality before write; `beads.Apply` self-validates each `DependsOn` target's existence (carried from Phase 1). ([workflow.md](specs/workflow.md))
- [ ] Spec slice resolution + context horizon — referenced file + linked neighbours to configured depth. ([specs-process.md](specs/specs-process.md))
- [ ] Spec-version pinning + recompile-the-delta — pin slice content hash per Brief; on spec edit, invalidate/re-derive affected issues. ([specs-process.md](specs/specs-process.md))
- [ ] Serialized merge queue — rebase onto current `main`, **re-gate the merged result**, conflict → sandboxed resolution issue. ([integration.md](specs/integration.md)) *(OPEN: integrate as its own role vs orchestrator function.)*
- [ ] Cumulative per-issue budget — surface `Usage` on the Result envelope; orchestrator tallies spend across the `on_failure` loop; per-model cost table for USD (carried from Phase 1). ([workflow.md](specs/workflow.md))
- [ ] Role→soul `selector` matching by issue tags; per-role model tiers. ([configuration.md](specs/configuration.md))

## Phase 4 — Control room *(coarse)*

Stack: templ + Tailwind standalone CLI + `embed.FS` + htmx/Alpine + SSE. ([control-room.md](specs/control-room.md), [observability.md](specs/observability.md))

- [ ] Web server scaffold + asset embedding (`go generate`: `templ generate` + Tailwind CLI).
- [ ] Board (kanban over beads, live via NATS→SSE).
- [ ] DAG view (server-side SVG, hover/drill via Alpine).
- [ ] Activity feed (NATS events → SSE).
- [ ] Issue/invocation detail (Brief, transcript, candidate diff, gate evidence, budget, retries).
- [ ] Dead-letter queue view — the primary human action surface.
- [ ] Budgets view (OTel metrics) + Provenance view (commit → issue→soul→model→prompt→evidence).
- [ ] Requirements **wizard** — steered conversation + alignment ledger + spec authoring + explicit-approval consent gate; Create and Resolve as one component. Trusted, not sandboxed. ([specs-process.md](specs/specs-process.md))
- [ ] OTel spans at broker/orchestrator/runner → trace backend; **replay** of an invocation's decision trail.

## Phase 5 — Production isolation & distribution *(coarse)*

- [ ] Firecracker sandbox backend (vsock transport, KVM microVM) — production target. ([components/sandbox.md](specs/components/sandbox.md))
- [ ] *(optional)* gVisor backend (medium-trust).
- [ ] Sandbox seeded-worktree ownership — docker backend `chown`s the seeded worktree to the container user so profile images needn't opt into `safe.directory` (carried from Phase 1). ([components/sandbox.md](specs/components/sandbox.md))
- [ ] Vetted package mirror/proxy — supply-chain mediation: pin, scan, log fetches. ([security.md](specs/security.md))
- [ ] Scoped short-lived secret minting — per-task git token that pushes only the task branch. ([components/runner.md](specs/components/runner.md))
- [ ] Distributed NATS (external cluster, JetStream replicas/retention) + runners across hosts.
- [ ] S3/MinIO artifact backend. ([components/artifact-store.md](specs/components/artifact-store.md))
- [ ] Provenance signing + key custody. *(OPEN, security.md.)*
- [ ] *(optional)* Warm sandbox pools; HA orchestrator via NATS-KV leader election. *(OPEN.)*

---

## Open decisions affecting the plan

These are `OPEN:` in the specs and may reshape tasks above:

- Mutation score threshold + operators (Phase 2 mutation gate).
- `integrate` as its own role/soul vs. orchestrator-owned with sandboxed conflict help (Phase 3).
- HA orchestrator: single instance (fine for v1) vs. leader election (Phase 5).
- Condition-expression language for pre/postconditions (shell exit-code vs. CEL) — affects
  config validation + the gate runner. Until it lands, `harness validate` gates conditions
  against an explicit registry that must be extended as new conditions/metrics are added.
- Exact module set drawn into the TCB boundary — must be pinned before autonomy is switched on for harness work.
