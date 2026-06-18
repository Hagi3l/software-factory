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
intent-authoring. Approving a Resolve in the wizard commits the refined spec and
**returns the dead-lettered issue to the ready pool** so it is re-dispatched against
the now-clarified spec: a blocked issue is neither in-flight nor merged, so the
continuous recompile sweep (below) does not reach it — the wizard reopens it
explicitly (clearing its stale spec pin), while the sweep re-pins and reissues the
rest of the affected work. The wizard shows that **blast radius** before the human
consents, so the consequence of the edit is visible at the moment of approval.

Both authoring new intent and refining it in response to an escalation happen
through the **same wizard** in the [control room](control-room.md) — "Create Task"
and "Resolve" are one flow in two entry modes. Everything else the human sees is
read-only situational awareness: the board, the [event stream](messaging.md),
[traces](observability.md), and the [dead-letter queue](workflow.md).

Each wizard session stores two things as provenance behind the spec it produces:
the **conversation transcript** (in the [artifact store](components/artifact-store.md))
and the **finalized decisions** (a short markdown sidecar in git, per epic/spec
area — recording both agreed decisions and forks deliberately left open, the latter
being pre-context for any later `needs-spec-clarification` escalation). The spec
itself stays the source of truth; these are the "why" behind it.
Git history of the decisions sidecar is the decision-evolution record — no separate
status or supersession machinery.

---

## Editing the tree, not just appending to it

The wizard **maintains** the spec tree; it does not only grow it. When new or refined
intent falls within a domain an existing spec already owns, the wizard **edits that file in
place** — folding the behaviour in additively, preserving the sections already there —
rather than spawning a parallel spec. It **creates a new spec file only when the work is a
genuinely new domain** no existing spec covers. Two reasons this is the default:

- **No sprawl.** One domain, one spec. A feature scattered into a new near-duplicate file
  fragments the contract the test author reads.
- **Grounding by inheritance.** A feature folded into an existing spec inherits that spec's
  place in the cross-link graph — its links into the `README.md` index and its sibling
  specs — so the bounded slice (below) resolves the right neighbours automatically. A fresh
  standalone file starts disconnected until every link is wired by hand.

The **`README.md` index is part of the tree the wizard keeps current.** When the *set* of
spec files changes — a new spec added, or one removed — the wizard updates the index in the
same draft, so the navigable graph stays complete and a newly-added spec is reachable from
the index agents navigate from. A pure edit to an existing spec changes no file set and
needs no index change.

This applies to **both wizard modes**: Create authors or edits intent; Resolve edits an
existing spec to remove the ambiguity that stuck an issue. Both write spec *edits*, and an
edit is not new work to seed — so the **issue-coverage rule binds only newly-created
specs**: every new spec must map to ≥1 seed issue (no orphaned prose nobody will build),
but editing an existing spec — including the index — seeds no work and needs no backing
issue. (This is exactly why Resolve can refine a spec and reopen the stuck issue without
seeding anything new.)

---

## Spec context horizon

Because spec files cross-link, an agent is given a **bounded slice**, not the whole
tree: the referenced file plus its linked neighbours to a configured depth.
Slurping the entire `specs/` tree would blow context and dilute focus. The
requirements planner owns link integrity (a natural postcondition on the
`requirements` stage: every link resolves; every newly-created spec maps to ≥1 issue —
edits to existing specs, including the index, seed no work, see [above](#editing-the-tree-not-just-appending-to-it)).

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
its spec slice. The orchestrator computes it at dispatch — when it materializes the
slice — over the slice's deterministic bytes, embeds it in the Brief, and **stores it
on the issue** (beads metadata), so the issue durably records the spec *version* its
work was derived from. (Unlike the spec *path*, which is set at creation and threaded
forward, the *hash* is pinned per dispatch, because it captures what the agent actually
worked against.)

When a human edits a spec file, the orchestrator diffs which issues
referenced it and **invalidates / re-derives** the affected ones — re-resolve each
slice, re-hash, and compare to the pinned hash; a mismatch means stale in-flight work
to reissue, and already-merged work may spawn new issues for the diff. Mental
model: **the factory recompiles the delta** when the spec changes.

This is realized as a **continuous reconcile sweep**, not an explicit edit-event hook:
the orchestrator periodically re-resolves every in-flight (in_progress) issue's slice
and re-hashes it, so re-resolving *subsumes* "diff which issues referenced the edited
file" — an issue whose slice does not include the edit re-hashes unchanged and is left
alone. An issue whose hash no longer matches its pin is **reissued**: returned to the
ready pool (lease cleared so it re-dispatches afresh, and the stale pin cleared so the
next dispatch re-pins the edited slice), and its in-flight attempt's eventual result is
ignored because the issue is no longer in_progress. The sweep is best-effort and
deliberately conservative: an issue with no spec or no pin, or whose slice fails to
resolve (the file is mid-edit or was deleted — an ambiguous signal), is left untouched
rather than disrupting live work.

Re-deriving **already-merged** work for a spec diff is handled per **(epic, spec-path)**
unit, not per closed issue — which is precisely what dedupes one edit across an epic's many
closed issues that share a path. This is the heavier half of the recompile: it reads
the full closed-issue table to catch minute-plus-granularity human edits, so it runs on
its own slower cadence rather than at dispatch frequency (see
[orchestrator](components/orchestrator.md#live-state-vs-durable-state--the-work-graph-projection)
for the loop mechanism). The orchestrator groups closed issues by
their `epic_id` and spec path, re-resolves and re-hashes that slice, and on a mismatch
against the pinned hash spawns **one fresh `plan` issue** for that epic and path (carrying
the epic's id and tags, branched from the epic's merged tip). Re-entry is at the
**planning** stage, not `author-tests`: a spec change can add, remove, or alter work items,
which only the decomposition planner can express, and the planner — reading the edited spec
against the already-merged code — decomposes only the delta. The orchestrator then **re-pins
the closed issues' hash to the new slice** so the next sweep sees them settled and does not
respawn, and it **skips the spawn when an open re-derivation `plan` issue for that
`(epic, spec-path)` already exists** (idempotency). Known coarseness: a provably-localized
single-criterion edit still triggers a full planning pass; a future refinement could
re-enter at `author-tests` when the drift is local.

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
