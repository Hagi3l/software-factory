# Runner

A per-host daemon. The only **long-lived NATS citizen** and the only **credential
holder** on its host. It turns a unit of ready work into a sandboxed agent
invocation and brokers everything that agent touches.

See also: [agent.md](agent.md), [sandbox.md](sandbox.md),
[../messaging.md](../messaging.md), [../security.md](../security.md).

---

## Responsibilities

1. **Pull work.** Subscribe as a [JetStream](../messaging.md) pull consumer on the
   role(s) it serves. Runners compete to pull; this gives load balancing and
   horizontal scale by simply starting more runners.
2. **Provision a sandbox.** Create a [sandbox](sandbox.md), seed it with a git
   worktree at the brief's base ref, inject the [Brief](agent.md#the-brief) and
   short-lived scoped credentials.
3. **Broker all I/O** (below).
4. **Harvest.** Collect the candidate branch, the Result envelope, logs, and
   provenance; write large evidence to the [artifact store](artifact-store.md)
   (content-addressed) **before reaping**. Publish the Result.
5. **Reap.** Destroy the sandbox unconditionally when the invocation ends.
6. **Ack.** Ack the JetStream message only after harvest; an un-acked message is
   redelivered (this *is* the lease — see [orchestrator.md](orchestrator.md)).

A runner is stateless across invocations; everything durable lives in beads/git.

---

## The broker — the one window to the world

The sandbox has **zero direct network**. Its only channel is a local transport to
the runner (vsock for Firecracker, unix socket for Docker). The runner relays a
*small allowlist* of destinations and **logs every call**:

| Brokered call | Destination | Notes |
|---------------|-------------|-------|
| model completion | model API | runner holds the key + the [provider adapter](../models.md); agent is provider-unaware |
| `publish` / events | NATS | agent never has NATS creds |
| package fetch | **package proxy** (public `proxy.golang.org` by default) | pinned by `go.sum` + checksum DB, logged at the broker, scanned post-fetch by the `qa` gate; a vetted pin/scan-at-fetch mirror is an optional swap |
| git push | task branch only | scoped token, minted per task |

Anything not on the allowlist is denied. This makes the runner the **single
audited chokepoint** for all agent egress — the load-bearing security control of
the whole factory. See [../security.md](../security.md).

**Package fetch crosses the boundary via an in-sandbox GOPROXY shim.** A zero-network
sandbox can't dial a proxy, and `go`'s `GOPROXY` speaks HTTP, not the broker's framed RPC —
so the toolchain image runs a tiny loopback HTTP server (`harness sandbox-goproxy`) that
forwards each module-proxy request over the broker to the runner, which performs the real
fetch and logs it. The shim holds no policy: the allowlist and the proxy base live on the
runner, so a destination the operator hasn't allowed is refused there (deny-by-default) and
the refusal is relayed back to `go` as a failed fetch. This keeps package fetch on exactly
the same audited path as every other egress.

## Model calls: the runner is the provider boundary

The agent sends a **canonical** model request (see [../models.md](../models.md)); the
runner selects the provider adapter for the soul's `model`, holds the API key,
translates to the provider's wire format, calls it, and relays a canonical response
— streaming tokens out to NATS for the live view as they arrive. Because every model
call passes through here, the runner is also where token [usage is tallied against
budgets](../workflow.md). The sandbox never holds a key and never learns which
provider answered.

Local-to-sandbox operations (read/write the worktree, run `build`/`test`/`lint`)
do **not** go through the broker — they execute inside the sandbox against the
worktree.

## The broker is also the observability collector

Because the broker sees every external call an agent makes, it is the natural place
to emit structured events (to NATS, for the live feed) and
[OpenTelemetry](../observability.md) spans (LLM turns, tool calls, git ops). The
security chokepoint doubles as the telemetry chokepoint — there is nowhere for an
agent to act unobserved. The full transcript captured here is what makes
[replay](../observability.md) possible; the runner harvests it to the
[artifact store](artifact-store.md) before teardown.

---

## Secrets

- The runner mints **per-task, short-lived, narrowly-scoped** credentials (ideally
  a git token that can push *only* the task's branch).
- Secrets are injected for the invocation lifetime and **die with the sandbox**.
- **No standing credentials in sandbox images.** Generated code must never be able
  to exfiltrate a durable secret.

---

## Lifecycle of one invocation

```
pull work  →  provision sandbox + worktree(base ref)  →  inject Brief + creds
           →  open broker channel
           →  agent runs (broker relays allowed I/O, logs everything)
           →  agent terminates (done | budget | escalation)
           →  harvest branch + Result + logs + provenance
           →  write large evidence to artifact store (content-addressed)
           →  publish Result   →   destroy sandbox   →   ack
```

---

## OPEN questions

- **Warm pools.** Cold-starting a microVM per task adds latency; a pool of
  pre-booted sandboxes could amortize it. Deferred until startup latency is a
  measured problem.
- **Per-host capacity / concurrency limits** and how they interact with epic
  budgets — policy TBD.
