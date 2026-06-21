package main

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/Loxstomper/harness/internal/messaging"
)

// TestBuildRunComponents exercises the composition root: it assembles the full
// kernel (embedded NATS + JetStream, artifact store, model registry, sandbox
// backend, agent loop, gate, runner, orchestrator) from the shipped config against a
// throwaway repo, then starts both loops and cancels them. It proves the wiring is
// internally consistent — every constructor's required collaborators are satisfied
// and both loops shut down cleanly on ctx cancel — without needing Docker, an API
// key, or a real merge (no work is dispatched, so nothing reaches the sandbox).
func TestBuildRunComponents(t *testing.T) {
	cfg, err := loadConfig(testConfigDir, "dev")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	resolvePersonas(cfg)

	repo := t.TempDir()
	log := slog.New(slog.DiscardHandler)

	comp, err := buildRunComponents(cfg, repo, runOptions{
		// A bd binary that does not exist: the orchestrator's Ready query will error
		// and be logged, but the loop must not crash — proving the wiring is robust to
		// a missing collaborator at the edges.
		bdBin: "bd-does-not-exist",
	}, log)
	if err != nil {
		t.Fatalf("buildRunComponents: %v", err)
	}
	defer comp.cleanup()

	if comp.orch == nil || comp.rnr == nil {
		t.Fatal("buildRunComponents returned nil component")
	}

	// Start both loops and let them idle briefly, then cancel. A clean ctx-cancel
	// shutdown returns nil from both.
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return comp.orch.Run(ctx) })
	g.Go(func() error { return comp.rnr.Run(ctx) })
	if err := g.Wait(); err != nil {
		t.Fatalf("run loops returned error on clean shutdown: %v", err)
	}
}

// TestBuildRunComponentsServe proves the co-located control room is assembled only when
// serveAddr is set: with it the run carries a non-nil server (cmdRun then serves it on
// the shared NATS), without it the server is nil and only the two loops run. The live
// NATS->SSE path itself is covered in internal/controlroom and .../live.
func TestBuildRunComponentsServe(t *testing.T) {
	cfg, err := loadConfig(testConfigDir, "dev")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	resolvePersonas(cfg)
	log := slog.New(slog.DiscardHandler)

	off, err := buildRunComponents(cfg, t.TempDir(), runOptions{bdBin: "bd"}, log)
	if err != nil {
		t.Fatalf("buildRunComponents (no serve): %v", err)
	}
	defer off.cleanup()
	if off.server != nil {
		t.Error("server built without serveAddr")
	}

	on, err := buildRunComponents(cfg, t.TempDir(), runOptions{bdBin: "bd", serveAddr: "127.0.0.1:0"}, log)
	if err != nil {
		t.Fatalf("buildRunComponents (serve): %v", err)
	}
	defer on.cleanup()
	if on.server == nil {
		t.Error("serveAddr set but no server built")
	}
}

// TestBuildRunComponentsCleanupReleases confirms cleanup can be called and that a
// second assembly on the same repo succeeds — i.e. the embedded server and JetStream
// store dir are released/reusable, matching the crash-and-resume model.
func TestBuildRunComponentsCleanupReleases(t *testing.T) {
	cfg, err := loadConfig(testConfigDir, "dev")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	resolvePersonas(cfg)
	repo := t.TempDir()
	log := slog.New(slog.DiscardHandler)

	for i := 0; i < 2; i++ {
		comp, err := buildRunComponents(cfg, repo, runOptions{bdBin: "bd"}, log)
		if err != nil {
			t.Fatalf("assembly %d: %v", i, err)
		}
		comp.cleanup()
	}
}

// TestBuildRunComponentsExternalNATS proves the distributed-deployment swap (T5.8): when the
// infra overlay's nats.url points at an external cluster, the composition root connects to it
// over TCP instead of starting an embedded server, and both loops run and shut down cleanly
// against it. The "external cluster" here is a separate embedded server with a TCP listener —
// from buildRunComponents' side it is reached exactly as a real cluster would be. This is the
// only seam that differs between a single-process dev run and a distributed one.
func TestBuildRunComponentsExternalNATS(t *testing.T) {
	// A free 127.0.0.1 port for the external server's client listener.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	ext, err := messaging.NewEmbeddedServer(messaging.ServerConfig{StoreDir: t.TempDir(), ClientAddr: addr})
	if err != nil {
		t.Fatalf("external server: %v", err)
	}
	defer ext.Shutdown()

	cfg, err := loadConfig(testConfigDir, "dev")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	resolvePersonas(cfg)
	cfg.Infra.NATS.URL = "nats://" + addr // point the run at the external cluster

	log := slog.New(slog.DiscardHandler)
	comp, err := buildRunComponents(cfg, t.TempDir(), runOptions{bdBin: "bd"}, log)
	if err != nil {
		t.Fatalf("buildRunComponents (external nats): %v", err)
	}
	defer comp.cleanup()
	if comp.orch == nil || comp.rnr == nil {
		t.Fatal("buildRunComponents returned nil component")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return comp.orch.Run(ctx) })
	g.Go(func() error { return comp.rnr.Run(ctx) })
	if err := g.Wait(); err != nil {
		t.Fatalf("run loops returned error on clean shutdown: %v", err)
	}
}
