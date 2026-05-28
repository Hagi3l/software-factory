package registry_test

import (
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/model/anthropic"
	"github.com/Loxstomper/harness/internal/model/openai"
	"github.com/Loxstomper/harness/internal/model/registry"
)

// TestNewResolvesProviderToAdapterType is the core of the registry: each provider
// string must build the matching concrete adapter. We assert the dynamic type
// because endpoint/key plumbing is opaque inside the SDK client and already covered
// by each adapter's own integration tests — here we only prove the routing.
func TestNewResolvesProviderToAdapterType(t *testing.T) {
	reg, err := registry.New(map[string]config.ModelProvider{
		"claude-opus-4-7": {Provider: config.ProviderAnthropic},
		"gpt-4o":          {Provider: config.ProviderOpenAI},
		"llama-3.3-70b":   {Provider: config.ProviderOpenAICompat, Endpoint: "http://ollama:11434/v1"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	a, err := reg.Adapter("claude-opus-4-7")
	if err != nil {
		t.Fatalf("Adapter(anthropic): %v", err)
	}
	if _, ok := a.(*anthropic.Adapter); !ok {
		t.Errorf("anthropic model resolved to %T, want *anthropic.Adapter", a)
	}

	o, err := reg.Adapter("gpt-4o")
	if err != nil {
		t.Fatalf("Adapter(openai): %v", err)
	}
	if _, ok := o.(*openai.Adapter); !ok {
		t.Errorf("openai model resolved to %T, want *openai.Adapter", o)
	}

	c, err := reg.Adapter("llama-3.3-70b")
	if err != nil {
		t.Fatalf("Adapter(openai-compat): %v", err)
	}
	if _, ok := c.(*openai.Adapter); !ok {
		t.Errorf("openai-compat model resolved to %T, want *openai.Adapter", c)
	}
}

func TestAdapterUnknownModel(t *testing.T) {
	reg, err := registry.New(map[string]config.ModelProvider{
		"gpt-4o": {Provider: config.ProviderOpenAI},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := reg.Adapter("does-not-exist"); err == nil {
		t.Fatal("Adapter for unregistered model: want error, got nil")
	}
}

func TestNewUnknownProvider(t *testing.T) {
	_, err := registry.New(map[string]config.ModelProvider{
		"weird": {Provider: "antropic"}, // typo
	})
	if err == nil {
		t.Fatal("New with unknown provider: want error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("error = %q, want it to mention unknown provider", err)
	}
}

func TestNewOpenAICompatRequiresEndpoint(t *testing.T) {
	_, err := registry.New(map[string]config.ModelProvider{
		"local": {Provider: config.ProviderOpenAICompat}, // no endpoint
	})
	if err == nil {
		t.Fatal("New with endpoint-less openai-compat: want error, got nil")
	}
	if !strings.Contains(err.Error(), "endpoint") {
		t.Errorf("error = %q, want it to mention the missing endpoint", err)
	}
}

func TestNewEmptyRegistryResolvesNothing(t *testing.T) {
	reg, err := registry.New(nil)
	if err != nil {
		t.Fatalf("New(nil): %v", err)
	}
	if _, err := reg.Adapter("anything"); err == nil {
		t.Fatal("Adapter on empty registry: want error, got nil")
	}
}
