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
  NATS → SSE. Build it into the interface from day one, not as a retrofit.
- **Optional capabilities** — prompt caching (a large cost saver on long agent
  loops), extended thinking / reasoning effort — are exposed as optional request
  fields that capable adapters honor and others ignore.

---

## OPEN questions

- The model-registry config shape (name → provider + endpoint; keys from env, never
  in config) — see [configuration.md](configuration.md).
- Per-role model defaults / cost tiers (cheap model for some roles, frontier for
  hard reasoning) — config policy, TBD.
