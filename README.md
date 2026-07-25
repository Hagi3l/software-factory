# Harness

**A secure, autonomous software factory.** Humans author specifications; a fleet of
ephemeral, sandboxed LLM agents plans, writes tests, implements, verifies, and
integrates the change; the terminal state is code **merged to `main`**. Humans never
read or write the code — when work fails, they refine the spec.

The threat model is deliberately adversarial: **assume the agents, and the code they
generate, may be hostile.** The architecture's job is to produce trustworthy software
anyway — a CI/CD pipeline whose build steps are hostile-by-assumption agents.

> 🎤 There's a talk about this project at
> [lochieashcroft.com/talks](https://lochieashcroft.com/talks/).

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

Work flows through a configurable DAG of stages, coordinated over NATS with
[beads](https://github.com/steveyegge/beads) as the work-item store. The core
invariants:

- **Zero-network sandboxes.** Each agent runs with no direct network access; all I/O
  (model calls, git, events) goes through the runner's broker against an allowlist.
  API keys live in the runner — the agent never sees them.
- **Producer ≠ verifier.** Tests are authored by a different *soul* (identity +
  persona + model + tools) than the implementor, and gates re-run in a fresh
  verification sandbox the producer never touched. Verification can fan out across
  model families (N-version diversity) so producer and verifier don't share blind
  spots.
- **Single writer.** Agents only ever *propose* a candidate branch and a Result
  envelope. The trusted orchestrator is the sole writer of the work graph and the
  only thing that merges.
- **Guaranteed termination.** Budgets and retry caps bound every piece of work; what
  can't pass dead-letters for a human to triage by refining the spec.

Every merged commit carries a provenance trailer: issue → soul → model → prompt →
evidence.

## Quick start

Requires Go (see `go.mod` for the version). The binary is self-contained — the web UI
is generated and committed, so a plain build needs no other toolchain.

```bash
make build                      # compile ./bin/harness
./bin/harness validate --config config   # load + validate the shipped config
./bin/harness serve             # browse the control room at http://127.0.0.1:8080
```

Running the full `spec → merged commit` loop additionally needs:

- a **Docker daemon** (the sandbox backend),
- a model — `ANTHROPIC_API_KEY` in the environment, or any OpenAI-compatible
  endpoint (a local Ollama works),
- the **beads** CLI (`bd`) — `brew install beads` on macOS, or a
  [Linux release](https://github.com/steveyegge/beads/releases).

Then follow [`docs/getting-started.md`](docs/getting-started.md) — it walks from a
fresh checkout to seeding work, watching the live control room, and approving the
first merge.

## Documentation

| You want to…                                    | Read |
|-------------------------------------------------|------|
| **Use it** — build, run, configure, CLI, web UI | [`docs/`](docs/README.md) — the operator guide |
| Understand **what it is** — the design contract | [`specs/`](specs/README.md) — the authoritative spec |
| Look up a **term of art** (soul, gate, bead…)   | [`specs/glossary.md`](specs/glossary.md) |
| See the **build order and status**              | [`IMPLEMENTATION_PLAN.md`](IMPLEMENTATION_PLAN.md) |

`specs/` is the source of truth for *what the harness is and how it must behave*;
`docs/` is the practical *how to use it*. When the two disagree, the spec wins — and
the right fix is to update the spec.

## Status

Working, end-to-end, in development: the kernel does `spec → implement → gate →
merge`, and on top of it independent verification (Phase 2), the full DAG with
decomposition and a merge queue (Phase 3), and the control room with the
Create-Task/Resolve wizard (Phase 4) are complete. What remains is Phase 5 —
production isolation and distribution (Firecracker, distributed NATS, scoped
secrets, signing).

Two honest caveats:

- Phases 2–4 were built by hand with human review, **not** self-hosted. The
  autonomous self-hosting loop is buildable and tested offline but has not been
  switched on.
- This is a research/portfolio project, not a supported product. Expect sharp edges.

Amusingly for a project about agent orchestration, the repo itself was largely built
by an agent loop — see `loop.sh` and `PROMPT_BUILD.md` for the harness that built the
harness.

## Development

```bash
make check          # the full local gate: go vet + golangci-lint v2 + unit tests
make test-unit      # unit tests only (go test -json into test/results/)
make generate       # regenerate templ views + Tailwind CSS after UI edits
```

Stack: Go · NATS for all inter-process comms · beads (`bd`) as the work store · a
model layer of canonical types + thin per-provider adapters over official SDKs (no
agent framework) · control room in templ + htmx + Alpine + Tailwind, embedded into
the binary via `embed.FS`.

## License

**TBD.** A license has not been chosen yet; until one is added, all rights are
reserved. If you want to use this for something, open an issue.
