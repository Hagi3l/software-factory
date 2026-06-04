# Secrets

A secret is a named, tagged credential whose value is **encrypted at rest**. Secrets are
listed, searched, revealed on demand, edited, and deleted. Access requires a session (see
[auth.md](auth.md)); activity is recorded (see [audit.md](audit.md)).

## Model

A secret has: `name`, optional `tag`, the sealed `value`, an optional `expires_at`, and
created/updated timestamps. Stored in the `secrets` table; the value column holds
`nonce || ciphertext` produced by `crypto.Seal` — **never** plaintext.

## Encryption at rest

- On create/update, the submitted value is sealed with the session's derived key
  (AES-256-GCM) before it reaches the store.
- On reveal, the stored blob is opened with the session key. A decryption failure returns
  an error; it never falls back to returning ciphertext.
- The store layer is encryption-agnostic: it persists and returns opaque blobs. Encryption
  lives entirely in `internal/crypto` and is invoked by the web layer.

## Behavior

- **List** (`/vault`) shows all secrets, newest-updated first, with name, tag, expiry
  state, and last-updated date. The plaintext value is never in the listing.
- **Search** (`/vault/search?q=`) returns an htmx fragment of secrets whose name or tag
  matches the query (case-insensitive substring); an empty query lists all.
- **Reveal** (`GET /secrets/{id}/reveal`) returns an htmx fragment containing the decrypted
  value and records a `reveal` audit entry.
- **Create / Edit** use a form; name is required; expiry, if given, is `YYYY-MM-DD`.
- **Delete** (`DELETE /secrets/{id}`) removes the secret and records a `delete` entry; the
  htmx call swaps the row out of the list.

## Expiry

A secret may carry an expiry date. The list marks an expired secret distinctly; the
dashboard counts secrets expiring within 7 days (or already expired). Expiry is advisory —
it does not delete or hide the secret.
