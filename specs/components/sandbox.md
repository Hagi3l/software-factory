# Sandbox

The isolation boundary an [agent](agent.md) runs inside. It is the unit of both
**ephemerality** and **isolation**: one work item, one sandbox, destroyed after.

See also: [runner.md](runner.md), [../security.md](../security.md),
[../configuration.md](../configuration.md).

---

## Requirements

A sandbox must provide:

- **Zero direct network.** No route to anything except a local channel to the
  [runner](runner.md), which brokers a small allowlist. This is non-negotiable —
  it is the property the whole security model rests on.
- **Strong isolation from the host.** The threat model assumes the agent loop and
  its generated code are hostile.
- **An ephemeral, writable git worktree** seeded at the brief's base ref.
- **Resource limits** (CPU, memory, disk, wall-clock) enforced from outside.
- **Deterministic teardown** — destroyed unconditionally when the invocation ends;
  no state survives.

---

## Pluggable backends

The backend is **config, not code** (`sandbox.backend`):

| Backend | Isolation | Startup | Use |
|---------|-----------|---------|-----|
| **Docker** | shared host kernel — *weak* | fast | local dev only |
| **gVisor** | user-space kernel | medium | medium-trust |
| **Firecracker** | own kernel (KVM microVM) | ~125ms | **production target** |

Docker is acceptable for local development ergonomics but **shares the host
kernel**, so it is not suitable for genuinely untrusted execution. Firecracker (or
at least gVisor) is the production target — its microVM model is exactly what runs
untrusted code at AWS Lambda / Fly.io scale.

Design the `Sandbox` interface around the **stricter** microVM model from the
start (explicit rootfs seeding, no casual bind mounts, local-socket I/O), so
moving from the Docker impl to Firecracker is not a rewrite.

```yaml
sandbox:
  backend: firecracker     # docker | gvisor | firecracker
  egress:  broker-only
  limits:  { cpu: 2, mem: 2Gi, disk: 8Gi, wall: 30m }
```

### Selection is honored, and fails closed

"Config, not code" is only true if the configured backend is actually the one that
runs. The runtime resolves `sandbox.backend` to a concrete backend **at startup**, and
a request it cannot satisfy is a **fatal startup error — never a silent downgrade**.
Selecting a backend the host cannot provide (e.g. `firecracker` without KVM) must fail
closed rather than degrade to a weaker one: running untrusted code under *less*
isolation than the operator asked for is a security regression, and a silent fallback
hides it. The mapping is exact — `docker` (or empty) → the Docker backend; `gvisor` →
gVisor (Docker pinned to the `runsc` OCI runtime); `firecracker` → the microVM backend,
or a clear error where unavailable; any other value → a configuration error. gVisor is
not a distinct provisioning path but the Docker backend with the runtime pinned, so
zero-network, worktree seeding, and teardown are identical across the two.

This is a startup contract, not just validation: [`harness validate`](../configuration.md)
checks the value is *well-formed*, but whether the selected backend is *available on
this host* can only be known when the runtime binds it, so the fail-closed check lives
there.

### Least-privilege boot on container backends

Zero-network is necessary but not sufficient: on a shared-kernel backend the
container's *process* privileges are the remaining escape surface, and Docker's
defaults (root exec user, the default capability set, unbounded PIDs, swap up to
2× the memory limit) are tuned for convenience, not hostile tenants. The
container-based backends (Docker, and gVisor via the same provisioning path) must
therefore boot **least-privilege**, enforced from outside the guest at provision
time — the same posture as the resource limits:

- **Drop all capabilities** (`--cap-drop=ALL`), re-adding only what the toolchain
  profile actually needs.
- **Forbid privilege escalation** (`--security-opt no-new-privileges`) — no setuid
  route back to what was dropped.
- **Cap the PID count** (`--pids-limit`) so a fork bomb exhausts the sandbox, not
  the shared host kernel.
