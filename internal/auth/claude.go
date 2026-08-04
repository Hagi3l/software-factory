package auth

import (
	"fmt"
	"io"
	"net/url"
	"strings"
)

// Claude provider modes accepted by login claude --mode.
const (
	ClaudeModeOpenAICompat = "openai-compat"
	ClaudeModeAnthropic    = "anthropic"
)

// RegisterClaudeProxy saves a Claude subscription proxy into the auth store.
// endpoint is required; token may be empty for keyless local bridges.
func RegisterClaudeProxy(endpoint, token, mode string) error {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return fmt.Errorf("auth: claude proxy endpoint is required")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("auth: invalid claude proxy endpoint %q", endpoint)
	}
	switch u.Scheme {
	case "http", "https":
		// ok
	default:
		return fmt.Errorf("auth: claude proxy endpoint must be http(s), got %q", u.Scheme)
	}
	mode = strings.TrimSpace(mode)
	if mode == "" {
		mode = ClaudeModeOpenAICompat
	}
	if mode != ClaudeModeOpenAICompat && mode != ClaudeModeAnthropic {
		return fmt.Errorf("auth: claude mode must be %s or %s", ClaudeModeOpenAICompat, ClaudeModeAnthropic)
	}

	s, err := Load()
	if err != nil {
		return err
	}
	s.Claude = &ClaudeProxy{
		AccessToken:  strings.TrimSpace(token),
		Endpoint:     strings.TrimRight(endpoint, "/"),
		ProviderMode: mode,
	}
	return Save(s)
}

// ClaudeCredentials returns the registered Claude proxy, or (nil, nil) if none.
func ClaudeCredentials() (*ClaudeProxy, error) {
	s, err := Load()
	if err != nil {
		return nil, err
	}
	if s.Claude == nil || s.Claude.Endpoint == "" {
		return nil, nil
	}
	return s.Claude, nil
}

// LoginClaude registers a Claude subscription proxy and prints a short summary.
func LoginClaude(out io.Writer, endpoint, token, mode string) error {
	if err := RegisterClaudeProxy(endpoint, token, mode); err != nil {
		return err
	}
	c, err := ClaudeCredentials()
	if err != nil {
		return err
	}
	path, _ := authPath()
	fmt.Fprintf(out, "Claude subscription proxy registered.\n")
	fmt.Fprintf(out, "  endpoint: %s\n", c.Endpoint)
	fmt.Fprintf(out, "  mode:     %s\n", c.ProviderMode)
	if c.AccessToken != "" {
		fmt.Fprintln(out, "  bearer:   set")
	} else {
		fmt.Fprintln(out, "  bearer:   none (keyless proxy)")
	}
	fmt.Fprintf(out, "Stored at %s\n", path)
	fmt.Fprint(out, `
Next: point model registry entries at this endpoint (openai-compat + family: anthropic),
or use provider: anthropic with the proxy base URL. See docs/selecting-provider.md.
`)
	return nil
}
