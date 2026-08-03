// Package auth implements subscription OAuth for xAI Grok (SuperGrok / X Premium+).
// Device-code flow against auth.x.ai; tokens stored at ~/.software-factory/auth.json (0600).
// When logged in, the model registry can use the access token as Bearer for openai-compat
// Grok models without an API key.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	ClientID       = "b1a00492-073a-47ea-816f-4c329264a828"
	DeviceCodeURL  = "https://auth.x.ai/oauth2/device/code"
	TokenURL       = "https://auth.x.ai/oauth2/token"
	Scope          = "openid profile email offline_access grok-cli:access api:access"
	DeviceGrant    = "urn:ietf:params:oauth:grant-type:device_code"
	RefreshGrant   = "refresh_token"
	DefaultPollSec = 5
	RefreshSkew    = 2 * time.Minute
	HTTPTimeout    = 30 * time.Second
)

// Token holds the OAuth credentials for one provider.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	TokenType    string    `json:"token_type,omitempty"`
}

// Store is the on-disk auth file (~/.software-factory/auth.json).
type Store struct {
	XAI *Token `json:"xai,omitempty"`
}

func authPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".software-factory", "auth.json"), nil
}

// Load reads the auth store. Missing file is not an error (empty store).
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

// Clear removes the xAI entry (logout).
func Clear() error {
	s, err := Load()
	if err != nil {
		return err
	}
	s.XAI = nil
	return Save(s)
}

// Status returns a human-readable summary of current auth.
func Status() (string, error) {
	s, err := Load()
	if err != nil {
		return "", err
	}
	if s.XAI == nil || s.XAI.AccessToken == "" {
		return "not logged in (no xAI / Grok subscription token)", nil
	}
	now := time.Now()
	if s.XAI.ExpiresAt.Before(now) {
		return fmt.Sprintf("logged in (xAI) — access token expired at %s (will refresh on next use)", s.XAI.ExpiresAt.Format(time.RFC3339)), nil
	}
	return fmt.Sprintf("logged in (xAI / Grok) — expires %s", s.XAI.ExpiresAt.Format(time.RFC3339)), nil
}

// AccessToken returns a valid access token, refreshing if needed.
// Returns ("", nil) when not logged in.
func AccessToken() (string, error) {
	s, err := Load()
	if err != nil {
		return "", err
	}
	if s.XAI == nil || s.XAI.AccessToken == "" {
		return "", nil
	}
	if time.Until(s.XAI.ExpiresAt) > RefreshSkew {
		return s.XAI.AccessToken, nil
	}
	if s.XAI.RefreshToken == "" {
		return s.XAI.AccessToken, nil
	}
	tok, err := refresh(s.XAI.RefreshToken)
	if err != nil {
		return "", err
	}
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
	vals.Set("client_id", ClientID)
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

// Login runs the device-code flow, prints the verification URL/code, polls until
// approved, and saves tokens. Returns the saved Token on success.
func Login(out io.Writer) (*Token, error) {
	client := &http.Client{Timeout: HTTPTimeout}
	vals := url.Values{}
	vals.Set("client_id", ClientID)
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
		tvals.Set("client_id", ClientID)
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
			fmt.Fprintf(out, "Logged in. Token stored at ~/.software-factory/auth.json\n")
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
