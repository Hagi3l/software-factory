# Integration

How verified candidate branches become commits on `main`. The mechanism is a
**serialized merge queue** (a merge train), which exists to close a correctness
gap that naive "merge when green" walks straight into.

See also: [workflow.md](workflow.md), [verification.md](verification.md),
[components/orchestrator.md](components/orchestrator.md).

---

## The trap: two green branches can break `main`

Each `implement` issue produces its own candidate branch off `main`
(`candidate/<issue-id>` — the canonical name the agent's `submit` commits onto and
the [broker](components/runner.md) refuses to deviate from). Many run in parallel. The decomposition planner serializes *known* conflicts with `blocked-by`
edges — but it cannot predict every collision.

Two branches that each pass their gate **independently** can still combine into a
broken `main`. The conflict need not even be textual: branch A renames a function,
branch B adds a caller of the old name; neither branch's gate ever saw the other,
both are green, and `main` breaks on merge.

Therefore "merge when the branch's QA passed" is unsafe. The gate result is only
valid against the tree the branch was tested on.

---

## The merge queue

Integration is **serialized**. `integrate` issues are popped from a queue in
[issue-graph](glossary.md#issue-dependency-graph) topological order and processed
one at a time:

```
1. pop next integrate issue (beads-topological order)
2. rebase its branch onto the CURRENT main tip
       └─ conflict? → spawn a sandboxed resolution issue, block, loop
3. re-run the FULL gate suite in a clean verification sandbox
   against the REBASED result — i.e. against what will actually land
       └─ fail? the combination broke something → spawn a fix issue, loop
4. advance main to the verified candidate + write the provenance trailer
5. next in queue
```

**Step 3 is load-bearing:** gates run against the *merged* result, not the branch
as originally authored. That is what catches the two-green-branches problem, and
it reuses the [producer ≠ verifier](verification.md) machinery against the
*combination* rather than a single branch.

**The queue announces itself.** Serialized integration is otherwise a black box —
nothing is observable between "the branch's gate passed" and "a commit appeared on
`main`," yet that interval is where rebases conflict and combinations break. So the
orchestrator publishes a typed [`merge-state` event](messaging.md) at each step a
candidate passes through — `queued → rebasing → re-gating → landed`, or the terminal
`conflicted` (step 2) / `regate-failed` (step 3). These are exactly the states the
queue already moves through as control flow; naming and emitting them makes the train
visible in flight via the [merge-queue view](control-room.md). Like
[issue-state](messaging.md) they are an additive, best-effort observability emit — the
authoritative queue state stays the git refs and beads, never reconstructed from the
events — and a `conflicted` / `regate-failed` event correlates with the
[dead-letter](control-room.md) or fix issue the same transition already routes.

**A clean rebase is a deterministic git computation, not an agent task.** It runs
on objects the orchestrator already holds in the integration repo (runners pushed
the candidate branches there), and applying a diff with `git rebase` executes no
code from the candidate — its own hooks and filters are never installed. So the
trusted layer performs a clean rebase directly; it does not need a sandbox, because
correctness is re-established by the step-3 re-gate (the rebased tree may be broken
even when it merged textually clean — that is exactly the two-green-branches case),
not by trusting the rebase. The sandbox is for the *other* case: conflict resolution
that needs an LLM runs **sandboxed** and produces a *proposed* rebase; the trusted
layer does the final `git` write. Untrusted environments never hold the keys to
`main`.

The queue was built up incrementally: the serialized rebase-onto-current-main (step
2, with a conflict escalating to the dead-letter queue) landed first; re-gating the
rebased result (step 3) followed; **sandboxed conflict resolution (the step-2 conflict
branch) is now realized too.** On a rebase conflict the orchestrator no longer
dead-letters: it spawns a sandboxed **resolution issue** for a `resolve` stage (a
`merge-resolver` soul, [configuration.md](configuration.md)), seeded at the *conflicting
candidate* as its base. The merge-resolver agent rebases that candidate onto `main` in
its sandbox, resolves the conflicts to the combined intent of both changes, and submits a
new candidate — which is gated (the resolve stage's postcondition re-runs the full check
suite) and then `produces: [integrate]`, re-entering the queue to rebase onto whatever
`main` has since become and re-gate before landing. The conflict loop is bounded by the
**same** termination guarantees as a fix loop — the retry cap and the cumulative per-issue
budget — so a conflict no rebase can resolve eventually dead-letters for human triage
rather than looping forever; with no `resolve` stage configured (the kernel) the conflict
falls back to the original dead-letter. The trusted layer never hands `main` to the
untrusted resolver: it only *proposes* a rebased candidate; the orchestrator re-gates it
(producer ≠ verifier) and performs the final `git` write itself. **Step 3 is realized:** when a candidate is
rebased, the trusted layer publishes the rebased result under a temporary
`refs/heads/integration/<issue-id>` branch (clonable, so a verification sandbox can
fetch it), re-runs the producing stage's full check suite against *that* tree in a
fresh producer-distinct sandbox, and advances `main` only on a pass — recording the
re-gate's own checks in the provenance trailer, since the combination is what landed.
A re-gate failure does **not** dead-letter (unlike a conflict, which is
deterministic): the combination may pass against a different `main`, so the
orchestrator routes a fix issue through the normal retry/budget machinery. A
fast-forward skips step 3 — it lands the exact tree the branch gate already graded,
so there is nothing new to verify.

---

## Provenance on merge

Every merge to `main` carries a trailer linking the commit back through the whole
chain (see [security.md](security.md)). The commit subject is the **issue's title**, so
`main`'s history reads like an ordinary project's; the trailer below the blank line is the
audited record:

```
Add single-use share link

Soul: implementor-go | Model: claude-opus-4-7 | Tests-Soul: test-author-go
Issue: bd-1234 | Prompt-SHA: 9af… | Verified: build@sha256:1c2…,test@sha256:8be…,gosec@sha256:0a4… | Traceability: sha256:7c1… | Transcript: sha256:3d2…
```

The subject is purely cosmetic — the durable reference is the `Issue` id on the trailer, and
the read path parses the trailer, never the subject — so a change whose issue carries no
title degrades to the subject `Integrate <id>`, never a dropped trailer.

Each `Verified` entry cites a passed check as `<name>@<evidence-hash>`, the hash
pointing into the [artifact store](components/artifact-store.md) at that check's
captured output; `Traceability` cites the `author-tests` stage's
[test↔spec traceability map](verification.md), threaded forward to the merge; and
`Transcript` cites the broker-captured agent conversation — the replayable decision
trail (see [observability.md](observability.md)) — all threaded forward to the merge
(see [security.md](security.md)).

beads issue → commit → signed evidence is a SLSA-style chain that makes every
autonomous change traceable, which matters precisely because no human reviewed it.

**The trailer requires a trusted commit, so the final step is *not* a literal
fast-forward.** A bare fast-forward would move `main` onto the agent's own commit,
leaving nowhere to attach a trailer the trusted layer vouches for. Instead the
orchestrator creates a **provenance commit on top of the verified candidate** —
same tree (no file changes), parent = candidate tip, authored by the harness
identity, message = the issue title (subject) followed by the trailer — then
advances `main` to it. So `main`'s tip is
always a trusted, attributable commit and the candidate history stays intact below.
When a signing key is configured the orchestrator **SSH-signs** this commit with the
harness identity (verified on read; see [security.md](security.md) "Signing the
provenance commit"), so `main`'s tip is not merely labeled with the harness's name but
cryptographically provable to be its work.
This stays within fast-forward semantics (`main` must be an ancestor of the
candidate; a non-fast-forward is still refused — that is what the rebase in step 2
is for), and it is idempotent: a redelivered accept re-detects the candidate as an
ancestor and writes nothing.

---

## Atomic feature integration (epic mode)

Everything above is **per-item** integration: each work item lands on `main` the
moment its own chain is verified. A feature that the [decomposition
planner](workflow.md) split into many children therefore arrives as **several**
commits — and `main` passes through partial-feature states on the way. That is the
right default (maximum throughput; children land as they finish), and it is the
**`per-item`** integration mode.

Some deployments want the unit of integration to be the **whole feature**, not the
work item — *one [wizard](control-room.md) conversation → one feature → one landing
on `main`*. The motivating case: a `main` push triggers a deploy, so a feature that
lands as five commits deploys five times, several of them incomplete. The
**`epic` integration mode** ([configuration.md](configuration.md)) makes a feature
land **atomically**: all of it, or none of it, in a single merge.

**The principle.** In epic mode the **`epic/<epic_id>` branch is "`main`" for
child-level integration**; the real `main` is advanced exactly once, by the epic's
terminal merge. Everywhere the per-item merge queue says "`main`" — the rebase
target, the fast-forward check, the re-gate base, conflict detection, the `resolve`
stage's rebase — read "the epic branch." `epic_id` is the root seed's id already
threaded forward across every issue of the epic (the same field
[`epic_budget`](workflow.md) aggregates over), so no new primitive is introduced;
the merge queue is simply *retargeted* per epic.

**The epic branch.** It is created off `main` when the epic begins, and the
[wizard's approval](control-room.md) commits the drafted spec as its **first
commit** — *not* onto `main`. This is load-bearing for atomicity: if the spec landed
on `main` at approval, `main` would move (and a deploy would fire) before the feature
existed. With the spec on the epic branch, `main` moves exactly once, at the terminal
merge, which brings the spec and all child code together.

**Completion.** The epic is done when its subtree has **drained**: every issue
sharing the `epic_id` is closed (integrated onto the epic branch) **and zero
invocations in that subtree are in flight**. The in-flight clause is what makes it
safe to declare done — only a running invocation can `request_subtask`, so an empty
in-flight set with no open issues means the subtree can no longer grow. The
orchestrator computes this as an **`epic_id` aggregate read** (like `epic_budget`,
[components/orchestrator.md](components/orchestrator.md)), on its slow sweep cadence,
not the dispatch hot path. On drain, the **epic root issue** advances to its terminal
merge.

**The terminal merge is a merge commit.** First parent = the current `main`, second
parent = the epic branch tip; subject = the feature's (epic root's) title; trailer =
the provenance record. Provenance is **two-tier**: each child's per-child provenance
commit stays reachable under the merge (issue → soul → model → prompt → per-child
evidence), and the merge commit records the **whole-feature** layer on top — so the
granular audit trail is preserved while `main`'s first-parent history reads as one
commit per feature.

**The whole-feature gate is emergent, not extra.** Children serialize onto the epic
branch, each rebasing onto the prior tip and re-gating (step 3 above). So when the
*last* child lands, the epic tip already holds all its siblings and was just
re-gated **as the complete feature**. In v1 (one active epic — below — so `main` is
quiescent during an epic) the epic branch is already based on the current `main`, so
the terminal merge introduces no new tree and needs no further gate: it writes the
merge commit and advances `main`. Re-gating *at* the terminal merge only becomes
load-bearing once `main` can move under an in-flight epic (concurrency, OPEN) — at
which point it is the same rebase-onto-current-`main` + re-gate this queue already
does, at epic granularity.

**All-or-nothing.** If any child dead-letters (a gate failure exhausting its retry
cap, a `resolve` that cannot land, an [`epic_budget`](workflow.md) breach), the
subtree never drains clean, so the terminal merge **never fires**: `main` is
untouched and the epic branch is **abandoned** (left for inspection or GC, never
merged). A feature lands whole or not at all — which is exactly what makes the next
deploy a *finished* feature. Triage is the usual lever: refine the spec, re-run the
wizard ([specs-process.md](specs-process.md)).

**v1 scope — one active epic.** Epic mode v1 runs **one epic at a time**: the wizard
**refuses** a second approval while an epic is in flight (the consent gate reports
the in-flight feature). This keeps `main` quiescent during an epic, which is what
lets the terminal merge skip re-gating. Default `per-item` mode is unaffected and
remains the kernel behaviour.

---

## Tradeoff and future work

Integration is serialized (one at a time) even though *implementation* is fully
parallel. This is fine for almost all projects. If integration ever becomes the
measured bottleneck, **optimistic / speculative batching** (merge several, re-gate
the batch, bisect on failure — as large CI merge queues do) can be added later.

> Do not build speculative batching until the serial queue is a proven bottleneck.

---

## OPEN questions

- ~~Whether `integrate` is its own [Role](glossary.md#role) with a dedicated
  integrator soul, or a built-in orchestrator function with sandboxed help only for
  conflict resolution.~~ **Decided:** `integrate` stays an **orchestrator-owned**
  function (the trusted layer performs the rebase + final `git` write to `main`);
  the *only* part handed to a [Role](glossary.md#role) is **conflict resolution**, via
  the sandboxed `resolve` stage / `merge-resolver` soul, which merely *proposes* a
  rebased candidate the orchestrator then re-gates and merges.
- Branch retention / cleanup policy after merge — TBD, but bounded by a hard
  invariant (below). The `epic/<epic_id>` branch is subject to the same invariant:
  it is deleted only after its terminal merge lands (its history survives via the
  merge commit's second parent), and an abandoned epic's branch is retained for
  triage, never GC'd while any of its issues is open/in-flight.
- **Concurrent epics (epic mode).** v1 runs **one epic at a time** (the wizard
  refuses a second approval while one is in flight). Lifting this needs a
  **two-level merge queue**: children serialize onto their own epic branch, and the
  epic→`main` terminal merges serialize onto `main` — at which point an in-flight
  epic whose `main` moved under it must **rebase its epic branch onto the new `main`
  and re-gate the whole feature** at the terminal merge (the same step-2/step-3
  machinery, at epic granularity). Deferred; tracked in `IMPLEMENTATION_PLAN.md`.

---

## Invariant — never GC a referenced candidate base

Downstream agent stages branch from their **predecessor's candidate branch** (it is
the base a produced issue branches from — see base threading in
[workflow.md](workflow.md)). Therefore **branch cleanup must never delete a candidate
branch still referenced as a base by an open/in-flight issue.** Any cleanup policy
that lands must check live base references first; a naive "delete the branch on
merge" breaks base threading and orphans downstream work. This holds regardless of
the retention policy eventually chosen.
