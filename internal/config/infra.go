package config

import (
	"fmt"
	"os"

	"github.com/Loxstomper/harness/internal/core"
)

// Infra is infra.<env>.yaml: the environment-specific overlay — sandbox backend,
// NATS endpoint, broker egress allowlist, artifact store, telemetry, and the model
// registry. A dev overlay swaps Firecracker for Docker without touching the
// workflow or souls. See specs/configuration.md.
type Infra struct {
	Sandbox   SandboxConfig            `yaml:"sandbox"`
	NATS      NATSConfig               `yaml:"nats"`
	Broker    BrokerConfig             `yaml:"broker"`
	Git       GitConfig                `yaml:"git,omitempty"`
	Artifacts ArtifactsConfig          `yaml:"artifacts"`
	OTel      OTelConfig               `yaml:"otel"`
	Signing   SigningConfig            `yaml:"signing,omitempty"`
	Models    map[string]ModelProvider `yaml:"models"`
}

// SandboxConfig selects the isolation backend and its resource ceiling. Egress is
// "broker-only" in every supported profile — the zero-network invariant (see
// specs/security.md, specs/components/sandbox.md).
type SandboxConfig struct {
	Backend  string                    `yaml:"backend"` // "firecracker" | "docker" | "gvisor"
	Egress   string                    `yaml:"egress"`  // "broker-only"
	Limits   SandboxLimits             `yaml:"limits"`
	Profiles map[string]SandboxProfile `yaml:"profiles,omitempty"` // logical soul.sandbox name -> concrete artifact
}

// Backend identifiers for SandboxConfig.Backend. Docker and gVisor boot a container
// image; Firecracker boots a rootfs — which is why ResolveImage picks a different
// SandboxProfile field per backend. See specs/components/sandbox.md.
const (
	BackendDocker      = "docker"
	BackendGVisor      = "gvisor"
	BackendFirecracker = "firecracker"
)

// SandboxProfile resolves the logical profile a soul names (core.Soul.Sandbox, e.g.
// "go-toolchain") to the concrete, backend-specific bootable artifact: a
// (digest-pinned) container image for the docker/gvisor backends, a rootfs for
// firecracker. An overlay is written for one backend, so exactly one field is
// populated, matching SandboxConfig.Backend. This is the sandbox analog of the
// ModelProvider registry — it keeps a soul backend- and environment-agnostic so the
// same "go-toolchain" name resolves to a local docker tag in dev and a pinned rootfs
// in prod with no soul edit. See specs/components/sandbox.md, specs/configuration.md.
type SandboxProfile struct {
	Image  string `yaml:"image,omitempty"`  // docker/gvisor: container image ref (ideally @sha256-pinned)
	Rootfs string `yaml:"rootfs,omitempty"` // firecracker: rootfs image path
}

// ResolveImage maps a logical profile name to the concrete artifact the active backend
// boots — the image (docker/gvisor) or rootfs (firecracker) of the matching profile
// entry. It is total: a profile not in the registry, or one missing the active
// backend's field, falls back to the profile name itself — the historical "name ==
// image tag" behavior, and what keeps the test-only and unconfigured paths working.
// Registry completeness (so production pins a real digest rather than degrading to the
// bare name) is enforced loudly at startup by Validate, not here.
func (sc SandboxConfig) ResolveImage(profile string) string {
	p, ok := sc.Profiles[profile]
	if !ok {
		return profile
	}
	artifact := p.Image
	if sc.Backend == BackendFirecracker {
		artifact = p.Rootfs
	}
	if artifact == "" {
		return profile
	}
	return artifact
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
	Allowlist []string `yaml:"allowlist"` // e.g. [llm-api, nats, package-proxy, git]
	// PackageProxy is the base URL of the Go module proxy package fetches are routed to
	// (T5.6). Empty resolves to the public default proxy.golang.org via PackageProxyURL:
	// the spec's supply-chain guarantees (go.sum + checksum-DB pinning, broker logging,
	// post-fetch gate scanning) hold against the public proxy, so a private vetted mirror
	// is an optional swap, not the default. Only consulted when DestPackageProxy is in the
	// allowlist (otherwise package fetch is denied). See specs/security.md Control 2.
	PackageProxy string `yaml:"package_proxy,omitempty"`
}

