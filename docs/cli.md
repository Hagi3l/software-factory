# CLI reference

`software-factory` is the single binary. It is the composition root — it wires the internal
packages together — and nothing in it enforces a guarantee; the components it
assembles (orchestrator, runner/broker, sandbox, gate) do.

```
software-factory validate   load + validate the config (the startup gate)
software-factory seed       author a spec + create a seed issue (CLI stand-in for the wizard)
software-factory run        run the in-process orchestrator + runner: the spec → merged-commit loop
software-factory approve    approve a parked integrate candidate (trusted-dev / TCB review gate)
software-factory reject     reject a parked integrate candidate
software-factory serve      start the control-room web server (no pipeline; static + read views)
software-factory login      Grok OAuth or Claude subscription-proxy registration
software-factory logout     clear stored subscription credentials
software-factory auth       status — show Grok / Claude auth
software-factory profile    list / detect / show stack profiles (go|node|python)
software-factory version    print the build version
software-factory help       print usage
```

Exit codes: `0` success · `1` command error (failed validation, bad config, crashed
run) · `2` usage error (unknown/missing command). Run any subcommand with `-h` for its
flags.

---

## `software-factory profile`

Stack profiles for multi-language targets ([profiles.md](profiles.md)).

| Command | Meaning |
|---------|---------|
| `profile list` | Shipped profiles (`go` → `config/`, `node` → `profiles/node`, `python` → `profiles/python`) |
| `profile detect --repo DIR` | Recommend a profile from lockfiles / manifests |
| `profile show NAME` | Image, Dockerfile, config path |

---

## `software-factory login` / `logout` / `auth`

Subscription credentials (see [selecting-provider.md](selecting-provider.md)).

| Command | Meaning |
|---------|---------|
| `login` / `login grok` | SuperGrok / X Premium+ device-code OAuth → `~/.software-factory/auth.json` |
| `login claude --proxy URL` | Register a Claude Pro/Max local subscription proxy (`--token`, `--mode` optional) |
| `logout [grok\|claude\|all]` | Clear one or all stored credentials |
| `auth status` | Print Grok and Claude login state |

Environment API keys always override the login store. Grok OAuth tokens are refreshed
on use during long `run` sessions.

---

## `software-factory validate`

Loads and validates the config directory — the startup gate. Run this before anything
else; `run` and `serve` perform the same validation internally and refuse to start on
a bad config.

| Flag | Default | Meaning |
|------|---------|---------|
| `--config DIR` | `config` | config directory (`factory.yaml`, `souls/`, `infra.<env>.yaml`) |
| `--env ENV` | `dev` | infra environment overlay to load (`infra.<env>.yaml`) |

A clean validation prints an OK line on stdout and exits 0. Non-fatal **advisories**
(e.g. producer and verifier sharing a model family) print to stderr as
`software-factory validate: warning: …` and still exit 0 — they're the operator's call, not a
startup blocker.

---

## `software-factory seed`

Authors a spec and creates a seed issue via the single-writer beads path — the CLI
stand-in for the control-room Create-Task wizard.

| Flag | Default | Meaning |
|------|---------|---------|
| `--title TITLE` | *(required)* | issue title |
| `--description TEXT` | — | issue description / spec summary |
| `--spec PATH` | — | spec markdown path (relative to `--repo`); created from title/description if absent |
| `--role ROLE` | the DAG's single entry stage | agent role to enter the pipeline at |
| `--repo DIR` | `.` | repository holding the beads store (`.beads`) and specs |
| `--config DIR` | `config` | config dir (used to resolve the entry role) |
| `--env ENV` | `dev` | infra environment overlay |
| `--bd PATH` | `bd` | path to the beads CLI |

The seed issue enters at the pipeline's entry stage (`plan` in the shipped config), so
the planner decomposes it into concrete child work items. A running `software-factory run`
picks it up automatically.

---

## `software-factory run`

Runs an in-process orchestrator + one runner over embedded NATS until interrupted.
This is the `spec → merged-commit` loop. SIGTERM / Ctrl-C drains cleanly.

| Flag | Default | Meaning |
|------|---------|---------|
| `--repo DIR` | `.` | integration repo: candidates are pushed and merged here, worktrees seeded from it |
| `--config DIR` | `config` | config directory |
| `--env ENV` | `dev` | infra environment overlay |
| `--bd PATH` | `bd` | path to the beads CLI |
| `--serve-addr HOST:PORT` | *(off)* | also serve the control room here; the live SSE feed shares this run's in-process NATS |
| `--nats-addr HOST:PORT` | *(in-process only)* | expose this run's NATS here so `software-factory approve`/`reject` can reach it |

Requires a Docker daemon and a configured model (API key from the environment). With
`--serve-addr`, the control room is co-located and its live feed has a real source;
with `--nats-addr`, a separate `approve`/`reject` process can publish decisions in.

---

## `software-factory approve` / `software-factory reject`

Decide a parked integrate candidate — the trusted-dev / TCB-review gate (the factory
writes code, a human reviews the diff). The decision is published over NATS to the
single-writer orchestrator, which applies it idempotently and is bound to the
candidate sha (a stale approval against a since-changed candidate is ignored).

```
software-factory approve [flags] <issue>
software-factory reject  [flags] <issue>
```

| Flag | Default | Meaning |
|------|---------|---------|
| `<issue>` | *(required, positional)* | the parked issue id |
| `--nats URL` | `nats://127.0.0.1:4222` | URL of the running factory's NATS listener (`software-factory run --nats-addr`) |
| `--approver WHO` | the OS user | who is deciding; recorded on the issue for audit |
| `--repo DIR` | `.` | repository holding the beads store (`.beads`) |
| `--bd PATH` | `bd` | path to the beads CLI |

**Approve** replays the preserved, gate-verified provenance onto the merge (re-gated by
the merge queue only if a rebase was needed) and closes the issue. **Reject** routes a
fresh fix attempt through the pipeline, or dead-letters it when retry caps are spent.

These need the running `software-factory run` to have been started with `--nats-addr`, since a
separate process can't reach an in-process-only NATS server.

---

## `software-factory serve`

Starts just the control-room web server — no pipeline, no NATS. Renders the static and
read-only views; the live feed (`/events`) returns 503 and data-backed views show a
"not attached" notice, because there's no run to read from. For live data, use
`software-factory run --serve-addr` instead.

| Flag | Default | Meaning |
|------|---------|---------|
| `--addr HOST:PORT` | `127.0.0.1:8080` | address to listen on |

See [the control room](control-room.md) for the views.

## `software-factory sandbox-goproxy` (internal)

Not run by operators — the `go-toolchain` sandbox image's entrypoint starts it inside the
sandbox. It is the in-sandbox GOPROXY shim: an HTTP server `go`'s `GOPROXY` points
at, which forwards each module-proxy request over the bind-mounted broker socket to the
runner. The runner fetches from the configured `broker.package_proxy` and logs the pull —
so a zero-network sandbox can fetch a dependency it doesn't already have cached, mediated at
the one egress chokepoint (or denied, if `package-proxy` isn't allowlisted). It holds no
policy; the allowlist and proxy base live on the runner. See [configuration.md](configuration.md)
(`broker`) and [security.md](../specs/security.md) Control 2.

| Flag | Default | Meaning |
|------|---------|---------|
| `--broker NET:ADDR` | `unix:/run/factory/broker.sock` | runner broker endpoint (`unix:<path>` or `vsock:<cid>:<port>`) |
| `--addr HOST:PORT` | `127.0.0.1:8123` | loopback address to serve the GOPROXY on |
