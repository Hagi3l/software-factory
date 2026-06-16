# Vault

A small, single-user **secrets vault**: unlock it with a master password, store credentials
encrypted at rest, reveal them on demand, and review an append-only audit log of every
sensitive action. Go + templ + htmx + Tailwind + SQLite, no JavaScript build step.

> ### About this repository — this is a live demo
>
> This repo is the **target of an autonomous software-factory demo**. The vault itself is
> real, established, working code — but the *new features* you see land here are not written
> by a human. A human describes a feature in plain language; a fleet of sandboxed, untrusted
> LLM agents then plans it, writes failing tests, implements it, passes an independent
> security re-gate, and merges it — with **no human reviewing the code**.
>
> So when you browse the history, the feature commits are authored by **`harness`**, not a
> person, and each carries a **provenance trailer** recording the model, the implementing
> soul, the *independent* test-authoring soul, and the gate checks (gosec, govulncheck,
> license-scan, red→green proof) that verified it — each citing a content-addressed evidence
> hash. That trailer *is* the accountability, because nobody reviewed the diff.
>
> `main` is **reset to a clean baseline at the start of each demo run** (the `seed` ref is
> the immutable starting point), so the history here is intentionally short — it shows one
> baseline plus whatever was just built on stage.
>
> The factory that does this lives in a separate, private repository. This repo only ever
> contains the vault and the features the agents add to it.

## Features

- **Master-password auth** — first-run setup, login, sign-out. Password hashed with
  **Argon2id**; the session holds the derived key in memory only.
- **Encryption at rest** — secret values are sealed with **AES-256-GCM**; plaintext is never
  written to the database, logged, or rendered into a list — only the dedicated reveal path
  returns it.
- **Reveal on demand** — secrets stay masked until explicitly revealed (an htmx fragment),
  and each reveal is audited.
- **Search** — filter secrets by name/metadata without exposing values.
- **Append-only audit log** — every sensitive action (login, create, reveal, delete) is
  recorded; the dashboard surfaces a recent-activity feed.

## Quickstart

```bash
make build          # compiles bin/vault (generated templ + Tailwind are committed)
make run            # serves on http://127.0.0.1:8000
```

Configuration is via environment:

| Var | Default | Purpose |
|-----|---------|---------|
| `VAULT_ADDR` | `127.0.0.1:8000` | listen address |
| `VAULT_DB`   | `vault.db`       | SQLite database path |

## Security posture

This app is security-sensitive by construction — which is the point of the demo: the
independent **gosec** + **govulncheck** gate re-audits the agents' crypto and auth code on
every change. Binding rules (see [`specs/`](specs/)): parameterized SQL only; `crypto/rand`
for all tokens/nonces/salts; `subtle.ConstantTimeCompare` for secret comparisons;
`HttpOnly` + `SameSite=Strict` session cookies behind auth middleware.

## Layout

```
cmd/vault/            entrypoint (HTTP server wiring)
internal/crypto/      AES-256-GCM sealing + Argon2id derivation
internal/store/       SQLite persistence (pure-Go modernc driver; parameterized SQL)
internal/web/         net/http handlers, session-cookie auth, view mapping
internal/web/views/   templ components (compiled to *_templ.go, committed)
internal/web/static/  vendored htmx + Alpine + compiled app.css (embedded)
specs/                what the vault is and must do (source of truth)
```

## Verification

Every change is independently verified — the same commands the harness `qa` stage runs in a
clean, zero-network sandbox:

```bash
make test-unit      # unit + httptest suite
make lint           # golangci-lint
make gosec          # SAST (fails closed on a finding)
make govulncheck    # known-vulnerability scan
make license-scan   # dependency licence policy
```

New behavior arrives with tests that prove it, authored independently of the implementation.

## Deployment

Pushes to `main` are deployed by [`.github/workflows/deploy.yml`](.github/workflows/deploy.yml):
CI builds a single static `CGO_ENABLED=0` binary (pure-Go SQLite + embedded assets, so no
runtime dependencies) and ships it to a VPS over SSH, where it runs as a `vault` systemd
service. See the workflow header for the required secrets and a sample unit file.
