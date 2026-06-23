# Workflow

How a change moves from a human-authored spec to code merged on `main`.

See also: [architecture.md](architecture.md), [verification.md](verification.md),
[integration.md](integration.md), [components/orchestrator.md](components/orchestrator.md).

---

## The pipeline

```
requirements (human + LLM)  →  spec(git) + seed epic(beads)        TRUSTED
      │
plan          decompose spec into work items + dependency edges    sandboxed
      │
author-tests  write FAILING acceptance tests from the spec         sandboxed
      │
implement     make the tests pass                                  sandboxed
      │
qa / security run gates: tests, mutation, scanners                 sandboxed*
      │            └─ fail → on_failure → new fix issue (loop)
integrate     merge queue: rebase, re-gate, fast-forward to main   TRUSTED
      │
[future]      promote / deploy                                     gated
```

\* The qa *agent* may run in a sandbox, but the authoritative [gate](verification.md)
runs in a separate clean [verification sandbox](glossary.md#verification-sandbox)
controlled by the orchestrator — producer ≠ verifier.

The terminal state is **merged to `main`**. Production deploy is deliberately out
of scope for now; it is anticipated as an appended stage behind a promotion/policy
gate, which the config-driven DAG makes additive.

**Per-item vs. atomic-feature integration.** By default each work item lands on
`main` independently (`integration.mode: per-item`), so a decomposed feature arrives
as several commits. The opt-in **`epic` mode** instead lands the whole feature
**atomically** — children integrate onto an `epic/<epic_id>` branch and `main`
advances once, when the epic's subtree drains. The pipeline and stages are identical
either way; only *where* `integrate` lands and *when* `main` moves change. See
[integration.md](integration.md).

---

## Stages and roles

Each stage references a [Role](glossary.md#role); [Souls](glossary.md#soul) fulfil
roles (see [configuration.md](configuration.md)). The stages:

| Stage | Role | Trust | Produces |
|-------|------|-------|----------|
| `requirements` | (human) | trusted, **not** sandboxed | spec files + seed issues |
| `plan` | decomposition planner | sandboxed | `author-tests` issues |
| `author-tests` | test author | sandboxed | `implement` issues |
| `implement` | implementor | sandboxed | `qa` issue |
| `qa` | security/QA | sandboxed | `integrate` issue |
| `integrate` | (orchestrator) | trusted | merge to `main` |

**Two distinct planners** exist and must not be conflated:

- The **requirements planner** is interactive and human-facing. Its conversation and its
  spec/issue authoring are trusted and run host-side (it runs no untrusted code *on the
  host*). When the work targets an existing codebase it may also *read* that code to ground
  its specs — but those model-chosen reads run inside a **read-only, zero-network sandbox**
  seeded from the repo, exactly like an agent's reads, so a model-directed command never
  touches the host. It still **writes** nothing but specs + seed issues, and only on human
  approval. It is the one place a human is in the loop, realised as the **Create-Task wizard**
  in the [control room](control-room.md). See [specs-process.md](specs-process.md).
- The **decomposition planner** is autonomous and sandboxed. It reads a seed issue
  plus its spec and breaks it into concrete work items with dependency edges. It is the
  pipeline's autonomous entry: the human seeds one `plan` issue and the planner produces
  the `author-tests` issues. Realised as an **ungated `kind: plan` stage** (an agent stage
  that is not sandbox-gated — see below); the planner proposes each child with
  `request_subtask` and ends with `submit_plan`, producing no candidate branch.

Humans set *what/why*; the autonomous planner sets *how/decomposition*.

---

## Emergent within a stage, declarative between stages

This resolves "is the workflow a fixed DAG or emergent?" — it is both, on
different axes:

- **Breadth is emergent.** A stage decides *how many* sibling issues it produces;
  this is data-dependent and cannot be known up front. The decomposition planner
  might emit three `implement` issues or thirty.
- **Depth is declarative.** Stage→stage transitions are config (`produces:`),
  applied by the **orchestrator**, never by the agent. When `implement` passes its
  gate, the *orchestrator* creates the `qa` issue. When it creates the next-stage
  issue it also seeds it with the predecessor's just-verified candidate branch as its
  base, so the downstream stage builds on the work already done — e.g. an `implement`
  issue branches from the `author-tests` candidate that holds the failing acceptance
  tests, not from `main`. (The base rides in beads metadata and is preserved across
  `on_failure` retries.)

Therefore **agents never know what stage comes next.** They do their node and emit
a Result. This keeps souls fully decoupled from the workflow shape: planners
create breadth, the orchestrator creates depth.

Emergent breadth is still **validated, not trusted**: a planner *proposes* child
issues in its Result; the orchestrator checks they are DAG-legal (valid roles,
every dependency target exists, edges keep the graph acyclic, within budget) before
writing them. The existence check is the harness's own, not delegated to the store:
the work-item store may treat a dependency id whose prefix differs from its own as an
unchecked external reference, so a hostile proposal naming a fabricated id would
otherwise plant a dangling edge — the orchestrator resolves every non-sibling target
against the store itself, prefix-blind, before applying the batch.

**Granularity is a correctness property, not just style.** Breadth is emergent, but each
emitted child must be a **single, coherent unit** the downstream stages can carry in one
pass — implementable in one `implement` invocation and pinned by one `author-tests` pass.
Bundling several concerns into one child (multiple subsystems, handlers, and their tests
in a single work item) is the planner's most damaging failure mode: it pushes a stage
past what one bounded invocation can do, so the agent churns to its turn/token ceiling and
then dead-letters or routes a costly retry (see [termination](#the-feedback-loop-and-termination)) —
the per-invocation [budget](#the-feedback-loop-and-termination) bounds the damage but does
not prevent the waste, and the retry typically repeats it. The planner therefore prefers
**more, smaller, single-concern children** with explicit dependency edges over fewer coarse
ones: finer breadth costs a little more fixed per-stage overhead but avoids the far larger
cost of a runaway invocation. This is a **binding principle** the planner persona carries
and a decomposition-quality review weighs; it is deliberately *not* a hard structural check
(the orchestrator cannot mechanically judge "one concern"), so unlike DAG-legality it is
enforced by persona + review, not by the validation gate.

**Split vertically, not by layer.** Granularity controls *how many* children; this controls
*along which axis*. "Single, coherent unit" means a child is provable through its own
user-facing behaviour — a **vertical slice** that cuts through whatever architectural layers it
needs (schema, store, handler, view) — **not** one horizontal layer of a larger feature.
Splitting one feature into a schema child, a store child, a handler child, and a view child is
the planner's other damaging failure mode, and it is *more* seductive than bundling because each
layer looks single-concern. But layers are not independently testable: a view's tests need its
handler, whose tests need its store. The test author for a leaf layer is then forced to write
whole-*feature* acceptance tests that exercise siblings which do not yet exist, and the
implementor for that layer finds either nothing in scope to build (a lower layer already
supplied it) or the whole feature (out of its scope) — and it dead-letters. So a feature small
enough to build and verify in one pass is **one** child; a larger one splits into vertical slices
that are *each* demonstrable end-to-end (e.g. "generate" vs "reveal-and-burn"), ordered with
dependency edges — never into layers. Like granularity, this is enforced by the planner persona
and decomposition review, not a structural gate.

**Each child also carries its own scope boundary.** Granularity bounds a child's *size*;
its body must also fix its *scope*. A single spec file routinely describes more than one
child's slice, and the downstream test author and implementor read the **whole** governing
file — so a child whose body names only what to build invites a faithful soul to over-build
behaviour the spec describes but the child does not own, or to collide with a sibling
editing the same file. Each child's body therefore states the boundary explicitly: what is
in scope, and what adjacent behaviour to leave untouched because it already exists or a
sibling owns it. This names the boundary without prescribing the implementation (still the
implementor's job) and, like granularity, is enforced by the planner persona and
decomposition review rather than a structural gate.

The `plan` stage has **no postcondition** and runs **no gate**: a planner produces no
candidate to verify in a sandbox, so its *acceptance is exactly this structural
validation*. The orchestrator additionally requires the planner produced at least one
child (a decomposition of nothing routes `on_failure` for a fresh attempt) and that each
child targets a role the stage declares it `produces` — so an untrusted planner cannot
inject work that skips a stage (e.g. an `implement` issue with no `author-tests`). The
children branch from `main` (fresh work, no predecessor candidate to thread); inter-child
ordering is expressed by naming a sibling's local key in `depends_on`, which the
single-writer beads layer resolves to the assigned id at write time.

---

## Pre- and post-conditions

Every stage may declare guards, evaluated by the **orchestrator** (the agent does
the work; the orchestrator decides whether it counts):

- **Precondition** — must hold before an issue may enter a stage. Usually
  "blockers closed" (a `blocked-by` edge), optionally a predicate.
- **Postcondition** — must hold for the stage to be considered done. Evaluated in
  a clean [verification sandbox](verification.md) *before* the issue is accepted.
- **`on_failure`** — the **mandatory automatic route** when a postcondition fails.
  Because there is no human in the loop, every gate that can fail needs a defined
  destination. A failed `qa` routes back to `implement` as a *new* fix issue.

```yaml
implement:
  precondition:  blockers-closed
  postcondition: [tests-red-then-green]
  on_failure:    implement
  produces:      [qa]
qa:
  postcondition: [tests-pass, "mutation>=0.8", gosec]
  on_failure:    implement
  produces:      [integrate]
```

---

## The feedback loop and termination

`on_failure` makes the [role flow](glossary.md#role-flow) a **bounded feedback
loop** — `qa → implement → qa → …`. The [issue graph](glossary.md#issue-dependency-graph)
stays acyclic (each retry is a *new* issue), but the role flow can cycle.

**Acyclicity does not guarantee termination.** A spec the factory cannot satisfy
would loop forever. Termination is guaranteed only by:

- **Budgets** — caps on tokens / money / wall-clock, at two levels: *within* one
  invocation (turns/tokens) and *across* the feedback loop (cumulative per
  issue/epic).
- **Retry caps** — a maximum number of `on_failure` cycles.

The two are complementary: the retry cap bounds *how many* attempts run, the budget bounds
*how much* those attempts may burn. A retry cap alone leaves a gap — a spec the factory
cannot satisfy could consume unbounded tokens within the cap — which the budget closes.
The cumulative *per-issue* token and dollar budget is realized: the runner stamps each
invocation's `Usage` onto its Result from the broker relay (the trusted egress chokepoint,
not the agent's self-report), and the orchestrator accumulates that spend across the
`on_failure` chain (threaded forward on each routed fix issue like the retry counter),
pricing it to USD through the model registry's per-model `cost` table
([configuration.md](configuration.md)). When the running per-issue total reaches a
configured `budget.tokens` or `budget.usd`, the issue dead-letters instead of spawning
another attempt.

The cumulative **wall-clock** cap works the same way: the runner stamps each invocation's
elapsed time onto its Result (the trusted side, like `Usage` — never the agent's
self-report), and the orchestrator threads a cumulative `SpentWall` across the `on_failure`
chain, dead-lettering when it reaches `budget.wall`. This is the *cross-loop cumulative*
wall; a *per-invocation* wall ceiling is separately enforced by the sandbox.

The cross-issue **`epic_budget`** cannot be a counter threaded forward like `Spent*`,
because an epic is a **fan-out DAG, not a line** — the planner emits parallel `implement`
issues, so threading a running total down each branch and summing at the `qa → integrate`
join would double-count the shared prefix. It is therefore an **aggregate read**: every
issue carries an `epic_id` (the root seed's id, threaded forward like the candidate base),
each issue records its closing spend when it reaches a terminal state, and before spawning
another attempt or advancing a stage the orchestrator sums the closing spend of all issues
sharing the `epic_id` — plus in-flight accrual and the just-finished invocation — and
dead-letters with an `epic-budget` escalation on a breach. The **single-writer**
orchestrator evaluates this serially, so concurrent siblings cannot race the check.

When either is breached, the work is **dead-lettered**: marked blocked, an alert
event emitted, and left for a human to triage by refining the spec (see the human
re-entry invariant in [specs-process.md](specs-process.md)). Triage happens through
the same [control-room wizard](control-room.md) used to create work. A pathological
issue can never wedge the pipeline — it always terminates into the DLQ.

```yaml
policy:
  max_retries: 3
  budget: { tokens: 2_000_000, usd: 20, wall: 2h }   # per issue
  epic_budget: { usd: 200 }
  dead_letter: harness.dlq
```

---

## Failure taxonomy

Three outcomes of an invocation, handled differently:

| Class | Example | Handling |
|-------|---------|----------|
| **Transient** | sandbox crash, LLM 5xx, network blip | retry the *same* issue (JetStream redelivery); no graph change |
| **Terminal-reject** | gate failed (tests red, mutation low, scanner finding) | `on_failure` route → new fix issue; counts against retry cap |
| **Escalation** | spec ambiguity, budget breach, max retries, illegal proposal | dead-letter → human spec refinement |

Transient failures lean on [JetStream](messaging.md) at-least-once redelivery, so a
dead runner's work is simply re-pulled elsewhere.
