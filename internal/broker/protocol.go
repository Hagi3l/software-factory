package broker

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Method names a brokered call. The set is closed and deny-by-default: the server
// dispatches only the methods named here, and only after the method's destination
// passes the egress allowlist. Anything else is rejected, never forwarded — this is
// the single audited chokepoint for all agent egress (see specs/components/runner.md,
// specs/security.md).
type Method string

const (
	// MethodCompletion relays a canonical model.Request to the provider and returns a
	// canonical model.Response. Destination: the LLM API.
	MethodCompletion Method = "model.completion"
	// MethodGitPush pushes the candidate branch the agent produced. Destination: git.
	MethodGitPush Method = "git.push"
	// MethodPublishEvent emits a best-effort progress/log event. Destination: NATS.
	MethodPublishEvent Method = "event.publish"
	// MethodFetchPackage proxies one Go module-proxy GET to the package proxy on the
	// broker allowlist (public proxy.golang.org by default). Destination: package-proxy.
	// It is how a zero-network sandbox fetches a dependency it does not already have
	// cached — every pull mediated and logged at the runner, the one egress chokepoint
	// (see specs/security.md Control 2, specs/components/runner.md). The agent never
	// speaks this directly: the in-sandbox GOPROXY shim (internal/goproxy) forwards `go`'s
	// module-proxy requests over it.
	MethodFetchPackage Method = "package.fetch"
)

// destination is the egress allowlist token a method maps to. The tokens match the
// names operators list in infra.<env>.yaml's broker.allowlist (config.BrokerConfig),
// so the server can deny a method whose destination an operator has not allowed. An
// unknown method maps to "" and is rejected as unknown rather than as denied.
func (m Method) destination() string {
	switch m {
	case MethodCompletion:
		return "llm-api"
	case MethodGitPush:
		return "git"
	case MethodPublishEvent:
		return "nats"
	case MethodFetchPackage:
		return "package-proxy"
	default:
		return ""
	}
}

// Request is the wire envelope an agent sends. Params is the method-specific payload
// as raw JSON so the envelope stays method-agnostic — the server decodes Params into
// the concrete request type only after it has decided the method is allowed.
type Request struct {
	Method Method          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is the wire envelope the runner returns. Exactly one of Result/Error is
// meaningful: on success Result carries the method's marshaled result (empty for
// fire-and-forget calls); on failure Error is set and Result is nil.
type Response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// Error is a brokered-call failure delivered back to the agent. It implements error
// so a client can return it directly. Code is machine-readable; Message is detail.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("broker: %s: %s", e.Code, e.Message) }

// Error codes. These distinguish a rejection by the broker itself (unknown method,
// denied destination, malformed request) from a failure of the brokered call (the
// model API errored, the push was refused), so the agent can react appropriately.
const (
	CodeUnknownMethod = "unknown_method" // method not in the closed set
	CodeDenied        = "denied"         // method's destination not in the egress allowlist
	CodeBadRequest    = "bad_request"    // unparseable envelope or params
	CodeHandlerError  = "handler_error"  // the brokered call itself failed
)

// GitPushRequest asks the runner to push the candidate branch the agent produced.
// This is the only brokered git operation, and the runner's per-task token can push
// ONLY the task branch — naming any other branch is refused by the runner (the agent
// never holds the token or the remote URL). See specs/security.md (scoped secrets).
type GitPushRequest struct {
	Branch string `json:"branch"` // the candidate/task branch to push
}

// GitPushResult reports the pushed head so the orchestrator can record it in the
// Result envelope and the provenance trailer (see specs/security.md).
type GitPushResult struct {
	Commit string `json:"commit"` // sha of the pushed branch head
}

// PublishRequest is a best-effort progress/log event. The runner maps it onto the
// agent's harness.agent.<id>.events subject — the agent holds no NATS credentials and
// does not know its own subject. Delivery is fire-and-forget; losing one is harmless
// (see specs/messaging.md), so the success response carries no body.
type PublishRequest struct {
	Type    string          `json:"type"`              // event kind, e.g. "progress" | "log"
	Payload json.RawMessage `json:"payload,omitempty"` // event body, opaque to the broker
}

// FetchPackageRequest is one Go module-proxy GET, forwarded verbatim by the in-sandbox
// GOPROXY shim (internal/goproxy). Path is the request path the `go` client asked for,
// e.g. "/github.com/pkg/errors/@v/list", "/github.com/pkg/errors/@v/v0.9.1.zip", or a
// "/sumdb/sum.golang.org/lookup/..." checksum-DB query: the runner prepends the configured
// proxy base and performs the fetch. Only the path crosses the boundary — the sandbox never
// learns the proxy URL, consistent with the broker hiding every destination from the agent.
type FetchPackageRequest struct {
	Path string `json:"path"`
}

// FetchPackageResult is the proxied response. Status is the upstream HTTP status echoed
// back so the `go` client sees a real 200/404/410 (go treats 404/410 as "not found, try
// the next proxy" and any other non-2xx as a hard error). Body is the response bytes
// (base64 over JSON); ContentType is echoed so go parses the payload correctly. The body is
// bounded by the runner (see maxPackageBytes in the relay) so it fits one broker frame.
type FetchPackageResult struct {
	Status      int    `json:"status"`
	ContentType string `json:"content_type,omitempty"`
	Body        []byte `json:"body,omitempty"`
}

// maxFrameSize caps a single length-prefixed frame. The server reads frames from the
// untrusted sandbox, so the length prefix is attacker-controlled: the cap is checked
// BEFORE allocating the buffer, so a malicious header cannot make the runner allocate
// arbitrary memory. 64 MiB comfortably holds a long agent conversation while bounding
// the blast radius.
const maxFrameSize = 64 << 20

// writeFrame marshals v to JSON and writes it as a 4-byte big-endian length prefix
// followed by the bytes. JSON keeps the protocol debuggable and lets canonical model
// types cross the boundary unchanged; the length prefix makes each call one
// self-delimited frame on the stream.
func writeFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("broker: marshal frame: %w", err)
	}
	if len(b) > maxFrameSize {
		return fmt.Errorf("broker: frame too large: %d bytes (max %d)", len(b), maxFrameSize)
	}
	var hdr [4]byte
	// #nosec G115 -- len(b) is bounded by the maxFrameSize check above (64 MiB), far
	// below math.MaxUint32, so this conversion cannot overflow or truncate.
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("broker: write frame header: %w", err)
	}
	if _, err := w.Write(b); err != nil {
		return fmt.Errorf("broker: write frame body: %w", err)
	}
	return nil
}

// readFrame reads one length-prefixed frame and unmarshals it into v. It rejects an
// oversized length before allocating (see maxFrameSize) and uses io.ReadFull so a
// short read on a closed or stalled connection is a clear error rather than silent
// truncation.
func readFrame(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return fmt.Errorf("broker: read frame header: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrameSize {
		return fmt.Errorf("broker: frame too large: %d bytes (max %d)", n, maxFrameSize)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return fmt.Errorf("broker: read frame body: %w", err)
	}
	if err := json.Unmarshal(buf, v); err != nil {
		return fmt.Errorf("broker: unmarshal frame: %w", err)
	}
	return nil
}
