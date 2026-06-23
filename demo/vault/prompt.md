Feature: one-time, single-use secret share link.

  I've already made all the decisions — treat this as converged intent. If every criterion below is testable, don't open ledger forks; draft the spec + the single seed issue
  now so I can Approve. This run is epic mode, so seed exactly one root.

  What it does: From a secret's row in /vault, an authenticated user generates a share link containing an unguessable token. Anyone with the link can reveal that secret's
  plaintext value exactly once, without logging in. After the first successful reveal — or after the link expires — the token burns and any further visit fails.

  Behavior / acceptance criteria:
  - A "Share" action on a secret creates a share token and shows the full link (/share/{token}) as an htmx fragment. Default expiry is 24 hours.
  - GET /share/{token} on a live, unused token returns a page showing the decrypted value once, then atomically marks the token consumed.
  - The token is generated with crypto/rand and is long enough to be unguessable; token lookup uses constant-time comparison.
  - Generating a share link records a share audit entry; a successful reveal records a reveal audit entry. The plaintext is never logged or stored in the token row.
  - The share reveal reuses the existing crypto path (crypto.Open); the token table stores only the secret id, a token hash, expiry, and a consumed flag — never plaintext.

  Must reject (return a generic "link invalid or expired" — no detail leaked):
  - an already-consumed token, an expired token, an unknown/malformed token.
  - Concurrent double-fetch of the same live token: only one request reveals; the other gets the rejection (the consume is atomic).

  Out of scope (say so in the spec so the planner doesn't over-build): email/SMS delivery, multiple recipients, N-time (>1) links, configurable per-link expiry UI, link
  revocation UI, rate limiting.

  The token table is new; everything else extends internal/store, internal/web, internal/web/views, and the audit log per specs/conventions.md. Link the new spec to
  specs/README.md so the crypto/SQL conventions ride along.

