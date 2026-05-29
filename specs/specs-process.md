# The Spec Process

How specifications are written, how humans interact with the factory, and how spec
drift is handled. This is *meta*: the rules governing the very documents the
factory consumes as the source of intent.

See also: [verification.md](verification.md), [workflow.md](workflow.md),
[components/agent.md](components/agent.md).

> The product specs the factory builds from live in a `specs/` tree **in the target
> project**. This document describes the format and process for those specs. (The
> specs in *this* directory describe the harness itself and follow the same
> conventions.)

---

## Format

- Specs are **markdown files** in a `specs/` directory, with a `README.md` index.
- Subsequent spec files and subdirectories are **cross-linked** (wiki-style); the
  index plus links form a navigable graph.
- Specs are **pure prose**. There is *no* requirement for structured
  acceptance-criteria blocks — the [test author](verification.md) interprets prose
  into tests.
- Specs are stored in **git** (versioned, the durable contract). beads issues
  *reference* spec files by path; spec text does **not** live in issue bodies.

The choice of pure prose is for authoring ergonomics. Its cost: the test author's
interpretation carries the full correctness burden, which is exactly why the
[trust mechanisms](verification.md) (independence, red→green, mutation, the
traceability map) matter.

---

## Specs are the only human lever

The factory's entire human interface is **authoring and refining specs.** Humans
do not read or write code — not in the happy path, and not when things go wrong.

This is the **human re-entry invariant:**

> The only human action on the system — including for stuck or dead-lettered work
> — is authoring/refining specs and seeding issues. Humans never edit code.

When work dead-letters (budget/retries exhausted, or an unresolved escalation), a
human reads the escalation and responds by **refining the spec** (resolving the
ambiguity, adjusting criteria, descoping) and re-seeding — never by patching code.
This keeps "humans never touch code" intact and makes the human's whole job
intent-authoring.

Both authoring new intent and refining it in response to an escalation happen
through the **same wizard** in the [control room](control-room.md) — "Create Task"
and "Resolve" are one flow in two entry modes. Everything else the human sees is
read-only situational awareness: the board, the [event stream](messaging.md),
[traces](observability.md), and the [dead-letter queue](workflow.md).

Each wizard session stores two things as provenance behind the spec it produces:
the **conversation transcript** (in the [artifact store](components/artifact-store.md))
and the **finalized decisions** (a short markdown sidecar in git, per epic/spec
area). The spec itself stays the source of truth; these are the "why" behind it.
Git history of the decisions sidecar is the decision-evolution record — no separate
status or supersession machinery.

---

## Spec context horizon

Because spec files cross-link, an agent is given a **bounded slice**, not the whole
tree: the referenced file plus its linked neighbours to a configured depth.
Slurping the entire `specs/` tree would blow context and dilute focus. The
requirements planner owns link integrity (a natural postcondition on the
`requirements` stage: every link resolves; every spec maps to ≥1 issue).

The slice is built by the trusted orchestrator, not the agent. Each issue carries a
**structured spec reference** (the repo-relative path of its governing file), set at
seed time and by the planner per child, and threaded forward across an epic's stages
like the candidate base. From it the orchestrator resolves the slice — breadth-first
over markdown cross-links to `spec_depth` hops (see [configuration.md](configuration.md)),
confined to the repository, external/anchor/non-markdown links skipped — and embeds it
in the [Brief](components/agent.md#the-brief). The slice is assembled deterministically
so it can be content-hashed for version pinning (below). An issue naming no spec gets no
slice and the agent falls back to the tree in its worktree.

---

## Spec drift

"Make sure the implementation stays aligned with the spec" is **not** primarily a
prompt instruction — asking the producer to self-certify alignment would
re-introduce the self-grading problem. The alignment *guarantee* is structural: the
independently-authored acceptance tests **are** the drift detector. Three distinct
kinds of drift, handled in three places:

| Drift | What it is | Handled by |
|-------|------------|------------|
| **Implementation diverges from spec** | code doesn't do what the spec says | the [gate](verification.md) — drift = failing tests. (Prompt nudge only reduces churn.) |
| **Discovered spec gap** | agent finds the spec ambiguous / contradictory / silent | **detect and escalate, never resolve** — return `needs-spec-clarification`; route to human re-entry. The agent never edits the spec and never guesses. |
| **Spec changes in-flight** | a human refines a spec while work is underway | **spec-version pinning** (below) — the prompt can't see this; it must be structural. |

### Spec-version pinning

Each issue's [Brief](components/agent.md#the-brief) pins the **content hash** of
its spec slice. When a human edits a spec file, the orchestrator diffs which issues
referenced it and **invalidates / re-derives** the affected ones — stale in-flight
work is reissued; already-merged work may spawn new issues for the diff. Mental
model: **the factory recompiles the delta** when the spec changes.

---

## What goes in the soul prompt (re: specs)

- Follow the referenced spec slice.
- Cite the spec for non-obvious decisions (feeds the traceability map).
- **Escalate ambiguity rather than invent intent.**
- Do not invent scope.

The prompt does *not* say "self-certify alignment" — certification lives in the
independent tests and the gate.

---

## OPEN questions

- A spec *linter* (link integrity, orphan specs, issue ↔ spec coverage) as a
  `requirements`-stage postcondition — desirable; mechanism TBD.
- Whether to sanitize spec content as a prompt-injection surface before it enters
  an agent's context (see [security.md](security.md)).
