# Harness

A **secure, autonomous software factory**. A human collaborates with an LLM to turn
an idea into specifications and seed work items; from there a fleet of ephemeral,
sandboxed agents — each with its own *soul* — plans, writes tests, implements,
verifies, and integrates the change, coordinating over NATS with
[beads](https://github.com/steveyegge/beads) as the work-item store. The terminal
state is code **merged to `main`**. Humans never read or write code; they author and
refine specs.

The threat model is deliberately adversarial: **assume the agents, and the code they
generate, may be hostile.** The architecture's job is to produce trustworthy software
anyway. Think of it as a CI/CD pipeline whose build steps are hostile-by-assumption
agents.

## How it works

```
       human writes a spec                      agents run, sandboxed + untrusted
   ┌──────────────────────┐        ┌──────────────────────────────────────────────────┐
   │  Create-Task wizard  │        │  plan → author-tests → implement → qa → integrate  │
   │  or `harness seed`   │ ─────► │     ▲          (each stage a fresh sandbox)        │ ─► main
   └──────────────────────┘        │     └────────── on_failure (bounded retries) ─────┘
                                    └──────────────────────────────────────────────────┘
                                       gates run producer ≠ verifier, in a clean sandbox
```

Work flows through a configurable DAG of stages. Each agent runs in a zero-network
sandbox and can only reach the outside world through the runner's broker. Whoever
produces an artifact never grades it: tests are authored by a different soul than the
implementor, and gates run in a *fresh* verification sandbox. Agents only ever
*propose* a candidate branch — the trusted orchestrator is the sole writer of the work
graph and the only thing that merges. Budgets and retry caps are the termination
guarantee; work that can't pass dead-letters for a human to triage by **refining the
spec**.

## Documentation map

| You want to…                                    | Read |
|-------------------------------------------------|------|
| **Use the harness** (build, run, configure, UI) | [`docs/`](docs/README.md) — the operator guide |
| Understand **what the harness is** (the design) | [`specs/`](specs/README.md) — the authoritative contract |
| See the **build order / status**                | [`IMPLEMENTATION_PLAN.md`](IMPLEMENTATION_PLAN.md) |
| Understand a **term of art**                     | [`specs/glossary.md`](specs/glossary.md) |

`specs/` is the source of truth for *what the harness is and how it must behave*;
`docs/` is the practical *how to use it*. When the two disagree, the spec wins — and
the right fix is to update the spec.

## Quick start

```bash
make build                      # compile ./bin/harness (self-contained; no toolchain needed)
./bin/harness validate          # load + validate config/ (the startup gate)
./bin/harness serve             # browse the control room at http://127.0.0.1:8080
```

To run the full `spec → merged commit` loop you need a Docker daemon and an
`ANTHROPIC_API_KEY` (or a local OpenAI-compatible endpoint). See
[`docs/getting-started.md`](docs/getting-started.md).

## Status

The kernel does `spec → implement → gate → merge` end-to-end. On top of it, Phase 2
(independent verification), Phase 3 (full DAG, decomposition, merge queue), and Phase
4 (control room + Create-Task/Resolve wizard) are complete. **Phase 5** (production
isolation & distribution — Firecracker, distributed NATS, scoped secrets, signing) is
the remaining engineering.

Phases 2–4 were built by hand with human review, **not** self-hosted: the autonomous
self-hosting loop is buildable and tested offline but has not been switched on (it
awaits a hosted capable model to drive the harness's own sandboxed agents). See
[`IMPLEMENTATION_PLAN.md`](IMPLEMENTATION_PLAN.md) for per-task detail.

## Stack

Go · NATS (all inter-process comms) · beads (`bd`) as the work store · a model layer
of canonical types + thin per-provider adapters (no agent framework) · control room
in templ + htmx + Alpine + Tailwind, embedded into the binary.

## Building & testing

`make check` is the full local gate (`go vet`, `golangci-lint run`, unit tests). See
[`CLAUDE.md`](CLAUDE.md) for the development conventions and
[`docs/getting-started.md`](docs/getting-started.md) for prerequisites.
</content>
</invoke>
