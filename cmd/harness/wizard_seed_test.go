package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/artifact"
	"github.com/Loxstomper/harness/internal/beads"
	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/controlroom/wizard"
)

// testConfig is a minimal DAG with a single pipeline entry stage (`plan` → role `planner`), the
// shape the consent-gate role legality is validated against.
func testConfig() *config.Config {
	return &config.Config{Harness: &config.Harness{DAG: map[string]config.Stage{
		"plan":         {Role: "planner", Kind: config.StageKindPlan, Produces: []string{"author-tests"}},
		"author-tests": {Role: "test-author", Produces: []string{"implement"}},
		"implement":    {Role: "implementor"},
	}}}
}

// validSeedRequest is a well-formed draft: one spec referenced by one seed issue, no broken links.
func validSeedRequest() wizard.SeedRequest {
	return wizard.SeedRequest{
		Summary: "Add CSV export to the orders report",
		Specs: []wizard.DraftSpec{{
			Path:    "specs/orders-export.md",
			Content: "# Orders Export\n\nExport the orders report as CSV.\n",
		}},
		Issues: []wizard.DraftIssue{{
			Title: "Export orders as CSV",
			Body:  "Build the export.",
			Spec:  "specs/orders-export.md",
		}},
		Decisions:  []wizard.DecisionRecord{{Point: "Format → CSV", Rationale: "Spreadsheet-friendly."}},
		Transcript: []byte(`[{"role":"user","text":"export orders"}]`),
	}
}

// TestValidateDraft covers the spec-authoring contract the consent gate enforces before any
// write: required content, safe paths under specs/, link integrity, issue→spec coverage, and
// role legality. It is pure except for os.Stat against the temp repo, so no git/beads is needed.
func TestValidateDraft(t *testing.T) {
	repo := t.TempDir()
	// An existing spec a draft may legally link to / reference.
	mustWrite(t, filepath.Join(repo, "specs", "glossary.md"), "# Glossary\n")
	s := &wizardSeeder{cfg: testConfig(), repo: repo}

	t.Run("valid passes", func(t *testing.T) {
		if err := s.validate(validSeedRequest()); err != nil {
			t.Fatalf("valid request rejected: %v", err)
		}
	})

	t.Run("link to existing file passes", func(t *testing.T) {
		req := validSeedRequest()
		req.Specs[0].Content += "\nSee the [glossary](glossary.md).\n"
		if err := s.validate(req); err != nil {
			t.Errorf("link to an existing repo file rejected: %v", err)
		}
	})

	t.Run("link between drafted specs passes", func(t *testing.T) {
		req := validSeedRequest()
		req.Specs[0].Content += "\nSee [details](orders-export-detail.md).\n"
		req.Specs = append(req.Specs, wizard.DraftSpec{Path: "specs/orders-export-detail.md", Content: "# Detail\n"})
		req.Issues = append(req.Issues, wizard.DraftIssue{Title: "Detail work", Spec: "specs/orders-export-detail.md"})
		if err := s.validate(req); err != nil {
			t.Errorf("link between drafted specs rejected: %v", err)
		}
	})

	// T4.32: editing a spec that already exists on disk is *not* new work to seed, so it needs no
	// backing issue (specs-process.md "the issue-coverage rule binds only newly-created specs").
	t.Run("edit to existing spec needs no issue", func(t *testing.T) {
		req := validSeedRequest()
		// Fold an additive edit into the on-disk glossary alongside the new orders spec; the
		// glossary edit is referenced by no issue and must still pass.
		req.Specs = append(req.Specs, wizard.DraftSpec{Path: "specs/glossary.md", Content: "# Glossary\n\nNew term.\n"})
		if err := s.validate(req); err != nil {
			t.Errorf("edit to an existing spec (no backing issue) rejected: %v", err)
		}
	})

	t.Run("README index edit needs no issue", func(t *testing.T) {
		mustWrite(t, filepath.Join(repo, "specs", "README.md"), "# Index\n")
		req := validSeedRequest()
		// The new orders spec is seeded; the index refresh that makes it reachable is an edit.
		req.Specs = append(req.Specs, wizard.DraftSpec{
			Path:    "specs/README.md",
			Content: "# Index\n\n- [Orders Export](orders-export.md)\n",
		})
		if err := s.validate(req); err != nil {
			t.Errorf("README index edit (no backing issue) rejected: %v", err)
		}
	})

	cases := map[string]func(*wizard.SeedRequest){
		"no issues":         func(r *wizard.SeedRequest) { r.Issues = nil },
		"no specs":          func(r *wizard.SeedRequest) { r.Specs = nil },
		"path outside specs": func(r *wizard.SeedRequest) { r.Specs[0].Path = "evil.md"; r.Issues[0].Spec = "evil.md" },
		"path traversal":    func(r *wizard.SeedRequest) { r.Specs[0].Path = "specs/../../etc/x.md" },
		"not markdown":      func(r *wizard.SeedRequest) { r.Specs[0].Path = "specs/orders.txt"; r.Issues[0].Spec = "specs/orders.txt" },
		"broken link":       func(r *wizard.SeedRequest) { r.Specs[0].Content += "\n[gone](nope.md)\n" },
		"orphan spec":       func(r *wizard.SeedRequest) { r.Issues[0].Spec = "" },
		"illegal role":      func(r *wizard.SeedRequest) { r.Issues[0].Role = "implementor" },
		"unknown issue spec": func(r *wizard.SeedRequest) { r.Issues[0].Spec = "specs/ghost.md" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := validSeedRequest()
			mutate(&req)
			if err := s.validate(req); err == nil {
				t.Errorf("%s: validate accepted an invalid draft", name)
			}
		})
	}
}

