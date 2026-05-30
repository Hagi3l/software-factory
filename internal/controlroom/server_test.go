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

	"github.com/Loxstomper/harness/internal/controlroom/live"
	"github.com/Loxstomper/harness/internal/controlroom/query"
	"github.com/Loxstomper/harness/internal/controlroom/views"
	"github.com/Loxstomper/harness/internal/core"
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

// TestActivityNotAttached proves the activity view degrades gracefully with no live
// source (standalone `harness serve`): the page renders a notice (200, inside the
// chrome) and the data fragment answers 503 — never a blank page or a hang.
func TestActivityNotAttached(t *testing.T) {
	s := New(Options{})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	r := get(t, ts, "/activity")
	if r.status != http.StatusOK {
		t.Fatalf("/activity status = %d, want 200", r.status)
	}
	if !strings.Contains(r.body, "Not attached") {
		t.Errorf("/activity missing not-attached notice, got: %s", r.body)
	}
	if !strings.Contains(r.body, `href="/static/app.css"`) {
		t.Errorf("/activity not wrapped in the base layout")
	}

	frag := get(t, ts, "/activity/items")
	if frag.status != http.StatusServiceUnavailable {
		t.Errorf("/activity/items status = %d, want 503", frag.status)
	}
}

// TestActivityRendersFeed proves the wired view renders the buffered events: the full
// page carries both a coalesced token line and a discrete progress event with the agent
// id, and the fragment endpoint returns just the rows (no page chrome) for the htmx swap.
func TestActivityRendersFeed(t *testing.T) {
	act := live.NewActivity(16)
	act.Record("inv-7", []byte(`{"type":"progress","payload":{"msg":"author-tests started"}}`))
	act.Record("inv-7", []byte(`{"type":"token","delta":"writing a test"}`))

	s := New(Options{Activity: act})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	r := get(t, ts, "/activity")
	if r.status != http.StatusOK {
		t.Fatalf("/activity status = %d, want 200", r.status)
	}
	for _, want := range []string{"author-tests started", "writing a test", "inv-7"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("/activity missing %q", want)
		}
	}

	frag := get(t, ts, "/activity/items")
	if frag.status != http.StatusOK {
		t.Fatalf("/activity/items status = %d, want 200", frag.status)
	}
	if strings.Contains(strings.ToLower(frag.body), "<!doctype html>") {
		t.Errorf("/activity/items should be a bare fragment, not a full page: %s", frag.body)
	}
	if !strings.Contains(frag.body, "writing a test") {
		t.Errorf("/activity/items missing rendered event, got: %s", frag.body)
	}
}

// TestDAGNotAttached proves the DAG view degrades gracefully with no read model (standalone
// `harness serve`): the page renders a notice (200, inside the chrome) and the SVG fragment
// answers 503 — never a blank page or a hang.
func TestDAGNotAttached(t *testing.T) {
	s := New(Options{})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	r := get(t, ts, "/dag")
	if r.status != http.StatusOK {
		t.Fatalf("/dag status = %d, want 200", r.status)
	}
	if !strings.Contains(r.body, "Not attached") {
		t.Errorf("/dag missing not-attached notice, got: %s", r.body)
	}
	if !strings.Contains(r.body, `href="/static/app.css"`) {
		t.Errorf("/dag not wrapped in the base layout")
	}

	frag := get(t, ts, "/dag/svg")
	if frag.status != http.StatusServiceUnavailable {
		t.Errorf("/dag/svg status = %d, want 503", frag.status)
	}
}

// TestDAGRendersGraph proves the wired view renders the dependency graph as SVG: the full
// page carries the node ids and the SVG element, and the fragment endpoint returns just the
// graph (no page chrome) for the htmx swap.
func TestDAGRendersGraph(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{
		{ID: "h-1", Title: "root", Status: "closed"},
		{ID: "h-2", Title: "child", Status: "open", DependsOn: []string{"h-1"}},
	}}
	reader := query.NewReader(issues, nil, nil)
	s := New(Options{Reader: reader})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	r := get(t, ts, "/dag")
	if r.status != http.StatusOK {
		t.Fatalf("/dag status = %d, want 200", r.status)
	}
	for _, want := range []string{"<svg", "h-1", "h-2", `data-node="h-2"`, `href="/issue/h-1"`} {
		if !strings.Contains(r.body, want) {
			t.Errorf("/dag missing %q", want)
		}
	}

	frag := get(t, ts, "/dag/svg")
	if frag.status != http.StatusOK {
		t.Fatalf("/dag/svg status = %d, want 200", frag.status)
	}
	if strings.Contains(strings.ToLower(frag.body), "<!doctype html>") {
		t.Errorf("/dag/svg should be a bare fragment, not a full page: %s", frag.body)
	}
	if !strings.Contains(frag.body, "<svg") {
		t.Errorf("/dag/svg missing the rendered svg, got: %s", frag.body)
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
