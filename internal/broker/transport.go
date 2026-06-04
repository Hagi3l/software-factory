package broker

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/mdlayher/vsock"
)

// The broker speaks the same one-request-per-connection protocol over two local
// transports: a unix domain socket (the Docker stand-in) and an AF_VSOCK socket
// (the production Firecracker channel — specs/components/runner.md). A microVM has
// no shared filesystem to bind-mount a unix socket into, so vsock is the only route
// in/out; modeling the transport as a (network, address) pair keeps every layer above
// — the runner, the sandbox, the agent's broker Client — transport-agnostic, so the
// Docker→Firecracker swap is a backend change, not a rewrite.
//
// A vsock address is "<cid>:<port>": the cid (context id) names the peer the *dialer*
// connects to (Host=2 for a guest agent reaching its host runner), and the port is the
// service. The listener binds its own machine's context id and uses only the port, so
// it ignores the cid half — but the same string is carried end to end so the agent and
// the runner agree on one endpoint value.

// parseVsockAddr splits a vsock endpoint "<cid>:<port>" into its numeric context id and
// port. Both halves are required and must be valid uint32s: unlike a unix path there is
// no lenient form to fall back on, so a malformed address is a hard error rather than
// something a backend discovers as a failed connect later.
func parseVsockAddr(address string) (cid uint32, port uint32, err error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return 0, 0, fmt.Errorf("broker: vsock address %q: want <cid>:<port>: %w", address, err)
	}
	c, err := strconv.ParseUint(host, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("broker: vsock cid %q: %w", host, err)
	}
	p, err := strconv.ParseUint(portStr, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("broker: vsock port %q: %w", portStr, err)
	}
	return uint32(c), uint32(p), nil
}

// listenVsock binds the broker's AF_VSOCK listener on the address's port. The listener
// binds the local machine's own context id (vsock.Listen infers it), so the cid half of
// the address — which only the dialer needs — is validated but otherwise unused here.
func listenVsock(address string) (net.Listener, error) {
	_, port, err := parseVsockAddr(address)
	if err != nil {
		return nil, err
	}
	ln, err := vsock.Listen(port, nil)
	if err != nil {
		return nil, fmt.Errorf("broker: listen vsock :%d: %w", port, err)
	}
	return ln, nil
}

// dialContext opens one connection to the broker over the configured transport. It is
// the single place the network string is interpreted on the dial side, mirroring the
// switch in Listen — so adding a transport touches exactly these two functions.
func dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	switch network {
	case "unix":
		var d net.Dialer
		return d.DialContext(ctx, network, address)
	case "vsock":
		return dialVsock(ctx, address)
	default:
		return nil, fmt.Errorf("broker: unsupported network %q", network)
	}
}

// dialVsock connects to "<cid>:<port>" over AF_VSOCK. vsock.Dial has no context-aware
// form, so the connect runs in a goroutine and the caller's ctx (deadline/cancel) is
// honored by racing it — a canceled invocation must not hang on a connect to a wedged
// or gone microVM. A connection that lands after cancellation is closed rather than
// leaked.
func dialVsock(ctx context.Context, address string) (net.Conn, error) {
	cid, port, err := parseVsockAddr(address)
	if err != nil {
		return nil, err
	}
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		conn, derr := vsock.Dial(cid, port, nil)
		if derr != nil {
			ch <- result{nil, derr}
			return
		}
		ch <- result{conn, nil}
	}()
	select {
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.conn != nil {
				_ = r.conn.Close()
			}
		}()
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("broker: dial vsock %s: %w", address, r.err)
		}
		return r.conn, nil
	}
}
