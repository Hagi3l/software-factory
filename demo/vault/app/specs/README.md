# Vault — specifications

This directory is the source of truth for **what the vault is and how it must behave**.
The app is an established, single-user secrets vault: a person unlocks it with a master
password, stores credentials encrypted at rest, reveals them on demand, and every
sensitive action is recorded in an audit log.

Humans author intent here; the implementation satisfies it. Follow the cross-links rather
than reading top-to-bottom.

## Feature specs

| Spec | Covers |
|------|--------|
| [auth.md](auth.md) | First-run setup, master-password login, session lifetime, sign-out. |
| [secrets.md](secrets.md) | The secret model, encryption at rest, create/edit/delete, reveal, search, expiry. |
| [audit.md](audit.md) | The append-only audit log and the dashboard activity feed. |

## Architecture & conventions (binding on every change)

The stack is **Go + templ + htmx + Tailwind + SQLite** (pure-Go `modernc.org/sqlite`). A
change is not complete until it keeps the gate green (see *Gate* below).

Layout:

```
cmd/vault/            entrypoint (HTTP server wiring)
internal/crypto/      AES-256-GCM sealing + Argon2id password/key derivation
internal/store/       SQLite persistence: schema, CRUD, audit (encryption-agnostic)
internal/web/         net/http handlers, session-cookie auth, view mapping
internal/web/views/   templ components (compiled to *_templ.go, committed)
internal/web/static/  vendored htmx + Alpine + compiled app.css (committed)
assets/app.tw.css     Tailwind input
specs/                this directory
```

Conventions a new feature must follow:

- **Layering.** Persistence lives in `internal/store` and uses **parameterized SQL only**.
  HTTP/handlers live in `internal/web`. Markup is a **templ component** in
  `internal/web/views`; handlers map store rows into the view types defined there. The
  store never imports the web layer; the views never import the store.
- **Encryption is non-negotiable.** Secret values are sealed with `crypto.Seal` and only
  opened with `crypto.Open`. The plaintext value **must never** be written to the database,
  logged, or rendered into a list view — only the dedicated reveal path returns it. The
  encryption key is held in the session in memory; the store stays encryption-agnostic.
- **Randomness & comparisons.** Use `crypto/rand` for all tokens/nonces/salts (never
  `math/rand`) and `subtle.ConstantTimeCompare` for secret comparisons.
- **Sessions are hardened.** Cookies are `HttpOnly` + `SameSite=Strict`; protected routes
  go through the `auth` middleware.
- **htmx fragments.** Partial updates (search results, reveal, row delete) return a templ
  fragment, not a full page.
- **Generated artifacts are committed.** After editing any `*.templ` or `assets/app.tw.css`,
  run `make generate` (templ + Tailwind) and commit the regenerated
  `*_templ.go` / `app.css`.

## Gate

Every change is independently verified by these commands (the harness `qa` stage runs them
in a clean, zero-network sandbox; locally `make check` runs the fast subset):

- `make test-unit` — unit + httptest suite (a build break also fails here)
- `make lint` — golangci-lint (US spellings in comments/identifiers — `misspell` is `locale: US`)
- `make gosec` — SAST; a finding fails closed
- `make govulncheck` — known-vulnerability scan
- `make license-scan` — dependency licence policy

New behavior must arrive with tests that prove it, authored independently of the
implementation.
