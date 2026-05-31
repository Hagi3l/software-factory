# The pipeline

How a spec becomes merged code, and what to do when it gets stuck. This is the
operator's view; the authoritative contracts are [`specs/workflow.md`](../specs/workflow.md),
[`specs/verification.md`](../specs/verification.md), and
[`specs/integration.md`](../specs/integration.md).

## The stages

The shipped DAG (`config/harness.yaml`) is the full pipeline:

```
requirements ─► plan ─► author-tests ─► implement ─► qa ─► integrate ─► main
  (human)      (agent)    (agent)        (agent)    (agent)  (trusted)
```

| Stage | Kind | Soul | What it does |
|-------|------|------|--------------|
| `requirements` | human | — | A person authors the spec and seeds one `plan` issue. The only human input. |
| `plan` | plan | `planner` | Reads the seed + spec, decomposes into concrete child work items with dependency edges. Not sandbox-gated — it writes no candidate, only proposals the orchestrator validates. |
| `author-tests` | agent | `test-author` | Writes the **failing** acceptance tests from the spec. Its `tests-red` gate proves they genuinely fail with no implementation present. |
| `implement` | agent | `implementor` | Makes those tests pass. Its `tests-red-then-green` gate proves the tests fail on the base and pass on the candidate — real implementation, not vacuous tests. |
| `qa` | agent | `security` | Spec-independent defence-in-depth: re-runs the tests plus a mutation-adequacy metric and three scanners (gosec, govulncheck, license-scan) in a clean sandbox. A different soul than the implementor. |
| `integrate` | trusted-merge | — | The orchestrator merges the candidate to `main` inline. In trusted-dev mode, parked for human approval first. |
| `resolve` | resolve | `merge-resolver` | Not in the linear flow. Spawned only when a verified candidate can't cleanly rebase onto a since-advanced `main`. Rebases, resolves conflicts, re-runs the full qa gate, and re-enters the merge queue. |

**Breadth is emergent, depth is declarative**: the planner decides *how many* children
a stage produces; the config's `produces:` edges decide *what stage comes next*. Each
produced child inherits the verified candidate branch of its parent as its base — so
the implementor starts from the failing tests, qa starts from the implementation, and
so on.

## The trust model in motion

Every guarantee shows up as a concrete step here:

- **Zero-network sandbox.** Each agent stage runs in a fresh sandbox with no direct
  network. Model calls, git push, and events all go through the runner's broker against
  an allowlist (`llm-api`, `nats`, `git` in dev).
- **Producer ≠ verifier.** Tests are authored by `test-author`; the implementation is
  graded by `implementor`'s own red→green proof *and* re-verified by `security` at qa —
  always a different soul, always in a clean sandbox the producer never touched.
- **Agents propose, the orchestrator accepts.** An agent returns a candidate branch and
  a Result envelope. It never writes the work graph (the orchestrator is the single
  writer) and never merges (integrate is trusted-only).
- **Provenance by construction.** Every merge commit carries a trailer tracing
  issue → soul → model → prompt → evidence, plus the transcript and candidate-diff
  hashes. The control room's Provenance and Replay views read it back.

## Termination: budgets, retries, dead-letters

A rejected candidate routes `on_failure` — a *fresh* attempt at the stage as a new
issue (keeping the graph acyclic). Two caps bound this:

- **`max_retries`** (shipped: 3) — how many `on_failure` cycles before the work
  dead-letters.
- **Budgets** — a per-issue cumulative cap (tokens + USD + wall-clock across the retry
  loop) and a per-epic aggregate cap. Breaching either dead-letters the work.

Dead-lettered work goes to the **dead-letter queue** (the `harness.dlq` subject; the
control room's Dead-letter view) for a human. Agents *escalate* ambiguity — they never
invent intent. **The only human lever, including for stuck work, is the spec.** You
don't edit the agent's code; you refine the requirement and let the pipeline re-derive.

## The approval gate (trusted-dev)

The shipped config runs in the **trusted-dev** profile (`policy.profile`): the harness
writes code and a human reviews every diff before it lands. Concretely, the integrate
stage carries a `human-approved` postcondition, so on a passing integrate the
orchestrator **parks** the candidate (blocked, recording its candidate ref and the
gate-verified provenance) and publishes an escalation — burning no retry.

```bash
harness approve --nats nats://127.0.0.1:4222 <issue>   # land it
harness reject  --nats nats://127.0.0.1:4222 <issue>   # send back a fix attempt
```

Approval is bound to the candidate sha: if the candidate changed since you looked, a
stale approval is ignored. See [cli.md](cli.md) for flags and
[configuration.md](configuration.md) for the profile and the TCB-path boundary.

> The alternative profile, `autonomous`, would require approval **only** for diffs that
> touch the Trusted Computing Base (`policy.tcb_paths` — orchestrator, runner, broker,
> sandbox, gate, messaging, config). That earns no-human-review for non-TCB work while
> keeping the trust-enforcing core permanently human-reviewed. The bootstrap stays in
> trusted-dev until a capable model drives autonomous runs.

## Resolve mode: unsticking dead-lettered work

When work dead-letters, the fix is to refine the spec — and the control room's
**Resolve mode** is the guided way to do it. Launched from a dead-lettered issue
(`/resolve/{id}`), it pre-loads the escalation, the governing spec slice, and the
transcript that raised it, then shows the **blast radius** of your proposed spec edit
(which in-flight issues would reissue and which merged groups would re-derive) before
an explicit APPROVE commits the refined spec and reopens the issue. See
[control-room.md](control-room.md).

## What happens on a spec edit

Editing a spec doesn't silently invalidate finished work. The orchestrator runs two
recompile sweeps: in-flight issues whose spec slice changed are reissued, and
already-merged `(epic, spec-path)` groups whose slice changed spawn a fresh `plan` pass
to re-derive only the delta against merged code. This is why the spec — not the code —
is the durable source of intent.
</content>
