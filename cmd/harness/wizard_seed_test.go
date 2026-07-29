package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Loxstomper/software-factory/internal/artifact"
	"github.com/Loxstomper/software-factory/internal/beads"
	"github.com/Loxstomper/software-factory/internal/config"
	"github.com/Loxstomper/software-factory/internal/controlroom/wizard"
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
		"no issues":          func(r *wizard.SeedRequest) { r.Issues = nil },
		"no specs":           func(r *wizard.SeedRequest) { r.Specs = nil },
		"path outside specs": func(r *wizard.SeedRequest) { r.Specs[0].Path = "evil.md"; r.Issues[0].Spec = "evil.md" },
		"path traversal":     func(r *wizard.SeedRequest) { r.Specs[0].Path = "specs/../../etc/x.md" },
		"not markdown": func(r *wizard.SeedRequest) {
			r.Specs[0].Path = "specs/orders.txt"
			r.Issues[0].Spec = "specs/orders.txt"
		},
		"broken link":        func(r *wizard.SeedRequest) { r.Specs[0].Content += "\n[gone](nope.md)\n" },
		"orphan spec":        func(r *wizard.SeedRequest) { r.Issues[0].Spec = "" },
		"illegal role":       func(r *wizard.SeedRequest) { r.Issues[0].Role = "implementor" },
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

// epicConfig is testConfig under integration.mode: epic (T7.5): the wizard then opens an
// epic/<epic_id> branch with the spec and enforces the one-active-epic consent gate.
func epicConfig() *config.Config {
	c := testConfig()
	c.Harness.Integration = &config.Integration{Mode: config.IntegrationEpic}
	return c
}

// gitInitWithMain initializes a repo with an initial commit on main — the precondition for cutting
// an epic branch (a real integration repo always carries main history).
func gitInitWithMain(t *testing.T, repo string) {
	t.Helper()
	gitInit(t, repo)
	mustWrite(t, filepath.Join(repo, "README.md"), "# repo\n")
	runCmd(t, repo, "git", "add", "README.md")
	runCmd(t, repo, "git", "commit", "-q", "-m", "init")
}

func requireGitBd(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not on PATH")
	}
}

// TestSeedEpicCommitsSpecOntoEpicBranch proves the load-bearing half of T7.5: under epic mode the
// approved spec lands on a fresh epic/<epic_id> branch cut from main, NOT on main, so main stays
// quiescent until the terminal merge (one feature, one landing, one deploy). It also asserts the
// spec stays readable from the working tree, since the orchestrator resolves an issue's spec slice
// by reading the repo's working tree (so the feature's spec must be reachable there even though it
// is committed only on the epic branch).
func TestSeedEpicCommitsSpecOntoEpicBranch(t *testing.T) {
	requireGitBd(t)
	repo := t.TempDir()
	gitInitWithMain(t, repo)
	runCmd(t, repo, "bd", "init", "--prefix", "harness")

	store, err := artifact.NewFilesStore(filepath.Join(repo, ".artifacts"))
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	bd := beads.New(beads.WithBinary("bd"), beads.WithDir(repo))
	s := newWizardSeeder(epicConfig(), repo, bd, store, nil)

	mainBefore := strings.TrimSpace(runCmd(t, repo, "git", "rev-parse", "refs/heads/main"))

	res, err := s.Seed(context.Background(), validSeedRequest())
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if len(res.Issues) != 1 {
		t.Fatalf("created %d issues, want 1", len(res.Issues))
	}
	epic := res.Issues[0].ID
	branch := "epic/" + epic

	// main did not move — the feature is held off main until the terminal merge.
	if mainAfter := strings.TrimSpace(runCmd(t, repo, "git", "rev-parse", "refs/heads/main")); mainAfter != mainBefore {
		t.Errorf("main moved on epic seed: %s -> %s (must stay quiescent)", mainBefore, mainAfter)
	}
	// The epic branch is the returned commit and is cut directly from main.
	if tip := strings.TrimSpace(runCmd(t, repo, "git", "rev-parse", "refs/heads/"+branch)); tip != res.Commit {
		t.Errorf("epic branch tip = %s, want returned commit %s", tip, res.Commit)
	}
	if parent := strings.TrimSpace(runCmd(t, repo, "git", "rev-parse", branch+"^")); parent != mainBefore {
		t.Errorf("epic commit parent = %s, want main %s", parent, mainBefore)
	}
	// The spec lives on the epic branch, never on main.
	if out := runCmd(t, repo, "git", "show", branch+":specs/orders-export.md"); !strings.Contains(out, "Orders Export") {
		t.Errorf("spec missing from epic branch: %q", out)
	}
	if err := exec.Command("git", "-C", repo, "cat-file", "-e", "main:specs/orders-export.md").Run(); err == nil {
		t.Error("spec leaked onto main (must be epic-branch-only until the terminal merge)")
	}
	// The spec is present in the working tree, so the orchestrator can resolve the slice.
	if _, err := os.Stat(filepath.Join(repo, "specs", "orders-export.md")); err != nil {
		t.Errorf("spec not in the working tree for resolution: %v", err)
	}
	// The seed issue carries no EpicID (it is the epic root; EpicOf folds it into its own epic).
	got, err := bd.Get(context.Background(), epic)
	if err != nil {
		t.Fatalf("read back seed issue: %v", err)
	}
	if got.EpicID != "" {
		t.Errorf("seed root EpicID = %q, want empty (it is its own epic)", got.EpicID)
	}
}

