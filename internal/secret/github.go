package secret

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultGitHubAPIBase is the public GitHub REST API. A GitHub Enterprise Server
// deployment overrides it (api_base in config) with its own /api/v3 host.
const DefaultGitHubAPIBase = "https://api.github.com"

// installationTokenUser is the git basic-auth username paired with a GitHub App
// installation token. GitHub documents this exact literal — the token is the password.
const installationTokenUser = "x-access-token"

// jwtLeeway / jwtTTL bound the App JWT used to request an installation token. GitHub caps
// the JWT lifetime at 10 minutes and rejects an iat in the future, so iat is backdated by
// jwtLeeway for clock skew and exp sits comfortably under the 10-minute ceiling.
const (
	jwtLeeway = 60 * time.Second
	jwtTTL    = 9 * time.Minute
)

// GitHubAppConfig configures a GitHub App installation-token minter. The private key is a
// runtime-provisioned secret referenced by PATH (the API-key / signing-key posture): it is
// read at mint time, never held in config and never baked into an image. AppID and
// InstallationID identify the App and its installation on the target org/repo; Repository
// ("owner/name") is the single repo the minted token is scoped to.
type GitHubAppConfig struct {
	APIBase        string // GitHub REST API base; empty => DefaultGitHubAPIBase
	AppID          string // the App's id (the JWT issuer)
	InstallationID string // the installation to mint a token for
	Repository     string // "owner/name" — the one repo the token may touch
	KeyPath        string // path to the App's PEM private key (read at mint time)

	// HTTPClient is a seam for tests; nil => a client with a short timeout. The runner host
	// has network (the sandbox does not), so this dials real GitHub.
	HTTPClient *http.Client

	// now is a clock seam for deterministic JWT timestamps in tests; nil => time.Now.
	now func() time.Time
}

// GitHubAppMinter mints short-lived installation access tokens via the GitHub App API and
// revokes them. One token authorizes a push to the configured repository with contents:write
// permission — GitHub tokens cannot be scoped to a single branch, so "only the task branch"
// is enforced by the runner's broker branch guard (defense in depth), not the token. The
// token's ~1h TTL plus immediate revoke-on-push is the short-lived half of the guarantee.
type GitHubAppMinter struct {
	cfg    GitHubAppConfig
	client *http.Client
	now    func() time.Time
}

var _ Minter = (*GitHubAppMinter)(nil)

// NewGitHubAppMinter builds a minter from cfg. It is network-free (no key read, no dial):
// the key is read and GitHub is contacted only on Mint, so a missing key or unreachable
// API surfaces on the first push, not at construction — safe in the network-free
// composition root, like the model registry and the OTLP exporter.
func NewGitHubAppMinter(cfg GitHubAppConfig) *GitHubAppMinter {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	now := cfg.now
	if now == nil {
		now = time.Now
	}
	if cfg.APIBase == "" {
		cfg.APIBase = DefaultGitHubAPIBase
	}
	return &GitHubAppMinter{cfg: cfg, client: client, now: now}
}

// installationTokenResponse is the relevant slice of the create-installation-token reply.
type installationTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Mint requests an installation access token scoped to the configured repository with
// contents:write (the push permission). The req.Branch is logged/carried but not enforced
// at the token level (see the type comment). The App private key is read here, signed into
// a short JWT, and exchanged for the installation token.
func (m *GitHubAppMinter) Mint(ctx context.Context, req MintRequest) (GitCredential, error) {
	jwt, err := m.appJWT()
	if err != nil {
		return GitCredential{}, err
	}

	// Scope the token to the single repository, contents:write only. The repositories field
	// takes bare repo names (the installation already fixes the owner), so split "owner/name".
	repoName := m.cfg.Repository
	if i := strings.LastIndex(repoName, "/"); i >= 0 {
		repoName = repoName[i+1:]
	}
	body, err := json.Marshal(map[string]any{
		"repositories": []string{repoName},
		"permissions":  map[string]string{"contents": "write"},
	})
	if err != nil {
		return GitCredential{}, fmt.Errorf("secret: marshal token request: %w", err)
	}

	url := strings.TrimRight(m.cfg.APIBase, "/") + "/app/installations/" + m.cfg.InstallationID + "/access_tokens"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return GitCredential{}, fmt.Errorf("secret: build token request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+jwt)
	httpReq.Header.Set("Accept", "application/vnd.github+json")
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := m.client.Do(httpReq)
	if err != nil {
		return GitCredential{}, fmt.Errorf("secret: request installation token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return GitCredential{}, fmt.Errorf("secret: installation token: %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}

	var tok installationTokenResponse
	if err := json.Unmarshal(respBody, &tok); err != nil {
		return GitCredential{}, fmt.Errorf("secret: decode installation token: %w", err)
	}
	if tok.Token == "" {
		return GitCredential{}, fmt.Errorf("secret: installation token response carried no token")
	}
	return GitCredential{Username: installationTokenUser, Token: tok.Token, Expiry: tok.ExpiresAt}, nil
}

// Revoke invalidates an installation token immediately (the token authorizes its own
// revocation: DELETE /installation/token). Best-effort — the caller treats a failure as
// non-fatal because the token's TTL is the backstop — but a clean revoke means the token
// dies with the push rather than living out its hour.
func (m *GitHubAppMinter) Revoke(ctx context.Context, cred GitCredential) error {
	if cred.Empty() {
		return nil
	}
	url := strings.TrimRight(m.cfg.APIBase, "/") + "/installation/token"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("secret: build revoke request: %w", err)
	}
	httpReq.Header.Set("Authorization", "token "+cred.Token)
	httpReq.Header.Set("Accept", "application/vnd.github+json")

	resp, err := m.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("secret: revoke installation token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("secret: revoke installation token: unexpected status %s", resp.Status)
	}
	return nil
}

// appJWT reads the App private key and mints a short RS256 JWT identifying the App — the
// credential GitHub exchanges for an installation token. The key is read here (mint time),
// so it can be a run-time-only mount.
func (m *GitHubAppMinter) appJWT() (string, error) {
	keyPEM, err := os.ReadFile(m.cfg.KeyPath) // #nosec G304 -- operator-supplied key path, runner-side, not untrusted agent input.
	if err != nil {
		return "", fmt.Errorf("secret: read app private key %s: %w", m.cfg.KeyPath, err)
	}
	key, err := parseRSAPrivateKey(keyPEM)
	if err != nil {
		return "", err
	}

	now := m.now()
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		"iat": now.Add(-jwtLeeway).Unix(),
		"exp": now.Add(jwtTTL).Unix(),
		"iss": m.cfg.AppID,
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("secret: marshal jwt header: %w", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("secret: marshal jwt claims: %w", err)
	}

	signingInput := b64url(headerJSON) + "." + b64url(claimsJSON)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("secret: sign jwt: %w", err)
	}
	return signingInput + "." + b64url(sig), nil
}

// b64url is base64url without padding — the JWT segment encoding.
func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// parseRSAPrivateKey decodes a PEM-encoded RSA private key in either PKCS#1
// ("RSA PRIVATE KEY") or PKCS#8 ("PRIVATE KEY") form — GitHub Apps download PKCS#1, but
// re-encoded PKCS#8 keys are common, so accept both rather than failing on a valid key.
func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("secret: app private key is not valid PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("secret: parse app private key: %w", err)
	}
	key, ok := keyAny.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("secret: app private key is %T, want an RSA key", keyAny)
	}
	return key, nil
}
