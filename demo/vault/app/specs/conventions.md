# Vault — engineering conventions (binding on every change)

These conventions are **the same for every work item**, so the harness injects this file
(and the [spec index](README.md)) into every agent's Brief as an *ambient spec* — you carry
them no matter which feature spec your issue points at. They are not optional style notes: a
change that breaks one fails the gate or the review. The feature contracts live in the
[feature specs](README.md#feature-specs); this file is *how* any of them must be built.

## Stack & layout

The stack is **Go + templ + htmx + Tailwind + SQLite** (pure-Go `modernc.org/sqlite`).

```
cmd/vault/            entrypoint (HTTP server wiring)
internal/crypto/      AES-256-GCM sealing + Argon2id password/key derivation
internal/store/       SQLite persistence: schema, CRUD, audit (encryption-agnostic)
internal/web/         net/http handlers, session-cookie auth, view mapping
internal/web/views/   templ components (compiled to *_templ.go, committed)
internal/web/static/  vendored htmx + Alpine + compiled app.css (committed)
assets/app.tw.css     Tailwind input
specs/                the specs (this directory)
```

## Conventions a new feature must follow

- **Layering.** Persistence lives in `internal/store` and uses **parameterized SQL only**
  (never string-built queries). HTTP/handlers live in `internal/web`. Markup is a **templ
  component** in `internal/web/views`; handlers map store rows into the view types defined
  there. The store never imports the web layer; the views never import the store.
- **A feature is a vertical slice, not a layer.** Those layers (`store` → `web` →
  `views`) are how *one* change is built, not how work is *divided*. A unit of work is a
  user-facing capability that cuts top-to-bottom through them — *generate a share link*,
  *reveal-and-burn a secret* — and is proven end-to-end through its HTTP handler with
  `httptest`. Never split a single feature into separate "add the migration", "add the
  store method", "add the handler", "add the templ component" tasks: a view or handler
  cannot be tested without the store beneath it, so a layer split leaves the leaf layers
  untestable in isolation. Build and verify each feature through its handler seam, whole.
- **Encryption is non-negotiable.** Secret values are sealed with `crypto.Seal` and only
  opened with `crypto.Open`. The plaintext value **must never** be written to the database,
  logged, or rendered into a list view — only the dedicated reveal path returns it. The
  encryption key is held in the session in memory; the store stays encryption-agnostic.
- **Randomness & comparisons.** Use `crypto/rand` for all tokens/nonces/salts (never
  `math/rand`) and `subtle.ConstantTimeCompare` for secret comparisons.
- **Sessions are hardened.** Cookies are `HttpOnly` + `SameSite=Strict`; protected routes
  go through the `auth` middleware.
- **htmx fragments.** Partial updates (search results, reveal, row delete) return a templ
  fragment, not a full page. Wire interactions with `hx-get` / `hx-post` / `hx-target`
  attributes, **not** an `onclick` handler: a Go expression interpolated into an `onclick`
  (or any `on*`) attribute is parsed by templ as a script expression and fails to generate —
  reach for an `hx-*` attribute (a plain string) and let the handler return the fragment.
- **Generated artifacts are committed.** After editing any `*.templ` or `assets/app.tw.css`,
  run `make generate` (templ + Tailwind) and commit the regenerated `*_templ.go` / `app.css`.
  A handler that references a component you have not regenerated will not compile in the gate.
- **No new dependencies.** The build sandbox is **zero-network** — `go get` cannot reach the
  internet, so a new module makes the gate fail to build. Solve features with the standard
  library and the modules already in `go.mod`; if a dependency seems truly required, escalate
  rather than adding one.

## Gate

Every change is independently verified by these commands (the harness `qa` stage runs them
in a clean, zero-network sandbox; locally `make check` runs the fast subset):

- `make test-unit` — unit + httptest suite (a build break also fails here)
- `make lint` — golangci-lint (US spellings in comments/identifiers — `misspell` is `locale: US`)
- `make gosec` — SAST; a finding fails closed
- `make govulncheck` — known-vulnerability scan
- `make license-scan` — dependency licence policy

New behaviour must arrive with tests that prove it, authored independently of the
implementation.
