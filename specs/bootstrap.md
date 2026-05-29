# Bootstrap

How the harness comes to build itself. This is the one spec about *building* the
harness rather than *running* it.

See also: [architecture.md](architecture.md),
[components/orchestrator.md](components/orchestrator.md),
[verification.md](verification.md).

---

## Two thresholds, not one

"When can the harness build itself?" has two distinct answers, and conflating them
is a mistake:

- **(a) Build itself as a trusted developer's tool** — *early*. As soon as a
  minimal kernel exists, the remaining specs can be filed as beads issues and the
  harness implements them, **with a human reviewing the diffs**. The verification
  machinery isn't trustworthy yet, so a human stays in the loop — but the agent is
  already doing the work.
- **(b) Build itself under its own adversarial guarantees** — *much later*. No
  human review, sandboxed, gated by independent tests + mutation + scanners. This
  requires the verification and isolation stack to be built **and itself trusted**
  — a genuine chicken-and-egg, since the thing that justifies no-human-review must
  exist and be trusted before it can justify anything.

The harness can *start* building itself long before it can *securely* build itself.

---

## The minimal kernel (the hand-built seed)

To produce one verified change end-to-end, hand-build the thinnest slice of the
design, roughly in dependency order:

1. **Config loader + `harness validate`** — see [configuration.md](configuration.md).
2. **beads integration** — read ready work, single-writer status transitions —
   see [components/orchestrator.md](components/orchestrator.md).
3. **Sandbox + Docker backend** — seed a worktree, local-socket I/O — see
   [components/sandbox.md](components/sandbox.md).
4. **Runner + broker** — relay LLM/git, log everything — see
   [components/runner.md](components/runner.md).
5. **Minimal agent loop** — LLM + fs/shell tools on the worktree — see
   [components/agent.md](components/agent.md).
6. **Gate runner** — clean checkout, `build` + `test` — see
   [verification.md](verification.md).
7. **Orchestrator loop** — dispatch → await → gate → fast-forward merge → advance.

At step 7 there is a kernel that does `spec → implement → gate → merge` for one
issue at a time. **That is the self-host point.**

### What to defer
The full pipeline collapses for bootstrap:

- The [DAG](workflow.md) reduces to `implement → gate → integrate` (the human does
  `plan` by writing specs; the implementor can write its own tests initially).
- The [merge queue](integration.md) is trivial with a single stream — no
  rebase/re-gate needed yet.
- [NATS](messaging.md) can be in-process (still spoken, per location transparency).
- Docker stands in for Firecracker.
- No [control room](control-room.md) yet — drive it from the CLI.

The harness then builds the deferred pieces — the independent `author-tests` stage,
mutation testing, the rebase/re-gate merge queue, Firecracker, distributed NATS,
more souls, the DLQ, the control room, provenance — each as its own beads issue.

---

## Testing the spine without a capable model

The kernel's machinery can be verified end-to-end *before* — and independently of —
any capable model, by driving it with a **deterministic fake model** (see
[models.md](models.md)) over a non-isolating
[local backend](components/sandbox.md): a scripted agent run carries a seed issue
through implement → gate → merge on a fixture repo, with no Docker and no network. It
pins the control-flow and tool contracts as a fast regression guard; a second,
Docker-backed run covers the isolation properties the local backend gives up.

This is orthogonal to the two thresholds above. It proves the *plumbing* is correct —
not whether the harness produces *correct* software autonomously, which is threshold
(b) and needs model judgement a fake cannot supply. Verifying the spine cheaply this
way is exactly what keeps the hand-built (a) phase honest while (b) is still out of
reach.

---

## The trust-bootstrap caveat

The components that *enforce* the guarantees — orchestrator, runner/broker, sandbox
config, the gate harness, the verification stack — are the **Trusted Computing
Base (TCB)**. An unverified harness cannot vouch for the security of its own
verifier; "who verifies the verifier" has no autonomous answer. Therefore:

- **TCB-touching changes stay human-reviewed**, even after self-hosting — arguably
  permanently, since the TCB is the root of trust.
- **Everything above the TCB** (new souls, new stages, product features, the
  control room) is where full autonomy is safe once the stack is solid.

So "the harness builds itself" should mean *it builds features and souls*, not *it
silently rewrites its own root of trust*. The eventual no-human-review promise
applies to **product work the harness does for others**; it is earned for harness
work only incrementally, and the TCB is the part never fully handed over.

---

## Progression

```
hand-build kernel (TCB seed)         human writes all the code
        │
self-host in trusted mode            harness writes code, human reviews diffs
        │
verification stack built + trusted   no-human-review earned for non-TCB work
        │
full product factory                 no-human-review for others' work; TCB still reviewed
```

---

## OPEN questions

- Exactly which modules are drawn into the TCB boundary — needs a precise list
  before autonomy is switched on for harness work.
- Whether early self-hosted changes should be gated by a lighter "trusted-dev"
  policy profile (e.g. human-approval postcondition) expressed in
  [configuration.md](configuration.md).
