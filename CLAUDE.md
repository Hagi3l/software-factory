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
Bootstrap scaffolding done (T0.1): Go module + `internal/` package layout exist; `make build` and `make test-unit` work. Building the kernel (Phase 1).

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
- Go. NATS for all inter-process comms (always). beads (`bd`) as work store.
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
golangci-lint v2 — `brew install golangci-lint`), then unit tests. `make test-*`
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
