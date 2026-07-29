// Package packageproxy is the host-side Go module-proxy egress shared by the runner's
// broker relay (the agent's sandbox) and the gate's verification sandbox. It is the one
// place the brokered package fetch is implemented — validate the untrusted request path,
// confine it to the configured proxy host, perform the logged HTTPS GET, and cap the body
// to one broker frame — so the producer's fetch and the verifier's re-gating fetch can
// never drift in what they permit or how they bound a response (specs/security.md Control 2,
// specs/verification.md). It deliberately does no telemetry or logging: each caller wraps a
// Fetch in its own tool-call span so the egress is observable in the right trace (the
// invocation trace for the relay, the gate-run trace for the verifier).
package packageproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Loxstomper/software-factory/internal/broker"
)

// DefaultTimeout bounds one package-proxy egress round-trip; together with MaxBytes it
// keeps a single fetch from hanging an invocation or overflowing one broker frame. A
// fetch must fail fast and free the broker connection — the wall budget is only a backstop.
const DefaultTimeout = 60 * time.Second

// MaxBytes caps the proxied body. The broker frames a whole response as one
// length-prefixed message capped at maxFrameSize (64 MiB), and base64 over JSON inflates
// the body ~1.33x, so the raw body must stay well under that. 32 MiB covers the vast
// majority of module zips; a larger module fails the fetch loudly rather than overflowing
// the frame (documented limitation — the broker protocol is one-shot, not streamed).
const MaxBytes = 32 << 20

// Fetcher performs the brokered package-proxy egress for a zero-network sandbox. The
// runner host (or the gate's controlling host) holds the network, so the fetch happens
// here and is the single audited chokepoint; the sandbox only ever sees the bytes.
// Integrity is not this layer's job — go.sum + the checksum DB (proxied through the same
// path) pin every module, and the qa gate scans post-fetch.
type Fetcher struct {
	base   string       // trusted proxy base URL (proxy.golang.org by default); empty = not configured
	client *http.Client // bounded client; the egress seam unit tests point at an httptest server
}

// NewFetcher builds a Fetcher forwarding to base. A nil client defaults to a bounded one
// (DefaultTimeout). An empty base yields a Fetcher whose Fetch errors "no package proxy
// configured", so callers can construct unconditionally and let Fetch report the disabled
// egress rather than branching at the call site.
func NewFetcher(base string, client *http.Client) *Fetcher {
	if client == nil {
		client = &http.Client{Timeout: DefaultTimeout}
	}
	return &Fetcher{base: base, client: client}
}

// Fetch forwards one Go module-proxy GET to the configured proxy and returns the upstream
// status/body. The path is untrusted (it comes from the in-sandbox GOPROXY shim), so it is
// validated and URL-joined onto the trusted base rather than concatenated — the request can
// never escape the proxy host. A nil receiver or an empty base errors rather than dialing
// nothing, so an unconfigured egress fails loudly.
func (f *Fetcher) Fetch(ctx context.Context, req broker.FetchPackageRequest) (broker.FetchPackageResult, error) {
	if f == nil || f.base == "" {
		return broker.FetchPackageResult{}, fmt.Errorf("package fetch: no package proxy configured")
	}
	if err := ValidatePath(req.Path); err != nil {
		return broker.FetchPackageResult{}, err
	}

	// Confine the request to the proxy host by construction: parse the trusted base and join
	// the untrusted (but validated) path onto it, rather than string-concatenating a URL the
	// path could hijack with a scheme or authority.
	base, err := url.Parse(f.base)
	if err != nil {
		return broker.FetchPackageResult{}, fmt.Errorf("package fetch: invalid proxy base %q: %w", f.base, err)
	}
	target := *base
	target.Path = strings.TrimRight(base.Path, "/") + pathOnly(req.Path)
	target.RawQuery = queryOnly(req.Path)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return broker.FetchPackageResult{}, fmt.Errorf("package fetch: build request: %w", err)
	}
	resp, err := f.client.Do(httpReq)
	if err != nil {
		return broker.FetchPackageResult{}, fmt.Errorf("package fetch %s: %w", req.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBytes+1))
	if err != nil {
		return broker.FetchPackageResult{}, fmt.Errorf("package fetch %s: read body: %w", req.Path, err)
	}
	if len(body) > MaxBytes {
		return broker.FetchPackageResult{}, fmt.Errorf("package fetch %s: response exceeds %d bytes (too large for one broker frame)", req.Path, MaxBytes)
	}
	return broker.FetchPackageResult{
		Status:      resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Body:        body,
	}, nil
}

// ValidatePath rejects a package-fetch path that is not a plain absolute request path.
// The path comes from the untrusted sandbox; though it is confined to the proxy host by
// URL-joining (Fetch), a malformed one (scheme, "..", control chars) is refused here so it
// never reaches the egress at all — defense in depth at the chokepoint.
func ValidatePath(p string) error {
	if p == "" || !strings.HasPrefix(p, "/") {
		return fmt.Errorf("package fetch: path %q must be an absolute request path", p)
	}
	if strings.Contains(p, "://") {
		return fmt.Errorf("package fetch: path %q must not contain a scheme", p)
	}
	if strings.Contains(p, "..") {
		return fmt.Errorf("package fetch: path %q must not contain %q", p, "..")
	}
	for _, c := range p {
		if c <= ' ' || c == 0x7f {
			return fmt.Errorf("package fetch: path %q contains a control or space character", p)
		}
	}
	return nil
}

// pathOnly / queryOnly split a forwarded module-proxy path into its path and query halves.
// The GOPROXY shim forwards `go`'s request as "<path>[?<query>]"; the sumdb tile/lookup
// endpoints can carry a query, the module endpoints do not.
func pathOnly(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

func queryOnly(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[i+1:]
	}
	return ""
}
