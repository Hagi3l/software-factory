# Audit log

Every sensitive action is recorded in an append-only audit log so the owner can see what
happened to the vault and when. See [secrets.md](secrets.md) and [auth.md](auth.md) for the
actions that emit entries.

## Model

An audit entry has an `action`, an optional `target` (the secret name, or empty for
account-level actions), and a UTC timestamp. Stored in the `audit_log` table. The log is
**append-only**: entries are never updated or deleted by application code.

## Recorded actions

- `login` — a successful authentication (target empty).
- `create`, `update`, `delete` — secret lifecycle (target = secret name).
- `reveal` — a secret's value was decrypted and shown (target = secret name).

## Dashboard activity feed

The vault page shows the most recent entries (newest first) in an activity panel, and a
summary card with the time of the last recorded activity. The feed is read-only.

A new feature that performs a sensitive action **must** append a corresponding audit entry,
so the log remains a complete record.
