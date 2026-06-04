package web

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harness-demo/vault/internal/store"
)

// newTestServer returns a running httptest server and a cookie-jar client. The client
// follows redirects, so POST→303→GET flows land on their final page.
func newTestServer(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "vault.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ts := httptest.NewServer(New(st))
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	return ts, &http.Client{Jar: jar}
}

func get(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func postForm(t *testing.T, c *http.Client, url string, form url.Values) (int, string) {
	t.Helper()
	resp, err := c.PostForm(url, form)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestSetupRequiredThenVault(t *testing.T) {
	ts, c := newTestServer(t)

	// First visit redirects to setup.
	_, body := get(t, c, ts.URL+"/")
	if !strings.Contains(body, "Create your vault") {
		t.Fatalf("expected setup page, got:\n%s", body)
	}

	// Setup logs in and lands on the vault.
	code, body := postForm(t, c, ts.URL+"/setup", url.Values{"password": {"masterpw12"}})
	if code != http.StatusOK || !strings.Contains(body, "Recent activity") {
		t.Fatalf("setup landing code=%d body=%s", code, body)
	}
}

func TestAuthRequired(t *testing.T) {
	ts, _ := newTestServer(t)
	// A bare client with no cookie jar should be bounced from /vault to /login.
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := noFollow.Get(ts.URL + "/vault")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("want 303 -> /login, got %d -> %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestSecretCreateRevealSearchDelete(t *testing.T) {
	ts, c := newTestServer(t)
	postForm(t, c, ts.URL+"/setup", url.Values{"password": {"masterpw12"}})

	// Create a secret.
	code, _ := postForm(t, c, ts.URL+"/secrets", url.Values{
		"name": {"prod-db"}, "tag": {"database"}, "value": {"hunter2"},
	})
	if code != http.StatusOK {
		t.Fatalf("create code=%d", code)
	}

	// It shows on the vault page.
	_, body := get(t, c, ts.URL+"/vault")
	if !strings.Contains(body, "prod-db") || !strings.Contains(body, "database") {
		t.Fatalf("secret not listed:\n%s", body)
	}
	// Plaintext must NOT be in the listing.
	if strings.Contains(body, "hunter2") {
		t.Fatal("plaintext value leaked into listing")
	}

	// Reveal returns the decrypted value.
	_, reveal := get(t, c, ts.URL+"/secrets/1/reveal")
	if !strings.Contains(reveal, "hunter2") {
		t.Fatalf("reveal did not return plaintext: %s", reveal)
	}

	// Search by tag returns the row; search for nonsense does not.
	_, hit := get(t, c, ts.URL+"/vault/search?q=database")
	if !strings.Contains(hit, "prod-db") {
		t.Fatalf("search hit missing: %s", hit)
	}
	_, miss := get(t, c, ts.URL+"/vault/search?q=zzzzz")
	if strings.Contains(miss, "prod-db") {
		t.Fatalf("search miss should be empty: %s", miss)
	}

	// Delete it.
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/secrets/1", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete code=%d", resp.StatusCode)
	}
	// The secret-list fragment (not the whole page — the audit feed legitimately still
	// mentions the deleted name) should no longer carry the row.
	_, after := get(t, c, ts.URL+"/vault/search?q=")
	if strings.Contains(after, "prod-db") {
		t.Fatal("secret still listed after delete")
	}
}

func TestWrongPasswordRejected(t *testing.T) {
	ts, c := newTestServer(t)
	postForm(t, c, ts.URL+"/setup", url.Values{"password": {"masterpw12"}})

	// Log out, then try a wrong password.
	postForm(t, c, ts.URL+"/logout", url.Values{})
	code, body := postForm(t, c, ts.URL+"/login", url.Values{"password": {"wrongpw"}})
	if code != http.StatusUnauthorized || !strings.Contains(body, "Incorrect master password") {
		t.Fatalf("wrong password code=%d body=%s", code, body)
	}
}
