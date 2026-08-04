package auth

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func withTempAuth(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "auth.json")
	SetPathForTest(p)
	t.Cleanup(func() { SetPathForTest("") })
	return p
}

func TestStoreRoundTripXAIAndClaude(t *testing.T) {
	withTempAuth(t)

	s := &Store{
		XAI: &Token{
			AccessToken:  "at",
			RefreshToken: "rt",
			ExpiresAt:    time.Now().Add(time.Hour).UTC().Truncate(time.Second),
			TokenType:    "Bearer",
		},
		Claude: &ClaudeProxy{
			Endpoint:     "http://127.0.0.1:9999/v1",
			ProviderMode: ClaudeModeOpenAICompat,
			AccessToken:  "c-tok",
		},
	}
	if err := Save(s); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.XAI == nil || got.XAI.AccessToken != "at" || got.XAI.RefreshToken != "rt" {
		t.Fatalf("xai = %+v", got.XAI)
	}
	if got.Claude == nil || got.Claude.Endpoint != "http://127.0.0.1:9999/v1" {
		t.Fatalf("claude = %+v", got.Claude)
	}
}

func TestClearSelective(t *testing.T) {
	withTempAuth(t)
	_ = Save(&Store{
		XAI:    &Token{AccessToken: "x"},
		Claude: &ClaudeProxy{Endpoint: "http://127.0.0.1:1", ProviderMode: ClaudeModeOpenAICompat},
	})
	if err := Clear(ProviderGrok); err != nil {
		t.Fatal(err)
	}
	s, _ := Load()
	if s.XAI != nil {
		t.Fatal("expected xai cleared")
	}
	if s.Claude == nil {
		t.Fatal("expected claude preserved")
	}
	if err := Clear("all"); err != nil {
		t.Fatal(err)
	}
	s, _ = Load()
	if s.XAI != nil || s.Claude != nil {
		t.Fatalf("expected empty store, got %+v", s)
	}
}

func TestStatusNotLoggedIn(t *testing.T) {
	withTempAuth(t)
	st, err := Status()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st, "not logged in") {
		t.Fatalf("status = %q", st)
	}
}

func TestRegisterClaudeProxy(t *testing.T) {
	withTempAuth(t)
	if err := RegisterClaudeProxy("http://127.0.0.1:8585/v1", "", ""); err != nil {
		t.Fatal(err)
	}
	c, err := ClaudeCredentials()
	if err != nil || c == nil {
		t.Fatalf("creds: %v %+v", err, c)
	}
	if c.ProviderMode != ClaudeModeOpenAICompat {
		t.Fatalf("mode = %q", c.ProviderMode)
	}
	if err := RegisterClaudeProxy("ftp://bad", "", ""); err == nil {
		t.Fatal("want error for ftp")
	}
	if err := RegisterClaudeProxy("", "", ""); err == nil {
		t.Fatal("want error for empty")
	}
}

func TestHardenVerificationURI(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"https://accounts.x.ai/device", "https://accounts.x.ai/device", false},
		{"http://127.0.0.1:9/ok", "http://127.0.0.1:9/ok", false},
		{"http://localhost:9/ok", "http://localhost:9/ok", false},
		{"http://evil.example/phish", "https://evil.example/phish", false},
		{"", "", true},
		{"file:///etc/passwd", "", true},
	}
	for _, tc := range cases {
		got, err := HardenVerificationURI(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("Harden(%q): want err", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("Harden(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Harden(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsPermanentRefreshErr(t *testing.T) {
	if !isPermanentRefreshErr(errStr("auth: refresh failed: invalid_grant: gone")) {
		t.Fatal("invalid_grant should be permanent")
	}
	if isPermanentRefreshErr(errStr("auth: refresh failed: temporarily_unavailable")) {
		t.Fatal("temporarily_unavailable should not be permanent")
	}
}

type errStr string

func (e errStr) Error() string { return string(e) }

func TestNormalizeProvider(t *testing.T) {
	if NormalizeProvider("grok") != ProviderXAI {
		t.Fatal()
	}
	if NormalizeProvider("claude") != ProviderClaude {
		t.Fatal()
	}
	if NormalizeProvider("") != ProviderXAI {
		t.Fatal("default is xai/grok")
	}
}

func TestAccessTokenEmpty(t *testing.T) {
	withTempAuth(t)
	tok, err := AccessToken()
	if err != nil || tok != "" {
		t.Fatalf("got %q %v", tok, err)
	}
}

func TestAccessTokenValid(t *testing.T) {
	withTempAuth(t)
	_ = Save(&Store{XAI: &Token{
		AccessToken: "live",
		ExpiresAt:   time.Now().Add(time.Hour),
	}})
	tok, err := AccessToken()
	if err != nil || tok != "live" {
		t.Fatalf("got %q %v", tok, err)
	}
}
