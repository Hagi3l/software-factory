# Selecting your LLM provider / subscription (Anthropic or Grok)

The software-factory supports both Anthropic (Claude) and any OpenAI-compatible endpoint, including xAI Grok.

## Quick switch

### Anthropic (default in shipped config)
1. Export your key: `export ANTHROPIC_API_KEY=sk-ant-...`
2. Keep (or set) soul `model:` fields to `claude-opus-4-8` (frontier roles) or `claude-sonnet-4-6` (QA).
3. Run as usual.

### Grok / xAI
1. Get an API key from https://console.x.ai (API access is separate from chat subscriptions).
2. Export the key. Either:
   - `export OPENAI_API_KEY=xai-...`  (works out of the box)
   - or `export XAI_API_KEY=xai-...` (after the registry enhancement)
3. In `config/infra.dev.yaml` the Grok models are already defined with `provider: openai-compat` and `endpoint: https://api.x.ai/v1`.
4. Update the soul files under `config/souls/`:
   - Change `model: claude-opus-4-8` → `model: grok-4.5` (planner, test-author, implementor)
   - Change `model: claude-sonnet-4-6` → `model: grok-4-fast` (security/QA)
5. Validate: `./bin/software-factory validate --config config`
6. Run.

You can mix providers (e.g. Grok for implementor, Claude for verifier) for N-version diversity.

## Registry key lookup
The runner reads:
- `ANTHROPIC_API_KEY` for anthropic models
- `OPENAI_API_KEY` (and optionally `XAI_API_KEY`) for openai / openai-compat models

Keys are never stored in config files.

## Costs
Update the `cost:` blocks in the models registry to match current vendor pricing so the dollar budget enforcement is accurate.
