// Package registry resolves a soul's declared model name to a concrete
// model.Adapter. It is the runner-side mapping described in specs/models.md and
// specs/configuration.md: the infra model registry (model name → provider, plus an
// endpoint for OpenAI-compatible backends) is turned into one ready-to-call adapter
// per entry, with API keys read from the environment — never from config.
//
// The registry lives in its own package (not internal/model) because it constructs
// the concrete anthropic/openai adapters, which themselves import internal/model;
// placing it here keeps the canonical model package dependency-free and avoids an
// import cycle. The runner holds a Registry and calls Adapter(soul.Model) per
// invocation.
package registry

import (
	"fmt"
	"os"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/model"
	"github.com/Loxstomper/harness/internal/model/anthropic"
	"github.com/Loxstomper/harness/internal/model/openai"

	anthropicopt "github.com/anthropics/anthropic-sdk-go/option"
	openaiopt "github.com/openai/openai-go/v3/option"
)

// Environment variables the registry reads API keys from. Keys live only in the
// runner's environment, never in config (specs/models.md, specs/security.md). A key
// is passed to the adapter only when present, so keyless local backends (Ollama,
// vLLM) still resolve.
const (
	EnvAnthropicKey = "ANTHROPIC_API_KEY"
	EnvOpenAIKey    = "OPENAI_API_KEY"
)

// Registry maps model names to pre-built adapters. Adapters are constructed eagerly
// at New (fail loud on an unknown provider, consistent with config validation being
// a startup gate) and reused across invocations; construction makes no network call.
type Registry struct {
	adapters map[string]model.Adapter
}

// New builds one adapter per registry entry. It fails on an unknown provider or an
// openai-compat entry missing its endpoint — defense-in-depth against a config that
// skipped validation (see config.Validate). An empty map yields a registry that
// resolves nothing; Adapter then reports the unregistered model.
func New(models map[string]config.ModelProvider) (*Registry, error) {
	adapters := make(map[string]model.Adapter, len(models))
	for name, mp := range models {
		a, err := build(name, mp)
		if err != nil {
			return nil, err
		}
		adapters[name] = a
	}
	return &Registry{adapters: adapters}, nil
}

// Adapter returns the adapter for a model name (i.e. a soul's Model). An
// unregistered name is an error — the same condition config.Validate catches before
// startup, re-checked here so a runtime dispatch never silently no-ops.
func (r *Registry) Adapter(modelName string) (model.Adapter, error) {
	a, ok := r.adapters[modelName]
	if !ok {
		return nil, fmt.Errorf("registry: no adapter registered for model %q", modelName)
	}
	return a, nil
}

// build constructs the adapter for one registry entry, selecting the provider
// adapter and injecting the API key from the environment (and, for openai-compat,
// the base URL). The two SDKs have distinct option packages, so each branch builds
// its own option slice.
func build(name string, mp config.ModelProvider) (model.Adapter, error) {
	switch mp.Provider {
	case config.ProviderAnthropic:
		var opts []anthropicopt.RequestOption
		if key := os.Getenv(EnvAnthropicKey); key != "" {
			opts = append(opts, anthropicopt.WithAPIKey(key))
		}
		// Effort (output_config.effort) is honored only on the native Anthropic adapter;
		// config.Validate guarantees it is unset for the other providers. WithEffort("") is
		// a no-op, so this is safe whether or not the entry sets it.
		return anthropic.New(name, opts...).WithEffort(mp.Effort), nil

	case config.ProviderOpenAI:
		var opts []openaiopt.RequestOption
		if key := os.Getenv(EnvOpenAIKey); key != "" {
			opts = append(opts, openaiopt.WithAPIKey(key))
		}
		return openai.New(name, opts...), nil

	case config.ProviderOpenAICompat:
		if mp.Endpoint == "" {
			return nil, fmt.Errorf("registry: model %q uses provider %s but has no endpoint", name, config.ProviderOpenAICompat)
		}
		opts := []openaiopt.RequestOption{openaiopt.WithBaseURL(mp.Endpoint)}
		if key := os.Getenv(EnvOpenAIKey); key != "" {
			opts = append(opts, openaiopt.WithAPIKey(key))
		}
		// prompt_caching is honored only on this openai-compat adapter (config.Validate
		// restricts the flag here); WithPromptCaching(false) is a no-op, so it is safe to chain
		// whether or not the entry opts in.
		return openai.New(name, opts...).WithPromptCaching(mp.PromptCaching), nil

	default:
		return nil, fmt.Errorf("registry: model %q has unknown provider %q", name, mp.Provider)
	}
}
