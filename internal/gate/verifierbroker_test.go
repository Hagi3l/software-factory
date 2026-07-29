package gate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Loxstomper/software-factory/internal/broker"
	"github.com/Loxstomper/software-factory/internal/model"
)

// serveVerifier binds a temp unix socket, serves r.verifierBroker() on it, and returns a
// broker.Client dialed at it. It mirrors how provisionVerifier wires the broker to the
// sandbox, but without a sandbox backend — the wiring under test (which handler, which
// allowlist) is independent of the isolation layer.
func serveVerifier(t *testing.T, r *Runner) *broker.Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "gate.sock")
	ln, err := broker.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = r.verifierBroker().Serve(ctx, ln) }()
	return broker.NewClient("unix", sock)
}

// TestVerifierBrokerDenyAllByDefault: with no package proxy configured the verifier reaches
// nothing — every brokered method is denied, preserving the zero-I/O verifier invariant.
func TestVerifierBrokerDenyAllByDefault(t *testing.T) {
	c := serveVerifier(t, New(nil, nil, nil, "", nil, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := c.FetchPackage(ctx, broker.FetchPackageRequest{Path: "/m/@v/list"}); err == nil {
		t.Error("deny-all verifier must deny package fetch, got nil")
	}
	if _, err := c.Complete(ctx, model.Request{}); err == nil {
		t.Error("deny-all verifier must deny model completion, got nil")
	}
	if _, err := c.GitPush(ctx, broker.GitPushRequest{Branch: "candidate/x"}); err == nil {
		t.Error("deny-all verifier must deny git push, got nil")
	}
}

// TestVerifierBrokerFetchOnly: WithPackageProxy lets the verifier pull a candidate's new
// dependency to re-gate it (T5.6a) — and ONLY that. Model calls, git push, and event
// publish stay denied (producer != verifier).
func TestVerifierBrokerFetchOnly(t *testing.T) {
	var hit bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("v1.0.0\n"))
	}))
	defer proxy.Close()

	c := serveVerifier(t, New(nil, nil, nil, "", nil, nil, WithPackageProxy(proxy.URL)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := c.FetchPackage(ctx, broker.FetchPackageRequest{Path: "/github.com/pkg/errors/@v/list"})
	if err != nil {
		t.Fatalf("fetch-only verifier must allow package fetch: %v", err)
	}
	if !hit || res.Status != http.StatusOK || string(res.Body) != "v1.0.0\n" {
		t.Errorf("fetch not proxied: hit=%v res=%+v", hit, res)
	}

	if _, err := c.Complete(ctx, model.Request{}); err == nil {
		t.Error("fetch-only verifier must still deny model completion, got nil")
	}
	if _, err := c.GitPush(ctx, broker.GitPushRequest{Branch: "candidate/x"}); err == nil {
		t.Error("fetch-only verifier must still deny git push, got nil")
	}
	if err := c.PublishEvent(ctx, broker.PublishRequest{Type: "log"}); err == nil {
		t.Error("fetch-only verifier must still deny event publish, got nil")
	}
}
