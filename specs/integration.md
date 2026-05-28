# Integration

How verified candidate branches become commits on `main`. The mechanism is a
**serialized merge queue** (a merge train), which exists to close a correctness
gap that naive "merge when green" walks straight into.

See also: [workflow.md](workflow.md), [verification.md](verification.md),
[components/orchestrator.md](components/orchestrator.md).

---

## The trap: two green branches can break `main`

Each `implement` issue produces its own branch off `main` (`bd/<id>`). Many run in
parallel. The decomposition planner serializes *known* conflicts with `blocked-by`
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
4. fast-forward merge to main + write the provenance trailer
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
Issue: bd-1234 | Prompt-SHA: 9af… | Verified: build,test,gosec,mutation
```

beads issue → commit → signed evidence is a SLSA-style chain that makes every
autonomous change traceable, which matters precisely because no human reviewed it.

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
- Branch retention / cleanup policy after merge — TBD.
