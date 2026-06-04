package broker

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Loxstomper/harness/internal/model"
	"github.com/mdlayher/vsock"
)

func TestParseVsockAddr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in       string
		wantCID  uint32
		wantPort uint32
		wantErr  bool
	}{
		{in: "2:5000", wantCID: 2, wantPort: 5000},
		{in: "1:0", wantCID: 1, wantPort: 0},
		{in: "4294967295:65535", wantCID: 4294967295, wantPort: 65535},
		{in: "", wantErr: true},
		{in: "5000", wantErr: true},          // no colon
		{in: "host:5000", wantErr: true},     // non-numeric cid
		{in: "2:port", wantErr: true},        // non-numeric port
		{in: "2:5000:6000", wantErr: true},   // too many parts
		{in: "4294967296:1", wantErr: true},  // cid overflows uint32
		{in: "1:4294967296", wantErr: true},  // port overflows uint32
		{in: "-1:1", wantErr: true},          // negative cid
	}
	for _, c := range cases {
		cid, port, err := parseVsockAddr(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseVsockAddr(%q): want error, got cid=%d port=%d", c.in, cid, port)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseVsockAddr(%q): unexpected error: %v", c.in, err)
			continue
		}
		if cid != c.wantCID || port != c.wantPort {
			t.Errorf("parseVsockAddr(%q) = cid %d, port %d; want cid %d, port %d", c.in, cid, port, c.wantCID, c.wantPort)
		}
	}
}

func TestDialContextRejectsUnsupportedNetwork(t *testing.T) {
	t.Parallel()
	if _, err := dialContext(context.Background(), "tcp", "127.0.0.1:1"); err == nil {
		t.Error("expected dialContext to reject tcp")
	}
}

// TestDialVsockHonorsCanceledContext proves the ctx race returns promptly (no hang) and
// reports an error when the context is already done. The address parses but has no
// listener, so whichever select branch wins, the call returns an error rather than
// blocking — the property the brokered round-trip relies on to abandon a dead microVM.
func TestDialVsockHonorsCanceledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := dialVsock(ctx, fmt.Sprintf("%d:1", vsock.Local))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("dialVsock with canceled context: want error, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dialVsock did not return promptly with a canceled context")
	}
}

// TestVsockIntegration drives the real broker Server and Client over an AF_VSOCK
// loopback connection (cid Local), proving the production transport carries the same
// one-request-per-connection protocol the unix path does. It skips where vsock is
// unavailable (no kernel module / non-Linux), so it stays portable.
func TestVsockIntegration(t *testing.T) {
	ln, err := Listen("vsock", "1:0") // port 0 = auto-assign; cid half is ignored by the listener
	if err != nil {
		t.Skipf("vsock unavailable, skipping: %v", err)
	}
	defer func() { _ = ln.Close() }()

	va, ok := ln.Addr().(*vsock.Addr)
	if !ok {
		t.Fatalf("listener addr = %T, want *vsock.Addr", ln.Addr())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := &fakeHandler{
		completeResp: model.Response{Text: "ok", Stop: model.StopEndTurn},
		pushResp:     GitPushResult{Commit: "deadbeef"},
	}
	srv := NewServer(h, WithAllowlist(allDestinations))
	go func() { _ = srv.Serve(ctx, ln) }()

	// The agent dials the listener's port at the Local context id (loopback stand-in
	// for Host=2 in a real microVM).
	c := NewClient("vsock", fmt.Sprintf("%d:%d", vsock.Local, va.Port))

	resp, err := c.Complete(ctx, model.Request{Messages: []model.Message{{Role: model.RoleUser, Text: "hi"}}})
	if err != nil {
		t.Fatalf("Complete over vsock: %v", err)
	}
	if resp.Text != "ok" {
		t.Errorf("completion text = %q, want ok", resp.Text)
	}

	push, err := c.GitPush(ctx, GitPushRequest{Branch: "task/x"})
	if err != nil {
		t.Fatalf("GitPush over vsock: %v", err)
	}
	if push.Commit != "deadbeef" {
		t.Errorf("commit = %q, want deadbeef", push.Commit)
	}

	if err := c.PublishEvent(ctx, PublishRequest{Type: "log"}); err != nil {
		t.Fatalf("PublishEvent over vsock: %v", err)
	}
}

// TestVsockEndpointStringRoundTrips guards the "<cid>:<port>" convention the sandbox
// Endpoint carries: what a backend formats is exactly what parseVsockAddr reads back.
func TestVsockEndpointStringRoundTrips(t *testing.T) {
	t.Parallel()
	addr := fmt.Sprintf("%d:%d", vsock.Host, 5005)
	if !strings.Contains(addr, ":") {
		t.Fatalf("formatted addr %q missing separator", addr)
	}
	cid, port, err := parseVsockAddr(addr)
	if err != nil {
		t.Fatalf("parseVsockAddr(%q): %v", addr, err)
	}
	if cid != vsock.Host || port != 5005 {
		t.Errorf("round-trip = cid %d port %d, want cid %d port 5005", cid, port, vsock.Host)
	}
}
