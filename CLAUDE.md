# CLAUDE.md

A **secure, autonomous software factory**: humans author specs; sandboxed, untrusted
LLM agents plan/test/implement/verify/integrate; terminal state is merged to `main`.
A CI/CD pipeline whose build steps are hostile-by-assumption agents.

## Source of truth
- `specs/` is authoritative for *what the harness is*. Start at `specs/README.md`
  (index); follow links, don't read top-to-bottom. `specs/glossary.md` defines terms.
- `IMPLEMENTATION_PLAN.md` is the build order (kernel-first).
- If the design needs to change, **update the spec** — don't just change code.

## Status
**Phases 2–4 essentially built out (by hand, human-reviewed — not self-hosted).**
The kernel does `spec → implement → gate → merge` end-to-end (a live run needs a
Docker daemon + `ANTHROPIC_API_KEY`); on top of it Phase 2 (independent
verification), Phase 3 (full DAG, decomposition, merge queue), and Phase 4 (control
room + Create-Task wizard, incl. **T4.15 Resolve mode**) are complete bar **T2.13**
(a non-fatal `harness validate` warning on producer/verifier model-family overlap —
N-version diversity is otherwise resolved as configuration, T2.11), with **T2.12**
and **T2.11** optional. **Phase
5** (production isolation & distribution — Firecracker, distributed NATS, scoped
secrets, signing) is the remaining engineering. `cmd/harness` exposes
`validate`/`seed`/`run`/`approve`/`reject`/`serve`; bootstrap config lives in
`config/` (`harness validate --config config`). The autonomous self-hosting loop is
buildable/testable offline but has **not been switched on** (no hosted capable
model). See `IMPLEMENTATION_PLAN.md` for the per-task detail.

## Invariants (don't break these in code)
- **Single-writer beads** — only the orchestrator writes; agents *propose* via Result.
- **Producer ≠ verifier** — gates run in a fresh orchestrator-controlled sandbox;
  tests authored by a different soul than the implementor.
- **Zero-network sandbox** — agents have no direct network; all I/O via runner/broker.
- **Model calls via broker** — agent emits canonical requests, runner holds key +
  adapter; provider-unaware. Never shell out to an agentic CLI.
- **Agents never merge** — they produce a candidate branch; the trusted layer merges.
- **Budgets = termination** — retry caps + budgets are the halting guarantee.

## Stack
- Go. NATS for all inter-process comms (always). beads (`bd`) as work store —
  install on macOS with `brew install beads` (currently v1.0.4, Dolt backend; a
  major bump from 0.62.0 — `bd dep add` treats a foreign-prefix dep id as an
  unvalidated external ref, so tests pin the db prefix via `bd init --prefix harness`).
  The Linux dev sandbox here also runs **v1.0.4** (matching brew), so its `dep add`
  silently accepts a nonexistent foreign-prefix target as an external ref — `beads.Apply`'s
  own existence check (T3.2) is what holds against this, not `dep add` strictness. (Earlier
  it ran v0.62.0, which still rejected such targets; install Linux builds from the
  `steveyegge/beads` GitHub releases — `beads_<ver>_linux_amd64.tar.gz`.) Note `bd
  list` **hides closed issues** by default — surfacing them (e.g. a board/DAG over
  all statuses) needs `--all` / `--status ... --flat` (this is `beads.ListAll`).
- Model layer: canonical types + thin per-provider adapters over official Go SDKs.
  No agent framework.
- Control room (later): htmx + Alpine + templ + Tailwind standalone CLI + embed.FS.

## Code search
Prefer LSP over Grep for code navigation (once code exists):
- **Go** (`gopls`): definitions, references, implementations, type info, diagnostics.
- **templ** (`templ lsp`): component defs/refs, Go-expression completions.
- Fall back to Grep only for text patterns, non-code files, or broad keyword sweeps.

## Build & test
`make check` is the full local gate: `go vet`, `golangci-lint run` (needs
golangci-lint v2 — `brew install golangci-lint`; if it's in `$(go env GOPATH)/bin`
but not on PATH, run `export PATH="$PATH:$(go env GOPATH)/bin"` first), then unit
tests. The `misspell` linter is `locale: US`, so use **US spellings in Go
comments/identifiers** (`behavior`/`neighbor`/`fulfill`/`modeled`); specs may stay
British. `make test-*`
targets emit `go test -json` to `test/results/`
(gitignored) — each target produces a `.json` (ndjson) + `.stderr`. If `jq` can't
parse the JSON, check the `.stderr` file for compile errors. Triage with `jq`
instead of dumping full output (swap in the target you ran):

```bash
# List all failed tests with package and test name
jq -s '[.[] | select(.Action=="fail" and .Test)] | .[] | {pkg: .Package, test: .Test}' \
  test/results/test-unit.json

# Show output for a specific failed test
jq -rs '[.[] | select(.Test=="TestFoo" and .Action=="output")] | .[].Output' \
  test/results/test-unit.json

# Count pass/fail/skip
jq -s '[.[] | select(.Test and (.Action=="pass" or .Action=="fail" or .Action=="skip"))]
  | group_by(.Action) | map({(.[0].Action): length}) | add' \
  test/results/test-unit.json

# 10 slowest tests
jq -s '[.[] | select(.Action=="pass" and .Test) | {test: .Test, pkg: .Package, elapsed: .Elapsed}]
  | sort_by(-.elapsed) | .[:10]' \
  test/results/test-unit.json

# Show all output for all failed tests (useful for triage)
jq -rs '
  [.[] | select(.Action=="fail" and .Test) | .Test] as $failed |
  [.[] | select(.Test as $t | $failed | index($t)) | select(.Action=="output")]
  | group_by(.Test) | .[] | {test: .[0].Test, output: [.[].Output] | join("")}' \
  test/results/test-unit.json
```

**Control room** (`internal/controlroom`, `harness serve`): views are **templ**
(`go install github.com/a-h/templ/cmd/templ` — already on PATH here) compiled to committed
`*_templ.go`; CSS is the **Tailwind v4 standalone CLI** (`make tailwind` fetches the pinned
binary into gitignored `bin/`). Run `make generate` after editing any `*.templ` or
`assets/app.tw.css` — it runs `templ generate` then Tailwind. A plain `make build` needs
neither tool (generated Go + compiled `app.css` are committed). The Tailwind input
(`assets/app.tw.css`) uses `@source` globs pointing at the views dir; vendored JS
(htmx/Alpine) lives in `assets/static/` and is embedded via `//go:embed`.

The live SSE feed (`GET /events`) needs the run's in-process NATS, so it is served
**co-located**: `harness run --serve-addr 127.0.0.1:8080` runs the factory *and* the
control room. Standalone `harness serve` has no NATS, so `/events` returns 503 there
(static views still render). The SSE substrate is `internal/controlroom/live` (`Hub`,
`Stream`, `StartAgentEventPump`).

When **writing** a test that needs a deterministic model (no API key, no Docker), use
`internal/model/modeltest` — `NewServer(t, []Turn)` is an `httptest` SSE server speaking
the OpenAI streaming wire format, scripted by request count; it drives the *real* `openai`
adapter, selected via an `openai-compat` model entry whose endpoint is patched to
`srv.URL()`. For a full `spec → implement → gate → merge` spine test without Docker, the
**test-only** non-isolating host-exec sandbox backend (defined in
`cmd/harness/spine_e2e_test.go`, compiled only under test) is injected through the
`runOptions.backend` seam in `buildRunComponents`. The Docker e2e variant is behind
`//go:build docker_e2e` (`make test-e2e-docker`).
