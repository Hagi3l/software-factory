# Authentication

The vault is single-user. Access is gated by a **master password** that both authenticates
the user and derives the at-rest encryption key. See [secrets.md](secrets.md) for how the
derived key is used, and [architecture in the index](README.md#architecture--conventions-binding-on-every-change).

## First-run setup

- When no user exists, every entry point redirects to `/setup`.
- Setup accepts a master password (minimum 8 characters) and creates the single `owner`
  user. The password is stored only as an **Argon2id verifier** (`crypto.HashPassword`);
  the plaintext is never persisted. A random per-vault salt is stored for key derivation.
- A successful setup logs the user in and lands on the vault.

## Login

- `/login` accepts the master password and verifies it against the stored Argon2id verifier
  using a **constant-time** comparison (`crypto.VerifyPassword`).
- A correct password derives the encryption key (`crypto.DeriveKey(password, salt)`), starts
  a session, and records a `login` audit entry.
- An incorrect password returns HTTP 401 and re-renders the login form with an error; it
  does **not** reveal whether the vault is initialized beyond the redirect to `/setup` when
  no user exists.

## Sessions

- A session is an opaque random id (`crypto.RandomToken`) stored in an `HttpOnly` cookie.
  Server-side it holds the username and the in-memory encryption key; the key is never
  persisted, so a process restart forces re-login.
- **Cookie `SameSite`.** This app is a demo artifact shown live inside the presentation
  deck's *cross-site* iframe, and `SameSite=Strict`/`Lax` cookies are dropped on cross-site
  frame loads — so the shipped default is `SameSite=None; Secure` (Secure is mandatory for
  `None`; the deployment terminates TLS, so this holds). Setting `VAULT_STRICT_COOKIE`
  restores the hardened `SameSite=Strict` for a standalone, non-embedded deployment. (Note:
  `None` only makes the cookie *eligible* cross-site — Safari still blocks third-party iframe
  cookies outright, so demo in Chrome/Firefox.)
- Sessions expire after 30 minutes. Protected routes redirect to `/login` when the cookie is
  missing or expired.
- Sign-out destroys the server-side session and clears the cookie.
