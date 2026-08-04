package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultClientID = "b1a00492-073a-47ea-816f-4c329264a828"
	// EnvClientID overrides the OAuth client_id (advanced / rotated clients).
	EnvClientID = "XAI_OAUTH_CLIENT_ID"

	Scope          = "openid profile email offline_access grok-cli:access api:access"
	DeviceGrant    = "urn:ietf:params:oauth:grant-type:device_code"
	RefreshGrant   = "refresh_token"
	DefaultPollSec = 5
	RefreshSkew    = 2 * time.Minute
	HTTPTimeout    = 30 * time.Second
)

// Overridable endpoints for tests (httptest). Production defaults point at auth.x.ai.
var (
	DeviceCodeURL = "https://auth.x.ai/oauth2/device/code"
	TokenURL      = "https://auth.x.ai/oauth2/token"
)

func clientID() string {
	if id := os.Getenv(EnvClientID); id != "" {
		return id
	}
	return defaultClientID
}

// AccessToken returns a valid xAI access token, refreshing if needed.
// Returns ("", nil) when not logged in.
func AccessToken() (string, error) {
	s, err := Load()
	if err != nil {
		return "", err
	}
	if s.XAI == nil || s.XAI.AccessToken == "" {
		return "", nil
	}
	// Fresh enough (or no expiry recorded): return as-is.
	if s.XAI.ExpiresAt.IsZero() || time.Until(s.XAI.ExpiresAt) > RefreshSkew {
		return s.XAI.AccessToken, nil
	}
	if s.XAI.RefreshToken == "" {
		// No refresh path — return current token and let the API fail if expired.
		return s.XAI.AccessToken, nil
	}
	tok, err := refresh(s.XAI.RefreshToken)
	if err != nil {
		if isPermanentRefreshErr(err) {
			_ = Clear(ProviderXAI)
			return "", fmt.Errorf("%w — run software-factory login again", err)
		}
		return "", err
	}
	// Retain previous refresh_token when the response omits a new one.
	if tok.RefreshToken == "" {
		tok.RefreshToken = s.XAI.RefreshToken
	}
	s.XAI = tok
	if err := Save(s); err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

type deviceCodeResp struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type tokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func postForm(endpoint string, vals url.Values) (*tokenResp, int, error) {
	client := &http.Client{Timeout: HTTPTimeout}
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(vals.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	var tr tokenResp
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("auth: invalid token response: %w", err)
	}
	return &tr, resp.StatusCode, nil
}

func refresh(refreshToken string) (*Token, error) {
	vals := url.Values{}
	vals.Set("grant_type", RefreshGrant)
	vals.Set("client_id", clientID())
	vals.Set("refresh_token", refreshToken)
	tr, status, err := postForm(TokenURL, vals)
	if err != nil {
		return nil, err
	}
	if status != 200 || tr.AccessToken == "" {
		msg := tr.Error
		if tr.ErrorDesc != "" {
			msg = tr.Error + ": " + tr.ErrorDesc
		}
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", status)
		}
		return nil, fmt.Errorf("auth: refresh failed: %s", msg)
	}
	exp := 3600
	if tr.ExpiresIn > 0 {
		exp = tr.ExpiresIn
	}
	return &Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(exp) * time.Second),
		TokenType:    tr.TokenType,
	}, nil
}

// isPermanentRefreshErr reports errors that mean the refresh token is dead.
func isPermanentRefreshErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"invalid_grant",
		"invalid_token",
		"unauthorized",
		"access_denied",
		"expired",
		"revoked",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	// HTTP 400/401/403 without transient markers
	if strings.Contains(msg, "http 400") || strings.Contains(msg, "http 401") || strings.Contains(msg, "http 403") {
		return true
	}
	return false
}

