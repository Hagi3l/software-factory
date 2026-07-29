# Harness — operator guide

Practical, task-oriented documentation for **running and using** the harness. This is
the *how-to*; the authoritative design contract for *what the harness is* lives in
[`specs/`](../specs/README.md), and the build order/status in
[`IMPLEMENTATION_PLAN.md`](../IMPLEMENTATION_PLAN.md). Where this guide simplifies, the
specs are the truth — and term definitions live in
[`specs/glossary.md`](../specs/glossary.md).

## Guides

1. **[Getting started](getting-started.md)** — prerequisites, building the binary, and
   running your first `spec → merged commit` loop.
2. **[CLI reference](cli.md)** — every subcommand (`validate`, `seed`, `run`, `serve`,
   `approve`, `reject`) and its flags.
3. **[The pipeline](pipeline.md)** — how a spec becomes merged code: the DAG stages,
   the trust model, dead-letters, approval, and Resolve mode.
4. **[Configuration](configuration.md)** — the `config/` directory: `factory.yaml`
   (the DAG, checks, policy), souls, and the infra overlay.
5. **[The control room](control-room.md)** — the web UI: every view, the live feed,
   and the Create-Task / Resolve wizard.

## The 60-second mental model

- A **human** authors a spec and seeds one work item. That is the *only* human input
  to the pipeline — everything downstream is autonomous.
- The **orchestrator** is the single writer of the work graph (in beads) and the only
  component that merges. It schedules stages and runs the gates.
- A **runner** provisions a zero-network **sandbox** per stage, runs the agent loop,
  and brokers all the agent's I/O (model calls, git push, events) against an
  allowlist. The agent never touches the network or the real repo directly.
- An **agent** is a **soul** (identity + persona + model + tools) invoked for one task.
  It *proposes* a candidate branch and a Result envelope; it never writes the work
  graph and never merges.
- **Producer ≠ verifier**: tests are authored by a different soul than the
  implementor, and gates re-run in a *fresh* sandbox the producer never touched.
- **Budgets + retry caps** are the termination guarantee. Work that exhausts them, or
  that an agent escalates, **dead-letters** for a human — who responds by refining the
  spec, never by editing code.
- **The pipeline is configuration, not code.** Stages, souls, checks, budgets, and
  sandbox images are all declared in [`config/`](configuration.md); a check is any
  command with an exit code. The shipped configs target Go — supporting another
  ecosystem means a new sandbox image and check set, not a fork.

## What you can do today

The kernel and every engineering phase through 15 are built and run end-to-end in
development (Docker or gVisor sandbox, embedded or distributed NATS, local-repo
merge). You can:

- Validate config, seed work, and run the full pipeline against a real or local model.
- Drive the trusted-dev approval gate with `software-factory approve` / `software-factory reject`.
- Browse the control room — Board, DAG, Activity, Dead-letter, Budgets, Provenance —
  and author specs through the Create-Task wizard, or unstick dead-lettered work
  through Resolve mode.

Most of Phase 5 (production isolation & distribution) has landed too — distributed
NATS, scoped short-lived secrets, the S3/MinIO artifact backend, provenance signing,
and the gVisor sandbox backend. What remains is the Firecracker microVM backend
(blocked on KVM hardware) and optional warm pools / HA orchestration. See the
[implementation plan](../IMPLEMENTATION_PLAN.md).