// TestValidateEpicRequiresSingleRoot proves epic mode admits exactly one root seed issue (the
// epic id is that root's id); a multi-root draft is refused, while per-item mode still accepts it.
func TestValidateEpicRequiresSingleRoot(t *testing.T) {
	repo := t.TempDir()
	req := validSeedRequest()
	req.Issues = append(req.Issues, wizard.DraftIssue{Title: "second root", Body: "x", Spec: "specs/orders-export.md"})

	epic := newWizardSeeder(epicConfig(), repo, nil, nil, nil)
	if err := epic.validate(req); err == nil || !strings.Contains(err.Error(), "exactly one root issue") {
		t.Fatalf("epic validate with 2 roots = %v, want single-root refusal", err)
	}
	perItem := newWizardSeeder(testConfig(), repo, nil, nil, nil)
	if err := perItem.validate(req); err != nil {
		t.Fatalf("per-item validate with 2 roots = %v, want nil (no single-root rule)", err)
	}
}

// TestSeedEpicRefusesSecondInFlight proves the one-active-epic consent gate: a second approval
// while the first epic's work is still open is refused, naming the in-flight feature.
func TestSeedEpicRefusesSecondInFlight(t *testing.T) {
	requireGitBd(t)
	repo := t.TempDir()
	gitInitWithMain(t, repo)
	runCmd(t, repo, "bd", "init", "--prefix", "harness")
	store, err := artifact.NewFilesStore(filepath.Join(repo, ".artifacts"))
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	bd := beads.New(beads.WithBinary("bd"), beads.WithDir(repo))
	s := newWizardSeeder(epicConfig(), repo, bd, store, nil)

	if _, err := s.Seed(context.Background(), validSeedRequest()); err != nil {
		t.Fatalf("first epic seed: %v", err)
	}
	_, err = s.Seed(context.Background(), validSeedRequest())
	if err == nil || !strings.Contains(err.Error(), "one feature at a time") {
		t.Fatalf("second epic seed = %v, want one-active-epic refusal", err)
	}
}

// TestSeedEpicGateTracksLanding proves the gate's two closed-but-not-done states: a drained epic
// (its only issue closed) is still "active" until its terminal merge lands it on main, and a
// second feature is admitted only once that landing commit is present.
func TestSeedEpicGateTracksLanding(t *testing.T) {
	requireGitBd(t)
	repo := t.TempDir()
	gitInitWithMain(t, repo)
	runCmd(t, repo, "bd", "init", "--prefix", "harness")
	store, err := artifact.NewFilesStore(filepath.Join(repo, ".artifacts"))
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	bd := beads.New(beads.WithBinary("bd"), beads.WithDir(repo))
	s := newWizardSeeder(epicConfig(), repo, bd, store, nil)

	res, err := s.Seed(context.Background(), validSeedRequest())
	if err != nil {
		t.Fatalf("first seed: %v", err)
	}
	epic := res.Issues[0].ID

	// Drain the epic (close its only issue) but do not land it: the gate still refuses, citing the
	// pending terminal merge.
	if err := bd.Close(context.Background(), epic); err != nil {
		t.Fatalf("close seed issue: %v", err)
	}
	_, err = s.Seed(context.Background(), validSeedRequest())
	if err == nil || !strings.Contains(err.Error(), "awaiting its terminal merge") {
		t.Fatalf("drained-but-unlanded gate = %v, want pending-terminal-merge refusal", err)
	}

	// Simulate the terminal merge: a commit on main citing the epic id in its provenance trailer
	// (the same form MergeEpic writes and greps for). The gate now admits the next feature.
	runCmd(t, repo, "git", "commit", "--allow-empty", "-q", "-m", "merge: land feature\n\nIssue: "+epic+" | provenance")
	if _, err := s.Seed(context.Background(), validSeedRequest()); err != nil {
		t.Fatalf("seed after the feature landed = %v, want success", err)
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
