# Stack profiles

The factory **kernel** (DAG, sandboxes, broker, beads, producer ≠ verifier) is
language-agnostic. A **profile** is a complete config tree that makes that kernel
grade a particular ecosystem.

| Profile | Config path | Image | Example projects |
|---------|-------------|-------|------------------|
| **go** | `config/` | `factory/go-toolchain:dev` | this repo (self-host) |
| **node** | `profiles/node/` | `factory/node-toolchain:dev` | get-chilld, tourney-hub-ai |
| **python** | `profiles/python/` | `factory/python-toolchain:dev` | f5-automation |

## Quick start

```bash
# From the software-factory checkout:
make build
./bin/software-factory profile detect --repo /path/to/your-app
./bin/software-factory bootstrap-repo --repo /path/to/your-app   # detect + bd init + print commands

# Build the recommended sandbox image (from factory repo root):
docker build -f deploy/node-toolchain.Dockerfile -t factory/node-toolchain:dev .
# or
docker build -f deploy/python-toolchain.Dockerfile -t factory/python-toolchain:dev .

# get-chilld (warm node_modules for zero-network):
docker build -f deploy/get-chilld.Dockerfile -t factory/get-chilld:dev /path/to/get-chilld

# Auth (Grok sub) then run — node/python profiles default to Grok models:
./bin/software-factory login
./bin/software-factory run \
  --config profiles/node \
  --env get-chilld \          # only for get-chilld baked image; omit for generic node
  --repo /path/to/your-app \
  --serve-addr 127.0.0.1:8080 \
  --nats-addr 127.0.0.1:4222
```

Node/Python souls use **`grok-4.5` / `grok-4-fast`** by default (Claude remains in the model
registry if you switch souls back). Same-family diversity advisory is expected until you
point the verifier at Claude.

## What each profile defines

Same kernel shape as `config/factory.yaml`:

- **DAG** — plan → author-tests → implement → qa → integrate (+ resolve)
- **checks** — shell commands the gate runs in a clean sandbox
- **souls** — language-appropriate personas + sandbox profile name
- **infra** — Docker image for that sandbox

| Profile | Primary gate commands |
|---------|----------------------|
| go | `make test-unit`, lint, gosec, govulncheck, mutation… |
| node | `factory-node-check test\|lint\|typecheck\|build` |
| python | `factory-python-check test\|lint\|typecheck` |

`factory-node-check` auto-detects **pnpm / yarn / npm** from lockfiles.

## Project requirements

### Node

- `package.json` with a **`test`** script (or `test:unit`) once work starts — the
  test-author soul may add vitest/jest if missing.
- Prefer `lint` and `build` scripts (Next.js already has `build` + `lint`).
- Lockfile present (`pnpm-lock.yaml` / `package-lock.json` / `yarn.lock`).

### Python

- `pyproject.toml` and/or `requirements.txt`.
- Tests discoverable by **pytest**.
- **ruff** / **mypy** config optional but recommended (tools are in the image).

### Go

- Use the default `config/` profile (existing bootstrap).

## Dependencies in zero-network sandboxes

Git worktrees **do not** include `node_modules` / `.venv` (gitignored). The Go
profile solves this with a brokered GOPROXY shim. Node/Python profiles today:

1. Prefer a **project image** that bakes a warm dependency cache (see `demo/vault`), or
2. Seed installs when package-proxy support exists for npm/PyPI (future), or
3. Commit vendored deps if your policy allows.

Gate scripts skip install when `node_modules` / a venv is already present in the
seeded tree.

## Adding a new profile

1. Copy `profiles/node/` → `profiles/<lang>/`.
2. Adjust `checks`, souls `sandbox:`, and prompts.
3. Add `deploy/<lang>-toolchain.Dockerfile` + check script.
4. Register in `internal/profile/detect.go` (`Known` + detect markers).
5. Document here.

No kernel code change is required when checks stay “command + exit code.”
