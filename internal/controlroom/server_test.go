package controlroom

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Loxstomper/harness/internal/controlroom/views"
)

// newTestServer builds a Server fronted by httptest, exercising the real routed handler
// without binding a socket. Asserting through the handler (not internal helpers) is what
// makes these tests verify the actual served surface.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := New(Options{Version: "test-1.2.3"})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// reply is a fully-read, body-closed HTTP response. Returning this rather than a live
// *http.Response keeps every call site leak-free (and satisfies the bodyclose linter).
type reply struct {
	status int
	header http.Header
	body   string
}

func get(t *testing.T, ts *httptest.Server, path string) reply {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read %s body: %v", path, err)
	}
	return reply{status: resp.StatusCode, header: resp.Header, body: string(body)}
}

// TestHomePage is the scaffold's core contract: the landing page renders the base layout
// as HTML, references all three embedded front-end assets, and stamps the build version —
// proving templ rendering and the layout chrome are wired end-to-end.
func TestHomePage(t *testing.T) {
	ts := newTestServer(t)
	r := get(t, ts, "/")

	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	for _, want := range []string{
		"<!doctype html>",
		`href="/static/app.css"`,
		`src="/static/htmx.min.js"`,
		`src="/static/htmx-ext-sse.min.js"`,
		`src="/static/alpine.min.js"`,
		"test-1.2.3", // version stamped into the page
		"control room",
	} {
		if !strings.Contains(strings.ToLower(r.body), strings.ToLower(want)) {
			t.Errorf("home page missing %q", want)
		}
	}
}

// TestNavRoutesResolve guards the single-source-of-truth invariant: every nav destination
// in views.NavItems is a registered route that renders inside the chrome. A dead nav link
// would be a scaffold bug this catches.
func TestNavRoutesResolve(t *testing.T) {
	ts := newTestServer(t)
	if len(views.NavItems) == 0 {
		t.Fatal("views.NavItems is empty — nothing to route")
	}
	for _, item := range views.NavItems {
		r := get(t, ts, item.Href)
		if r.status != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", item.Href, r.status)
			continue
		}
		if !strings.Contains(r.body, item.Label) {
			t.Errorf("GET %s: body missing label %q", item.Href, item.Label)
		}
		// The nav itself must be present on every page (the layout wraps each view).
		if !strings.Contains(r.body, `href="/static/app.css"`) {
			t.Errorf("GET %s: not wrapped in the base layout", item.Href)
		}
	}
}

// TestStaticAssets verifies the embedded assets are served with the right content type and
// the immutable cache header, and that the Tailwind-compiled CSS carries real utility
// rules (so the build pipeline, not just the embed, is exercised).
func TestStaticAssets(t *testing.T) {
	ts := newTestServer(t)

	cases := []struct {
		path        string
		wantCType   string
		wantContent string
	}{
		{"/static/app.css", "text/css", "tailwindcss"},
		{"/static/htmx.min.js", "javascript", "htmx"},
		{"/static/alpine.min.js", "javascript", ""},
		{"/static/htmx-ext-sse.min.js", "javascript", "Server Sent Events"},
	}
	for _, tc := range cases {
		r := get(t, ts, tc.path)
		if r.status != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", tc.path, r.status)
			continue
		}
		if ct := r.header.Get("Content-Type"); !strings.Contains(ct, tc.wantCType) {
			t.Errorf("GET %s: Content-Type = %q, want to contain %q", tc.path, ct, tc.wantCType)
		}
		if cc := r.header.Get("Cache-Control"); !strings.Contains(cc, "max-age") {
			t.Errorf("GET %s: Cache-Control = %q, want a max-age", tc.path, cc)
		}
		if tc.wantContent != "" && !strings.Contains(r.body, tc.wantContent) {
			t.Errorf("GET %s: body missing %q", tc.path, tc.wantContent)
		}
	}
}

func TestHealthz(t *testing.T) {
	ts := newTestServer(t)
	r := get(t, ts, "/healthz")
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", r.status)
	}
	if strings.TrimSpace(r.body) != "ok" {
		t.Errorf("body = %q, want ok", r.body)
	}
}

// TestUnknownRoute confirms exact-match routing: an unregistered path 404s rather than
// silently rendering the home page (a real risk with a catch-all "/" handler).
func TestUnknownRoute(t *testing.T) {
	ts := newTestServer(t)
	r := get(t, ts, "/does-not-exist")
	if r.status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", r.status)
	}
}

// TestListenAndServeGracefulShutdown drives the real socket path: bind an ephemeral port,
// serve a request, then cancel the context and require a clean (nil) return. This is the
// `harness serve` lifecycle contract — Ctrl-C is a clean stop.
func TestListenAndServeGracefulShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // release the port; the server rebinds it.

	s := New(Options{Version: "test"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe(ctx, addr) }()

	// Poll until the server is accepting, then assert it serves.
	var resp *http.Response
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = http.Get("http://" + addr + "/healthz")
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server never came up: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListenAndServe returned %v, want nil on ctx cancel", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("ListenAndServe did not return after ctx cancel")
	}
}

// TestListenAndServeBindError surfaces a real bind failure (a bad address) as a non-nil
// return rather than a silent hang.
func TestListenAndServeBindError(t *testing.T) {
	s := New(Options{})
	err := s.ListenAndServe(context.Background(), "256.256.256.256:99999")
	if err == nil {
		t.Fatal("expected a bind error for an invalid address, got nil")
	}
}