- **Pin swap to the memory limit** (`--memory-swap` = `limits.mem`) so the memory
  cap is a real ceiling, not a soft target the guest can double through swap.

These controls are the backend's responsibility precisely because the guest is
untrusted: nothing inside the sandbox can be relied on to confine itself. A
microVM backend (Firecracker) gets the equivalent isolation from the VM boundary
itself; the container backends have to ask the runtime for it explicitly.

---

## Profile → image resolution

A soul names a **logical sandbox profile** (`soul.sandbox`, e.g. `go-toolchain`) —
the *toolchain it needs*, not where that toolchain's bytes live. The infra overlay's
`sandbox.profiles` registry resolves that name to a **concrete, backend-specific
bootable artifact**: a (digest-pinned) image for Docker/gVisor, a rootfs for
Firecracker. This is the same indirection the [`models` registry](../configuration.md)
gives the soul's `model` name, and for the same reasons:

- **Backends need different artifacts.** Docker boots an image reference; Firecracker
  boots a rootfs. The *same* profile name resolves to different concrete things per
  backend, so the soul stays backend- and environment-agnostic and swapping backends
  stays config, not a rewrite.
- **Provenance pins bytes, not a tag.** Under a hostile-toolchain threat model the image
  is load-bearing — a poisoned toolchain poisons every build *and* every gate. Resolving
  to a digest (`registry/img@sha256:…`) and recording that digest in provenance is what
  makes *provenance by construction* real for the toolchain, not just the model.

```yaml
# infra.<env>.yaml
sandbox:
  backend: docker
  profiles:
    go-toolchain:
      image: harness/go-toolchain@sha256:…       # docker / gvisor read `image`
      # rootfs: /var/lib/harness/go-toolchain.ext4   # firecracker reads `rootfs`
```

Resolution happens where the orchestrator/runner **build the sandbox spec**, not inside
the backend: the backend contract stays "boot this concrete artifact", which is what
keeps the Docker→Firecracker move a swap rather than a rewrite. The logical profile name
rides in provenance/telemetry; the resolved digest rides alongside it.
[`harness validate`](../configuration.md) gates startup on every `soul.sandbox`
resolving to a `profiles` entry that carries the field the active backend needs.