// HardenVerificationURI ensures the user-facing approve URL is HTTPS
// (or http://localhost / http://127.0.0.1 for local test servers).
func HardenVerificationURI(uri string) (string, error) {
	if uri == "" {
		return "", errors.New("auth: empty verification URI")
	}
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("auth: invalid verification URI: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return uri, nil
	case "http":
		host := strings.ToLower(u.Hostname())
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return uri, nil
		}
		// Upgrade plain http host to https rather than opening an insecure approve page.
		u.Scheme = "https"
		return u.String(), nil
	default:
		return "", fmt.Errorf("auth: verification URI must be https (got %q)", u.Scheme)
	}
}

// Login runs the xAI device-code flow, prints the verification URL/code, polls
// until approved, and saves tokens. Returns the saved Token on success.
func Login(out io.Writer) (*Token, error) {
	client := &http.Client{Timeout: HTTPTimeout}
	vals := url.Values{}
	vals.Set("client_id", clientID())
	vals.Set("scope", Scope)
	req, err := http.NewRequest(http.MethodPost, DeviceCodeURL, strings.NewReader(vals.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: device code request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("auth: device code HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var dc deviceCodeResp
	if err := json.Unmarshal(body, &dc); err != nil {
		return nil, fmt.Errorf("auth: invalid device code response: %w", err)
	}
	if dc.DeviceCode == "" || dc.UserCode == "" {
		return nil, errors.New("auth: device code response missing device_code or user_code")
	}

	uri := dc.VerificationURIComplete
	if uri == "" {
		uri = dc.VerificationURI
	}
	if uri == "" {
		uri = "https://accounts.x.ai"
	}
	uri, err = HardenVerificationURI(uri)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "Open this URL in a browser and approve access:\n\n  %s\n\n", uri)
	if dc.VerificationURIComplete == "" {
		fmt.Fprintf(out, "User code: %s\n\n", dc.UserCode)
	}
	fmt.Fprint(out, "Waiting for approval")

	interval := dc.Interval
	if interval <= 0 {
		interval = DefaultPollSec
	}
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	if dc.ExpiresIn <= 0 {
		deadline = time.Now().Add(15 * time.Minute)
	}

	for {
		if time.Now().After(deadline) {
			fmt.Fprintln(out)
			return nil, errors.New("auth: device code expired — run login again")
		}
		time.Sleep(time.Duration(interval) * time.Second)
		fmt.Fprint(out, ".")

		tvals := url.Values{}
		tvals.Set("grant_type", DeviceGrant)
		tvals.Set("client_id", clientID())
		tvals.Set("device_code", dc.DeviceCode)
		tr, status, err := postForm(TokenURL, tvals)
		if err != nil {
			fmt.Fprintln(out)
			return nil, err
		}
		switch {
		case status == 200 && tr.AccessToken != "":
			fmt.Fprintln(out, " approved.")
			exp := 3600
			if tr.ExpiresIn > 0 {
				exp = tr.ExpiresIn
			}
			tok := &Token{
				AccessToken:  tr.AccessToken,
				RefreshToken: tr.RefreshToken,
				ExpiresAt:    time.Now().Add(time.Duration(exp) * time.Second),
				TokenType:    tr.TokenType,
			}
			s, err := Load()
			if err != nil {
				return nil, err
			}
			s.XAI = tok
			if err := Save(s); err != nil {
				return nil, err
			}
			path, _ := authPath()
			fmt.Fprintf(out, "Logged in (Grok / xAI). Token stored at %s\n", path)
			return tok, nil
		case tr.Error == "authorization_pending":
			continue
		case tr.Error == "slow_down":
			interval += 5
			if interval > 30 {
				interval = 30
			}
			continue
		case tr.Error == "access_denied", tr.Error == "authorization_denied":
			fmt.Fprintln(out)
			return nil, errors.New("auth: access denied by user")
		case tr.Error == "expired_token":
			fmt.Fprintln(out)
			return nil, errors.New("auth: device code expired — run login again")
		default:
			fmt.Fprintln(out)
			msg := tr.Error
			if tr.ErrorDesc != "" {
				msg += ": " + tr.ErrorDesc
			}
			if msg == "" {
				msg = fmt.Sprintf("HTTP %d", status)
			}
			return nil, fmt.Errorf("auth: %s", msg)
		}
	}
}
