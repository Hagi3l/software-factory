# Getting started

This walks you from a fresh checkout to a running `spec → merged commit` loop.

## Prerequisites

| For…                                  | You need |
|---------------------------------------|----------|
| Building the binary                   | Go (see `go.mod` for the version) |
| The full local gate (`make check`)    | `golangci-lint` v2 (`brew install golangci-lint`) |
| Running the autonomous pipeline       | A **Docker daemon** (the bootstrap sandbox backend) |
| Model calls                           | `ANTHROPIC_API_KEY` in the environment, **or** a local OpenAI-compatible endpoint (e.g. Ollama) |
| The work store                        | the **beads** CLI (`bd`) — `brew install beads` on macOS, or the Linux release from the [beads GitHub releases](https://github.com/steveyegge/beads/releases) |
| Editing the control-room UI           | `templ` and the Tailwind standalone CLI (only to *regenerate*; a plain build needs neither) |

API keys come **from the environment, never from config**. The runner injects the key
into the broker; the sandboxed agent never sees it.

## Build

```bash
make build
```

This compiles a **self-contained** `./bin/harness` — the control-room templates and
CSS are generated and committed, so a plain build needs no web toolchain. The binary
embeds the entire UI.

Other useful targets:

```bash
make check          # full local gate: go vet + golangci-lint + unit tests
make test-unit      # unit tests only (emits go test -json to test/results/)
make generate       # regenerate templ + Tailwind after editing the UI (needs templ + tailwind)
```

> If `golangci-lint` or `templ` is installed under `$(go env GOPATH)/bin` but not on
> your `PATH`, run `export PATH="$PATH:$(go env GOPATH)/bin"` first.

## 1. Validate the config

Everything is config-driven and validated before anything runs. Start here:

```bash
./bin/harness validate --config config
```

A clean run prints an OK line and exits 0; any startup-breaking fault is a loud error.
Non-fatal advisories (e.g. a producer/verifier sharing a model family) print to stderr
as `warning:` lines but still exit 0. See [configuration.md](configuration.md).

## 2. Browse the control room (no model or Docker needed)

```bash
./bin/harness serve --addr 127.0.0.1:8080
```

Open <http://127.0.0.1:8080>. Standalone `serve` renders all the static views, but the
data-backed views show a "not attached" notice and the live feed (`/events`) returns
503 — because it has no running pipeline to read from. To see live data, co-locate the
control room with a run (next step). See [control-room.md](control-room.md).

## 3. Run the full pipeline

The pipeline needs a Docker daemon and a model. Point `--repo` at the integration
repository whose `main` candidates are merged into, and whose `.beads` store holds the
work graph.

```bash
export ANTHROPIC_API_KEY=sk-...            # or configure a local openai-compat model
./bin/harness run \
  --config config \
  --repo /path/to/integration-repo \
  --serve-addr 127.0.0.1:8080              # co-locate the live control room
```

`run` starts an in-process orchestrator + one runner over embedded NATS and processes
work until interrupted (SIGTERM/Ctrl-C drains cleanly). With `--serve-addr` set, the
control room is served from the same process and its live SSE feed has a real source.

### Seed some work

In another shell, author a spec and create the first work item:

```bash
./bin/harness seed \
  --repo /path/to/integration-repo \
  --title "Add a /healthz endpoint" \
  --description "Return 200 OK with body 'ok'." \
  --spec specs/healthz.md
```

This writes the spec (if `--spec` doesn't exist it's created from the title/description)
and creates a seed issue that enters the pipeline at its entry stage (`plan`). The
running orchestrator picks it up and drives it through the DAG. Watch progress in the
control room's Board and Activity views.

> In production this seeding step is the **Create-Task wizard** in the control room;
> `harness seed` is the CLI stand-in. See [the pipeline](pipeline.md).

## 4. Approve a candidate (trusted-dev mode)

The shipped config runs in **trusted-dev** profile: every integrate is parked for a
human to review the diff before it lands. When an issue reaches integrate it blocks
awaiting approval. To approve it you need the run to expose its NATS so a separate
process can reach it:

```bash
# add --nats-addr to the run command:
./bin/harness run ... --nats-addr 127.0.0.1:4222

# then, in another shell:
./bin/harness approve --nats nats://127.0.0.1:4222 <issue-id>
# or
./bin/harness reject  --nats nats://127.0.0.1:4222 <issue-id>
```

Approve replays the gate-verified provenance onto the merge and closes the issue;
reject routes a fresh fix attempt (or dead-letters when retries are spent). See
[the pipeline](pipeline.md) and [cli.md](cli.md).

## Where things land

- **Merged code** → `main` in your `--repo`, each commit carrying a provenance trailer
  (issue → soul → model → prompt → evidence).
- **Work graph** → the beads store (`.beads/`) in `--repo`.
- **Evidence** (transcripts, gate output, diffs) → the artifact store (in dev, files
  under `./.harness/artifacts`, per the infra overlay).

## Troubleshooting

- **`validate` fails** — read the error; it names the offending config key. The startup
  gate refuses to run on a bad config by design.
- **`/events` returns 503** — you're on a standalone `harness serve`. Use `harness run
  --serve-addr` so the feed has a live NATS source.
- **qa lint/scanner checks fail closed** — the golangci-lint/gosec/govulncheck/license/mutation
  tooling and the offline vuln DB are baked into the `go-toolchain` sandbox image (T5.3,
  `deploy/go-toolchain.Dockerfile`). If these checks fail for lack of tooling, the resolved
  profile image is stale or unbuilt — rebuild it (`docker build -f deploy/go-toolchain.Dockerfile
  -t go-toolchain .`) so the gate runs them offline under the zero-network invariant.
- **`make check` times out** — a known infra flake under full-suite parallel NATS load;
  re-run, or run the single package in isolation. See `IMPLEMENTATION_PLAN.md`.
</content>
