package broker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Loxstomper/harness/internal/model"
)

// allDestinations is the egress set an operator typically configures; tests that want
// the gate to pass use it so they exercise the handler path, not the deny path.
var allDestinations = []string{"llm-api", "git", "nats"}

// fakeHandler records what it was called with and returns canned responses/errors. It
// stands in for the runner's real relay (plan T1.12) so the protocol and the
// deny-by-default gate can be tested without a model API, git, or NATS.
type fakeHandler struct {
	gotComplete  model.Request
	completeResp model.Response
	completeErr  error

	gotPush  GitPushRequest
	pushResp GitPushResult
	pushErr  error

	gotEvent PublishRequest
	eventErr error
}

func (f *fakeHandler) Complete(_ context.Context, req model.Request) (model.Response, error) {
	f.gotComplete = req
	return f.completeResp, f.completeErr
}

func (f *fakeHandler) GitPush(_ context.Context, req GitPushRequest) (GitPushResult, error) {
	f.gotPush = req
	return f.pushResp, f.pushErr
}

func (f *fakeHandler) PublishEvent(_ context.Context, ev PublishRequest) error {
	f.gotEvent = ev
	return f.eventErr
}

// pipeClient returns a Client wired to srv over a fresh in-memory pipe per call, so a
// round-trip can be exercised end-to-end (client framing -> server dispatch -> handler
// -> response framing) with no socket.
func pipeClient(srv *Server) *Client {
	c := NewClient("unix", "/test.sock")
	c.dial = func(ctx context.Context) (net.Conn, error) {
		cli, server := net.Pipe()
		go srv.handleConn(ctx, server)
		return cli, nil
	}
	return c
}

func TestCompleteRoundTrip(t *testing.T) {
	h := &fakeHandler{completeResp: model.Response{
		Text:  "done",
		Usage: model.Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 3},
		Stop:  model.StopEndTurn,
	}}
	c := pipeClient(NewServer(h, WithAllowlist(allDestinations)))

	req := model.Request{
		System: "be helpful",
		Messages: []model.Message{
			{Role: model.RoleUser, Text: "hi"},
		},
		Tools: []model.ToolDef{
			{Name: "read_file", Description: "read a file", Params: json.RawMessage(`{"type":"object"}`)},
		},
		MaxTokens: 1024,
	}
	got, err := c.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !reflect.DeepEqual(got, h.completeResp) {
		t.Errorf("response = %+v, want %+v", got, h.completeResp)
	}
	if !reflect.DeepEqual(h.gotComplete, req) {
		t.Errorf("handler got %+v, want %+v (request must survive the boundary unchanged)", h.gotComplete, req)
	}
}

func TestGitPushRoundTrip(t *testing.T) {
	h := &fakeHandler{pushResp: GitPushResult{Commit: "abc123"}}
	c := pipeClient(NewServer(h, WithAllowlist(allDestinations)))

	got, err := c.GitPush(context.Background(), GitPushRequest{Branch: "task/bd-1"})
	if err != nil {
		t.Fatalf("GitPush: %v", err)
	}
	if got.Commit != "abc123" {
		t.Errorf("commit = %q, want abc123", got.Commit)
	}
	if h.gotPush.Branch != "task/bd-1" {
		t.Errorf("handler got branch %q, want task/bd-1", h.gotPush.Branch)
	}
}

func TestPublishEventRoundTrip(t *testing.T) {
	h := &fakeHandler{}
	c := pipeClient(NewServer(h, WithAllowlist(allDestinations)))

	err := c.PublishEvent(context.Background(), PublishRequest{Type: "progress", Payload: json.RawMessage(`{"pct":50}`)})
	if err != nil {
		t.Fatalf("PublishEvent: %v", err)
	}
	if h.gotEvent.Type != "progress" || string(h.gotEvent.Payload) != `{"pct":50}` {
		t.Errorf("handler got %+v, want type=progress payload={\"pct\":50}", h.gotEvent)
	}
}

func TestHandlerErrorPropagates(t *testing.T) {
	h := &fakeHandler{completeErr: errors.New("model API 503")}
	c := pipeClient(NewServer(h, WithAllowlist(allDestinations)))

	_, err := c.Complete(context.Background(), model.Request{})
	var be *Error
	if !errors.As(err, &be) {
		t.Fatalf("error = %v, want *broker.Error", err)
	}
	if be.Code != CodeHandlerError {
		t.Errorf("code = %q, want %q", be.Code, CodeHandlerError)
	}
	if be.Message != "model API 503" {
		t.Errorf("message = %q, want the underlying error text", be.Message)
	}
}

