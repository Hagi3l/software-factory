# Models

The harness is **model-agnostic**: any tool-calling-capable model can power any
[soul](components/agent.md), chosen per soul by config. The agent loop is built in
Go and calls model APIs **directly** — it never shells out to an existing agentic
CLI.

See also: [components/agent.md](components/agent.md) (the loop + tools),
[components/runner.md](components/runner.md) (key custody + adapters),
[configuration.md](configuration.md), [observability.md](observability.md).

---

## The provider abstraction lives in the runner

The agent runs inside a [zero-network sandbox](components/sandbox.md), so its model
calls — like all I/O — go through the broker. The agent emits a **canonical** model
request to its [runner](components/runner.md); the runner holds the keys, selects
the provider adapter for the soul's `model`, translates to the provider wire
format, calls the API, and relays a canonical response back.

```
agent (sandbox) ──canonical Request──▶ runner ──adapter──▶ { Anthropic | OpenAI-compat } API
                ◀─canonical Response──        ◀──────────
```

The agent is **provider-unaware**: it never holds a key and never knows which
provider answered. Three things therefore collapse into one mechanism:

- **model-agnosticism** — `model: gpt-4o` vs `claude-opus-4-7` vs `llama-3.3-70b`
  is config on the soul; the loop logic is identical.
- **zero-network + key custody** — keys live only in the trusted runner.
- **budget + telemetry** — the runner sees every call, so it is the natural point
  to enforce [budgets](workflow.md) and emit [spans](observability.md).

---

## Canonical types

Lowest-common-denominator types plus thin per-provider adapters. **No framework** —
wrap the official provider Go SDKs inside each adapter.

```go
type ToolDef  struct { Name, Description string; Params JSONSchema }
type ToolCall struct { ID, Name string; Args json.RawMessage }
type Message  struct { Role Role; Text string; ToolCalls []ToolCall; ToolResults []ToolResult }
type Response struct { Text string; ToolCalls []ToolCall; Usage Usage; Stop StopReason }
```

Tools are defined once in this canonical form; adapters translate them to each
model's function-calling format. See [components/agent.md](components/agent.md) for
the tool taxonomy.

---

## Provider adapters

| Adapter | Covers | Status |
|---------|--------|--------|
| **Anthropic** | Claude models | v1 |
| **OpenAI-compatible** | OpenAI, plus local Ollama / vLLM / Together / etc. | v1 |
| Gemini | Google models | deferred |

One OpenAI-compatible adapter covers a large surface (anything speaking that API)
for little code. Each adapter translates canonical ↔ provider for: messages, tool
definitions, tool calls/results, usage, and streaming.

---

## Deterministically fakeable by construction

Because the agent is provider-unaware and the runner selects the adapter from config,
the model layer is **faked without touching production code**: point an
`openai-compat` model entry at a local fake endpoint that returns scripted completions
and the runner cannot tell it from Ollama or a hosted API. This is what lets the whole
`spec → implement → gate → merge` spine be driven **deterministically** in tests — no
hosted key, no nondeterminism — over the non-isolating [local backend](components/sandbox.md).

A deterministic fake validates the *machinery* — routing, the tool contract, gating,
merge, provenance — and nothing more. It cannot validate model *judgement*, which is
why autonomous self-hosting still awaits a capable runtime model (see
[bootstrap.md](bootstrap.md)).

---

## Constraints and the hard parts

- **Require tool-calling-capable models.** Models with weak/no function-calling
  don't fit; this is a documented constraint, not a brittle prompt-based fallback.
- **Tool-call format divergence** (Anthropic `tool_use`/`tool_result` vs. OpenAI
  `tool_calls`/`tool` role) is absorbed entirely by the adapters — the bulk of
  adapter code.
- **Usage normalization.** Providers report tokens differently; the canonical
  `Usage` normalizes it so the runner can enforce budgets uniformly.
- **Streaming is first-class.** Needed for the live "watch an agent think" view and
  the [wizard](control-room.md); adapters stream, and the runner fans tokens out to
  NATS → SSE. Build it into the interface from day one, not as a retrofit. A stream
  event carries either an assistant **text** delta or a **reasoning** delta
  (`StreamEvent.TextDelta` / `ReasoningDelta`) — a "thinking" model emits its chain
  of thought on a separate channel (Anthropic `thinking` blocks; the OpenAI-compatible
  `reasoning` / `reasoning_content` delta field local servers like Ollama use), so an
  agent whose visible turn is *only tool calls* is still observable as it reasons. The
  adapter normalizes both into the one canonical channel; the [broker](components/runner.md)
  labels them (`token` vs `reasoning`) for the feed.
- **Transient provider faults are absorbed at the relay.** A rate limit (429), a
  provider 5xx, or a mid-stream reset is a property of the *wire*, not of the
  invocation — but an error surfaced out of the completion call is fatal: the
  message is Nak'd and the whole invocation re-runs, discarding the sandbox and
  every token already spent. So the runner's relay — the layer that knows the
  provider — retries transient completion failures with **bounded backoff** before
  giving up. Terminal errors (auth, malformed request, context overflow) are never
  retried; a failed stream is re-issued as a fresh request, not resumed
  mid-stream. Retries stay inside the termination guarantee: every attempt's
  billed usage counts toward the invocation's budget and the sandbox wall clock
  keeps running, so a provider outage exhausts the budget and dead-letters rather
  than looping forever.
- **Optional capability fields** — prompt caching, reasoning effort — are **per-model
  config the adapter emits**, not canonical-`Request` fields. See the next section.

---

## Optional capability fields

