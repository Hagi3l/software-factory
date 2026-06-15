# Security

The harness is a **secure software factory**: it must produce trustworthy software
while assuming its own workers are hostile. Security is not a feature here, it is
the premise.

See also: [architecture.md](architecture.md),
[components/sandbox.md](components/sandbox.md),
[components/runner.md](components/runner.md), [verification.md](verification.md).

---

## Threat model

**Assume the agent loop and the code it generates may be hostile.** Concretely,
defend against:

- An agent that is **prompt-injected** (via spec content, a fetched dependency, a
  tool result) into acting against intent.
- **Generated code** that tries to exfiltrate data, reach the network, escape the
  sandbox, or persist beyond its task.
- **Supply-chain attacks** — a malicious or typo-squatted dependency pulled during
  a build.
- An agent attempting to **corrupt the work graph** or **merge unreviewed code**.

Out of scope for now: a compromised *trusted* component (orchestrator/runner host)
and production deploy (deferred).

---

## Trust boundaries

**Trusted:** orchestrator, runner, the human-facing requirements planner (runs no
untrusted code *on the host*), beads, `main`.

**Untrusted:** every sandboxed agent and every artifact it produces.

The controls below all derive from keeping that line bright.

The requirements planner is on the trusted side because its conversation and its spec/issue
authoring run host-side with no code execution. When it grounds specs in an existing codebase
(see [control-room.md](control-room.md)), the *reads* it issues are model-directed, so they are
**not** treated as trusted: they execute inside a read-only, zero-network sandbox seeded from
the repo, behind a deny-all broker, torn down per session. This keeps the trust line bright —
a model-chosen command reaches neither the host nor the network — and is strictly a *narrowing*
of capability: the planner gains a confined code-*reading* surface and no write, host, or egress
path.

---

## Control 1 — Zero-network sandboxes, brokered egress

The single most important control. A sandboxed agent has **no direct network**.
Its only channel is a local socket to its [runner](components/runner.md), which
relays a small **allowlist** and logs every call:

- LLM API · NATS · package proxy (public `proxy.golang.org` by default) · git (task branch only).

Everything else is denied by default. The runner is the **one audited chokepoint**
for all agent egress. A compromised agent simply has nowhere to send data and
nothing to pull from. See [components/sandbox.md](components/sandbox.md).

---

## Control 2 — Supply-chain mediation

Dependency fetches are the obvious attack on an autonomous factory. All package
access is routed through a proxy on the broker allowlist, so every pull is **mediated
and logged** at the one chokepoint and nothing is fetched off-allowlist.

The default proxy is the **public `proxy.golang.org`**, not a private vetted mirror.
This is a deliberate simplification: the supply-chain guarantees the mirror would add
are already covered by primitives we have, so a parallel scanning mirror is not on the
default path.

- **Pinning** — `go.sum` plus the public Go checksum database (`sum.golang.org`) pin
  every module to exact, tamper-evident bytes; a substituted dependency fails the
  checksum, not a mirror policy.
- **Logging** — the proxy is on the broker allowlist, so the runner logs every fetch
  regardless of where it resolves.
- **Scanning** — the `qa` gate runs `govulncheck` + license-scan on every change (see
  [verification.md](verification.md)), so vulnerable/ill-licensed dependencies are
  caught *post-fetch at the gate* rather than pre-fetch at the proxy.

A private **vetted mirror/proxy that pins, scans, and logs at fetch time** remains an
**optional** deployment swap — point the allowlisted proxy at it — for organizations
that want to *block* a bad dependency before it is ever pulled rather than reject it at
the gate. It is not required for the guarantees above.

