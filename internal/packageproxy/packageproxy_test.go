package packageproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Loxstomper/software-factory/internal/broker"
)

func TestFetchProxiesPathAndEchoesBody(t *testing.T) {
	var gotPath, gotQuery string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("v1.0.0\nv1.1.0\n"))
	}))
	defer proxy.Close()

	f := NewFetcher(proxy.URL, proxy.Client())
	// A sumdb lookup carries a query — it must flow through too (pinning is path-agnostic).
	res, err := f.Fetch(context.Background(), broker.FetchPackageRequest{Path: "/sumdb/sum.golang.org/lookup/x@v1.0.0?foo=bar"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if gotPath != "/sumdb/sum.golang.org/lookup/x@v1.0.0" || gotQuery != "foo=bar" {
		t.Errorf("proxy got path %q query %q, want the request path+query joined onto the base", gotPath, gotQuery)
	}
	if res.Status != http.StatusOK || string(res.Body) != "v1.0.0\nv1.1.0\n" {
		t.Errorf("result = %+v, want status 200 and the proxied body", res)
	}
	if !strings.HasPrefix(res.ContentType, "text/plain") {
		t.Errorf("content-type = %q, want the upstream text/plain echoed", res.ContentType)
	}
}

func TestFetchEchoesUpstreamStatus(t *testing.T) {
	// 404/410 must be echoed, not swallowed: go reads them as "not found, try the next proxy".
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer proxy.Close()

	f := NewFetcher(proxy.URL, proxy.Client())
	res, err := f.Fetch(context.Background(), broker.FetchPackageRequest{Path: "/x/@v/v9.9.9.info"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Status != http.StatusGone {
		t.Errorf("status = %d, want 410 echoed from upstream", res.Status)
	}
}

func TestFetchNotConfigured(t *testing.T) {
	// An empty base (and a nil Fetcher) must error rather than dial nothing.
	if _, err := NewFetcher("", nil).Fetch(context.Background(), broker.FetchPackageRequest{Path: "/x/@v/list"}); err == nil {
		t.Error("empty-base Fetch must error, got nil")
	}
	var f *Fetcher
	if _, err := f.Fetch(context.Background(), broker.FetchPackageRequest{Path: "/x/@v/list"}); err == nil {
		t.Error("nil-receiver Fetch must error, got nil")
	}
}

func TestFetchRejectsMalformedPathBeforeAnyEgress(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("malformed path must be rejected before any egress")
	}))
	defer proxy.Close()

	f := NewFetcher(proxy.URL, proxy.Client())
	for _, bad := range []string{"", "no-leading-slash", "/has/../traversal", "https://evil.example/x", "/has space"} {
		if _, err := f.Fetch(context.Background(), broker.FetchPackageRequest{Path: bad}); err == nil {
			t.Errorf("path %q: want rejection, got nil error", bad)
		}
	}
}

func TestFetchCapsOversizeBody(t *testing.T) {
	// A body larger than MaxBytes fails loudly rather than overflowing one broker frame.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, MaxBytes+1))
	}))
	defer proxy.Close()

	f := NewFetcher(proxy.URL, proxy.Client())
	if _, err := f.Fetch(context.Background(), broker.FetchPackageRequest{Path: "/big/@v/v1.0.0.zip"}); err == nil {
		t.Error("oversize body must error, got nil")
	}
}

func TestValidatePath(t *testing.T) {
	ok := []string{"/github.com/pkg/errors/@v/list", "/sumdb/sum.golang.org/lookup/x@v1.0.0", "/m/@latest"}
	for _, p := range ok {
		if err := ValidatePath(p); err != nil {
			t.Errorf("ValidatePath(%q) = %v, want nil", p, err)
		}
	}
	bad := []string{"", "rel", "/a/../b", "scheme://x", "/has\tcontrol"}
	for _, p := range bad {
		if err := ValidatePath(p); err == nil {
			t.Errorf("ValidatePath(%q) = nil, want error", p)
		}
	}
}
