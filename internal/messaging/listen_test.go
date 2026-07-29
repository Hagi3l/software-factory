package messaging

import (
	"net"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// TestEmbeddedServerClientAddrAcceptsExternalConn proves the opt-in TCP listener (T2.10): a
// server started with ClientAddr accepts a connection from a SEPARATE nats.Connect (as
// `software-factory approve` would), and a message published there reaches an in-process subscriber.
// This is what makes the cross-process approval loop work in the single-host bootstrap.
func TestEmbeddedServerClientAddrAcceptsExternalConn(t *testing.T) {
	addr := freeAddr(t)
	srv, err := NewEmbeddedServer(ServerConfig{StoreDir: t.TempDir(), ClientAddr: addr})
	if err != nil {
		t.Fatalf("NewEmbeddedServer with ClientAddr: %v", err)
	}
	defer srv.Shutdown()

	// In-process subscriber (the orchestrator's side).
	inproc, err := srv.Connect()
	if err != nil {
		t.Fatalf("in-process connect: %v", err)
	}
	defer inproc.Close()
	sub, err := inproc.SubscribeSync("factory.approvals")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// External TCP client (the `software-factory approve` side) — a separate connection over the addr.
	ext, err := nats.Connect("nats://" + addr)
	if err != nil {
		t.Fatalf("external connect to %s: %v", addr, err)
	}
	defer ext.Close()
	if err := ext.Publish("factory.approvals", []byte("ok")); err != nil {
		t.Fatalf("external publish: %v", err)
	}
	if err := ext.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("in-process subscriber never received the external message: %v", err)
	}
	if string(msg.Data) != "ok" {
		t.Errorf("payload = %q, want ok", msg.Data)
	}
}

// TestEmbeddedServerRejectsBadClientAddr: a malformed client address fails loudly at startup
// rather than leaving the server silently in-process only.
func TestEmbeddedServerRejectsBadClientAddr(t *testing.T) {
	if _, err := NewEmbeddedServer(ServerConfig{StoreDir: t.TempDir(), ClientAddr: "not-an-addr"}); err == nil {
		t.Fatal("NewEmbeddedServer accepted a malformed client addr")
	}
}

// freeAddr returns a currently-free 127.0.0.1 address for the listener to bind.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}