// DestPackageProxy is the egress-allowlist token (and broker destination) for package
// fetches — the value an operator lists in broker.allowlist to permit dependency pulls.
// It is the single source of truth shared by config and the broker's Method.destination;
// the other destination tokens (llm-api, nats, git) are likewise plain strings the broker
// owns. See specs/components/runner.md.
const DestPackageProxy = "package-proxy"

// defaultPackageProxy is the public Go module proxy used when broker.package_proxy is
// unset. The public proxy is the deliberate default (security.md Control 2): pinning by
// go.sum + the public checksum DB, logging at the broker, and post-fetch scanning at the
// qa gate already cover what a private mirror would add.
const defaultPackageProxy = "https://proxy.golang.org"

// PackageProxyURL resolves the package-proxy base URL: the operator's broker.package_proxy
// when set, else the public default. Total, so the relay always has a concrete base when
// the package-proxy destination is allowlisted.
func (b BrokerConfig) PackageProxyURL() string {
	if b.PackageProxy != "" {
		return b.PackageProxy
	}
	return defaultPackageProxy
}

// PackageProxyAllowed reports whether the package-proxy egress destination is in the
// allowlist — i.e. whether the runner should permit (and the relay perform) package
// fetches at all. Deny-by-default: absent the token, a fetch is rejected at the broker.
func (b BrokerConfig) PackageProxyAllowed() bool {
	for _, d := range b.Allowlist {
		if d == DestPackageProxy {
			return true
		}
	}
	return false
}

// GitConfig configures where the candidate branch is pushed and how that push is
// authenticated (T5.7, specs/security.md Control 3). It is the production replacement for
// the bootstrap local-repo push: Remote names a real git remote, and GitHubApp (when set)
// is the authority the runner mints a per-task, short-lived push token from.
//
// Empty Remote keeps the bootstrap shape — the candidate branch is applied to the local
// source repo on the runner host, no token. A set Remote with no GitHubApp pushes
// unauthenticated (valid for a file:// remote — the dev shape and what the offline tests
// exercise). A set Remote with GitHubApp pushes authenticated by a minted installation
// token that dies with the push. The `git` egress destination must be in broker.allowlist
// for any push to be permitted at all (deny-by-default) — see Warnings for the advisory.
type GitConfig struct {
	Remote    string          `yaml:"remote,omitempty"`     // real git remote URL the candidate branch is pushed to; empty = local-repo apply
	GitHubApp GitHubAppConfig `yaml:"github_app,omitempty"` // the token-minting authority (optional; unset = unauthenticated remote)
}

// GitHubAppConfig configures the GitHub App installation-token minter (T5.7). A GitHub App
// installation token scopes to the repository with contents:write permission and lasts at
// most an hour — branch-level scoping is not a token capability, so "only the task branch"
// is enforced by the runner's broker branch guard (the agent never names another branch and
// the runner refuses one if it did). The private key follows the API-key / signing-key
// posture: a runtime-provisioned SECRET referenced by PATH (PrivateKey), read at mint time,
// NEVER committed or baked into an image; its existence is therefore not checked at config
// time (a missing/unreadable key fails loudly on the first push — fail-closed).
type GitHubAppConfig struct {
	APIBase        string `yaml:"api_base,omitempty"`     // GitHub REST API base; empty = public api.github.com (Enterprise Server overrides)
	AppID          string `yaml:"app_id,omitempty"`       // the App's id (the JWT issuer)
	InstallationID string `yaml:"installation_id,omitempty"` // the installation to mint a token for
	Repository     string `yaml:"repository,omitempty"`   // "owner/name" — the one repo the token is scoped to
	PrivateKey     string `yaml:"private_key,omitempty"`  // PATH to the App's PEM private key (a runtime secret, never the bytes)
}

// Active reports whether the GitHub App minter is fully configured and should be built. All
// four fields are required to mint a token; a partial block is a config error (see
// validateGit), not a silently-disabled minter.
func (g GitHubAppConfig) Active() bool {
	return g.AppID != "" && g.InstallationID != "" && g.Repository != "" && g.PrivateKey != ""
}

// set reports whether any GitHub App field is populated — used by validation to flag a
// partial block (some fields set but not Active).
func (g GitHubAppConfig) set() bool {
	return g.AppID != "" || g.InstallationID != "" || g.Repository != "" || g.PrivateKey != ""
}