func TestDispatchUnknownMethodRejected(t *testing.T) {
	s := NewServer(&fakeHandler{}, WithAllowlist(allDestinations))
	resp := s.dispatch(context.Background(), Request{Method: "evil.exfiltrate"})
	if resp.Error == nil || resp.Error.Code != CodeUnknownMethod {
		t.Fatalf("resp = %+v, want CodeUnknownMethod", resp.Error)
	}
}

func TestDispatchDeniesDestinationNotInAllowlist(t *testing.T) {
	// Allow git but not llm-api: a completion must be denied even though the method is known.
	s := NewServer(&fakeHandler{}, WithAllowlist([]string{"git"}))
	resp := s.dispatch(context.Background(), Request{Method: MethodCompletion, Params: json.RawMessage(`{}`)})
	if resp.Error == nil || resp.Error.Code != CodeDenied {
		t.Fatalf("resp = %+v, want CodeDenied", resp.Error)
	}
}

func TestDefaultServerDeniesEverything(t *testing.T) {
	// No WithAllowlist -> deny-by-default: every destination is rejected.
	s := NewServer(&fakeHandler{})
	for _, m := range []Method{MethodCompletion, MethodGitPush, MethodPublishEvent} {
		resp := s.dispatch(context.Background(), Request{Method: m, Params: json.RawMessage(`{}`)})
		if resp.Error == nil || resp.Error.Code != CodeDenied {
			t.Errorf("method %q: resp = %+v, want CodeDenied", m, resp.Error)
		}
	}
}

func TestDispatchBadParams(t *testing.T) {
	s := NewServer(&fakeHandler{}, WithAllowlist(allDestinations))
	resp := s.dispatch(context.Background(), Request{Method: MethodCompletion, Params: json.RawMessage(`not-json`)})
	if resp.Error == nil || resp.Error.Code != CodeBadRequest {
		t.Fatalf("resp = %+v, want CodeBadRequest", resp.Error)
	}
}

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := Request{Method: MethodGitPush, Params: json.RawMessage(`{"branch":"x"}`)}
	if err := writeFrame(&buf, in); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	var out Request
	if err := readFrame(&buf, &out); err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if out.Method != in.Method || string(out.Params) != string(in.Params) {
		t.Errorf("round-trip = %+v, want %+v", out, in)
	}
}

func TestReadFrameRejectsOversizeHeader(t *testing.T) {
	// A malicious peer claims a huge body; readFrame must reject on the size check
	// before allocating, even though only the 4-byte header is present.
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], maxFrameSize+1)
	var v map[string]any
	if err := readFrame(bytes.NewReader(hdr[:]), &v); err == nil {
		t.Fatal("expected error for oversize frame, got nil")
	}
}

func TestReadFrameTruncatedBody(t *testing.T) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 100) // claims 100 bytes...
	r := bytes.NewReader(append(hdr[:], []byte("only ten..")...))
	var v map[string]any
	if err := readFrame(r, &v); err == nil {
		t.Fatal("expected error for truncated body, got nil")
	}
}

func TestUnixSocketIntegration(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "b.sock")
	ln, err := Listen("unix", sock)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := &fakeHandler{
		completeResp: model.Response{Text: "ok", Stop: model.StopEndTurn},
		pushResp:     GitPushResult{Commit: "deadbeef"},
	}
	srv := NewServer(h, WithAllowlist(allDestinations))
	go func() { _ = srv.Serve(ctx, ln) }()

	c := NewClient("unix", sock)

	resp, err := c.Complete(ctx, model.Request{Messages: []model.Message{{Role: model.RoleUser, Text: "hi"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "ok" {
		t.Errorf("completion text = %q, want ok", resp.Text)
	}

	push, err := c.GitPush(ctx, GitPushRequest{Branch: "task/x"})
	if err != nil {
		t.Fatalf("GitPush: %v", err)
	}
	if push.Commit != "deadbeef" {
		t.Errorf("commit = %q, want deadbeef", push.Commit)
	}

	if err := c.PublishEvent(ctx, PublishRequest{Type: "log"}); err != nil {
		t.Fatalf("PublishEvent: %v", err)
	}

	// An unknown method over the real wire must come back denied, not forwarded.
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := writeFrame(conn, Request{Method: "evil"}); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	var raw Response
	if err := readFrame(conn, &raw); err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if raw.Error == nil || raw.Error.Code != CodeUnknownMethod {
		t.Errorf("unknown method over wire: resp = %+v, want CodeUnknownMethod", raw.Error)
	}
}

func TestListenRejectsUnsupportedNetwork(t *testing.T) {
	if _, err := Listen("vsock", "1:2"); err == nil {
		t.Error("expected vsock to be rejected (Firecracker, Phase 5)")
	}
	if _, err := Listen("tcp", "127.0.0.1:0"); err == nil {
		t.Error("expected non-unix network to be rejected")
	}
}
