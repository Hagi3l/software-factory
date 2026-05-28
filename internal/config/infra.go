package config

import (
	"fmt"
	"os"
)

// Infra is infra.<env>.yaml: the environment-specific overlay — sandbox backend,
// NATS endpoint, broker egress allowlist, artifact store, telemetry, and the model
// registry. A dev overlay swaps Firecracker for Docker without touching the
// workflow or souls. See specs/configuration.md.
type Infra struct {
	Sandbox   SandboxConfig            `yaml:"sandbox"`
	NATS      NATSConfig               `yaml:"nats"`
	Broker    BrokerConfig             `yaml:"broker"`
	Artifacts ArtifactsConfig          `yaml:"artifacts"`
	OTel      OTelConfig               `yaml:"otel"`
	Models    map[string]ModelProvider `yaml:"models"`
}

// SandboxConfig selects the isolation backend and its resource ceiling. Egress is
// "broker-only" in every supported profile — the zero-network invariant (see
// specs/security.md, specs/components/sandbox.md).
type SandboxConfig struct {
	Backend string        `yaml:"backend"` // "firecracker" | "docker" | "gvisor"
	Egress  string        `yaml:"egress"`  // "broker-only"
	Limits  SandboxLimits `yaml:"limits"`
}

// SandboxLimits is the per-sandbox resource ceiling enforced by the backend.
type SandboxLimits struct {
	CPU  int      `yaml:"cpu"`            // CPU cores
	Mem  string   `yaml:"mem"`            // memory as a k8s-style quantity, e.g. "2Gi"; parsed by the backend
	Disk string   `yaml:"disk,omitempty"` // disk as a k8s-style quantity, e.g. "8Gi"; parsed by the backend; optional
	Wall Duration `yaml:"wall"`           // wall-clock ceiling for an invocation
}

// NATSConfig points runners and the orchestrator at the NATS endpoint. JetStream
// stream definitions themselves are bootstrap defaults owned by the messaging
// package; only the env-varying knobs are surfaced here.
type NATSConfig struct {
	URL       string          `yaml:"url"`
	JetStream JetStreamConfig `yaml:"jetstream"`
}

// JetStreamConfig carries the environment-specific JetStream knobs. Concrete stream
// definitions (subjects, retention) live in the messaging package; retention,
// replicas, and max-age are marked OPEN in specs/messaging.md, so only the two
// knobs that genuinely vary by environment are surfaced here — dev runs a single
// short-lived replica, a distributed cluster runs more. Both are optional.
type JetStreamConfig struct {
	Replicas int      `yaml:"replicas,omitempty"`
	MaxAge   Duration `yaml:"max_age,omitempty"`
}

// BrokerConfig is the egress allowlist the runner enforces on behalf of the
// sandbox. Deny-by-default: only named destinations are reachable, and only via the
// broker (see specs/components/runner.md, specs/security.md).
type BrokerConfig struct {
	Allowlist []string `yaml:"allowlist"` // e.g. [llm-api, nats, package-mirror, git]
}

// ArtifactsConfig selects the content-addressed artifact store backend.
type ArtifactsConfig struct {
	Backend string `yaml:"backend"`        // "files" (dev) | "s3" (distributed)
	Path    string `yaml:"path,omitempty"` // filesystem root for the files backend
}

// OTelConfig configures trace/metric export. An empty Endpoint disables export.
type OTelConfig struct {
	Endpoint string `yaml:"endpoint,omitempty"`
}

// ModelProvider maps a model name (declared by core.Soul.Model) to its provider
// adapter and, for OpenAI-compatible backends, an endpoint. API keys are NEVER in
// config — the runner injects them from the environment (see specs/models.md).
type ModelProvider struct {
	Provider string `yaml:"provider"`           // "anthropic" | "openai" | "openai-compat"
	Endpoint string `yaml:"endpoint,omitempty"` // base URL for openai-compat backends (Ollama/vLLM)
}

// LoadInfra reads and unmarshals an infra.<env>.yaml overlay. It parses strictly
// (unknown keys are errors). A missing file, malformed YAML, or unknown key fails
// here; cross-references (e.g. a soul.Model with no registry entry) are validate's
// job.
func LoadInfra(path string) (*Infra, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read infra file %s: %w", path, err)
	}
	var in Infra
	if err := unmarshalStrict(data, &in); err != nil {
		return nil, fmt.Errorf("config: parse infra file %s: %w", path, err)
	}
	return &in, nil
}