// DestGitPush is the egress-allowlist token (and broker destination) for git push — the
// value an operator lists in broker.allowlist to permit the candidate-branch push. Single
// source of truth shared by config and the broker's Method.destination.
const DestGitPush = "git"

// GitPushAllowed reports whether the git egress destination is in the allowlist — i.e.
// whether the runner will broker a push at all. Deny-by-default: absent the token, the push
// is rejected at the broker regardless of git.remote.
func (b BrokerConfig) GitPushAllowed() bool {
	for _, d := range b.Allowlist {
		if d == DestGitPush {
			return true
		}
	}
	return false
}

// ArtifactsConfig selects the content-addressed artifact store backend. The files
// backend fits the single-host dev story; the s3 backend is for distributed
// deployments where runners on many hosts and the control room share one bucket.
// Credentials are NEVER in config — like model API keys, the s3 backend reads them
// from the environment (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN).
type ArtifactsConfig struct {
	Backend  string `yaml:"backend"`            // "files" (dev) | "s3" (distributed)
	Path     string `yaml:"path,omitempty"`     // filesystem root for the files backend
	Bucket   string `yaml:"bucket,omitempty"`   // object bucket for the s3 backend
	Endpoint string `yaml:"endpoint,omitempty"` // s3 endpoint host[:port], optional scheme (MinIO/non-AWS); empty = AWS regional endpoint
	Region   string `yaml:"region,omitempty"`   // s3 region (required when endpoint is empty)
}

// OTelConfig configures OTLP export of all three OTel signals — traces, metrics, and
// logs — off one endpoint (specs/observability.md "three signals, one endpoint"). An
// empty Endpoint disables export; "stdout" prints offline; any other value is an
// OTLP/gRPC collector host:port.
//
// Headers are sent with every export — the auth + routing metadata an authenticated
// backend (OpenObserve, Grafana Cloud, Honeycomb) requires. Following the same discipline
// as the model registry's API keys, a CREDENTIAL value is never literal here: a header
// whose name looks like a credential must be an environment reference of the form
// ${ENV_VAR}, resolved at startup by ResolveHeaders. Non-credential routing metadata
// (e.g. organization, stream-name) may be a plain literal. Validation (validateOTel)
// enforces this; a ${ENV_VAR} that is unset at runtime resolves to empty (the export then
// fails auth loudly at the backend, the same fail-closed posture as a missing API key).
//
// TLS selects transport security for the dial: false (default) is insecure, the
// local-collector posture localhost:4317 expects; true uses the host's root CAs for an
// authenticated public backend reached over the internet.
type OTelConfig struct {
	Endpoint string            `yaml:"endpoint,omitempty"`
	Headers  map[string]string `yaml:"headers,omitempty"`
	TLS      bool              `yaml:"tls,omitempty"`
}

// ResolveHeaders returns the export headers with every ${ENV_VAR} reference expanded from
// the process environment (an unset var expands to ""). Plain-literal values pass through
// unchanged. Resolution happens here, at the last responsible moment before the exporter
// is built, so a credential lives in config only as an env reference, never as a secret —
// the same posture the model registry takes with API keys. Returns nil for no headers so
// the exporter adds no WithHeaders option.
func (o OTelConfig) ResolveHeaders() map[string]string {
	if len(o.Headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(o.Headers))
	for k, v := range o.Headers {
		out[k] = os.Expand(v, os.Getenv)
	}
	return out
}

// SigningConfig configures provenance-commit signing and verification (T5.10,
// specs/security.md). The trusted layer authors a provenance commit on top of every
// verified candidate (specs/integration.md); signing it with the harness's own SSH
// identity is what makes "the audit trail is the accountability" cryptographic rather
// than a forgeable plaintext trailer — main's tip becomes a commit only the harness
// could have produced.
//
// The scheme is git-native SSH signing (gpg.format=ssh): no GPG keyring or external
// daemon, verification is a public-key check against an allowed-signers file that
// anyone can hold. Key custody follows the API-key posture — the private key is a
// runtime-provisioned SECRET referenced by path here, NEVER committed or baked into an
// image; in dev it is an operator-supplied key file, in production it is delivered by a
// secret manager / ssh-agent to the orchestrator host (the deployment remainder, like
// the NATS cluster of T5.8 or the S3 bucket of T5.9). The path's existence is therefore
// not checked at config time (the secret may be mounted only at run time on the signing
// host) — a missing/unreadable key fails loudly on the first merge attempt (fail-closed).
//
// Key signs the provenance commit (orchestrator side); AllowedSigners verifies it on
// read (control-room side). They are independent capabilities: a control-room-only host
// configures AllowedSigners alone to verify without ever signing.
type SigningConfig struct {
	Enabled        bool   `yaml:"enabled,omitempty"`         // turn provenance signing on (requires Key)
	Key            string `yaml:"key,omitempty"`             // path to the SSH private signing key (the harness identity)
	AllowedSigners string `yaml:"allowed_signers,omitempty"` // path to the allowed-signers file (principal -> public key) used to verify on read
}