Some provider features are pure cost/quality dials with no bearing on the loop's
logic — the agent should stay unaware of them. So they are **not** on the canonical
`Request` (which stays `{System, Messages, Tools, MaxTokens}`); they are **config on
the model registry entry**, threaded to the adapter by the runner and put on the wire
only by adapters that support them. An entry that omits a field runs at the provider
default; an adapter that does not understand one ignores it. This keeps the agent
provider-unaware while letting a deployment tune each model.

- **Reasoning effort** — `effort: low|medium|high|xhigh|max` on the model entry. The
  intelligence↔latency↔cost dial for a reasoning model: lower effort means fewer,
  more-consolidated tool calls and less deliberation, which is what bounds *turn
  count*, and so wall-clock, on a long agent loop. The Anthropic adapter maps it to
  `output_config.effort`. The OpenAI-compatible adapter also carries it, but that surface
  is heterogeneous — a gateway like OpenRouter takes a level-based effort control two
  different ways depending on the model — so a companion **`effort_param`** on the entry
  selects the wire form: `reasoning` sends the unified `reasoning: {effort}` object (OpenAI
  o-series, DeepSeek, Gemini-thinking, Claude pre-4.6); `verbosity` sends the top-level
  `verbosity` field, which is what Claude 4.6+/5 map to `output_config.effort` (there
  `reasoning.effort` is a silent no-op). `effort_param` is **required** whenever `effort` is
  set on `openai-compat` and rejected elsewhere; config validation rejects `effort` on
  providers with no equivalent (native `openai` is not yet wired), and a non-fatal advisory
  flags an Anthropic-family slug that picks `reasoning` (the no-op case).

- **Prompt caching** — the largest single cost saver on the agent loop, whose every
  turn re-sends the whole conversation: a stable prefix (persona + the Brief's ambient
  specs and spec) plus a history that grows only at the tail; without it each turn
  re-pays full input price for *everything*. A caching adapter therefore marks **two**
  breakpoints: the **Brief** (the stable prefix, a cache read from turn 2 on) and the
  **latest message** (the growing tail — each turn pays the cache-write premium only on
  the delta since the previous turn and reads everything before it at the cache-read
  rate, ~0.1×). Marking only the Brief would leave the accumulated tool results — which
  dominate a deep run's input by an order of magnitude — re-billed at full price every
  turn, so the tail breakpoint is the load-bearing one. The **Anthropic adapter caches
  by default** (system + first + latest message). The **OpenAI-compatible adapter caches
  opt-in per model** (`prompt_caching: true`), with the same two-breakpoint scheme,
  because that surface is mixed: OpenAI- and DeepSeek-style backends cache
  *automatically* (no marker needed) and a strict local server may reject an unknown
  field, so the markers are sent only where a backend both needs and accepts them — e.g.
  Anthropic models served through an OpenAI-compatible gateway, which forwards the
  markers and sticky-routes to keep the cache warm. Cache read/write token counts
  normalize into the canonical `Usage` so the runner prices them like any other tokens.
  One inherent limit: the provider cache has a short TTL (Anthropic: ~5 minutes on the
  default tier), so a single tool call that runs longer than that — a cold `run_gate`
  self-check, a slow suite under `run_tests` — forfeits the cache for the next turn no
  matter where the breakpoints sit. That is a provider property, not an adapter bug;
  a first-party deployment can buy the extended TTL where it matters.

---

## Per-role model tiers

Different roles want different model strength. Decomposition, test authoring, and
implementation are hard reasoning and want a frontier model; a role whose output is
**independently re-graded** can run a cheaper one, because a weak result is caught
downstream rather than trusted. There is **no separate "tier" type** — a tier is just
which model a [soul](components/agent.md) names. A role maps to a set of souls, the
orchestrator picks one per issue by selector (see [configuration.md](configuration.md)),
and the chosen soul's `model` is what the runner resolves; the model is therefore
**resolved per issue**, not globally, and is recorded in the provenance trailer so the
tier a change ran under is auditable.

The safest place to economize is a role guarded by **producer ≠ verifier**: the kernel's
`security`/`qa` soul runs a mid-tier model because every byte it produces is re-run by the
independent gate in a clean sandbox (scanners + mutation + the red→green proof), so a
mistake costs a rejected candidate, not a bad merge. Which model serves which role is
config policy, tunable per deployment; the bootstrap commits frontier for
planner/test-author/implementor and mid-tier for security.

## Helper souls — two models in one sandbox

Most invocations resolve exactly one model: the soul's. The [`explore`
tool](components/agent.md#explore--distilled-comprehension) is the exception — it runs a
nested read-only sub-loop on a **cheap** model while the parent runs on its (frontier) one,
so a single sandbox drives **two** trusted-pinned model identities at once. This stays
inside the existing seam with two small additions:

- The canonical model request the sandbox emits carries a **sub-context selector**, so the
  runner knows which of the invocation's models a given call belongs to and routes it to the
  right adapter (the parent's, or the explorer's). See [messaging.md](messaging.md).
- The selector is **pinned by the trusted dispatch and enforced by the runner** — the agent
  names the *tool*, never the *model*. The runner resolves the explorer soul's `model` from
  config exactly as it resolves the parent's, and refuses any other. This keeps *the model is
  resolved by the trusted layer* intact even with two models in one sandbox; both are
  recorded in the provenance trailer.

The runner meters the explorer's stream against its own **fixed sub-budget**
(`policy.explore_budget`, see [configuration.md](configuration.md)) under the parent task's
ceiling — the same choke point where it already enforces [budgets](workflow.md) and emits
[spans](observability.md), so no new accounting path. The explorer is the archetypal cheap
tier: read-only and independently discardable, its output never trusted verbatim (it ships
*checkable* anchors), so a weak model there costs at worst a re-search — the same
producer≠verifier-style economy that lets the qa soul run mid-tier.

## OPEN questions

*(none — the registry config shape and per-role tiers above are realized.)*
