package goproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Loxstomper/harness/internal/broker"
)

// fakeFetcher records the forwarded request and returns a canned broker result/error,
// standing in for the runner's relay reached over the broker.
type fakeFetcher struct {
	got broker.FetchPackageRequest
	res broker.FetchPackageResult
	err error
}

func (f *fakeFetcher) FetchPackage(_ context.Context, req broker.FetchPackageRequest) (broker.FetchPackageResult, error) {
	f.got = req
	return f.res, f.err
}

func TestHandlerForwardsGETAndWritesResponse(t *testing.T) {
	f := &fakeFetcher{res: broker.FetchPackageResult{Status: 200, ContentType: "text/plain", Body: []byte("v0.9.1\n")}}
	srv := httptest.NewServer(Handler(f, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/pkg/errors/@v/list")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if f.got.Path != "/github.com/pkg/errors/@v/list" {
		t.Errorf("forwarded path = %q, want the request path verbatim", f.got.Path)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (upstream status echoed)", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain" {
		t.Errorf("content-type = %q, want text/plain (echoed)", ct)
	}
	if string(body) != "v0.9.1\n" {
		t.Errorf("body = %q, want the upstream body", body)
	}
}

func TestHandlerForwardsQuery(t *testing.T) {
	// sumdb lookups carry a query; it must reach the broker so checksum-DB pinning works.
	f := &fakeFetcher{res: broker.FetchPackageResult{Status: 200, Body: []byte("ok")}}
	srv := httptest.NewServer(Handler(f, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/sumdb/sum.golang.org/tile/8/0/x123?foo=bar")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if f.got.Path != "/sumdb/sum.golang.org/tile/8/0/x123?foo=bar" {
		t.Errorf("forwarded path = %q, want path?query preserved", f.got.Path)
	}
}

func TestHandlerRejectsNonGET(t *testing.T) {
	f := &fakeFetcher{}
	srv := httptest.NewServer(Handler(f, nil))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/x/@v/list", "text/plain", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405 (module proxy is read-only)", resp.StatusCode)
	}
	if f.got.Path != "" {
		t.Errorf("non-GET must not reach the broker, got path %q", f.got.Path)
	}
}

func TestHandlerBrokerErrorIs502(t *testing.T) {
	// A denied/failed fetch at the runner surfaces as a 502 so `go` reports a clear failure.
	f := &fakeFetcher{err: &broker.Error{Code: broker.CodeDenied, Message: "destination not in allowlist"}}
	srv := httptest.NewServer(Handler(f, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/x/@v/list")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 on a broker error", resp.StatusCode)
	}
}

func TestHandlerDefaultsZeroStatusTo200(t *testing.T) {
	f := &fakeFetcher{res: broker.FetchPackageResult{Body: []byte("x")}}
	srv := httptest.NewServer(Handler(f, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/x/@latest")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 when the result carries no status", resp.StatusCode)
	}
}
