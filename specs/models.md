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
- **Optional capabilities** — prompt caching (a large cost saver on long agent
  loops), extended thinking / reasoning effort — are exposed as optional request
  fields that capable adapters honor and others ignore.

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

## OPEN questions

*(none — the registry config shape and per-role tiers above are realized.)*
