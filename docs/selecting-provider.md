# Selecting your LLM provider / subscription

You can run the factory with:

1. **Classic API keys** (Anthropic or xAI pay-per-token)
2. **Monthly subscription via local proxy** (no API keys — recommended quick path)
3. **Native Grok OAuth** (coming: `software-factory login` — uses your SuperGrok / X Premium+ subscription directly)

## Easy commands (native Grok OAuth)

```bash
software-factory login          # device-code flow for Grok / SuperGrok / X Premium+
software-factory logout
software-factory auth status
```

Tokens are stored at `~/.software-factory/auth.json` (mode 0600). Once logged in, set souls to Grok models and run — no API key env vars needed.

---

## 1. Monthly subscription via local proxy (works today, no API keys)

This is the fastest way to use your existing monthly plan without managing keys.

### Grok / SuperGrok / X Premium+

Use a local OpenAI-compatible proxy that authenticates with your subscription via OAuth (examples: grok-proxy, Hermes-derived tools, OpenClaw-style bridges).

1. Install and run the proxy (one-time login in browser or device-code).
2. It exposes something like `http://127.0.0.1:8585/v1`.
3. In `config/infra.dev.yaml` point the Grok models at it:

```yaml
grok-4.5:
  provider: openai-compat
  endpoint: http://127.0.0.1:8585/v1   # your local subscription proxy
  family: xai
  cost: { input_per_mtok: 0.20, output_per_mtok: 0.50 }
```

4. Update soul `model:` fields to `grok-4.5` / `grok-4-fast`.
5. No `OPENAI_API_KEY` or `XAI_API_KEY` required.

Usage draws from your monthly subscription quota.

### Claude Pro / Max

Use a community proxy that wraps the Claude Code CLI (logged in with your Pro/Max subscription). Examples include claude-code-proxy style tools that expose a local OpenAI-compatible or Anthropic endpoint.

Point the factory’s models (or a custom openai-compat / anthropic entry) at that localhost endpoint the same way. No Anthropic API key needed.

---

## 2. Classic API keys

### Anthropic
```bash
export ANTHROPIC_API_KEY=sk-ant-...
# souls use claude-opus-4-8 / claude-sonnet-4-6
```

### Grok / xAI (pay-per-token)
```bash
export OPENAI_API_KEY=xai-...          # or XAI_API_KEY
# souls use grok-4.5 / grok-4-fast (endpoint already set to https://api.x.ai/v1)
```

Keys are never written to config files — the runner injects them.

---

## 3. Mixing providers

You can register both Claude and Grok models at the same time and assign different souls different models (e.g. Grok for implementation, Claude for independent verification). This gives genuine N-version diversity.

---

## Costs & budgets

Keep the `cost:` blocks in `infra.*.yaml` accurate so the factory’s dollar-budget termination still works correctly. Update them from the current vendor pricing pages.

---

## Status of native OAuth

Native `software-factory login` (device-code against auth.x.ai) is being implemented. Once landed you will only need:

```bash
software-factory login
# edit souls to grok models if not already
./bin/software-factory validate --config config
```

No proxy process and no API keys required for Grok.
