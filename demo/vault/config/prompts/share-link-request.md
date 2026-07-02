I want one-time, single-use secret share links: from a secret's row in the vault, the owner generates an expiring link; anyone with the link can reveal that secret's value exactly once, without logging in; then the link is dead forever.

I have already made every design decision below — record them in the alignment ledger as agreed (with these rationales), and if nothing is contradictory, propose the draft this turn. Do not ask clarifying questions; anything I have not specified is out of scope. This run is in epic integration mode, so seed exactly ONE root issue for the whole feature.

Decisions, all final:

1. **Token.** 32 bytes from `crypto/rand`, URL-safe base64. The share URL is `/share/{token}`. The database stores only the token's SHA-256 — never the token itself — and lookup compares in constant time. A leaked database must not reveal usable links.

2. **Crypto — this is the load-bearing one.** The vault's AES key is derived from the master password and lives only in the owner's session, so a sessionless public endpoint cannot open the stored blob. Therefore, at GENERATE time (owner authenticated): decrypt the secret with the session key, then re-seal the plaintext with a key derived from the share token itself (the existing Argon2id derivation, per-share random salt stored alongside). The share row holds `token_hash`, `salt`, `nonce||ciphertext`, `secret name`, `expires_at`, `created_at`. The reveal endpoint derives the key from the presented token and opens the blob — no session, no master key, no plaintext at rest. A share is a snapshot: later edits to the secret do not update it.

3. **Single use + expiry.** Reveal atomically deletes the share row in the same transaction that reads it — a second request (or a race) gets the same 404 as a wrong token. Links expire 1 hour after generation; an expired link 404s identically and is deleted on encounter. Wrong, used, and expired tokens are indistinguishable to the caller.

4. **Audit.** Generate records a `share-create` entry and reveal records a `share-reveal` entry (target = secret name), consistent with specs/audit.md — every sensitive action appends an entry.

5. **UI, minimal.** A "share" affordance on each secret row that calls generate and shows the full URL with a copy button (htmx fragment, consistent with existing rows). The public reveal page is a bare page showing the secret name and value, styled like the login page, stating plainly that the link has now been destroyed. No share management/listing UI.

Acceptance criteria:

- Generating requires an authenticated session; the response contains a URL whose token is 32 random bytes, URL-safe base64.
- The stored share row contains no plaintext secret value and not the token itself (only its SHA-256).
- GET `/share/{token}` with a valid, unexpired, unused token returns the secret's plaintext value exactly once, with no session cookie, and writes a `share-reveal` audit entry.
- A second GET with the same token returns 404 with no secret content; concurrent first reveals yield exactly one success.
- A token past its 1-hour expiry returns 404 and never decrypts; wrong/used/expired responses are indistinguishable.
- Token-hash comparison is constant-time; the token-derived key opens the share blob without the master password.
- Generate writes a `share-create` audit entry.

Out of scope (do not build): rate limiting, share revocation/management UI, configurable expiry, multi-use links, email/notification, password-protected shares, changes to existing secret CRUD behavior.

Grounding, to keep exploration short: the seams are `internal/store/store.go` + `internal/store/migrations.go` (new `shares` table), `internal/crypto` (Seal/Open/DeriveKey/RandomToken already exist — reuse, do not add primitives), `internal/web/server.go` (routes; note the auth middleware must exempt `/share/{token}`), and the specs to extend are `specs/secrets.md` (or a new linked `specs/share-links.md` referencing `specs/README.md` conventions), `specs/audit.md` for the two new actions. Follow `specs/conventions.md` throughout (parameterized SQL only, `crypto/rand` only, no new modules). This decomposes naturally into two to three work items — for example store+crypto+generate, and the public reveal endpoint+UI — not more.
