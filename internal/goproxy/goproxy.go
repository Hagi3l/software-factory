// Package goproxy is the in-sandbox GOPROXY shim (T5.6). It runs INSIDE the zero-network
// sandbox, bound to loopback, and bridges `go`'s module-proxy requests to the runner's
// broker over the bind-mounted broker socket — the one egress chokepoint.
//
// Why a shim at all: a sandbox has no direct network, so `go mod download` cannot reach
// proxy.golang.org. `go` can only target a GOPROXY that is an http(s) URL or a file path —
// it cannot speak the runner's framed broker RPC, and it cannot dial a unix socket. So the
// shim is the smallest possible adapter: an HTTP server `go` points GOPROXY at, which
// forwards each request to the runner as a broker package.fetch call. The runner (the
// trusted host side) performs the real fetch against the package proxy and logs it; the
// sandbox never holds the proxy URL or any network route.
//
// Everything the shim does is one Fetcher call per request; it holds no policy of its own.
// The egress allowlist, the proxy base, and the supply-chain guarantees all live on the
// runner — deny-by-default there means a fetch the operator has not allowed is rejected
// before it leaves the host, and the shim simply relays that rejection back to `go`.
package goproxy

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/Loxstomper/software-factory/internal/broker"
)

// Fetcher is the single broker call the shim makes for each module-proxy request.
// *broker.Client satisfies it; kept minimal so the Handler is unit-testable with a fake.
type Fetcher interface {
	FetchPackage(ctx context.Context, req broker.FetchPackageRequest) (broker.FetchPackageResult, error)
}

// Handler returns the http.Handler that serves the Go module-proxy protocol by forwarding
// each GET to the runner's broker. It is path-agnostic: module endpoints (/@v/list,
// /@v/<ver>.info|.mod|.zip, /@latest) and the checksum-DB endpoints (/sumdb/...) all flow
// through unchanged, so go.sum + checksum-DB pinning keep working through the same chokepoint.
//
// Only GET is served (the module-proxy protocol is read-only); anything else is 405. A
// broker error (the proxy is denied by the allowlist, or the fetch failed) becomes a 502 so
// `go` reports a clear failure rather than misreading a partial body — deny-by-default at
// the runner surfaces here as a failed fetch, which is exactly right.
func Handler(f Fetcher, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "goproxy: only GET is supported", http.StatusMethodNotAllowed)
			return
		}

		// Forward the request verbatim as "<escaped-path>[?<query>]". EscapedPath preserves the
		// module-path escaping go uses (e.g. uppercase encoded as "!a"); the sumdb tile/lookup
		// endpoints carry a query, the module endpoints do not.
		reqPath := r.URL.EscapedPath()
		if r.URL.RawQuery != "" {
			reqPath += "?" + r.URL.RawQuery
		}

		res, err := f.FetchPackage(r.Context(), broker.FetchPackageRequest{Path: reqPath})
		if err != nil {
			log.WarnContext(r.Context(), "goproxy: broker fetch failed", "path", reqPath, "err", err)
			http.Error(w, "goproxy: "+err.Error(), http.StatusBadGateway)
			return
		}

		if res.ContentType != "" {
			w.Header().Set("Content-Type", res.ContentType)
		}
		status := res.Status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if _, err := w.Write(res.Body); err != nil {
			log.DebugContext(r.Context(), "goproxy: write response", "path", reqPath, "err", err)
		}
	})
	return mux
}
