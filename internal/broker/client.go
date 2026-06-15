package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	"github.com/Loxstomper/harness/internal/model"
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
// runner attaches the key and the provider adapter for the invocation's soul.
func (c *Client) Complete(ctx context.Context, req model.Request) (model.Response, error) {
	var resp model.Response
	err := c.roundTrip(ctx, MethodCompletion, req, &resp)
	return resp, err
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
