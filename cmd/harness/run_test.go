package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
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
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

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
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	for i := 0; i < 2; i++ {
		comp, err := buildRunComponents(cfg, repo, runOptions{bdBin: "bd"}, log)
		if err != nil {
			t.Fatalf("assembly %d: %v", i, err)
		}
		comp.cleanup()
	}
}
