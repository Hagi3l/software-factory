package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/gate"
)

// orchWithRepo builds a bare Orchestrator carrying just the integration repo path, a
// spec-slice depth, and a discard logger — all buildBrief's spec resolution reads.
func orchWithRepo(repo string, depth int) *Orchestrator {
	return &Orchestrator{
		opts: Options{Repo: repo, Config: &config.Config{Harness: &config.Harness{SpecDepth: depth}}},
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func mustWriteSpec(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestBuildBriefResolvesSpecSlice proves the orchestrator hands the agent the bounded spec
// slice — the issue's referenced file plus its linked neighbors to the configured depth —
// resolved from the integration repo, rather than the empty slice the bootstrap used to
// ship (T3.5, see internal/spec, specs/specs-process.md).
func TestBuildBriefResolvesSpecSlice(t *testing.T) {
	repo := t.TempDir()
	mustWriteSpec(t, filepath.Join(repo, "specs", "orders.md"), "# Orders\nsee [validation](validation.md)\n")
	mustWriteSpec(t, filepath.Join(repo, "specs", "validation.md"), "# Validation\nreject negatives\n")

	o := orchWithRepo(repo, 1)
	brief := o.buildBrief(core.Issue{ID: "iss-1", Role: "implement", Spec: "specs/orders.md"}, config.Stage{}, core.Soul{})

	if !strings.Contains(brief.Spec, "# Orders") {
		t.Errorf("slice missing the referenced file:\n%s", brief.Spec)
	}
	if !strings.Contains(brief.Spec, "# Validation") {
		t.Errorf("depth-1 slice missing the linked neighbor:\n%s", brief.Spec)
	}
	if !strings.Contains(brief.Spec, "<!-- spec: specs/orders.md -->") {
		t.Errorf("slice missing the file marker:\n%s", brief.Spec)
	}
}

// An issue that names no spec gets an empty slice (and the agent falls back to the specs/
// tree in its worktree) — the kernel path, which must keep working unchanged.
func TestBuildBriefNoSpecYieldsEmptySlice(t *testing.T) {
	o := orchWithRepo(t.TempDir(), 1)
	brief := o.buildBrief(core.Issue{ID: "iss-1", Role: "implement"}, config.Stage{}, core.Soul{})
	if brief.Spec != "" {
		t.Errorf("issue with no spec must get an empty slice, got %q", brief.Spec)
	}
}

// A spec reference that does not resolve (a missing file on the host) degrades to an empty
// slice and dispatches anyway, rather than wedging the issue — degraded context, not a
// dead pipeline (the same best-effort discipline harvest uses).
func TestBuildBriefMissingSpecDegradesToEmptySlice(t *testing.T) {
	o := orchWithRepo(t.TempDir(), 1)
	brief := o.buildBrief(core.Issue{ID: "iss-1", Role: "implement", Spec: "specs/gone.md"}, config.Stage{}, core.Soul{})
	if brief.Spec != "" {
		t.Errorf("an unresolvable spec must degrade to an empty slice, got %q", brief.Spec)
	}
}

// Accepting an agent stage threads the issue's governing spec forward onto the produced
// next-stage issue, like Base/TraceMap, so author-tests, implement, and qa of one epic all
// resolve the same contract (T3.5).
func TestHandleResultAcceptThreadsSpecForward(t *testing.T) {
	cfg := kernelConfig(2)
	cfg.Harness.DAG["implement"] = config.Stage{Role: "implement", Produces: []string{"qa"}, OnFailure: "implement"}
	cfg.Harness.DAG["qa"] = config.Stage{Role: "qa", Produces: []string{"integrate"}}
	cfg.Souls = append(cfg.Souls, core.Soul{Name: "qa-soul", Role: "qa", Sandbox: "go-toolchain"})
	bd := newFakeBeads()
	is := inProgress("iss-1", "implement", 0)
	is.Spec = "specs/orders.md" // set at seed/plan, threaded this far
	bd.put(is)
	o, _ := newOrch(t, cfg, bd, &fakeGate{report: gate.Report{Passed: true}}, &fakeMerger{})

	res := core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: "candidate/iss-1"}}
	if _, err := o.handleResult(context.Background(), res); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	_, _, _, _, applied := bd.snap()
	if len(applied) != 1 || applied[0].Issue.Role != "qa" {
		t.Fatalf("applied = %+v, want one qa proposal", applied)
	}
	if applied[0].Issue.Spec != "specs/orders.md" {
		t.Errorf("produced issue Spec = %q, want specs/orders.md (threaded forward)", applied[0].Issue.Spec)
	}
}

// Routing a failure to a fresh attempt preserves the governing spec, so a retry resolves
// the same contract its predecessor did (alongside Base/TraceMap/Tags).
func TestRoutePreservesSpec(t *testing.T) {
	cfg := kernelConfig(2)
	bd := newFakeBeads()
	is := inProgress("iss-1", "implement", 0)
	is.Spec = "specs/orders.md"
	bd.put(is)
	o, _ := newOrch(t, cfg, bd, &fakeGate{report: gate.Report{Passed: false}}, &fakeMerger{})

	res := core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: "candidate/iss-1"}}
	if _, err := o.handleResult(context.Background(), res); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	_, _, _, _, applied := bd.snap()
	if len(applied) != 1 {
		t.Fatalf("applied = %+v, want one fix proposal", applied)
	}
	if fix := applied[0].Issue; fix.Spec != "specs/orders.md" || fix.Attempt != 1 {
		t.Errorf("fix issue Spec/Attempt = %q/%d, want specs/orders.md/1 (preserved across the retry)", fix.Spec, fix.Attempt)
	}
}
