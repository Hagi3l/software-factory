package gate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/Loxstomper/harness/internal/artifact"
	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/sandbox"
)

// testStore is a real content-addressed files store rooted in a temp dir, so the gate
// tests exercise the actual persistence path rather than a fake.
func testStore(t *testing.T) artifact.Store {
	t.Helper()
	s, err := artifact.NewFilesStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesStore: %v", err)
	}
	return s
}

// readArtifact returns the bytes stored under hash, failing the test if absent.
func readArtifact(t *testing.T, s artifact.Store, hash string) []byte {
	t.Helper()
	rc, err := s.Get(context.Background(), hash)
	if err != nil {
		t.Fatalf("Get %s: %v", hash, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	return b
}

// erroringStore fails every Put, modeling a store outage so a test can prove evidence
// persistence is best-effort: the verdict stands and the ref degrades to empty.
type erroringStore struct{}

func (erroringStore) Put(context.Context, string, io.Reader) (core.ArtifactRef, error) {
	return core.ArtifactRef{}, errors.New("artifact store is down")
}
func (erroringStore) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("artifact store is down")
}
func (erroringStore) Has(context.Context, string) (bool, error) { return false, nil }

// --- fakes -------------------------------------------------------------------

// scriptedSandbox returns a canned ExecResult per `sh -c` command string, records the
// commands it ran and how many times it was torn down. If execErr is set every Exec
// fails to run (modeling a dead sandbox).
type scriptedSandbox struct {
	id        string
	results   map[string]sandbox.ExecResult
	execErr   error
	execed    []string
	teardowns int
}

func (s *scriptedSandbox) ID() string { return s.id }

func (s *scriptedSandbox) Exec(_ context.Context, cmd sandbox.Command) (sandbox.ExecResult, error) {
	if s.execErr != nil {
		return sandbox.ExecResult{}, s.execErr
	}
	key := ""
	if cmd.Path == "sh" && len(cmd.Args) == 2 {
		key = cmd.Args[1]
	}
	s.execed = append(s.execed, key)
	return s.results[key], nil
}

func (s *scriptedSandbox) Teardown(context.Context) error { s.teardowns++; return nil }

type fakeBackend struct {
	sb          *scriptedSandbox            // returned when byRef has no entry for the seeded ref
	byRef       map[string]*scriptedSandbox // seeded ref -> sandbox, for the two-sandbox red→green path
	provErr     error
	gotSpec     sandbox.Spec   // the last spec provisioned (single-sandbox assertions)
	specs       []sandbox.Spec // every spec provisioned, in order
	provisioned int
}

func (b *fakeBackend) Provision(_ context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	b.provisioned++
	b.gotSpec = spec
	b.specs = append(b.specs, spec)
	if b.provErr != nil {
		return nil, b.provErr
	}
	// A red→green gate provisions one sandbox per ref (base + candidate); byRef lets a
	// test script each ref independently. Command-check tests set only sb.
	if b.byRef != nil {
		if sb, ok := b.byRef[spec.Workspace.BaseRef]; ok {
			return sb, nil
		}
	}
	return b.sb, nil
}

// testRegistry maps the two bootstrap-style command checks the gate tests script. A
// stage's postconditions resolve to commands through it, the way config.Harness.Checks
// does at run time.
func testRegistry() Registry {
	return Registry{"build": "make build", "test": "make test-unit"}
}

func testCandidate() Candidate {
	return Candidate{
		Repo:           "/repo",
		Ref:            core.CandidateBranch("issue-1"),
		Postconditions: []string{"build", "test"},
		Profile:        "go-toolchain",
		Limits:         config.SandboxLimits{CPU: 2, Mem: "2Gi", Wall: config.Duration(30 * time.Minute)},
	}
}

// --- tests -------------------------------------------------------------------

// Resolve turns declared postconditions into ordered checks via the registry, and a
// postcondition with no registered command is an error (a config/gate disagreement).
func TestRegistryResolve(t *testing.T) {
	checks, err := testRegistry().Resolve([]string{"test", "build"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Order follows the postcondition list, and each check's Name is the postcondition
	// identifier it realizes (so the report and provenance cite the declared name).
	if len(checks) != 2 || checks[0].Name != "test" || checks[0].Cmd != "make test-unit" || checks[1].Name != "build" {
		t.Fatalf("Resolve = %+v, want [test->make test-unit, build->make build] in order", checks)
	}
}

func TestRegistryResolveUnknownErrors(t *testing.T) {
	if _, err := testRegistry().Resolve([]string{"build", "mutation>=0.8"}); err == nil {
		t.Fatal("Resolve accepted a postcondition with no registered command, want an error")
	}
}

// All checks pass: the report is green, the verification sandbox is seeded with the
// candidate branch ref (a clean checkout distinct from the producer's), and it is
// always torn down.
func TestRunAllChecksPass(t *testing.T) {
	sb := &scriptedSandbox{id: "gate-sb", results: map[string]sandbox.ExecResult{
		"make build":     {ExitCode: 0, Stdout: []byte("built")},
		"make test-unit": {ExitCode: 0, Stdout: []byte("ok")},
	}}
	be := &fakeBackend{sb: sb}
	store := testStore(t)
	g := New(be, testRegistry(), store, t.TempDir(), nil, nil)

	report, err := g.Run(context.Background(), testCandidate())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed {
		t.Errorf("report.Passed = false, want true")
	}
	if len(report.Checks) != 2 {
		t.Fatalf("ran %d checks, want 2", len(report.Checks))
	}
	if !report.Checks[0].Passed || string(report.Checks[0].Stdout) != "built" {
		t.Errorf("build check = %+v, want passed with captured stdout", report.Checks[0])
	}
	// Every check's evidence is persisted to the artifact store and its ref stamped onto
	// the result, so the orchestrator can cite each verified check by hash. The persisted
	// record is content-addressed and carries the check's name, command, and captured
	// output.
	for _, want := range []struct {
		idx    int
		name   string
		stdout string
	}{{0, "build", "built"}, {1, "test", "ok"}} {
		cr := report.Checks[want.idx]
		if cr.Evidence.Hash == "" {
			t.Fatalf("%s check has no persisted evidence ref", want.name)
		}
		if cr.Evidence.Kind != core.ArtifactKindGateEvidence {
			t.Errorf("%s evidence kind = %q, want %q", want.name, cr.Evidence.Kind, core.ArtifactKindGateEvidence)
		}
		ev := readArtifact(t, store, cr.Evidence.Hash)
		if !bytes.Contains(ev, []byte("check: "+want.name)) || !bytes.Contains(ev, []byte(want.stdout)) {
			t.Errorf("%s evidence = %q, want it to record the check name and captured stdout", want.name, ev)
		}
	}
	// The verification sandbox must be seeded from the candidate branch, not the base.
	if be.gotSpec.Workspace.BaseRef != core.CandidateBranch("issue-1") {
		t.Errorf("seeded ref = %q, want the candidate branch", be.gotSpec.Workspace.BaseRef)
	}
	if be.gotSpec.Profile != "go-toolchain" {
		t.Errorf("profile = %q, want go-toolchain", be.gotSpec.Profile)
	}
	if be.gotSpec.Broker.Network != "unix" || be.gotSpec.Broker.Address == "" {
		t.Errorf("broker endpoint = %+v, want a unix socket", be.gotSpec.Broker)
	}
	if sb.teardowns != 1 {
		t.Errorf("teardowns = %d, want 1 (always reaped)", sb.teardowns)
	}
}

// A failing check fails the gate and stops the run (fail-fast): a failed build means
// the test never runs.
func TestRunStopsAtFirstFailure(t *testing.T) {
	sb := &scriptedSandbox{id: "gate-sb", results: map[string]sandbox.ExecResult{
		"make build":     {ExitCode: 2, Stderr: []byte("compile error")},
		"make test-unit": {ExitCode: 0},
	}}
	be := &fakeBackend{sb: sb}
	store := testStore(t)
	g := New(be, testRegistry(), store, t.TempDir(), nil, nil)

	report, err := g.Run(context.Background(), testCandidate())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Passed {
		t.Errorf("report.Passed = true, want false")
	}
	if len(report.Checks) != 1 {
		t.Fatalf("ran %d checks, want 1 (fail-fast after build)", len(report.Checks))
	}
	if report.Checks[0].ExitCode != 2 || string(report.Checks[0].Stderr) != "compile error" {
		t.Errorf("build result = %+v, want exit 2 with captured stderr", report.Checks[0])
	}
	// A rejected gate's output is exactly what a human triages, so the failing check's
	// evidence must be persisted too — recording the fail verdict and captured stderr.
	if report.Checks[0].Evidence.Hash == "" {
		t.Fatal("failing check has no persisted evidence ref")
	}
	ev := readArtifact(t, store, report.Checks[0].Evidence.Hash)
	if !bytes.Contains(ev, []byte("status: fail")) || !bytes.Contains(ev, []byte("compile error")) {
		t.Errorf("failing-check evidence = %q, want it to record the fail status and stderr", ev)
	}
	if len(sb.execed) != 1 || sb.execed[0] != "make build" {
		t.Errorf("commands run = %v, want only [make build]", sb.execed)
	}
	if sb.teardowns != 1 {
		t.Errorf("teardowns = %d, want 1", sb.teardowns)
	}
}

// A sandbox that cannot run a check at all is a gate infrastructure failure (error),
// not a gate verdict — the orchestrator retries it rather than routing on_failure.
func TestRunExecErrorIsInfraFailure(t *testing.T) {
	sb := &scriptedSandbox{id: "gate-sb", execErr: errors.New("sandbox is gone")}
	be := &fakeBackend{sb: sb}
	g := New(be, testRegistry(), nil, t.TempDir(), nil, nil)

	if _, err := g.Run(context.Background(), testCandidate()); err == nil {
		t.Fatal("Run returned nil error for a dead sandbox, want an error")
	}
	if sb.teardowns != 1 {
		t.Errorf("teardowns = %d, want 1 (reaped even on infra failure)", sb.teardowns)
	}
}

// A stage that declared no postconditions resolves to no checks, which would pass
// every candidate — a configuration error, caught before any sandbox is provisioned.
func TestRunNoChecksErrors(t *testing.T) {
	be := &fakeBackend{sb: &scriptedSandbox{id: "gate-sb"}}
	g := New(be, testRegistry(), nil, t.TempDir(), nil, nil)
	c := testCandidate()
	c.Postconditions = nil
	if _, err := g.Run(context.Background(), c); err == nil {
		t.Fatal("Run accepted a gate with no checks, want an error")
	}
	if be.provisioned != 0 {
		t.Errorf("provisioned %d sandboxes for a checkless gate, want 0", be.provisioned)
	}
}

// An unresolvable postcondition (no command in the registry) is a config fault and
// fails the gate before a sandbox is spent, not after.
func TestRunUnresolvablePostconditionErrors(t *testing.T) {
	be := &fakeBackend{sb: &scriptedSandbox{id: "gate-sb"}}
	g := New(be, testRegistry(), nil, t.TempDir(), nil, nil)
	c := testCandidate()
	c.Postconditions = []string{"build", "no-such-check"}
	if _, err := g.Run(context.Background(), c); err == nil {
		t.Fatal("Run accepted an unresolvable postcondition, want an error")
	}
	if be.provisioned != 0 {
		t.Errorf("provisioned %d sandboxes for an unresolvable gate, want 0", be.provisioned)
	}
}

// A provisioning failure surfaces as an error (the gate could not produce a verdict).
func TestRunProvisionFailure(t *testing.T) {
	be := &fakeBackend{provErr: errors.New("no host capacity")}
	g := New(be, testRegistry(), nil, t.TempDir(), nil, nil)
	if _, err := g.Run(context.Background(), testCandidate()); err == nil {
		t.Fatal("Run returned nil error when provisioning failed, want an error")
	}
}

// A store that fails every write must not change the verdict: evidence persistence is
// best-effort (it degrades provenance, never correctness). The gate still passes the
// candidate; the evidence refs are simply empty.
func TestRunEvidencePersistenceIsBestEffort(t *testing.T) {
	sb := &scriptedSandbox{id: "gate-sb", results: map[string]sandbox.ExecResult{
		"make build":     {ExitCode: 0, Stdout: []byte("built")},
		"make test-unit": {ExitCode: 0, Stdout: []byte("ok")},
	}}
	be := &fakeBackend{sb: sb}
	g := New(be, testRegistry(), erroringStore{}, t.TempDir(), nil, nil)

	report, err := g.Run(context.Background(), testCandidate())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed || len(report.Checks) != 2 {
		t.Fatalf("report = %+v, want a green verdict over 2 checks despite the store outage", report)
	}
	for _, cr := range report.Checks {
		if cr.Evidence.Hash != "" {
			t.Errorf("%s check has evidence ref %q, want empty (store write failed)", cr.Name, cr.Evidence.Hash)
		}
	}
}

// --- red→green proof (T2.3) --------------------------------------------------

// redGreenRegistry maps the acceptance-test check the red→green proof reuses. The proof
// has no entry of its own; it resolves to the tests-pass command, run against two refs.
func redGreenRegistry() Registry {
	return Registry{core.CheckAcceptanceTests: "make test-unit"}
}

// redGreenCandidate is a candidate whose only postcondition is the red→green proof, with
// the base ref the orchestrator threads (the ref the candidate branched from).
func redGreenCandidate() Candidate {
	c := testCandidate()
	c.Postconditions = []string{core.PostconditionRedGreen}
	c.BaseRef = "main"
	return c
}

// Resolve binds the reserved red→green proof to the acceptance-test command (it has no
// entry of its own), and marks it as the proof kind so Run executes it against two refs.
func TestResolveRedGreenProof(t *testing.T) {
	checks, err := redGreenRegistry().Resolve([]string{core.PostconditionRedGreen})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(checks) != 1 || checks[0].Name != core.PostconditionRedGreen || checks[0].Cmd != "make test-unit" {
		t.Fatalf("Resolve = %+v, want the proof bound to the tests-pass command", checks)
	}
	if checks[0].kind != redGreenProof {
		t.Errorf("kind = %d, want redGreenProof", checks[0].kind)
	}
}

// Declaring the proof with no acceptance-test command registered is a config fault — the
// gate cannot resolve a command to run, so it errors before any sandbox is spent.
func TestResolveRedGreenWithoutAcceptanceCommandErrors(t *testing.T) {
	if _, err := (Registry{"gosec": "gosec ./..."}).Resolve([]string{core.PostconditionRedGreen}); err == nil {
		t.Fatal("Resolve accepted the red→green proof with no acceptance-test command, want an error")
	}
}

// The proof passes when the acceptance tests FAIL on the base (red) and PASS on the
// candidate (green). The gate provisions two sandboxes — one per ref — and the evidence
// record captures both runs so the proof is auditable.
func TestRunRedGreenProofPasses(t *testing.T) {
	base := &scriptedSandbox{id: "base-sb", results: map[string]sandbox.ExecResult{
		"make test-unit": {ExitCode: 1, Stdout: []byte("FAIL: feature absent")},
	}}
	cand := &scriptedSandbox{id: "cand-sb", results: map[string]sandbox.ExecResult{
		"make test-unit": {ExitCode: 0, Stdout: []byte("ok: feature present")},
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
	if !report.Passed || len(report.Checks) != 1 {
		t.Fatalf("report = %+v, want a single passing proof", report)
	}
	cr := report.Checks[0]
	if cr.Base == nil || cr.Base.ExitCode != 1 || cr.ExitCode != 0 {
		t.Fatalf("proof result = %+v, want base exit 1 (red) and candidate exit 0 (green)", cr)
	}
	// Two sandboxes were provisioned, one seeded at the base and one at the candidate.
	if be.provisioned != 2 {
		t.Fatalf("provisioned %d sandboxes, want 2 (base + candidate)", be.provisioned)
	}
	seeded := map[string]bool{}
	for _, s := range be.specs {
		seeded[s.Workspace.BaseRef] = true
	}
	if !seeded["main"] || !seeded[core.CandidateBranch("issue-1")] {
		t.Errorf("seeded refs = %v, want both the base (main) and the candidate", seeded)
	}
	if base.teardowns != 1 || cand.teardowns != 1 {
		t.Errorf("teardowns base=%d cand=%d, want 1 each", base.teardowns, cand.teardowns)
	}
	// The evidence record cites both halves: the failing base run and the passing
	// candidate run, so a human can audit that the tests actually exercise the change.
	if cr.Evidence.Hash == "" {
		t.Fatal("proof has no persisted evidence ref")
	}
	ev := readArtifact(t, store, cr.Evidence.Hash)
	for _, want := range []string{"kind: red-green", "status: pass", "FAIL: feature absent", "ok: feature present"} {
		if !bytes.Contains(ev, []byte(want)) {
			t.Errorf("evidence = %q, want it to contain %q", ev, want)
		}
	}
}

// The proof FAILS when the tests pass on the base too: that means they don't exercise
// the new behavior (vacuously green) — exactly what red→green exists to catch. The
// gate must not accept such a candidate even though its own tests are green.
func TestRunRedGreenFailsWhenBaseIsNotRed(t *testing.T) {
	base := &scriptedSandbox{id: "base-sb", results: map[string]sandbox.ExecResult{
		"make test-unit": {ExitCode: 0, Stdout: []byte("ok even without the change")},
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
	if report.Passed {
		t.Fatal("report.Passed = true, want false (tests are green on the base, so vacuous)")
	}
	ev := readArtifact(t, store, report.Checks[0].Evidence.Hash)
	if !bytes.Contains(ev, []byte("status: fail")) {
		t.Errorf("evidence = %q, want status: fail", ev)
	}
}

// The proof FAILS when the tests do not pass on the candidate: the change does not make
// the acceptance tests green. (Here the base is red, as required, but the candidate
// stays red too.)
func TestRunRedGreenFailsWhenCandidateIsNotGreen(t *testing.T) {
	base := &scriptedSandbox{id: "base-sb", results: map[string]sandbox.ExecResult{
		"make test-unit": {ExitCode: 1},
	}}
	cand := &scriptedSandbox{id: "cand-sb", results: map[string]sandbox.ExecResult{
		"make test-unit": {ExitCode: 1, Stderr: []byte("still failing")},
	}}
	be := &fakeBackend{byRef: map[string]*scriptedSandbox{
		"main":                          base,
		core.CandidateBranch("issue-1"): cand,
	}}
	g := New(be, redGreenRegistry(), testStore(t), t.TempDir(), nil, nil)

	report, err := g.Run(context.Background(), redGreenCandidate())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Passed {
		t.Fatal("report.Passed = true, want false (candidate tests are not green)")
	}
}

// A red→green proof with no base ref threaded is a wiring fault: it fails before any
// sandbox is provisioned, the way other resolution faults do.
func TestRunRedGreenMissingBaseRefErrors(t *testing.T) {
	be := &fakeBackend{sb: &scriptedSandbox{id: "gate-sb"}}
	g := New(be, redGreenRegistry(), nil, t.TempDir(), nil, nil)
	c := redGreenCandidate()
	c.BaseRef = ""
	if _, err := g.Run(context.Background(), c); err == nil {
		t.Fatal("Run accepted a red→green proof with no base ref, want an error")
	}
	if be.provisioned != 0 {
		t.Errorf("provisioned %d sandboxes for a base-less proof, want 0", be.provisioned)
	}
}

// A command check mixed with a red→green proof runs both, each against the right ref(s):
// the command check only on the candidate, the proof on base + candidate.
func TestRunRedGreenWithCommandCheck(t *testing.T) {
	base := &scriptedSandbox{id: "base-sb", results: map[string]sandbox.ExecResult{
		"make test-unit": {ExitCode: 1},
	}}
	cand := &scriptedSandbox{id: "cand-sb", results: map[string]sandbox.ExecResult{
		"make test-unit": {ExitCode: 0},
		"gosec ./...":    {ExitCode: 0},
	}}
	be := &fakeBackend{byRef: map[string]*scriptedSandbox{
		"main":                          base,
		core.CandidateBranch("issue-1"): cand,
	}}
	reg := Registry{core.CheckAcceptanceTests: "make test-unit", "gosec": "gosec ./..."}
	g := New(be, reg, testStore(t), t.TempDir(), nil, nil)
	c := redGreenCandidate()
	c.Postconditions = []string{core.PostconditionRedGreen, "gosec"}

	report, err := g.Run(context.Background(), c)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed || len(report.Checks) != 2 {
		t.Fatalf("report = %+v, want both checks passing", report)
	}
	// The command check ran only against the candidate; the base sandbox ran the tests
	// once (the red half).
	if len(base.execed) != 1 || base.execed[0] != "make test-unit" {
		t.Errorf("base ran %v, want only [make test-unit]", base.execed)
	}
}

// testsRedCandidate is a candidate whose only postcondition is the tests-red proof —
// the author-tests stage's gate. Unlike red→green it needs no base ref: the acceptance
// tests run once, against the candidate, and must fail.
func testsRedCandidate() Candidate {
	c := testCandidate()
	c.Postconditions = []string{core.PostconditionTestsRed}
	c.BaseRef = ""
	return c
}

// Resolve binds the reserved tests-red proof to the acceptance-test command (it has no
// entry of its own) and marks it the redProof kind, so Run grades it on a nonzero exit.
func TestResolveTestsRedProof(t *testing.T) {
	checks, err := redGreenRegistry().Resolve([]string{core.PostconditionTestsRed})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(checks) != 1 || checks[0].Name != core.PostconditionTestsRed || checks[0].Cmd != "make test-unit" {
		t.Fatalf("Resolve = %+v, want the proof bound to the tests-pass command", checks)
	}
	if checks[0].kind != redProof {
		t.Errorf("kind = %d, want redProof", checks[0].kind)
	}
}

// The tests-red proof passes when the acceptance tests FAIL on the candidate: the test
// author wrote real, executing tests that fail because no implementation exists yet. It
// spends exactly one sandbox (no base ref), and the evidence labels the nonzero exit as
// the expected outcome so it does not read as a contradiction.
func TestRunTestsRedProofPassesWhenCandidateFails(t *testing.T) {
	cand := &scriptedSandbox{id: "cand-sb", results: map[string]sandbox.ExecResult{
		"make test-unit": {ExitCode: 1, Stdout: []byte("FAIL: behavior not implemented")},
	}}
	be := &fakeBackend{sb: cand}
	store := testStore(t)
	g := New(be, redGreenRegistry(), store, t.TempDir(), nil, nil)

	report, err := g.Run(context.Background(), testsRedCandidate())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed || len(report.Checks) != 1 {
		t.Fatalf("report = %+v, want a single passing proof", report)
	}
	cr := report.Checks[0]
	if cr.Base != nil || cr.ExitCode != 1 || !cr.Passed {
		t.Fatalf("proof result = %+v, want no base, candidate exit 1, passed", cr)
	}
	// One sandbox only — the proof has no base ref.
	if be.provisioned != 1 {
		t.Fatalf("provisioned %d sandboxes, want 1 (candidate only)", be.provisioned)
	}
	ev := readArtifact(t, store, cr.Evidence.Hash)
	for _, want := range []string{"kind: tests-red", "status: pass", "must be nonzero", "FAIL: behavior not implemented"} {
		if !bytes.Contains(ev, []byte(want)) {
			t.Errorf("evidence = %q, want it to contain %q", ev, want)
		}
	}
}

// The tests-red proof FAILS when the acceptance tests PASS on the candidate: a suite
// that is green with no implementation is vacuous (or tests nothing), exactly what this
// proof exists to catch before an implement attempt is spent on it.
func TestRunTestsRedProofFailsWhenCandidatePasses(t *testing.T) {
	cand := &scriptedSandbox{id: "cand-sb", results: map[string]sandbox.ExecResult{
		"make test-unit": {ExitCode: 0, Stdout: []byte("ok with no implementation — vacuous")},
	}}
	be := &fakeBackend{sb: cand}
	store := testStore(t)
	g := New(be, redGreenRegistry(), store, t.TempDir(), nil, nil)

	report, err := g.Run(context.Background(), testsRedCandidate())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Passed {
		t.Fatal("report.Passed = true, want false (tests are green with no implementation, so vacuous)")
	}
	ev := readArtifact(t, store, report.Checks[0].Evidence.Hash)
	if !bytes.Contains(ev, []byte("status: fail")) {
		t.Errorf("evidence = %q, want status: fail", ev)
	}
}

// --- metric check (T2.7) -----------------------------------------------------

// mutationRegistry maps the mutation metric to its measurement command. A "mutation>=X"
// postcondition resolves to this command under the metric name, the way a command-check
// postcondition resolves under its own name.
func mutationRegistry() Registry {
	return Registry{core.MetricMutation: "measure-mutation"}
}

// mutationCandidate is a candidate whose only postcondition is a mutation-score threshold.
// A metric check needs no base ref — it runs once, against the candidate.
func mutationCandidate() Candidate {
	c := testCandidate()
	c.Postconditions = []string{"mutation>=0.8"}
	c.BaseRef = ""
	return c
}

// Resolve binds a metric comparison to the command registered under its metric name and
// records the operator and threshold, so Run grades the printed score against them.
func TestResolveMetricCheck(t *testing.T) {
	checks, err := mutationRegistry().Resolve([]string{"mutation>=0.8"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(checks) != 1 || checks[0].Name != "mutation>=0.8" || checks[0].Cmd != "measure-mutation" {
		t.Fatalf("Resolve = %+v, want the comparison bound to the mutation command", checks)
	}
	if checks[0].kind != metricCheck || checks[0].op != ">=" || checks[0].threshold != 0.8 {
		t.Errorf("check = %+v, want metricCheck with op >= threshold 0.8", checks[0])
	}
}

// A metric comparison whose metric has no registered command is unresolvable — the same
// config fault as a missing command-check entry — and fails before any sandbox is spent.
func TestResolveMetricWithoutCommandErrors(t *testing.T) {
	if _, err := testRegistry().Resolve([]string{"mutation>=0.8"}); err == nil {
		t.Fatal("Resolve accepted a metric comparison with no registered command, want an error")
	}
}

// The metric check passes when the measurement command runs cleanly and prints a score
// that satisfies the comparison. It spends exactly one sandbox (no base ref), and the
// evidence records the measured score and the comparison so the verdict is auditable.
func TestRunMetricPassesAboveThreshold(t *testing.T) {
	sb := &scriptedSandbox{id: "gate-sb", results: map[string]sandbox.ExecResult{
		"measure-mutation": {ExitCode: 0, Stdout: []byte("mutation score: 0.85\n")},
	}}
	be := &fakeBackend{sb: sb}
	store := testStore(t)
	g := New(be, mutationRegistry(), store, t.TempDir(), nil, nil)

	report, err := g.Run(context.Background(), mutationCandidate())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed || len(report.Checks) != 1 {
		t.Fatalf("report = %+v, want a single passing metric check", report)
	}
	cr := report.Checks[0]
	if cr.Metric == nil || !cr.Metric.Parsed || cr.Metric.Score != 0.85 {
		t.Fatalf("metric result = %+v, want parsed score 0.85", cr.Metric)
	}
	if be.provisioned != 1 {
		t.Fatalf("provisioned %d sandboxes, want 1 (candidate only — a metric check needs no base)", be.provisioned)
	}
	ev := readArtifact(t, store, cr.Evidence.Hash)
	for _, want := range []string{"kind: metric", "score 0.85", "want >= 0.8", "status: pass"} {
		if !bytes.Contains(ev, []byte(want)) {
			t.Errorf("evidence = %q, want it to contain %q", ev, want)
		}
	}
}

// The metric check fails when the measured score is below the threshold — exactly the
// weak-test signal mutation testing exists to catch.
func TestRunMetricFailsBelowThreshold(t *testing.T) {
	sb := &scriptedSandbox{id: "gate-sb", results: map[string]sandbox.ExecResult{
		"measure-mutation": {ExitCode: 0, Stdout: []byte("0.5")},
	}}
	be := &fakeBackend{sb: sb}
	store := testStore(t)
	g := New(be, mutationRegistry(), store, t.TempDir(), nil, nil)

	report, err := g.Run(context.Background(), mutationCandidate())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Passed {
		t.Fatal("report.Passed = true, want false (measured score 0.5 is below the 0.8 threshold)")
	}
	if report.Checks[0].Metric.Score != 0.5 {
		t.Errorf("score = %v, want 0.5", report.Checks[0].Metric.Score)
	}
	ev := readArtifact(t, store, report.Checks[0].Evidence.Hash)
	if !bytes.Contains(ev, []byte("status: fail")) {
		t.Errorf("evidence = %q, want status: fail", ev)
	}
}

// A nonzero exit from the measurement command fails the check closed even when the printed
// score would pass: the tool could not measure cleanly, so the score is unverifiable and
// must not gate green.
func TestRunMetricFailsWhenToolErrors(t *testing.T) {
	sb := &scriptedSandbox{id: "gate-sb", results: map[string]sandbox.ExecResult{
		"measure-mutation": {ExitCode: 1, Stdout: []byte("0.99"), Stderr: []byte("gremlins: build failed")},
	}}
	be := &fakeBackend{sb: sb}
	g := New(be, mutationRegistry(), testStore(t), t.TempDir(), nil, nil)

	report, err := g.Run(context.Background(), mutationCandidate())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Passed {
		t.Fatal("report.Passed = true, want false (the tool exited nonzero, so its score is unverifiable)")
	}
}

// Output that does not parse to a number fails the check closed: an unmeasurable score is
// not a passing one, and the evidence says so rather than recording a phantom 0.
func TestRunMetricFailsOnUnparseableScore(t *testing.T) {
	sb := &scriptedSandbox{id: "gate-sb", results: map[string]sandbox.ExecResult{
		"measure-mutation": {ExitCode: 0, Stdout: []byte("scan produced no score")},
	}}
	be := &fakeBackend{sb: sb}
	store := testStore(t)
	g := New(be, mutationRegistry(), store, t.TempDir(), nil, nil)

	report, err := g.Run(context.Background(), mutationCandidate())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Passed {
		t.Fatal("report.Passed = true, want false (the command printed no parseable score)")
	}
	if report.Checks[0].Metric.Parsed {
		t.Error("Metric.Parsed = true, want false for unparseable output")
	}
	ev := readArtifact(t, store, report.Checks[0].Evidence.Hash)
	if !bytes.Contains(ev, []byte("score unparseable")) {
		t.Errorf("evidence = %q, want it to record an unparseable score", ev)
	}
}

// --- independent scanners (T2.6) ---------------------------------------------

// scannerRegistry maps the three spec-independent scanners the qa gate runs as plain
// command checks: SAST, vulnerability, and dependency/license. They need no built-in
// check kind — the gate already grades a command on its exit code — so they resolve and
// run exactly like tests-pass (see specs/verification.md, specs/configuration.md).
func scannerRegistry() Registry {
	return Registry{
		"gosec":        "gosec ./...",
		"govulncheck":  "govulncheck ./...",
		"license-scan": "go-licenses check ./...",
	}
}

// The three scanners resolve to ordinary command checks (cmdCheck) — defense in depth is
// "many independent checks," and each is just a command graded on exit code, not a new
// kind. This pins that the generality T2.1/T2.2 built already absorbs scanners.
func TestResolveScannerChecks(t *testing.T) {
	checks, err := scannerRegistry().Resolve([]string{"gosec", "govulncheck", "license-scan"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(checks) != 3 {
		t.Fatalf("resolved %d checks, want 3", len(checks))
	}
	for i, want := range []struct{ name, cmd string }{
		{"gosec", "gosec ./..."},
		{"govulncheck", "govulncheck ./..."},
		{"license-scan", "go-licenses check ./..."},
	} {
		if checks[i].Name != want.name || checks[i].Cmd != want.cmd || checks[i].kind != cmdCheck {
			t.Errorf("check[%d] = %+v, want %s -> %q as a command check", i, checks[i], want.name, want.cmd)
		}
	}
}

// All three scanners clean (exit 0): the gate passes, runs each once against the
// candidate, and emits each scanner's captured report as evidence cited by its name — so
// the provenance trailer can list gosec@<hash>, govulncheck@<hash>, license-scan@<hash>.
// This is the "each a gate check emitting evidence" contract for T2.6.
func TestRunScannerChecksEmitEvidence(t *testing.T) {
	sb := &scriptedSandbox{id: "gate-sb", results: map[string]sandbox.ExecResult{
		"gosec ./...":             {ExitCode: 0, Stdout: []byte("Issues: 0")},
		"govulncheck ./...":       {ExitCode: 0, Stdout: []byte("No vulnerabilities found.")},
		"go-licenses check ./...": {ExitCode: 0, Stdout: []byte("all licenses allowed")},
	}}
	be := &fakeBackend{sb: sb}
	store := testStore(t)
	g := New(be, scannerRegistry(), store, t.TempDir(), nil, nil)

	c := testCandidate()
	c.Postconditions = []string{"gosec", "govulncheck", "license-scan"}

	report, err := g.Run(context.Background(), c)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed || len(report.Checks) != 3 {
		t.Fatalf("report = %+v, want a green verdict over 3 scanner checks", report)
	}
	for _, want := range []struct{ name, report string }{
		{"gosec", "Issues: 0"},
		{"govulncheck", "No vulnerabilities found."},
		{"license-scan", "all licenses allowed"},
	} {
		var cr *CheckResult
		for i := range report.Checks {
			if report.Checks[i].Name == want.name {
				cr = &report.Checks[i]
			}
		}
		if cr == nil {
			t.Fatalf("no result for scanner %q", want.name)
		}
		if cr.Evidence.Hash == "" {
			t.Fatalf("%s scanner has no persisted evidence ref to cite in provenance", want.name)
		}
		ev := readArtifact(t, store, cr.Evidence.Hash)
		if !bytes.Contains(ev, []byte("check: "+want.name)) || !bytes.Contains(ev, []byte(want.report)) {
			t.Errorf("%s evidence = %q, want it to record the scanner name and its captured report", want.name, ev)
		}
	}
}

// A scanner that reports findings (non-zero exit) fails the gate closed, and its report
// is persisted as evidence — a rejected security gate is exactly what a human triages from
// the dead-letter queue, so the findings must survive the sandbox teardown.
func TestRunScannerFindingsFailClosed(t *testing.T) {
	sb := &scriptedSandbox{id: "gate-sb", results: map[string]sandbox.ExecResult{
		"govulncheck ./...": {ExitCode: 3, Stdout: []byte("Vulnerability #1: GO-2024-0001 in golang.org/x/net")},
	}}
	be := &fakeBackend{sb: sb}
	store := testStore(t)
	g := New(be, scannerRegistry(), store, t.TempDir(), nil, nil)

	c := testCandidate()
	c.Postconditions = []string{"govulncheck"}

	report, err := g.Run(context.Background(), c)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Passed {
		t.Fatal("report.Passed = true, want false (the vulnerability scanner reported a finding)")
	}
	ev := readArtifact(t, store, report.Checks[0].Evidence.Hash)
	if !bytes.Contains(ev, []byte("status: fail")) || !bytes.Contains(ev, []byte("GO-2024-0001")) {
		t.Errorf("evidence = %q, want it to record the fail status and the vulnerability finding", ev)
	}
}