// TestSeedCommitsAndCreates is the consent gate end to end against real git + beads + an artifact
// store: an approved draft writes the spec + decisions sidecar, commits them, stores the
// transcript, and creates the seed issue through the single-writer path with its spec reference.
func TestSeedCommitsAndCreates(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not on PATH")
	}
	repo := t.TempDir()
	gitInit(t, repo)
	runCmd(t, repo, "bd", "init", "--prefix", "harness")

	store, err := artifact.NewFilesStore(filepath.Join(repo, ".artifacts"))
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	bd := beads.New(beads.WithBinary("bd"), beads.WithDir(repo))
	s := newWizardSeeder(testConfig(), repo, bd, store, nil)

	res, err := s.Seed(context.Background(), validSeedRequest())
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}

	// Spec + sidecar are on disk and committed (clean working tree).
	if _, err := os.Stat(filepath.Join(repo, "specs", "orders-export.md")); err != nil {
		t.Errorf("spec not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "specs", "decisions", "orders-export.md")); err != nil {
		t.Errorf("decisions sidecar not written: %v", err)
	}
	if status := runCmd(t, repo, "git", "status", "--porcelain", "--", "specs"); strings.TrimSpace(status) != "" {
		t.Errorf("specs not committed (dirty): %q", status)
	}
	if res.Commit == "" {
		t.Error("no commit sha returned")
	}

	// Transcript stored under the returned hash.
	if res.TranscriptRef == "" {
		t.Error("no transcript ref returned")
	} else if ok, _ := store.Has(context.Background(), res.TranscriptRef); !ok {
		t.Errorf("transcript not present in the store under %q", res.TranscriptRef)
	}

	// The seed issue was created via the single-writer path with its role + spec reference.
	if len(res.Issues) != 1 {
		t.Fatalf("created %d issues, want 1", len(res.Issues))
	}
	got, err := bd.Get(context.Background(), res.Issues[0].ID)
	if err != nil {
		t.Fatalf("read back seed issue: %v", err)
	}
	if got.Role != "planner" {
		t.Errorf("seed issue role = %q, want planner", got.Role)
	}
	if got.Spec != "specs/orders-export.md" {
		t.Errorf("seed issue spec = %q, want specs/orders-export.md", got.Spec)
	}
	if !strings.Contains(got.Body, "Transcript:") || !strings.Contains(got.Body, "Decisions:") {
		t.Errorf("seed issue body missing the provenance footer: %q", got.Body)
	}
}

// TestDecisionsSidecarPath proves the sidecar is keyed by the first spec's area (base name).
func TestDecisionsSidecarPath(t *testing.T) {
	got := decisionsSidecarPath([]wizard.DraftSpec{{Path: "specs/orders-export.md"}})
	if got != "specs/decisions/orders-export.md" {
		t.Errorf("sidecar path = %q", got)
	}
	if got := decisionsSidecarPath(nil); got != "specs/decisions/task.md" {
		t.Errorf("fallback sidecar path = %q", got)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitInit(t *testing.T, repo string) {
	t.Helper()
	runCmd(t, repo, "git", "init", "-q", "-b", "main")
	runCmd(t, repo, "git", "config", "user.name", "harness")
	runCmd(t, repo, "git", "config", "user.email", "harness@localhost")
}

func runCmd(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}
