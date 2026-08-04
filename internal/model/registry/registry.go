// Package registry resolves a soul's declared model name to a concrete
// model.Adapter. It is the runner-side mapping described in specs/models.md and
// specs/configuration.md: the infra model registry (model name → provider, plus an
// endpoint for OpenAI-compatible backends) is turned into one ready-to-call adapter
// per entry, with API keys read from the environment or from software-factory login
// credentials — never from config.
//
// Credential priority (first non-empty wins):
//
//	openai-compat (xAI / Grok family or known xAI hosts):
//	  OPENAI_API_KEY → XAI_API_KEY → auth.AccessToken() (OAuth, refresh on request)
//	openai-compat (other, including Claude subscription proxies):
//	  OPENAI_API_KEY → Claude store bearer if endpoint matches registered proxy
//	anthropic:
//	  ANTHROPIC_API_KEY → Claude store bearer; optional base URL from Claude proxy
//	  when provider_mode is anthropic
//	openai (native):
//	  OPENAI_API_KEY
//
// OAuth-sourced keys are injected per HTTP request via SDK middleware so long-running
// `software-factory run` processes refresh tokens instead of baking an expired Bearer
// at construction time.
package registry

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Loxstomper/software-factory/internal/auth"
	"github.com/Loxstomper/software-factory/internal/config"
	"github.com/Loxstomper/software-factory/internal/model"
	"github.com/Loxstomper/software-factory/internal/model/anthropic"
	"github.com/Loxstomper/software-factory/internal/model/openai"

	anthropicopt "github.com/anthropics/anthropic-sdk-go/option"
	openaiopt "github.com/openai/openai-go/v3/option"
)

