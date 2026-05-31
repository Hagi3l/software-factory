# Harness Specifications

This directory is the source of truth for **what the harness is and how it must
behave**. It is written for humans first and agents second. In the system these
specs describe, this is the *only* place humans author intent — everything
downstream is autonomous.

These are design/requirements specs (the *what* and the contracts), not
implementation docs. They are cross-linked; follow the links rather than reading
top-to-bottom.

---

## Vision

A **secure, autonomous software factory.** A human collaborates with an LLM to
turn an idea into specifications and seed work items. From there, a fleet of
ephemeral, sandboxed agents — each with its own *soul* — plans, writes tests,
implements, verifies, and integrates the change, coordinating over NATS with
[beads](https://github.com) as the work-item store. The terminal state is code
**merged to `main`**. Humans never read or write code; they author and refine
specs.

The threat model is deliberately adversarial: **assume the agents, and the code
they generate, may be hostile.** The architecture's job is to produce trustworthy
software anyway.

---

## Core principles

These recur throughout every spec. When a design decision is unclear, resolve it
in favour of these.

1. **Humans own intent; agents own execution.** The only human lever — including
   for stuck work — is the spec. Agents *escalate* ambiguity, they never invent
   intent. See [specs-process.md](specs-process.md).
2. **Untrusted by default.** An agent's loop *and* the code it produces may be
   hostile. Agents run in sandboxes with **zero direct network**; the runner
   brokers all I/O. See [security.md](security.md), [sandbox](components/sandbox.md).
3. **Producer ≠ verifier.** Whoever produces an artifact never grades it. Tests
   are authored independently of the implementation; gates run in a *fresh*
   verification sandbox; agents *propose*, the orchestrator *accepts*. See
   [verification.md](verification.md).
4. **Single source of truth, single writer.** Beads holds the work graph; only
   the orchestrator writes it. Agents propose mutations that are validated for
   DAG-legality before being applied. See [orchestrator](components/orchestrator.md).
5. **Location transparency.** Everything speaks NATS, so agents and runners may be
   co-located in one process or distributed across hosts with no code change. See
   [messaging.md](messaging.md).
6. **Stateless souls.** A soul is identity (config); it carries no cross-task
   memory. All durable state lives in beads, git, and these specs. See
   [agent](components/agent.md).
7. **Config-driven.** Workflow, souls, and infrastructure are declarative config,
   validated before anything runs. See [configuration.md](configuration.md).
8. **Bounded autonomy.** Budgets and retry caps are the *termination guarantee*,
   not merely cost control. Breaches dead-letter for human triage. See
   [workflow.md](workflow.md).
9. **Emergent within a stage, declarative between stages.** Agents decide *how
   many* work items a stage produces; config decides *what stage comes next*. See
   [workflow.md](workflow.md).
10. **Provenance by construction.** Every change traces to issue → soul → model →
    prompt → evidence. See [security.md](security.md).

---

## Reading order

New to the harness? Read in this order:

1. [architecture.md](architecture.md) — the big picture: trust model, topology, the two graphs.
2. [workflow.md](workflow.md) — how work flows from a spec to merged code.
3. [components/agent.md](components/agent.md) — what a single agent invocation is.
4. [verification.md](verification.md) — how the factory trusts output with no human review.
5. [control-room.md](control-room.md) — the human's window and only action surface, and [observability.md](observability.md) behind it.
6. Everything else as referenced.

[glossary.md](glossary.md) defines every term of art. When a word is capitalised
oddly (Soul, Role, Runner, Brief), it's defined there.

---

## The spec tree

| Spec | Covers |
|------|--------|
| [architecture.md](architecture.md) | Principles in depth, trust boundaries, the orchestrator/runner/agent topology, the issue-DAG vs role-flow distinction. |
| [workflow.md](workflow.md) | The DAG stages (requirements → plan → author-tests → implement → qa → integrate), pre/post conditions, `on_failure`, the feedback loop, termination & budgets. |
| [verification.md](verification.md) | Independent test authoring, red→green proof, mutation testing, gates in a clean sandbox, the test↔spec traceability map. |
| [integration.md](integration.md) | The serialized merge queue, rebasing onto `main`, re-gating the merged result, conflict resolution. |
| [security.md](security.md) | Threat model, trust boundaries, egress allowlist, scoped short-lived secrets, SLSA-style provenance. |
| [messaging.md](messaging.md) | NATS subject taxonomy, JetStream streams, the broker protocol, single-writer semantics. |
| [configuration.md](configuration.md) | `harness.yaml`, `souls/*.yaml`, infra overlays, and config validation. |
| [models.md](models.md) | Model-agnostic agent loop, the provider abstraction in the runner, canonical types, the adapters. |
| [specs-process.md](specs-process.md) | How specs are written, the human re-entry invariant, spec-drift handling, spec-version pinning. |
| [observability.md](observability.md) | Broker-as-collector, the three stores, the OTel trace model, live vs. history, replayability. |
| [control-room.md](control-room.md) | The web UI: stack, the views, live/historical rendering, and the Create-Task/Resolve wizard. |
| [bootstrap.md](bootstrap.md) | How the harness comes to build itself: the minimal kernel, the TCB caveat, the trusted→autonomous progression. |
| [components/orchestrator.md](components/orchestrator.md) | The scheduler + gatekeeper + sole beads writer and its reconciliation loop. |
| [components/runner.md](components/runner.md) | The per-host daemon: sandbox lifecycle and the I/O broker. |
| [components/agent.md](components/agent.md) | The `Agent` interface, the `Soul`, the invocation lifecycle, the task envelope. |
| [components/sandbox.md](components/sandbox.md) | Isolation backends (Firecracker/Docker/gVisor), zero-network enforcement, worktree seeding. |
| [components/artifact-store.md](components/artifact-store.md) | Content-addressed store for transcripts, gate evidence, and diffs; harvested before sandbox teardown. |

---

## Status

These specs are the contract the implementation satisfies, and most have been
**validated and refined by implementation** through Phase 4 (kernel + independent
verification + full DAG + control room; see `IMPLEMENTATION_PLAN.md`). Decisions are
recorded as the specs assert them. Open questions are called out inline with
**OPEN:** markers; the few that remain are concentrated in Phase 5 (production
isolation & distribution) — the spec tree currently carries none, the live ones
being tracked in the plan.
