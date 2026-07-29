package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	"github.com/Loxstomper/software-factory/internal/model"
)

// Client is the agent side of the broker, used from inside the sandbox. It dials the
// runner's local socket and makes one round-trip per call. It holds no credentials and
// is unaware of which provider, remote, or NATS subject ultimately serves a call —
// that is the whole point of the broker boundary (see specs/components/runner.md).
type Client struct {
	network string
	address string
	// dial opens one connection to the runner. It is a seam so the round-trip logic can
	// be tested over an in-memory pipe without a real socket; the default dials the
	// configured endpoint.
	dial func(ctx context.Context) (net.Conn, error)
}

// NewClient builds a Client that dials the given local endpoint. For Docker the
// endpoint is the unix socket bind-mounted into the sandbox; for Firecracker it is the
// AF_VSOCK channel ("<cid>:<port>"). dialContext (transport.go) interprets the network,
// so the Client itself is transport-agnostic. The dial happens per call, not at
// construction.
func NewClient(network, address string) *Client {
	return &Client{
		network: network,
		address: address,
		dial: func(ctx context.Context) (net.Conn, error) {
			return dialContext(ctx, network, address)
		},
	}
}

// Complete relays a canonical model request and returns the canonical response. The
// runner attaches the key and the provider adapter for the invocation's soul (the parent
// sub-context). The explore tool's sub-loop uses ExploreCompleter instead so its calls are
// routed to the pinned explorer model and metered against the explore sub-budget.
func (c *Client) Complete(ctx context.Context, req model.Request) (model.Response, error) {
	return c.complete(ctx, SubContextParent, "", req)
}

// complete relays one tagged completion. The tag (and, for the explorer, the per-call stream)
// travels in CompletionParams so the runner routes to the right adapter and budget; the agent
// itself stays provider- and tier-unaware — it never names a model, only a sub-context.
func (c *Client) complete(ctx context.Context, sub SubContext, stream string, req model.Request) (model.Response, error) {
	var resp model.Response
	err := c.roundTrip(ctx, MethodCompletion, CompletionParams{SubContext: sub, Stream: stream, Request: req}, &resp)
	return resp, err
}

// ExploreCompleter returns a Completer whose every call is tagged to the explorer sub-context
// and the given per-call stream id. The explore tool mints one per explore call (a fresh stream
// so the runner's sub-budget resets per call) and drives its read-only sub-loop through it — so
// those completions run on the runner-pinned explorer model, metered against policy.explore_budget,
// with no way for the sandbox to reach a stronger tier (T12.2). The returned type satisfies the
// agent's Completer seam structurally.
func (c *Client) ExploreCompleter(stream string) *SubCompleter {
	return &SubCompleter{c: c, sub: SubContextExplorer, stream: stream}
}

// SubCompleter is a Completer bound to a fixed sub-context tag (and, for the explorer, a per-call
// stream). It exists so a helper sub-loop can drive its own model calls through the same broker
// while the runner routes and meters them independently of the parent stream.
type SubCompleter struct {
	c      *Client
	sub    SubContext
	stream string
}

// Complete relays a completion tagged to this sub-completer's sub-context and stream.
func (s *SubCompleter) Complete(ctx context.Context, req model.Request) (model.Response, error) {
	return s.c.complete(ctx, s.sub, s.stream, req)
}

// GitPush asks the runner to push the candidate branch. Only the task branch is
// pushable; any other branch is refused by the runner.
func (c *Client) GitPush(ctx context.Context, req GitPushRequest) (GitPushResult, error) {
	var res GitPushResult
	err := c.roundTrip(ctx, MethodGitPush, req, &res)
	return res, err
}

// PublishEvent emits a best-effort progress/log event. It returns once the runner has
// accepted the event for publication; a nil error does not guarantee delivery (events
// are fire-and-forget — see specs/messaging.md).
func (c *Client) PublishEvent(ctx context.Context, ev PublishRequest) error {
	return c.roundTrip(ctx, MethodPublishEvent, ev, nil)
}

// FetchPackage proxies one Go module-proxy GET through the runner to the package proxy on
// the broker allowlist. The runner prepends the configured proxy base to req.Path, fetches,
// and returns the upstream status/body. Used by the in-sandbox GOPROXY shim (internal/goproxy),
// not the agent loop directly.
func (c *Client) FetchPackage(ctx context.Context, req FetchPackageRequest) (FetchPackageResult, error) {
	var res FetchPackageResult
	err := c.roundTrip(ctx, MethodFetchPackage, req, &res)
	return res, err
}

// roundTrip performs one request/response exchange on a fresh connection: marshal the
// params, write the request frame, read the response frame, and either return the
// broker's Error or decode the result into out (which may be nil for ack-only calls).
func (c *Client) roundTrip(ctx context.Context, method Method, params, out any) error {
	pb, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("broker: marshal %s params: %w", method, err)
	}

	conn, err := c.dial(ctx)
	if err != nil {
		return fmt.Errorf("broker: dial %s %s: %w", c.network, c.address, err)
	}
	defer func() { _ = conn.Close() }()

	if err := writeFrame(conn, Request{Method: method, Params: pb}); err != nil {
		return err
	}
	var resp Response
	if err := readFrame(conn, &resp); err != nil {
		return err
	}
	if resp.Error != nil {
		return resp.Error
	}
	if out != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("broker: unmarshal %s result: %w", method, err)
		}
	}
	return nil
}
