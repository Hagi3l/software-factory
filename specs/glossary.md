# Glossary

Shared vocabulary for the harness. When a spec capitalises a term oddly, it refers
to a definition here.

See also: [architecture.md](architecture.md), [README.md](README.md).

---

### Agent
An ephemeral, sandboxed process that performs exactly one work item and then dies.
Untrusted. Has a [Soul](#soul) and runs an agentic loop. Defined in
[components/agent.md](components/agent.md).

### Artifact store
A content-addressed store for the large evidence an invocation produces
(transcripts, gate outputs, mutation reports, diffs), harvested before the sandbox
is destroyed and referenced by hash from beads and provenance. See
[components/artifact-store.md](components/artifact-store.md).

### Beads (bd)
A git-backed, dependency-aware issue tracker used as the work-item store. Its
`blocked-by` edges form the [issue dependency graph](#issue-dependency-graph); its
ready-work query feeds the scheduler. Written only by the orchestrator.

### Brief
The task envelope handed *into* a sandbox: the issue, the resolved
[spec slice](#spec-slice), the base git ref, the postconditions to satisfy, and
the [Soul](#soul). The agent's entire input. See [components/agent.md](components/agent.md).

### Broker
The runner's relay that gives a zero-network sandbox controlled access to an
allowlist of external destinations (LLM API, NATS, package mirror, git). The single
audited chokepoint for all agent I/O. See [components/runner.md](components/runner.md),
[security.md](security.md).

### Dead-letter (DLQ)
Where work goes when it exhausts its [budget](#budget) or retry cap, or raises an
unrecoverable escalation. A human triages it by refining the spec. See
[workflow.md](workflow.md).

### Budget
A cap (tokens / money / wall-clock) on an invocation and on an epic. The
*termination guarantee* for the autonomous system, not just cost control. See
[workflow.md](workflow.md).

### Control room
The web UI ([control-room.md](control-room.md)): the human's read-only window into
the factory plus the [wizard](#wizard) — their only place to act.

### Gate
A postcondition check (build, test, mutation score, security scan) the orchestrator
runs in a clean [verification sandbox](#verification-sandbox) to decide whether a
candidate is accepted. See [verification.md](verification.md).

### Issue dependency graph
The acyclic, append-only DAG of beads issues and their `blocked-by` edges. Distinct
from the [role flow](#role-flow). See [architecture.md](architecture.md).

### Orchestrator
The single scheduler + gatekeeper + sole beads writer. Watches ready work,
dispatches, gates results, advances the graph, reconciles. Executes nothing itself.
See [components/orchestrator.md](components/orchestrator.md).

### Plan stage (decomposition planner)
The autonomous, **sandboxed** workflow stage (`kind: plan`) whose soul decomposes a
seed issue into a DAG of child work items. Untrusted like any agent; it *proposes*
children the orchestrator validates. Distinct from the
[requirements planner](#requirements-planner). See [workflow.md](workflow.md),
[components/orchestrator.md](components/orchestrator.md).

### Producer ≠ verifier
The principle that whoever produces an artifact never grades it. Applied to tests
vs. code, results vs. producer, and mutations vs. proposer. See
[verification.md](verification.md).

### Provider adapter
A thin translator (held by the runner, over the official Go SDK) between the
harness's canonical model interface and a specific provider's wire format
(Anthropic, OpenAI-compatible). What makes the harness model-agnostic. See
[models.md](models.md).

### Replay
Reconstructing an invocation's full decision trail (what the LLM saw and did, step
by step) from broker-captured events and the [artifact store](#artifact-store). The
differentiator for observability and the audit mechanism for no-human-review. See
[observability.md](observability.md).

### Requirements planner
The **trusted, non-sandboxed** conversational planner behind the
[wizard](#wizard)'s Create-Task / Resolve flow. It helps a human elicit intent and
draft specs + seed issues, talking to the model layer directly (no broker, no
sandbox — it runs no untrusted code). Distinct from the autonomous
[plan stage](#plan-stage-decomposition-planner). See
[control-room.md](control-room.md), [specs-process.md](specs-process.md).

### Result (envelope)
What an agent returns *out of* the sandbox: status, candidate branch ref, evidence,
and any proposed child issues. Proposals only — the orchestrator validates and
applies. See [components/agent.md](components/agent.md).

### Role
The workflow-level abstraction a DAG stage references (e.g. `implementor`).
[Souls](#soul) *fulfil* roles. A role may map to a set of souls, selected per issue
by a `selector`. See [configuration.md](configuration.md).

### Role flow
The workflow viewed as transitions between roles. A *bounded feedback loop*
(qa→implement→qa), distinct from the [issue dependency graph](#issue-dependency-graph).
See [architecture.md](architecture.md).

### Runner
A per-host daemon that pulls work, runs ephemeral agents in sandboxes, brokers their
I/O, and reaps them. The only long-lived NATS citizen and credential holder on its
host. See [components/runner.md](components/runner.md).

### Sandbox
The isolation boundary an agent runs inside (Firecracker / Docker / gVisor). Zero
direct network. See [components/sandbox.md](components/sandbox.md).

### Soul
An agent's identity package: name, role, model, persona/prompt, tools, sandbox
profile. Stateless — carries no cross-task memory. Souls fulfil roles. See
[components/agent.md](components/agent.md), [configuration.md](configuration.md).

### Spec slice
The bounded portion of the `specs/` tree handed to an agent: the referenced file
plus its linked neighbours to a depth, not the whole tree. See
[specs-process.md](specs-process.md).

### Trace
The observability model for one invocation: epic → issue → invocation → spans
(boot, llm-turn, tool-call, gate-run). Maps to OpenTelemetry. See
[observability.md](observability.md).

### Trusted Computing Base (TCB)
The components that *enforce* the harness's guarantees — orchestrator, runner/broker,
sandbox config, the gate harness, the verification stack. An unverified harness
cannot vouch for its own verifier, so **TCB-touching changes stay human-reviewed
even after self-hosting** (arguably permanently). The boundary is operationally the
`policy.tcb_paths` globs that force `human-approved` regardless of profile. See
[bootstrap.md](bootstrap.md), [configuration.md](configuration.md).

### Verification sandbox
A *fresh* sandbox, controlled by the orchestrator and distinct from the producing
agent's, in which [gates](#gate) run so that an untrusted process never grades its
own output. See [verification.md](verification.md).

### Wizard
The interactive "Create Task" / "Resolve" flow in the [control room](#control-room)
through which a human authors or refines specs and seeds issues — the only
human-in-the-loop surface and the consent gate. See
[control-room.md](control-room.md), [specs-process.md](specs-process.md).