Both the producing agent's sandbox **and** the
[verification sandbox](glossary.md#verification-sandbox) reach the proxy through the **one**
host-side fetcher, gated by the same allowlist opt-in: allowlisting `package-proxy` grants
the egress to both, so a candidate that adds a new dependency can be built by the producer
*and* re-gated by the verifier against the identical pinned bytes. The verifier is otherwise
deny-all (see [Control 4](#control-4--producer--verifier)); package fetch is the only egress
it is ever granted, and it cannot push, call a model, or emit events.

---

## Control 3 — Scoped, short-lived secrets

- The runner mints **per-task** credentials, **narrowly scoped** (ideally a git
  token that can push *only* the task's branch).
- Injected for the invocation lifetime; **die with the sandbox**.
- **No standing credentials baked into images.** Generated code must never be able
  to capture a durable secret.

### Minting the push token

The candidate-branch push is the one egress that needs a write credential, and it is the
sharpest test of "the secret never reaches the sandbox": the agent must get its branch out,
but it must never hold a token that could push anything else, anywhere else, later.

- **The token lives only on the trusted runner.** The agent reaches git solely through the
  broker's `git.push` (branch name only — no URL, no token); the runner does the real push
  host-side. So the credential is never injected into the network-less sandbox at all — the
  agent is *remote-unaware* exactly as it is *provider-unaware* for model calls. This is a
  deliberate reading of "injected for the invocation lifetime": the secret is scoped to the
  invocation on the **runner**, not handed to the sandbox.
- **Scheme: GitHub App installation token.** The runner mints an installation access token
  scoped to the one repository with `contents:write`, signs the App JWT with a private key
  it reads at mint time, and pushes with it. A GitHub token **cannot** be scoped to a single
  branch, so "**only the task branch**" is enforced by the runner's broker **branch guard**
  (it refuses any branch but `candidate/<issue>`), not by the token — the token supplies the
  repo-scoped, short-lived credential; the guard supplies the branch scope. This split is the
  honest mechanism behind "a token that can push only the task's branch".
- **Dies with its one use.** The token is minted just before the push and **revoked the
  instant the push completes** — tighter than the invocation lifetime; its ~1h TTL is only a
  backstop if revocation fails. Revoke is best-effort (a transient failure does not throw away
  a good candidate — the TTL bounds exposure), logged when it fails.
- **Key custody.** The App private key follows the **API-key / signing-key posture** — a
  runtime-provisioned secret referenced by **path** in config, never committed or baked into
  an image, read only on the trusted runner at mint time. Production delivery is a
  secret-manager mount (the deployment remainder, like the signing key of Control 10).
- **Dev / single-host shape.** With no remote configured the candidate branch is applied to
  the local source repo (the bootstrap path, no token). A remote with no App configured pushes
  unauthenticated — valid for a `file://` remote — so the mechanism is exercisable offline
  without a credential authority. The `git` egress destination must be allowlisted either way
  (deny-by-default).

---

## Control 4 — Producer ≠ verifier

Verification runs in a fresh sandbox the orchestrator controls, never the
producer's; tests are authored independently of code; agents propose and the
orchestrator applies. This prevents untrusted processes from grading their own
output or merging their own code. The verifier's sandbox is deny-all — no model
calls, git push, or events — with the lone, principled exception of the
[package-proxy egress](#control-2--supply-chain-mediation) (so a new-dependency
candidate is re-gatable against the same pinned bytes the producer fetched). See
[verification.md](verification.md), [integration.md](integration.md).

---

## Control 5 — Single-writer work graph

Only the orchestrator writes beads. Agents *propose* mutations, which are validated
for DAG-legality (valid roles, existing dependency targets, acyclic edges, in budget)
before being applied. A confused or compromised agent cannot inject garbage, cycles,
or dangling edges into the work graph. The orchestrator checks every proposed
dependency target's existence itself — prefix-blind, never trusting the store to
reject a foreign-prefix id it would treat as an unchecked external reference.
See [components/orchestrator.md](components/orchestrator.md).

---

## Control 6 — Bounded autonomy

[Budgets and retry caps](workflow.md) cap blast radius and guarantee termination.
A runaway or adversarially-looping task always terminates into the dead-letter
queue rather than consuming unbounded compute.

---

## Control 7 — Untrusted artifacts are never interpreted

The [control room](control-room.md) serves agent-produced artifacts (transcripts,
gate evidence, diffs, prompts) back to a human's browser. Those bytes are
**untrusted agent output** and must never be interpreted as markup or script. The
artifact endpoint therefore serves content as **`text/plain`** with
**`X-Content-Type-Options: nosniff`**, defeating both direct HTML/JS execution and
MIME-sniffing into an executable type. This keeps a hostile agent from turning the
human's one window into the factory into an XSS vector. See
[control-room.md](control-room.md), [components/artifact-store.md](components/artifact-store.md).

---

## Provenance by construction

Because no human reviews the code, the **audit trail is the accountability**.
Every merged change carries a chain: **beads issue → commit → signed evidence**,
recorded as a commit trailer:

```
Soul: implementor-go | Model: claude-opus-4-7 | Tests-Soul: test-author-go
Issue: bd-1234 | Prompt-SHA: 9af… | Verified: build@sha256:1c2…,test@sha256:8be…,gosec@sha256:0a4… | Traceability: sha256:7c1… | Transcript: sha256:3d2…
```

This is a SLSA-style provenance record: every autonomous change is attributable to
the soul, model, prompt, and the evidence that gated it. `Soul` is the
**implementor**; `Tests-Soul` is the independent **test author** — recording both on
the commit makes [producer ≠ verifier](verification.md) auditable from the trailer
alone, not merely enforced at runtime. A change with no `author-tests` stage in its
lineage carries `Tests-Soul: (none)`, the same self-describing degradation as a missing
`Traceability`. Each entry in `Verified`
cites a passed check as `<name>@<evidence-hash>`, the hash pointing into the
content-addressed [artifact store](components/artifact-store.md) at that check's
captured output — so verification is auditable down to the exact bytes, not merely a
list of check names. `Traceability` cites the [test↔spec traceability map](verification.md)
the `author-tests` stage produced (threaded forward to the merge), the window into how the
test author read the pure-prose spec. `Transcript` cites the full broker-captured agent
conversation — every LLM request/response the runner relayed for the producing invocation —
the **replayable decision trail** that lets a human reconstruct exactly what the model saw
and did (see [observability.md](observability.md)). The `Prompt-SHA`, the evidence hashes,
the traceability hash, and the transcript hash are all such pointers, so a record cannot be
silently altered. A check whose evidence failed to persist degrades to a bare `<name>`, and
a change with no `author-tests` stage in its lineage carries `Traceability: (none)` (and an
invocation whose transcript could not be harvested carries `Transcript: (none)`) —
self-describing, like a missing `Prompt-SHA`, never a dropped verdict.

So the trailer can be vouched for by the trusted layer, the tip of `main` is always
a **harness-authored provenance commit** sitting on top of the verified candidate,
never the agent's own commit fast-forwarded into place. See
[integration.md](integration.md).

### Signing the provenance commit

The trailer is only as trustworthy as the commit carrying it: a plaintext trailer on an
unsigned commit is forgeable by anyone with write access to the integration repo. So the
harness-authored provenance commit is **cryptographically signed with the harness's own
identity**, and **verified on read**. This is what makes "the audit trail is the
accountability" real rather than aspirational — `main`'s tip is a commit only the harness
could have produced.

- **Scheme: git-native SSH signing** (`gpg.format=ssh`). No GPG keyring or external daemon;
  verification is a public-key check against an **allowed-signers file** (principal → harness
  public key) that anyone may hold. Only the trusted provenance commit is signed — the
  agent's own candidate commits beneath it are untrusted by construction and never signed.
- **Key custody.** The private signing key is the harness identity; it follows the same
  posture as model API keys — a **runtime-provisioned secret**, referenced by path in config,
  **never committed to the repo or baked into a sandbox image**. In single-host/dev it is an
  operator-supplied key file; in production it is delivered by a secret manager / ssh-agent to
  the orchestrator host (the deployment remainder). Generated code can never capture it: it
  lives only on the trusted orchestrator, never inside a sandbox.
- **Verify on read.** The [control room](control-room.md) verifies each merged commit's
  signature against the allowed-signers file and surfaces the verdict (signed / unsigned /
  unverified) on the provenance view, so a human auditing the chain sees not just *what*
  produced a change but that the trusted layer vouches for the record. A signature by an
  unrecognized key is flagged distinctly — it is more suspicious than an unsigned commit.
- **Artifacts need no separate signature.** Every artifact the trailer cites (prompt,
  transcript, gate evidence, traceability map) is **content-addressed** — its hash *is* its
  integrity proof — and those hashes live inside the signed commit message. Signing the
  commit therefore transitively authenticates every cited artifact; a second artifact-signing
  mechanism would be a redundant source of truth.

---

## OPEN questions

- ~~Signing scheme / key custody for provenance trailers — TBD.~~ **Decided:** git-native
  **SSH signing** of the harness-authored provenance commit, verified on read against an
  allowed-signers file; the private key is a runtime-provisioned secret on the trusted
  orchestrator (never committed/baked), production custody via secret-manager/ssh-agent is
  the deployment remainder. See "Signing the provenance commit" above.
- Whether spec content itself should be treated as an injection surface and
  sanitized before entering an agent's context — likely yes; mechanism TBD.
