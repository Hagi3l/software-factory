# Selecting your LLM provider / subscription

You can run the factory with:

1. **Native Grok OAuth** (`software-factory login`) — SuperGrok / X Premium+ subscription
2. **Claude subscription via local proxy** (`software-factory login claude --proxy …`)
3. **Classic API keys** (Anthropic or xAI pay-per-token)
4. **Any OpenAI-compatible endpoint** (Ollama, OpenRouter, etc.)

API keys and OAuth tokens are **never written to config**. The runner injects them;
sandboxed agents never see them.

---

## Easy commands

```bash
software-factory login              # Grok / SuperGrok / X Premium+ (device-code)
software-factory login claude --proxy http://127.0.0.1:PORT/v1
software-factory logout [grok|claude|all]
software-factory auth status
```

Tokens and proxy metadata are stored at `~/.software-factory/auth.json` (mode 0600).

Both Grok and Claude can be logged in at once — useful for **N-version diversity**
(e.g. Grok implementor + Claude verifier).

---

## 1. Grok / SuperGrok / X Premium+ (native OAuth)

```bash
software-factory login              # or: login grok
# approve in the browser
software-factory auth status
```

1. Point souls at `grok-4.5` / `grok-4-fast` (edit `config/souls/*.yaml` and
   `requirements_planner.model` in `factory.yaml` if you want the wizard on Grok too).
2. Leave `OPENAI_API_KEY` / `XAI_API_KEY` **unset** so the registry uses the OAuth Bearer.
3. Shipped `config/infra.dev.yaml` defaults Grok models to
   `https://cli-chat-proxy.grok.com/v1` (subscription-friendly). For **pay-per-token**
   API keys, switch the endpoint back to `https://api.x.ai/v1` and set a key.

```yaml
grok-4.5:
  provider: openai-compat
  endpoint: https://cli-chat-proxy.grok.com/v1
  family: xai
```

Credential order for Grok models: `OPENAI_API_KEY` → `XAI_API_KEY` → OAuth access token
(refreshed automatically on use).

---

## 2. Claude Pro / Max (subscription proxy)

Anthropic does not offer a public third-party device-code flow for Claude Pro/Max.
Use a **local proxy** that authenticates with your subscription (Claude Code CLI
bridges, Hermes-style tools, etc.), then register it:

```bash
# proxy must already be running and speaking OpenAI-compat or Anthropic Messages
software-factory login claude --proxy http://127.0.0.1:8585/v1
# optional bearer if the proxy requires one:
software-factory login claude --proxy http://127.0.0.1:8585/v1 --token SECRET
# native Anthropic wire format instead of openai-compat:
software-factory login claude --proxy http://127.0.0.1:8585 --mode anthropic
```

Add (or uncomment) a model entry that hits that endpoint:

```yaml
claude-opus-sub:
  provider: openai-compat
  endpoint: http://127.0.0.1:8585/v1   # same URL as login --proxy
  family: anthropic
  cost: { input_per_mtok: 5, output_per_mtok: 25 }
```

Point the souls you want on Claude at `claude-opus-sub` (or keep default
`claude-opus-4-8` with `ANTHROPIC_API_KEY` for pay-per-token).

---

## 3. Classic API keys

### Anthropic
```bash
export ANTHROPIC_API_KEY=sk-ant-...
# souls use claude-opus-4-8 / claude-sonnet-4-6
```

### Grok / xAI (pay-per-token)
```bash
export OPENAI_API_KEY=xai-...          # or XAI_API_KEY
# souls use grok-4.5 / grok-4-fast
# set endpoint to https://api.x.ai/v1 in infra if you changed it for subscription
```

Environment keys **always override** the login store.

---

## 4. Mixing providers (recommended)

Register both Claude and Grok models and assign different souls different models
(e.g. Grok for implementation, Claude for independent verification). That gives
genuine N-version diversity and silences the same-family advisory from
`software-factory validate`.

---

## Costs & budgets

Keep the `cost:` blocks in `infra.*.yaml` accurate so the factory’s dollar-budget
termination still works. Update them from the current vendor pricing pages.
Subscription paths often bill against a monthly quota rather than per-token; cost
blocks still bound worst-case accounting inside the factory.