// Active reports whether provenance signing should happen: it is enabled and a key path
// is configured. The merger signs the integration commit only when Active; otherwise it
// writes the same unsigned commit as before (so a deployment with no key is unchanged).
func (s SigningConfig) Active() bool { return s.Enabled && s.Key != "" }

// ModelProvider maps a model name (declared by core.Soul.Model) to its provider
// adapter and, for OpenAI-compatible backends, an endpoint. API keys are NEVER in
// config — the runner injects them from the environment (see specs/models.md).
type ModelProvider struct {
	Provider string    `yaml:"provider"`           // one of the Provider* constants
	Endpoint string    `yaml:"endpoint,omitempty"` // base URL for openai-compat backends (Ollama/vLLM)
	Cost     ModelCost `yaml:"cost,omitempty"`     // per-million-token price, the tokens→USD table
}

// ModelCost is a model's per-million-token price: the table that converts a recorded
// token Usage into USD so the orchestrator can enforce the dollar budget that bounds the
// on_failure loop (see specs/workflow.md, specs/models.md). Per-million-token is the unit
// model vendors publish, so the numbers map directly to a price sheet. Every field is
// optional and a zero rate prices that dimension at $0, so a model with no cost block
// contributes nothing to USD accounting — its spend is still bounded by the token and
// retry caps, which never depend on the cost table. Prices are not secrets (unlike API
// keys, which are never in config), so they live here in the model registry.
type ModelCost struct {
	InputPerMTok      float64 `yaml:"input_per_mtok,omitempty"`       // full-rate prompt tokens
	OutputPerMTok     float64 `yaml:"output_per_mtok,omitempty"`      // generated tokens
	CacheWritePerMTok float64 `yaml:"cache_write_per_mtok,omitempty"` // input tokens written to the prompt cache
	CacheReadPerMTok  float64 `yaml:"cache_read_per_mtok,omitempty"`  // input tokens served from the cache
}

// USD converts a recorded token usage to dollars at this model's rates. Each dimension
// is priced independently — full-rate input, output, cache write, and cache read all bill
// differently — and summed; per-million pricing means each term is count*rate/1e6. An
// unpriced dimension (zero rate) adds nothing, so an empty ModelCost yields $0.
func (c ModelCost) USD(u core.Usage) float64 {
	const perMillion = 1_000_000.0
	return float64(u.InputTokens)*c.InputPerMTok/perMillion +
		float64(u.OutputTokens)*c.OutputPerMTok/perMillion +
		float64(u.CacheCreationTokens)*c.CacheWritePerMTok/perMillion +
		float64(u.CacheReadTokens)*c.CacheReadPerMTok/perMillion
}

// Provider identifiers for ModelProvider.Provider. These are the single source of
// truth shared by config validation (see validate.go) and the runner-side model
// registry that turns each entry into a provider adapter (see
// internal/model/registry). OpenAICompat covers any server speaking the OpenAI wire
// protocol (Ollama, vLLM, Together, …) and so requires an Endpoint.
const (
	ProviderAnthropic    = "anthropic"
	ProviderOpenAI       = "openai"
	ProviderOpenAICompat = "openai-compat"
)

// LoadInfra reads and unmarshals an infra.<env>.yaml overlay. It parses strictly
// (unknown keys are errors). A missing file, malformed YAML, or unknown key fails
// here; cross-references (e.g. a soul.Model with no registry entry) are validate's
// job.
func LoadInfra(path string) (*Infra, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is an operator-supplied config location, not untrusted agent input.
	if err != nil {
		return nil, fmt.Errorf("config: read infra file %s: %w", path, err)
	}
	var in Infra
	if err := unmarshalStrict(data, &in); err != nil {
		return nil, fmt.Errorf("config: parse infra file %s: %w", path, err)
	}
	return &in, nil
}
