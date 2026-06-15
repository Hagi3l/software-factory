# Architecture

The big picture: the trust model, the runtime topology, and the two graphs that
govern how work moves. Read this before any other spec.

See also: [workflow.md](workflow.md), [security.md](security.md),
[components/orchestrator.md](components/orchestrator.md), [glossary.md](glossary.md).

---

## The shape

The harness is a **CI/CD pipeline whose build steps happen to be LLM agents, and
whose threat model assumes those build steps are hostile.** That framing drives
every decision: it is not "a multi-agent system" with security bolted on, it is a
verification pipeline that uses agents as untrusted producers.

Three kinds of process exist at runtime:

- **Orchestrator** — the *scheduler* and *gatekeeper*. The only process that
  watches and writes [beads](components/orchestrator.md). It computes ready work,
  dispatches it, runs gates, advances the workflow graph, routes failures, and
  reconciles stranded work. **It executes nothing itself.**
- **Runner** — a per-host daemon. The only *long-lived* NATS citizen on its host
  and the only thing with credentials. It pulls work, spins up an ephemeral agent
  inside a sandbox, **brokers all of that agent's I/O**, harvests the result, and
  reaps the sandbox. See [components/runner.md](components/runner.md).
- **Agent** — an ephemeral, sandboxed process that lives for exactly one work
  item, then dies. It has a *Soul* (identity/config) and runs an agentic loop. It
  is **untrusted**. See [components/agent.md](components/agent.md).

```
┌─ TRUSTED ─────────────────────────┐        ┌─ UNTRUSTED (sandbox) ──────┐
│ orchestrator   (scheduler/gate)   │        │  agent  (one invocation)   │
│ runner         (broker + creds)   │◄──────►│   • LLM loop               │
│ requirements planner (human-facing)│ vsock/ │   • tools on its worktree  │
│ beads + git main                  │  uds   │   • NO direct network      │
└───────────────────────────────────┘        └────────────────────────────┘
        ▲  NATS / JetStream  ▲
        └── distributable: runners may live on any host ──┘
```

Everything between processes is [NATS](messaging.md), so the diagram's topology is
a *deployment choice*, not a code constraint. A runner can be a goroutine in the
orchestrator's process today and a separate binary on another host tomorrow.

---

## The trust boundary

This is the single most important line in the system.

**Trusted:** the orchestrator, the runner, the human-facing requirements planner
(it runs no untrusted code), beads, and `main`.

**Untrusted:** every sandboxed agent and every artifact it produces — including
its own reasoning loop, which may be prompt-injected.

Consequences that ripple through every other spec:

- An untrusted agent has **zero direct network**. Its only channel is a local
  socket to its runner, which brokers a small allowlist of destinations (LLM API,
  NATS, package proxy, git). One chokepoint to audit. See
  [security.md](security.md).
- An untrusted agent **cannot write beads**. It *proposes* changes; the
  orchestrator validates and writes them. See "single writer" below.
- An untrusted agent **cannot grade its own work**. Verification runs in a
  separate, clean sandbox the orchestrator controls. See
  [verification.md](verification.md).
- An untrusted agent **cannot merge to `main`**. It produces a *candidate branch*;
  the trusted merge queue decides. See [integration.md](integration.md).

The recurring pattern is **producer ≠ verifier**, applied at three levels: tests
authored independently of code, results graded independently of the producer, and
mutations applied independently of the proposer.

---

## Single source of truth, single writer

Beads holds the entire work graph. **Only the orchestrator writes to it.** Agents
are sandboxed and untrusted, so they cannot mutate the source of truth directly;
instead each agent returns a *Result envelope* proposing changes, and the
orchestrator validates them before applying. This single rule buys two things:

- **Consistency** — no inter-agent races on the graph; one writer, total order.
- **Security** — untrusted code can't corrupt the work graph. A confused or
  compromised agent that proposes a dependency cycle or an illegal transition is
  simply rejected at validation.

"Done" is therefore a *proposal*, not a fact. An agent declaring itself finished
yields a candidate; the issue only transitions to accepted after the
orchestrator's independent gate passes.

---

## The two graphs

The original framing was "a DAG of agent flows." In practice **two distinct
graphs** are in play, and conflating them causes confusion:

1. **The issue dependency graph** (in beads) is a true **DAG** — append-only and
   acyclic. A fix-issue depends on its predecessor; edges never point backward.
   The orchestrator enforces acyclicity when it applies proposed mutations.

2. **The role flow** (the workflow) is a **bounded feedback loop**. `qa` failing
   routes back to `implement` (`on_failure`), so the *roles* cycle even though the
   *issues* don't.

The critical consequence: **acyclicity does not guarantee termination.** The
feedback loop could iterate forever on a spec the factory cannot satisfy. The only
thing that guarantees the system halts is the **budget / retry cap → dead-letter**
mechanism. Budgets are not a cost feature; they are the halting condition. See
[workflow.md](workflow.md).

---

## A unit of work, end to end

```
human + LLM   Create-Task wizard → spec(git) + seed issue(beads)   TRUSTED
    │
orchestrator  sees ready issue → dispatches to a Role          (scheduler)
    │
runner        pulls work → seeds sandbox + worktree → brokers I/O
    │
agent         runs its loop → proposes a Result (branch+evidence)   UNTRUSTED
    │
orchestrator  gates the result in a CLEAN sandbox              (gatekeeper)
    │           ├─ accept  → advance graph, create next-stage issue
    │           ├─ reject  → on_failure route, new fix issue
    │           └─ escalate→ dead-letter for human spec refinement
    │
merge queue   rebase onto main → re-gate merged result → fast-forward   TRUSTED
```

Each box maps to a spec: [workflow.md](workflow.md),
[components/orchestrator.md](components/orchestrator.md),
[components/runner.md](components/runner.md),
[components/agent.md](components/agent.md), [verification.md](verification.md),
[integration.md](integration.md). Humans drive the first and last human-touch
points through the [control room](control-room.md); the whole run is captured for
[observability](observability.md).
