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
2. rebase its branch onto the CURRENT main tip          (in a sandbox)
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

Conflict resolution that needs an LLM still runs **sandboxed** and produces a
*proposed* rebase; the trusted layer does the final `git` write. Untrusted
environments never hold the keys to `main`.

---

## Provenance on merge

Every merge to `main` carries a trailer linking the commit back through the whole
chain (see [security.md](security.md)):

```
Soul: implementor-go | Model: claude-opus-4-7
Issue: bd-1234 | Prompt-SHA: 9af… | Verified: build@sha256:1c2…,test@sha256:8be…,gosec@sha256:0a4… | Traceability: sha256:7c1…
```

Each `Verified` entry cites a passed check as `<name>@<evidence-hash>`, the hash
pointing into the [artifact store](components/artifact-store.md) at that check's
captured output; `Traceability` cites the `author-tests` stage's
[test↔spec traceability map](verification.md), threaded forward to the merge (see
[security.md](security.md)).

beads issue → commit → signed evidence is a SLSA-style chain that makes every
autonomous change traceable, which matters precisely because no human reviewed it.

**The trailer requires a trusted commit, so the final step is *not* a literal
fast-forward.** A bare fast-forward would move `main` onto the agent's own commit,
leaving nowhere to attach a trailer the trusted layer vouches for. Instead the
orchestrator creates a **provenance commit on top of the verified candidate** —
same tree (no file changes), parent = candidate tip, authored by the harness
identity, message = the trailer — then advances `main` to it. So `main`'s tip is
always a trusted, attributable commit and the candidate history stays intact below.
This stays within fast-forward semantics (`main` must be an ancestor of the
candidate; a non-fast-forward is still refused — that is what the rebase in step 2
is for), and it is idempotent: a redelivered accept re-detects the candidate as an
ancestor and writes nothing.

---

## Tradeoff and future work

Integration is serialized (one at a time) even though *implementation* is fully
parallel. This is fine for almost all projects. If integration ever becomes the
measured bottleneck, **optimistic / speculative batching** (merge several, re-gate
the batch, bisect on failure — as large CI merge queues do) can be added later.

> Do not build speculative batching until the serial queue is a proven bottleneck.

---

## OPEN questions

- Whether `integrate` is its own [Role](glossary.md#role) with a dedicated
  integrator soul, or a built-in orchestrator function with sandboxed help only for
  conflict resolution. Leaning: orchestrator-owned, sandboxed help on demand.
- Branch retention / cleanup policy after merge — TBD. Note that downstream agent
  stages now rely on the predecessor candidate branch persisting (it is the base a
  produced issue branches from — see base threading in [workflow.md](workflow.md)), so
  any future cleanup policy must not remove a candidate still referenced as a base.
