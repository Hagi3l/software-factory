package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/beads"
)

// TestEnsureSpec covers the spec-authoring half of seed: it writes a starter from
// the title/description when the file is absent, and never clobbers an operator's
// hand-written spec when it already exists.
func TestEnsureSpec(t *testing.T) {
	dir := t.TempDir()

	// Absent -> written from title + description.
	p := filepath.Join(dir, "nested", "feature.md")
	if err := ensureSpec(p, "Add widgets", "support widgets end to end"); err != nil {
		t.Fatalf("ensureSpec(new): %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read written spec: %v", err)
	}
	if !strings.Contains(string(got), "# Add widgets") || !strings.Contains(string(got), "support widgets end to end") {
		t.Fatalf("starter spec missing title/description:\n%s", got)
	}

	// Present -> left untouched.
	custom := filepath.Join(dir, "existing.md")
	if err := os.WriteFile(custom, []byte("# Hand authored\n\ndetail\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureSpec(custom, "Different Title", "different desc"); err != nil {
		t.Fatalf("ensureSpec(existing): %v", err)
	}
	got, _ = os.ReadFile(custom)
	if string(got) != "# Hand authored\n\ndetail\n" {
		t.Fatalf("ensureSpec overwrote an existing spec:\n%s", got)
	}
}

// TestSeedIntegration drives cmdSeed against the real bd binary in a fresh database:
// it must create one issue at the inferred entry role and write the spec file. It
// skips when bd is absent (as the other beads integration tests do) — bd is a hard
// runtime dependency, but the wiring is provable without it via the unit tests above.
func TestSeedIntegration(t *testing.T) {
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not on PATH; skipping seed integration test")
	}
	repo := t.TempDir()
	bdInit(t, repo)

	err := cmdSeed([]string{
		"--config", testConfigDir,
		"--repo", repo,
		"--title", "Implement the thing",
		"--description", "make the thing work",
		"--spec", "specs/thing.md",
	})
	if err != nil {
		t.Fatalf("cmdSeed: %v", err)
	}

	// The spec was authored under the repo.
	if _, err := os.Stat(filepath.Join(repo, "specs", "thing.md")); err != nil {
		t.Fatalf("seed did not write spec: %v", err)
	}

	// The issue exists, at the entry role, ready to dispatch.
	bd := beads.New(beads.WithDir(repo))
	ready, err := bd.Ready(context.Background())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(ready) != 1 {
		t.Fatalf("expected 1 ready issue, got %d", len(ready))
	}
	if ready[0].Role != "test-author" {
		t.Fatalf("seed issue role = %q, want test-author (the DAG entry stage)", ready[0].Role)
	}
	if ready[0].Title != "Implement the thing" {
		t.Fatalf("seed issue title = %q", ready[0].Title)
	}
}

// bdInit creates a fresh beads database in dir (mirrors the helper in the beads
// integration tests).
func bdInit(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("bd", "init")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bd init: %v\n%s", err, out)
	}
}
