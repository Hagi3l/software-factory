// Package auth implements subscription credentials for LLM providers.
//
// Grok / SuperGrok / X Premium+: native device-code OAuth against auth.x.ai.
// Claude Pro/Max: register a local subscription proxy (no public third-party
// Anthropic consumer OAuth). Tokens and proxy metadata live in
// ~/.software-factory/auth.json (mode 0600). API keys in the environment always
// win over this store when the model registry resolves credentials.
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// EnvAuthFile overrides the default auth.json path (tests and unusual layouts).
const EnvAuthFile = "SOFTWARE_FACTORY_AUTH_FILE"

// Provider names stored in auth.json and accepted by the CLI.
const (
	ProviderXAI       = "xai"
	ProviderGrok      = "grok" // CLI alias for xai
	ProviderClaude    = "claude"
	ProviderAnthropic = "anthropic" // CLI alias for claude
)

// Token holds OAuth-style credentials for one provider slot.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
}

// ClaudeProxy is a registered local (or remote) subscription bridge for Claude.
// AccessToken is optional — many local proxies need no bearer.
type ClaudeProxy struct {
	AccessToken  string    `json:"access_token,omitempty"`
	Endpoint     string    `json:"endpoint"`
	ProviderMode string    `json:"provider_mode"` // openai-compat | anthropic
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
}

// Store is the on-disk auth file.
type Store struct {
	XAI    *Token       `json:"xai,omitempty"`
	Claude *ClaudeProxy `json:"anthropic,omitempty"` // JSON key kept as anthropic for stability
}

var (
	pathMu       sync.Mutex
	pathOverride string // tests only
)

// SetPathForTest forces the auth file path. Call with "" to clear. Not for production.
func SetPathForTest(p string) {
	pathMu.Lock()
	defer pathMu.Unlock()
	pathOverride = p
}

func authPath() (string, error) {
	if p := os.Getenv(EnvAuthFile); p != "" {
		return p, nil
	}
	pathMu.Lock()
	override := pathOverride
	pathMu.Unlock()
	if override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".software-factory", "auth.json"), nil
}

// Load reads the auth store. A missing file is not an error (empty store).
func Load() (*Store, error) {
	p, err := authPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &Store{}, nil
		}
		return nil, err
	}
	var s Store
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("auth: corrupt auth.json: %w", err)
	}
	return &s, nil
}

// Save writes the store with mode 0600.
func Save(s *Store) error {
	p, err := authPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// NormalizeProvider maps CLI aliases to canonical store keys.
func NormalizeProvider(p string) string {
	switch p {
	case "", ProviderGrok, ProviderXAI:
		return ProviderXAI
	case ProviderClaude, ProviderAnthropic:
		return ProviderClaude
	case "all":
		return "all"
	default:
		return p
	}
}

// Clear removes one provider slot (or all). provider is a CLI name or "all".
func Clear(provider string) error {
	s, err := Load()
	if err != nil {
		return err
	}
	switch NormalizeProvider(provider) {
	case ProviderXAI:
		s.XAI = nil
	case ProviderClaude:
		s.Claude = nil
	case "all":
		s.XAI = nil
		s.Claude = nil
	default:
		return fmt.Errorf("auth: unknown provider %q (want grok|xai|claude|anthropic|all)", provider)
	}
	return Save(s)
}

// Status returns a human-readable multi-line summary of current auth.
func Status() (string, error) {
	s, err := Load()
	if err != nil {
		return "", err
	}
	now := time.Now()
	var lines []string

	// Grok / xAI
	if s.XAI == nil || s.XAI.AccessToken == "" {
		lines = append(lines, "grok (xAI): not logged in")
	} else if !s.XAI.ExpiresAt.IsZero() && s.XAI.ExpiresAt.Before(now) {
		lines = append(lines, fmt.Sprintf(
			"grok (xAI): logged in — access token expired at %s (will refresh on next use)",
			s.XAI.ExpiresAt.Format(time.RFC3339),
		))
	} else if !s.XAI.ExpiresAt.IsZero() {
		lines = append(lines, fmt.Sprintf(
			"grok (xAI): logged in — expires %s",
			s.XAI.ExpiresAt.Format(time.RFC3339),
		))
	} else {
		lines = append(lines, "grok (xAI): logged in")
	}

	// Claude
	if s.Claude == nil || s.Claude.Endpoint == "" {
		lines = append(lines, "claude: not logged in (no subscription proxy registered)")
	} else {
		mode := s.Claude.ProviderMode
		if mode == "" {
			mode = "openai-compat"
		}
		tok := "no bearer"
		if s.Claude.AccessToken != "" {
			tok = "bearer set"
		}
		lines = append(lines, fmt.Sprintf(
			"claude: proxy %s (%s, %s)",
			s.Claude.Endpoint, mode, tok,
		))
	}

	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out, nil
}
