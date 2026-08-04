package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestLoginDeviceCodeFlow exercises the happy path against httptest endpoints
// (no live auth.x.ai). Poll interval is forced to 0 via the device response so the
// test does not sleep for DefaultPollSec.
func TestLoginDeviceCodeFlow(t *testing.T) {
	withTempAuth(t)

	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/device/code", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":               "dev-code",
			"user_code":                 "ABCD-EFGH",
			"verification_uri":          "https://accounts.x.ai/device",
			"verification_uri_complete": "https://accounts.x.ai/device?user_code=ABCD-EFGH",
			"expires_in":                600,
			"interval":                  1, // 1s between polls (min practical in this flow)
		})
	})
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		n := polls.Add(1)
		if n < 2 {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-xyz",
			"refresh_token": "refresh-xyz",
			"expires_in":    3600,
			"token_type":    "Bearer",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	oldDevice, oldToken := DeviceCodeURL, TokenURL
	DeviceCodeURL = srv.URL + "/oauth2/device/code"
	TokenURL = srv.URL + "/oauth2/token"
	t.Cleanup(func() {
		DeviceCodeURL, TokenURL = oldDevice, oldToken
	})

	var buf bytes.Buffer
	tok, err := Login(&buf)
	if err != nil {
		t.Fatalf("Login: %v\n%s", err, buf.String())
	}
	if tok.AccessToken != "access-xyz" || tok.RefreshToken != "refresh-xyz" {
		t.Fatalf("token = %+v", tok)
	}
	got, err := AccessToken()
	if err != nil || got != "access-xyz" {
		t.Fatalf("AccessToken = %q %v", got, err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("accounts.x.ai")) {
		t.Fatalf("output missing verification URL: %s", buf.String())
	}
}
