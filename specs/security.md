# Security

The harness is a **secure software factory**: it must produce trustworthy software
while assuming its own workers are hostile. Security is not a feature here, it is
the premise.

See also: [architecture.md](architecture.md),
[components/sandbox.md](components/sandbox.md),
[components/runner.md](components/runner.md), [verification.md](verification.md).

---

## Threat model

**Assume the agent loop and the code it generates may be hostile.** Concretely,
defend against:

- An agent that is **prompt-injected** (via spec content, a fetched dependency, a
  tool result) into acting against intent.
- **Generated code** that tries to exfiltrate data, reach the network, escape the
  sandbox, or persist beyond its task.
- **Supply-chain attacks** — a malicious or typo-squatted dependency pulled during
  a build.
- An agent attempting to **corrupt the work graph** or **merge unreviewed code**.

Out of scope for now: a compromised *trusted* component (orchestrator/runner host)
and production deploy (deferred).

---

## Trust boundaries

**Trusted:** orchestrator, runner, the human-facing requirements planner (runs no
untrusted code), beads, `main`.

**Untrusted:** every sandboxed agent and every artifact it produces.

The controls below all derive from keeping that line bright.

---

## Control 1 — Zero-network sandboxes, brokered egress

The single most important control. A sandboxed agent has **no direct network**.
Its only channel is a local socket to its [runner](components/runner.md), which
relays a small **allowlist** and logs every call:

- LLM API · NATS · vetted package mirror · git (task branch only).

Everything else is denied by default. The runner is the **one audited chokepoint**
for all agent egress. A compromised agent simply has nowhere to send data and
nothing to pull from. See [components/sandbox.md](components/sandbox.md).

---

## Control 2 — Supply-chain mediation

Dependency fetches are the obvious attack on an autonomous factory. All package
access is routed through a **vetted mirror/proxy** that pins, scans, and logs what
is pulled. The `qa` gate additionally runs dependency/vulnerability/license scans
(see [verification.md](verification.md)).

---

## Control 3 — Scoped, short-lived secrets

- The runner mints **per-task** credentials, **narrowly scoped** (ideally a git
  token that can push *only* the task's branch).
- Injected for the invocation lifetime; **die with the sandbox**.
- **No standing credentials baked into images.** Generated code must never be able
  to capture a durable secret.

---

## Control 4 — Producer ≠ verifier

Verification runs in a fresh sandbox the orchestrator controls, never the
producer's; tests are authored independently of code; agents propose and the
orchestrator applies. This prevents untrusted processes from grading their own
output or merging their own code. See [verification.md](verification.md),
[integration.md](integration.md).

---

## Control 5 — Single-writer work graph

Only the orchestrator writes beads. Agents *propose* mutations, which are validated
for DAG-legality (valid roles, acyclic edges, in budget) before being applied. A
confused or compromised agent cannot inject garbage or cycles into the work graph.
See [components/orchestrator.md](components/orchestrator.md).

---

## Control 6 — Bounded autonomy

[Budgets and retry caps](workflow.md) cap blast radius and guarantee termination.
A runaway or adversarially-looping task always terminates into the dead-letter
queue rather than consuming unbounded compute.

---

## Provenance by construction

Because no human reviews the code, the **audit trail is the accountability**.
Every merged change carries a chain: **beads issue → commit → signed evidence**,
recorded as a commit trailer:

```
Soul: implementor-go | Model: claude-opus-4-7
Issue: bd-1234 | Prompt-SHA: 9af… | Verified: build@sha256:1c2…,test@sha256:8be…,gosec@sha256:0a4…
```

This is a SLSA-style provenance record: every autonomous change is attributable to
the soul, model, prompt, and the evidence that gated it. Each entry in `Verified`
cites a passed check as `<name>@<evidence-hash>`, the hash pointing into the
content-addressed [artifact store](components/artifact-store.md) at that check's
captured output — so verification is auditable down to the exact bytes, not merely a
list of check names. The `Prompt-SHA` and the evidence hashes are both such pointers,
so a record cannot be silently altered. A check whose evidence failed to persist
degrades to a bare `<name>` (self-describing, like a missing `Prompt-SHA`), never a
dropped verdict. Commits/artifacts should be signed with the harness's identity.

So the trailer can be vouched for by the trusted layer, the tip of `main` is always
a **harness-authored provenance commit** sitting on top of the verified candidate,
never the agent's own commit fast-forwarded into place. See
[integration.md](integration.md).

---

## OPEN questions

- Signing scheme / key custody for provenance trailers — TBD.
- Whether spec content itself should be treated as an injection surface and
  sanitized before entering an agent's context — likely yes; mechanism TBD.
