# Vault — specifications

This directory is the source of truth for **what the vault is and how it must behave**.
The app is an established, single-user secrets vault: a person unlocks it with a master
password, stores credentials encrypted at rest, reveals them on demand, and every
sensitive action is recorded in an audit log.

Humans author intent here; the implementation satisfies it. This file is the **index** —
a thin hub of pointers. Follow the links to the contract you need; `read_file` any spec on
demand rather than expecting them all in context.

## Feature specs

| Spec | Covers |
|------|--------|
| [auth.md](auth.md) | First-run setup, master-password login, session lifetime, sign-out. |
| [secrets.md](secrets.md) | The secret model, encryption at rest, create/edit/delete, reveal, search, expiry. |
| [audit.md](audit.md) | The append-only audit log and the dashboard activity feed. |

## Conventions

The engineering conventions binding on **every** change — stack, layout, layering,
encryption, randomness, session hardening, htmx fragments, committed generated artifacts, the
no-new-dependencies rule, and the gate — live in **[conventions.md](conventions.md)**. They
are not feature-specific, so the harness injects them (with this index) into every agent's
Brief; read them before writing code.