**Producer and verifier resolve the same profile.** The gate grades a candidate in the
*producer soul's* profile — the tests must compile and run on the same toolchain —
resolved through the same registry to the same concrete image, still in a fresh,
producer-distinct sandbox (see [below](#two-distinct-sandboxes-per-work-item)).

The package data a zero-network gate needs (the `govulncheck` vulnerability DB, licence
metadata) is **baked into the profile image**, never fetched — the same offline guarantee
the build relies on. A *fresh* dependency the baked module cache does not hold is fetched
**through the broker**, not directly: the image runs an in-sandbox GOPROXY shim that
forwards `go`'s module-proxy requests to the runner's package proxy (see
[runner.md](runner.md)), so even a brand-new dependency stays on the one logged egress path.
What stays open is *caching* those fetches across invocations (below).

### Per-language language server

The profile image is also where the **language server** lives — `gopls` in
`go-toolchain`, `tsserver` in `ts-toolchain`, etc. — alongside the toolchain it serves.
The agent's [semantic tools](agent.md#semantic-tools-lsp-backed) (`find_symbol`,
`references`, `rename`, …) are language-*neutral*; the *backing server* is whatever the
profile carries. So "Go has gopls, TS has tsserver" is an image fact, not a tool fact,
and the canonical tool surface never grows per language.

To keep the semantic tools image-agnostic, each profile exposes its servers through a
**fixed launch convention**: a `languageId`→launch-command **manifest** baked into the
image at a known path (`/etc/harness/language-servers.json`), rather than the tool
hard-coding a server per language. The tool reads the manifest, resolves the file under
the cursor to a `languageId` (by extension), and launches that entry's command over the
local channel. The session is launched **lazily** on the first semantic call and torn
down deterministically with the sandbox, like any other in-sandbox state. The manifest
is baked from the *same* file the harness embeds and validates (`lsmanifest`), so the
format the tools resolve and the file the image carries cannot drift. The image digest
already pinned in provenance therefore pins the *language-server version* and its manifest
too, so semantic results are reproducible. (Demo scope ships the `go`→`gopls` entry only;
`.templ`/`.css` ride the text floor.)

The session manager runs the server as a **streamed process** — a long-lived child with
its stdin/stdout attached, so it stays warm across many queries and speaks JSON-RPC for
the whole invocation rather than cold-starting per call. This is a *streamed Exec*, not a
new hole in the boundary: the server still runs **inside** the sandbox and the host never
reaches in (the same property `Exec` has). Backends expose it as an **optional capability**
(`SessionOpener`); a backend without it — or a sandbox profile carrying no manifest — leaves
the manager inert, so every semantic read degrades to the text floor exactly as
[agent.md](agent.md#semantic-tools-lsp-backed) requires. The manager is also what couples
the edit tools to the session: every `edit_file`/`write_file` notifies it (`didChange`), so
a warm server never reads stale text.

---

## A non-isolating local backend (testing only)

Exercising the full `spec → implement → gate → merge` spine in a test needs *faithful
execution*, not real isolation. A **local host-exec backend** satisfies the
`Backend`/`Sandbox` interface by running each command in an ephemeral host tempdir — a
real git worktree, a real shell, the candidate branch extracted as a git bundle over
the same `Exec` channel the container backends use — instead of inside a container.
This makes orchestration testable without a container runtime, giving a fast, hermetic
end-to-end test of the control flow.

It is deliberately **not an isolation backend**: it shares the host kernel *and
network* and enforces no resource limits, so it **must never run untrusted agents**.
It is therefore **not config-selectable** — `sandbox.backend` accepts only the
isolation backends in the table above; the local backend is injected by tests alone
and can never appear in a real deployment. The isolation properties it gives up
(zero-network, limits, deterministic teardown) are covered instead by a separate
Docker-backed end-to-end test.

---

## Local transport to the runner

Because there is no network, the agent reaches the runner over a local channel:

- **Firecracker:** vsock.
- **Docker:** unix domain socket.

The agent's "tools for the outside world" are RPCs over this channel; the runner
[brokers](runner.md#the-broker--the-one-window-to-the-world) them.

---

## Two distinct sandboxes per work item

Note the deliberate separation enforced by **producer ≠ verifier**:

1. The **agent's sandbox** — where the candidate is *produced* (untrusted).
2. A fresh **[verification sandbox](../verification.md)** — where the orchestrator
   *grades* the candidate, distinct from and after the producer's. An untrusted
   process never grades its own output.

---

## OPEN questions

- **Image publish & digest pinning** — the *composition* of each role image is now
  defined: a per-profile `deploy/*.Dockerfile` bakes the toolchain, the module
  cache (offline build), the gate tooling (golangci-lint, gosec, govulncheck + an
  offline vuln-DB snapshot, go-licenses, gremlins), and the per-language server +
  manifest. What stays is the *publish* half — pushing each built image to a registry
  under the **pinned digest** the `profiles` registry references. The default publish
  target is a **public registry** (e.g. `ghcr.io`/Docker Hub), not private registry
  infrastructure: the byte-pinning guarantee comes from the **digest**
  (`registry/img@sha256:…`), which is content-addressed and so holds identically no
  matter which registry hosts the image — a public registry weakens nothing the
  threat model relies on, and removes the standing-up-private-infra blocker. The dev
  overlay still resolves the local `harness/go-toolchain:dev` tag; production pins
  `<public-registry>/…@sha256:…`. (A private registry stays an optional swap for
  air-gapped or scan-on-push deployments.)
- **Caching** package downloads across invocations without weakening the egress
  control (e.g. a read-through vetted mirror) — TBD; see [runner.md](runner.md).