// Environment variables the registry reads API keys from. Keys live only in the
// runner's environment or the auth store, never in config (specs/models.md,
// specs/security.md). A key is passed to the adapter only when present, so keyless
// local backends (Ollama, vLLM) still resolve.
const (
	EnvAnthropicKey = "ANTHROPIC_API_KEY"
	EnvOpenAIKey    = "OPENAI_API_KEY"
	EnvXAIKey       = "XAI_API_KEY"
	// EnvGrokClientVersion overrides the x-grok-client-version header sent to
	// cli-chat-proxy.grok.com (subscription OAuth). The proxy rejects missing/old
	// versions with HTTP 426.
	EnvGrokClientVersion = "SOFTWARE_FACTORY_GROK_CLIENT_VERSION"

	// DefaultGrokClientVersion is a floor accepted by cli-chat-proxy (min 0.1.202).
	// Bump when the proxy raises its floor; override with SOFTWARE_FACTORY_GROK_CLIENT_VERSION.
	DefaultGrokClientVersion = "0.2.118"
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
// adapter and injecting credentials (env first, then software-factory login store).
func build(name string, mp config.ModelProvider) (model.Adapter, error) {
	switch mp.Provider {
	case config.ProviderAnthropic:
		var opts []anthropicopt.RequestOption
		if key := os.Getenv(EnvAnthropicKey); key != "" {
			opts = append(opts, anthropicopt.WithAPIKey(key))
		} else if c, err := auth.ClaudeCredentials(); err != nil {
			return nil, err
		} else if c != nil {
			if c.ProviderMode == auth.ClaudeModeAnthropic && c.Endpoint != "" {
				opts = append(opts, anthropicopt.WithBaseURL(c.Endpoint))
			}
			if c.AccessToken != "" {
				// Refresh-safe if token is ever rotated in-store mid-run.
				tok := c.AccessToken
				opts = append(opts, anthropicopt.WithMiddleware(func(req *http.Request, next anthropicopt.MiddlewareNext) (*http.Response, error) {
					// Re-read store so a re-login mid-process is picked up.
					if cur, err := auth.ClaudeCredentials(); err == nil && cur != nil && cur.AccessToken != "" {
						tok = cur.AccessToken
					}
					req.Header.Set("x-api-key", tok)
					req.Header.Set("Authorization", "Bearer "+tok)
					return next(req)
				}))
			}
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
		return openai.New(name, opts...).WithIdleTimeout(time.Duration(mp.IdleTimeout)), nil

	case config.ProviderOpenAICompat:
		if mp.Endpoint == "" {
			return nil, fmt.Errorf("registry: model %q uses provider %s but has no endpoint", name, config.ProviderOpenAICompat)
		}
		opts := []openaiopt.RequestOption{openaiopt.WithBaseURL(mp.Endpoint)}
		if err := appendOpenAICompatAuth(&opts, name, mp); err != nil {
			return nil, err
		}
		// prompt_caching and effort are honored only on this openai-compat adapter (config.Validate
		// restricts them here); WithPromptCaching(false) and WithEffort("", "") are no-ops, so both
		// are safe to chain whether or not the entry opts in. WithEffort's param (validated to
		// reasoning|verbosity) selects the wire form the effort level rides on.
		return openai.New(name, opts...).
			WithPromptCaching(mp.PromptCaching).
			WithEffort(mp.Effort, mp.EffortParam).
			WithIdleTimeout(time.Duration(mp.IdleTimeout)), nil

	default:
		return nil, fmt.Errorf("registry: model %q has unknown provider %q", name, mp.Provider)
	}
}

// appendOpenAICompatAuth injects credentials for an openai-compat model.
// Static env keys use WithAPIKey; OAuth / store tokens use per-request middleware
// so refresh works across a long run.
func appendOpenAICompatAuth(opts *[]openaiopt.RequestOption, name string, mp config.ModelProvider) error {
	// cli-chat-proxy.grok.com requires Grok CLI identity headers (426 without them).
	if needsGrokCLIClientHeaders(mp) {
		*opts = append(*opts, openaiopt.WithMiddleware(grokCLIClientHeadersMiddleware))
	}

	if key := os.Getenv(EnvOpenAIKey); key != "" {
		*opts = append(*opts, openaiopt.WithAPIKey(key))
		return nil
	}
	if isXAIModel(name, mp) {
		if key := os.Getenv(EnvXAIKey); key != "" {
			*opts = append(*opts, openaiopt.WithAPIKey(key))
			return nil
		}
		// OAuth: inject Bearer on every request (refresh via auth.AccessToken).
		*opts = append(*opts, openaiopt.WithMiddleware(func(req *http.Request, next openaiopt.MiddlewareNext) (*http.Response, error) {
			tok, err := auth.AccessToken()
			if err != nil {
				return nil, err
			}
			if tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
			return next(req)
		}))
		return nil
	}
	// Claude subscription proxy: match registered endpoint, optional bearer.
	c, err := auth.ClaudeCredentials()
	if err != nil {
		return err
	}
	if c != nil && endpointsMatch(mp.Endpoint, c.Endpoint) {
		if c.AccessToken != "" {
			*opts = append(*opts, openaiopt.WithMiddleware(func(req *http.Request, next openaiopt.MiddlewareNext) (*http.Response, error) {
				tok := c.AccessToken
				if cur, err := auth.ClaudeCredentials(); err == nil && cur != nil && cur.AccessToken != "" {
					tok = cur.AccessToken
				}
				if tok != "" {
					req.Header.Set("Authorization", "Bearer "+tok)
				}
				return next(req)
			}))
		}
		// Keyless local proxies: no Authorization header.
		return nil
	}
	return nil
}

// isXAIModel reports whether this registry entry should use Grok/xAI OAuth credentials.
func isXAIModel(name string, mp config.ModelProvider) bool {
	if fam := strings.ToLower(strings.TrimSpace(mp.Family)); fam == "xai" || fam == "grok" {
		return true
	}
	// Explicit family wins in ModelFamily; here we also check endpoint host.
	if hostContains(mp.Endpoint, "api.x.ai") || hostContains(mp.Endpoint, "cli-chat-proxy.grok.com") || hostContains(mp.Endpoint, "x.ai") {
		return true
	}
	// Bare model names used in the shipped config.
	n := strings.ToLower(name)
	return strings.HasPrefix(n, "grok-") || strings.HasPrefix(n, "xai/")
}

func hostContains(endpoint, needle string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return strings.Contains(strings.ToLower(endpoint), needle)
	}
	return strings.Contains(strings.ToLower(u.Host), needle)
}

func endpointsMatch(a, b string) bool {
	na := strings.TrimRight(strings.TrimSpace(a), "/")
	nb := strings.TrimRight(strings.TrimSpace(b), "/")
	return strings.EqualFold(na, nb)
}

// needsGrokCLIClientHeaders is true for the subscription chat proxy that enforces
// official-client identity (see 426 "Grok CLI version (none) is outdated").
func needsGrokCLIClientHeaders(mp config.ModelProvider) bool {
	return hostContains(mp.Endpoint, "cli-chat-proxy.grok.com")
}

func grokClientVersion() string {
	if v := strings.TrimSpace(os.Getenv(EnvGrokClientVersion)); v != "" {
		return v
	}
	return DefaultGrokClientVersion
}

// grokCLIClientHeadersMiddleware stamps the identity headers the subscription
// proxy expects (same set used by the official Grok Build / grok-shell client).
func grokCLIClientHeadersMiddleware(req *http.Request, next openaiopt.MiddlewareNext) (*http.Response, error) {
	req.Header.Set("User-Agent", "xai-grok-cli")
	req.Header.Set("x-grok-client-identifier", "grok-shell")
	req.Header.Set("x-grok-client-version", grokClientVersion())
	return next(req)
}
