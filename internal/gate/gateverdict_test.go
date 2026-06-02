package gate

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Loxstomper/harness/internal/artifact"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/sandbox"
)

// decodeVerdict reads the gate-verdict record the gate harvested and decodes it, failing the
// test if it is absent or malformed.
func decodeVerdict(t *testing.T, s artifact.Store, hash string) core.GateVerdict {
	t.Helper()
	if hash == "" {
		t.Fatal("report carries no verdict-record hash")
	}
	var v core.GateVerdict
	if err := json.Unmarshal(readArtifact(t, s, hash), &v); err != nil {
		t.Fatalf("decode verdict record: %v", err)
	}
	return v
}

// TestRunHarvestsCommandVerdict proves a passing command-check gate harvests an assembled
// gate-verdict record — kind "command" per check, Passed true, each check citing its own
// gate-evidence hash (the record is the index over the per-check output). The record is
// stamped onto the Report so the orchestrator can thread it onto the issue.
func TestRunHarvestsCommandVerdict(t *testing.T) {
	sb := &scriptedSandbox{id: "gate-sb", results: map[string]sandbox.ExecResult{
		"make build":     {ExitCode: 0, Stdout: []byte("built")},
		"make test-unit": {ExitCode: 0, Stdout: []byte("ok")},
	}}
	store := testStore(t)
	g := New(&fakeBackend{sb: sb}, testRegistry(), store, t.TempDir(), nil, nil)

	report, err := g.Run(context.Background(), testCandidate())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Verdict.Kind != core.ArtifactKindGateVerdict {
		t.Errorf("verdict ref kind = %q, want %q", report.Verdict.Kind, core.ArtifactKindGateVerdict)
	}
	v := decodeVerdict(t, store, report.Verdict.Hash)
	if !v.Passed || len(v.Checks) != 2 {
		t.Fatalf("verdict = %+v, want Passed over 2 checks", v)
	}
	for i, want := range []string{"build", "test"} {
		c := v.Checks[i]
		if c.Name != want || c.Kind != core.GateCheckCommand || !c.Passed {
			t.Errorf("check %d = %+v, want command %q passed", i, c, want)
		}
		// The verdict indexes the per-check evidence by hash — the same hash stamped on the
		// Report's CheckResult, so the record points back at the captured output.
		if c.Evidence == "" || c.Evidence != report.Checks[i].Evidence.Hash {
			t.Errorf("check %d evidence = %q, want it to match the check's evidence hash %q",
				i, c.Evidence, report.Checks[i].Evidence.Hash)
		}
	}
}

// TestRunHarvestsRedGreenVerdict proves the verdict records a red→green proof with the base
// run's exit (the red half) alongside the candidate verdict, so the verification view can
// render "fails on base, passes on candidate" without re-reading the evidence blob.
func TestRunHarvestsRedGreenVerdict(t *testing.T) {
	base := &scriptedSandbox{id: "base-sb", results: map[string]sandbox.ExecResult{
		"make test-unit": {ExitCode: 1, Stdout: []byte("FAIL: feature absent")},
	}}
	cand := &scriptedSandbox{id: "cand-sb", results: map[string]sandbox.ExecResult{
		"make test-unit": {ExitCode: 0, Stdout: []byte("ok")},
	}}
	be := &fakeBackend{byRef: map[string]*scriptedSandbox{
		"main":                          base,
		core.CandidateBranch("issue-1"): cand,
	}}
	store := testStore(t)
	g := New(be, redGreenRegistry(), store, t.TempDir(), nil, nil)

	report, err := g.Run(context.Background(), redGreenCandidate())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	v := decodeVerdict(t, store, report.Verdict.Hash)
	if len(v.Checks) != 1 {
		t.Fatalf("verdict has %d checks, want 1", len(v.Checks))
	}
	c := v.Checks[0]
	if c.Kind != core.GateCheckRedGreen || !c.Passed || c.ExitCode != 0 {
		t.Errorf("red→green check = %+v, want kind red-green, passed, candidate exit 0", c)
	}
	if c.Base == nil || c.Base.ExitCode != 1 {
		t.Errorf("base run = %+v, want the red half's nonzero exit recorded", c.Base)
	}
	if c.Metric != nil {
		t.Errorf("Metric = %+v, want nil for a red→green proof", c.Metric)
	}
}

// TestRunHarvestsMetricVerdict proves a metric check's verdict carries the measured score and
// the comparison it was graded against — the mutation-score-vs-threshold the view renders.
func TestRunHarvestsMetricVerdict(t *testing.T) {
	sb := &scriptedSandbox{id: "gate-sb", results: map[string]sandbox.ExecResult{
		"measure-mutation": {ExitCode: 0, Stdout: []byte("score 0.91")},
	}}
	store := testStore(t)
	g := New(&fakeBackend{sb: sb}, mutationRegistry(), store, t.TempDir(), nil, nil)

	report, err := g.Run(context.Background(), mutationCandidate())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	v := decodeVerdict(t, store, report.Verdict.Hash)
	if len(v.Checks) != 1 {
		t.Fatalf("verdict has %d checks, want 1", len(v.Checks))
	}
	c := v.Checks[0]
	if c.Kind != core.GateCheckMetric || !c.Passed {
		t.Errorf("metric check = %+v, want kind metric, passed", c)
	}
	if c.Metric == nil || !c.Metric.Parsed || c.Metric.Score != 0.91 || c.Metric.Op != ">=" || c.Metric.Threshold != 0.8 {
		t.Errorf("metric = %+v, want score 0.91 (>= 0.8), parsed", c.Metric)
	}
}

// TestRunVerdictRecordsFailure proves a rejected gate still harvests a verdict (Passed=false)
// — a rejected candidate's verdict is exactly what a human triages from the dead-letter queue.
func TestRunVerdictRecordsFailure(t *testing.T) {
	sb := &scriptedSandbox{id: "gate-sb", results: map[string]sandbox.ExecResult{
		"make build":     {ExitCode: 2, Stderr: []byte("compile error")},
		"make test-unit": {ExitCode: 0},
	}}
	store := testStore(t)
	g := New(&fakeBackend{sb: sb}, testRegistry(), store, t.TempDir(), nil, nil)

	report, err := g.Run(context.Background(), testCandidate())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	v := decodeVerdict(t, store, report.Verdict.Hash)
	if v.Passed {
		t.Error("verdict Passed = true, want false (build failed)")
	}
	if len(v.Checks) != 1 || v.Checks[0].Passed || v.Checks[0].ExitCode != 2 {
		t.Errorf("verdict checks = %+v, want one failed build at exit 2", v.Checks)
	}
}

// TestRunVerdictHarvestIsBestEffort proves a store that fails every write leaves the verdict
// ref empty without changing the verdict — harvesting is best-effort, like per-check evidence.
func TestRunVerdictHarvestIsBestEffort(t *testing.T) {
	sb := &scriptedSandbox{id: "gate-sb", results: map[string]sandbox.ExecResult{
		"make build":     {ExitCode: 0},
		"make test-unit": {ExitCode: 0},
	}}
	g := New(&fakeBackend{sb: sb}, testRegistry(), erroringStore{}, t.TempDir(), nil, nil)

	report, err := g.Run(context.Background(), testCandidate())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed {
		t.Errorf("report.Passed = false, want true despite the store outage")
	}
	if report.Verdict.Hash != "" {
		t.Errorf("verdict ref = %q, want empty (store write failed)", report.Verdict.Hash)
	}
}
