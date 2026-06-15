package secret

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mustGenKey generates a throwaway 2048-bit RSA key for the tests.
func mustGenKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

// testKey generates a throwaway RSA key and writes its PKCS#1 PEM to a temp file, returning
// the path and the public key for JWT verification. PKCS#1 is what GitHub Apps download.
func testKey(t *testing.T, dir string) (string, *rsa.PublicKey) {
	t.Helper()
	key := mustGenKey(t)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	path := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return path, &key.PublicKey
}

// verifyJWT checks an RS256 JWT against pub and returns the decoded claims. It is the
// server-side assertion that the minter signed a well-formed App JWT.
func verifyJWT(t *testing.T, token string, pub *rsa.PublicKey) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt: want 3 segments, got %d", len(parts))
	}
	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("jwt: decode signature: %v", err)
	}
	digest := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("jwt: signature does not verify: %v", err)
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("jwt: decode claims: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatalf("jwt: unmarshal claims: %v", err)
	}
	return claims
}

func TestGitHubAppMinterMintAndRevoke(t *testing.T) {
	dir := t.TempDir()
	keyPath, pub := testKey(t, dir)

	const wantToken = "ghs_installationtoken"
	expires := time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)
	fixedNow := time.Date(2026, 6, 16, 11, 30, 0, 0, time.UTC)

	var (
		gotMintAuth, gotRevokeAuth string
		gotBody                    map[string]any
		mintHits, revokeHits       int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/789/access_tokens":
			mintHits++
			gotMintAuth = r.Header.Get("Authorization")
			// The bearer credential must be a valid App JWT signed with the App key.
			jwt := strings.TrimPrefix(gotMintAuth, "Bearer ")
			claims := verifyJWT(t, jwt, pub)
			if claims["iss"] != "123" {
				t.Errorf("jwt iss = %v, want 123", claims["iss"])
			}
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &gotBody)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      wantToken,
				"expires_at": expires.Format(time.RFC3339),
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
			revokeHits++
			gotRevokeAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	m := NewGitHubAppMinter(GitHubAppConfig{
		APIBase:        srv.URL,
		AppID:          "123",
		InstallationID: "789",
		Repository:     "acme/widgets",
		KeyPath:        keyPath,
		now:            func() time.Time { return fixedNow },
	})

	cred, err := m.Mint(context.Background(), MintRequest{IssueID: "iss-1", Branch: "candidate/iss-1"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if cred.Username != "x-access-token" || cred.Token != wantToken {
		t.Errorf("cred = %+v, want username x-access-token token %q", cred, wantToken)
	}
	if !cred.Expiry.Equal(expires) {
		t.Errorf("cred.Expiry = %v, want %v", cred.Expiry, expires)
	}
	if mintHits != 1 {
		t.Errorf("mint hits = %d, want 1", mintHits)
	}
	if !strings.HasPrefix(gotMintAuth, "Bearer ") {
		t.Errorf("mint auth = %q, want Bearer prefix", gotMintAuth)
	}
	// The token must be scoped to the single repo (bare name) with contents:write only.
	if repos, _ := gotBody["repositories"].([]any); len(repos) != 1 || repos[0] != "widgets" {
		t.Errorf("repositories = %v, want [widgets]", gotBody["repositories"])
	}
	if perms, _ := gotBody["permissions"].(map[string]any); perms["contents"] != "write" {
		t.Errorf("permissions = %v, want contents:write", gotBody["permissions"])
	}

	if err := m.Revoke(context.Background(), cred); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if revokeHits != 1 {
		t.Errorf("revoke hits = %d, want 1", revokeHits)
	}
	if gotRevokeAuth != "token "+wantToken {
		t.Errorf("revoke auth = %q, want token %q", gotRevokeAuth, wantToken)
	}
}

func TestGitHubAppMinterMintErrors(t *testing.T) {
	dir := t.TempDir()
	keyPath, _ := testKey(t, dir)

	t.Run("non-201 status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"message":"Bad credentials"}`)
		}))
		defer srv.Close()
		m := NewGitHubAppMinter(GitHubAppConfig{APIBase: srv.URL, AppID: "1", InstallationID: "2", Repository: "o/r", KeyPath: keyPath})
		if _, err := m.Mint(context.Background(), MintRequest{}); err == nil {
			t.Fatal("Mint with 401: want error, got nil")
		}
	})

	t.Run("missing key file", func(t *testing.T) {
		m := NewGitHubAppMinter(GitHubAppConfig{APIBase: "http://unused", AppID: "1", InstallationID: "2", Repository: "o/r", KeyPath: filepath.Join(dir, "absent.pem")})
		if _, err := m.Mint(context.Background(), MintRequest{}); err == nil {
			t.Fatal("Mint with missing key: want error, got nil")
		}
	})

	t.Run("malformed key", func(t *testing.T) {
		bad := filepath.Join(dir, "bad.pem")
		if err := os.WriteFile(bad, []byte("not a pem"), 0o600); err != nil {
			t.Fatal(err)
		}
		m := NewGitHubAppMinter(GitHubAppConfig{APIBase: "http://unused", AppID: "1", InstallationID: "2", Repository: "o/r", KeyPath: bad})
		if _, err := m.Mint(context.Background(), MintRequest{}); err == nil {
			t.Fatal("Mint with malformed key: want error, got nil")
		}
	})
}

// TestParseRSAPrivateKeyPKCS8 proves a PKCS#8-encoded key (the re-encoded form) is accepted
// too, not just the PKCS#1 GitHub ships — a valid key must not be rejected on encoding.
func TestParseRSAPrivateKeyPKCS8(t *testing.T) {
	key := mustGenKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	got, err := parseRSAPrivateKey(pemBytes)
	if err != nil {
		t.Fatalf("parseRSAPrivateKey(PKCS8): %v", err)
	}
	if got.N.Cmp(key.N) != 0 {
		t.Error("parsed key does not match original")
	}
	if _, err := parseRSAPrivateKey([]byte("garbage")); err == nil {
		t.Error("parseRSAPrivateKey(garbage): want error, got nil")
	}
}

func TestGitCredentialEmpty(t *testing.T) {
	if !(GitCredential{}).Empty() {
		t.Error("zero credential should be Empty")
	}
	if (GitCredential{Token: "x"}).Empty() {
		t.Error("credential with a token should not be Empty")
	}
}
