package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"

	"github.com/Loxstomper/harness/internal/model"
)

// Handler performs the brokered calls. The broker package owns the protocol — the
// framing, the closed method set, and the deny-by-default gate — but not the actual
// relaying: a Handler is what holds the provider adapter + API key, the scoped git
// token, and the NATS connection. The runner supplies the real implementation (plan
// T1.12); tests supply a fake. Splitting it this way keeps the security-critical
// dispatch testable on its own and lets the relay logic land once adapters exist.
//
// A Handler error means the brokered call itself failed (the model API errored, the
// push was refused); the server turns it into a CodeHandlerError Response. It is NOT
// where allowlist or method-validity decisions happen — the server makes those before
// the Handler is ever called.
type Handler interface {
	Complete(ctx context.Context, req model.Request) (model.Response, error)
	GitPush(ctx context.Context, req GitPushRequest) (GitPushResult, error)
	PublishEvent(ctx context.Context, ev PublishRequest) error
	FetchPackage(ctx context.Context, req FetchPackageRequest) (FetchPackageResult, error)
}

// Server is the runner side of the broker: it accepts connections from the sandbox,
// decodes one Request per connection, enforces the deny-by-default gate, and calls
// the Handler. It is the single audited chokepoint for agent egress.
type Server struct {
	handler Handler
	allow   map[string]bool // destinations an operator has allowed (from config.BrokerConfig.Allowlist)
}

// ServerOption configures a Server.
type ServerOption func(*Server)

// WithAllowlist sets the egress destinations the server permits. A method whose
// destination (Method.destination) is absent is denied with CodeDenied. The runner
// sources this from infra.<env>.yaml broker.allowlist. With no allowlist the server
// denies every destination — deny-by-default is the secure default, not an oversight.
func WithAllowlist(destinations []string) ServerOption {
	return func(s *Server) {
		s.allow = make(map[string]bool, len(destinations))
		for _, d := range destinations {
			s.allow[d] = true
		}
	}
}

// NewServer builds a Server. Without WithAllowlist it denies all destinations.
func NewServer(h Handler, opts ...ServerOption) *Server {
	s := &Server{handler: h, allow: map[string]bool{}}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Listen binds the broker's local socket. It is split from Serve so a caller can bind
// synchronously (and know the socket exists) before handing the listener to Serve in a
// goroutine. Two transports are supported: unix (the Docker stand-in) and vsock (the
// Firecracker production channel — a microVM has no shared filesystem for a unix
// socket; see transport.go). For unix it first removes any stale socket file, since
// the runner is the sole owner of this ephemeral per-invocation path.
func Listen(network, address string) (net.Listener, error) {
	switch network {
	case "unix":
		if err := os.Remove(address); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("broker: clear stale socket %s: %w", address, err)
		}
		ln, err := net.Listen(network, address)
		if err != nil {
			return nil, fmt.Errorf("broker: listen %s %s: %w", network, address, err)
		}
		return ln, nil
	case "vsock":
		return listenVsock(address)
	default:
		return nil, fmt.Errorf("broker: unsupported network %q", network)
	}
}

// Serve accepts connections on ln until ctx is canceled or ln is closed, handling each
// in its own goroutine. Canceling ctx closes ln, which unblocks Accept; the resulting
// post-close Accept error is treated as a clean shutdown rather than surfaced.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil //nolint:nilerr // ctx cancellation closed ln; this Accept error is a clean shutdown, not a failure
			}
			return fmt.Errorf("broker: accept: %w", err)
		}
		go s.handleConn(ctx, conn)
	}
}

// handleConn serves exactly one request on conn, then closes it. One request per
// connection keeps responses unambiguous without any in-band correlation: the reply
// on a connection is the reply to its single request. If the request frame cannot even
// be read (the untrusted peer sent garbage), the server still attempts a CodeBadRequest
// reply before dropping the connection.
func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	var req Request
	if err := readFrame(conn, &req); err != nil {
		_ = writeFrame(conn, errorResponse(CodeBadRequest, err.Error()))
		return
	}
	_ = writeFrame(conn, s.dispatch(ctx, req))
}

// dispatch is the deny-by-default gate. It rejects an unknown method, then a method
// whose destination the allowlist does not permit, and only then decodes the params
// and calls the Handler. No path reaches the Handler without passing both checks.
func (s *Server) dispatch(ctx context.Context, req Request) Response {
	dest := req.Method.destination()
	if dest == "" {
		return errorResponse(CodeUnknownMethod, fmt.Sprintf("unknown method %q", req.Method))
	}
	if !s.allow[dest] {
		return errorResponse(CodeDenied, fmt.Sprintf("destination %q not in allowlist", dest))
	}

	switch req.Method {
	case MethodCompletion:
		var mr model.Request
		if err := json.Unmarshal(req.Params, &mr); err != nil {
			return errorResponse(CodeBadRequest, fmt.Sprintf("completion params: %v", err))
		}
		resp, err := s.handler.Complete(ctx, mr)
		if err != nil {
			return errorResponse(CodeHandlerError, err.Error())
		}
		return resultResponse(resp)

	case MethodGitPush:
		var pr GitPushRequest
		if err := json.Unmarshal(req.Params, &pr); err != nil {
			return errorResponse(CodeBadRequest, fmt.Sprintf("git push params: %v", err))
		}
		res, err := s.handler.GitPush(ctx, pr)
		if err != nil {
			return errorResponse(CodeHandlerError, err.Error())
		}
		return resultResponse(res)

	case MethodPublishEvent:
		var ev PublishRequest
		if err := json.Unmarshal(req.Params, &ev); err != nil {
			return errorResponse(CodeBadRequest, fmt.Sprintf("publish params: %v", err))
		}
		if err := s.handler.PublishEvent(ctx, ev); err != nil {
			return errorResponse(CodeHandlerError, err.Error())
		}
		return Response{} // fire-and-forget: ack with an empty body

	case MethodFetchPackage:
		var fr FetchPackageRequest
		if err := json.Unmarshal(req.Params, &fr); err != nil {
			return errorResponse(CodeBadRequest, fmt.Sprintf("package fetch params: %v", err))
		}
		res, err := s.handler.FetchPackage(ctx, fr)
		if err != nil {
			return errorResponse(CodeHandlerError, err.Error())
		}
		return resultResponse(res)
	}

	// Unreachable: every method with a non-empty destination is handled above.
	return errorResponse(CodeUnknownMethod, fmt.Sprintf("unhandled method %q", req.Method))
}

func errorResponse(code, msg string) Response {
	return Response{Error: &Error{Code: code, Message: msg}}
}

// resultResponse marshals a method result into a Response. A marshal failure is a bug
// in a result type (it controls its own shape), so it surfaces as a handler error.
func resultResponse(v any) Response {
	b, err := json.Marshal(v)
	if err != nil {
		return errorResponse(CodeHandlerError, fmt.Sprintf("marshal result: %v", err))
	}
	return Response{Result: b}
}
