package messaging

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// TestStreamConfigsApplyOptions proves the env-varying JetStream knobs (T5.8) propagate
// into every stream definition: the replication factor applies uniformly, while only the
// result stream is age-bounded (work is consume-once; dlq/approvals must survive until a
// human acts). The zero value falls back to the bootstrap defaults so a dev run that omits
// the knobs behaves exactly as before. Pure unit — no server needed.
func TestStreamConfigsApplyOptions(t *testing.T) {
	for _, c := range streamConfigs(StreamOptions{Replicas: 3, ResultMaxAge: 48 * time.Hour}) {
		if c.Replicas != 3 {
			t.Errorf("stream %s replicas = %d, want 3", c.Name, c.Replicas)
		}
		switch c.Name {
		case StreamResult:
			if c.MaxAge != 48*time.Hour {
				t.Errorf("result max-age = %v, want 48h", c.MaxAge)
			}
		default:
			if c.MaxAge != 0 {
				t.Errorf("stream %s max-age = %v, want 0 (unbounded)", c.Name, c.MaxAge)
			}
		}
	}
	for _, c := range streamConfigs(StreamOptions{}) {
		if c.Replicas != 1 {
			t.Errorf("default stream %s replicas = %d, want 1", c.Name, c.Replicas)
		}
		if c.Name == StreamResult && c.MaxAge != defaultResultMaxAge {
			t.Errorf("default result max-age = %v, want %v", c.MaxAge, defaultResultMaxAge)
		}
	}
}

// TestSetupStreamsAppliesOptions proves SetupStreams actually creates the result stream with
// the configured retention override and that re-applying the SAME options (as the
// orchestrator does on every startup) reconciles rather than drifts — the contract the
// orchestrator relies on to not silently reset the streams back to the defaults.
func TestSetupStreamsAppliesOptions(t *testing.T) {
	js := startTestServer(t)
	ctx := context.Background()
	opts := StreamOptions{Replicas: 1, ResultMaxAge: 48 * time.Hour}
	if err := SetupStreams(ctx, js, opts); err != nil {
		t.Fatalf("SetupStreams: %v", err)
	}
	if err := SetupStreams(ctx, js, opts); err != nil {
		t.Fatalf("SetupStreams (2nd): %v", err)
	}

	res, err := js.Stream(ctx, StreamResult)
	if err != nil {
		t.Fatalf("Stream(result): %v", err)
	}
	rinfo, err := res.Info(ctx)
	if err != nil {
		t.Fatalf("result Info: %v", err)
	}
	if rinfo.Config.MaxAge != 48*time.Hour {
		t.Errorf("result stream max-age = %v, want 48h (override not applied)", rinfo.Config.MaxAge)
	}

	// The work stream stays unbounded regardless of the result override.
	work, err := js.Stream(ctx, StreamWork)
	if err != nil {
		t.Fatalf("Stream(work): %v", err)
	}
	winfo, err := work.Info(ctx)
	if err != nil {
		t.Fatalf("work Info: %v", err)
	}
	if winfo.Config.MaxAge != 0 {
		t.Errorf("work stream max-age = %v, want 0 (unbounded)", winfo.Config.MaxAge)
	}
}

// TestConnectExternal proves the distributed-deployment connection path (T5.8): connecting to
// an external server over TCP via its url — exactly what buildRunComponents does when
// nats.url is set instead of starting the embedded server — supports the full JetStream
// lifecycle (stream setup + a work round-trip). The "external" server here is itself an
// embedded one with a TCP listener, but Connect reaches it as a separate process would.
func TestConnectExternal(t *testing.T) {
	addr := freeAddr(t)
	srv, err := NewEmbeddedServer(ServerConfig{StoreDir: t.TempDir(), ClientAddr: addr})
	if err != nil {
		t.Fatalf("NewEmbeddedServer: %v", err)
	}
	defer srv.Shutdown()

	nc, err := Connect("nats://"+addr, nats.Name("test"))
	if err != nil {
		t.Fatalf("Connect external: %v", err)
	}
	defer nc.Close()

	js, err := JetStream(nc)
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	ctx := context.Background()
	if err := SetupStreams(ctx, js, StreamOptions{}); err != nil {
		t.Fatalf("SetupStreams over external conn: %v", err)
	}
	cons, err := EnsureWorkConsumer(ctx, js, "implement", 2*time.Second)
	if err != nil {
		t.Fatalf("EnsureWorkConsumer: %v", err)
	}
	if _, err := js.Publish(ctx, WorkSubject("implement"), []byte("hi")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := fetchOne(t, cons); string(got.Data()) != "hi" {
		t.Errorf("payload = %q, want hi", got.Data())
	}
}

// TestConnectExternalUnreachable proves a failed dial is surfaced as an error (the common
// "is the cluster reachable?" case) rather than a silent nil connection.
func TestConnectExternalUnreachable(t *testing.T) {
	if _, err := Connect("nats://127.0.0.1:1"); err == nil {
		t.Fatal("Connect to an unreachable address returned nil error")
	}
}
