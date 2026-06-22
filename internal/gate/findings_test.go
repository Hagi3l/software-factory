package gate

import (
	"context"
	"testing"
	"time"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/sandbox"
)

// A failing test, in go test -json form on stdout — the machine-readable output the gate's
// go-test adapter parses into findings.
const goTestJSONFail = `{"Action":"run","Package":"x","Test":"TestAdd"}
{"Action":"output","Package":"x","Test":"TestAdd","Output":"=== RUN   TestAdd\n"}
{"Action":"output","Package":"x","Test":"TestAdd","Output":"    add_test.go:12: want 5, got 4\n"}
{"Action":"output","Package":"x","Test":"TestAdd","Output":"--- FAIL: TestAdd (0.00s)\n"}
{"Action":"fail","Package":"x","Test":"TestAdd","Elapsed":0}
{"Action":"fail","Package":"x","Elapsed":0}
`

// A passing go test -json stream — ndjson-shaped, but no failures, so zero findings.
const goTestJSONPass = `{"Action":"run","Package":"x","Test":"TestAdd"}
{"Action":"pass","Package":"x","Test":"TestAdd","Elapsed":0}
{"Action":"pass","Package":"x","Elapsed":0}
`

// A gosec finding, in -fmt=json form.
const gosecJSON = `{"Issues":[{"severity":"HIGH","confidence":"HIGH","rule_id":"G401","details":"Use of weak cryptographic primitive","file":"crypto.go","line":"7","code":"md5.New()"}]}`

// TestAdapterForSelectsByCheckIdentity proves the per-tool adapter is chosen by the check's
// identity: the proofs and the acceptance-test check map to the go-test adapter; each scanner
// name to its own; an unknown name to nil (the graceful fallback — grade on exit code, no
// findings).
func TestAdapterForSelectsByCheckIdentity(t *testing.T) {
	cases := []struct {
		check   Check
		wantNil bool
	}{
		{Check{Name: core.CheckAcceptanceTests, kind: cmdCheck}, false},
		{Check{Name: core.PostconditionRedGreen, kind: redGreenProof}, false},
		{Check{Name: core.PostconditionTestsRed, kind: redProof}, false},
		{Check{Name: checkGosec, kind: cmdCheck}, false},
		{Check{Name: checkGovulncheck, kind: cmdCheck}, false},
		{Check{Name: checkGolangciLint, kind: cmdCheck}, false},
		{Check{Name: checkLicenseScan, kind: cmdCheck}, false},
		{Check{Name: "mutation", kind: metricCheck}, true},
		{Check{Name: "some-bespoke-scanner", kind: cmdCheck}, true},
	}
	for _, tc := range cases {
		got := adapterFor(tc.check)
		if (got == nil) != tc.wantNil {
			t.Errorf("adapterFor(%q kind=%d) nil=%v, want nil=%v", tc.check.Name, tc.check.kind, got == nil, tc.wantNil)
		}
	}
}

// TestFindingsForFallsBackToStderr proves stdout is parsed by default but stderr is the
// fallback when stdout is empty (a tool that wrote everything to stderr).
func TestFindingsForFallsBackToStderr(t *testing.T) {
	check := Check{Name: core.CheckAcceptanceTests, kind: cmdCheck}
	if fs := findingsFor(check, []byte(goTestJSONFail), nil); len(fs) != 1 {
		t.Fatalf("stdout parse: got %d findings, want 1", len(fs))
	}
	if fs := findingsFor(check, nil, []byte(goTestJSONFail)); len(fs) != 1 {
		t.Fatalf("stderr fallback: got %d findings, want 1", len(fs))
	}
	// No adapter → nil regardless of output.
	if fs := findingsFor(Check{Name: "bespoke", kind: cmdCheck}, []byte(goTestJSONFail), nil); fs != nil {
		t.Fatalf("no-adapter check yielded findings: %+v", fs)
	}
	// Human-format (non-ndjson) test output degrades to NO findings — never a fabricated
	// "build" finding from the parser's compile-failure path. This is the shipped-config case
	// (a `make` target that prints plain `ok / FAIL` or routes -json to a file).
	if fs := findingsFor(check, []byte("ok  \tx\t0.1s\n"), nil); len(fs) != 0 {
		t.Fatalf("human test output yielded findings: %+v", fs)
	}
}

// TestRunPopulatesPerCheckFindings is the end-to-end wire-up: when the registry's commands
// emit machine-readable output, the gate parses each check's output into findings, and the
// harvested verdict carries them — so the verification view and a retry Brief render the
// compact findings, not the raw dump (T9.5).
func TestRunPopulatesPerCheckFindings(t *testing.T) {
	const testsCmd = "go test -json ./..."
	const gosecCmd = "gosec -fmt=json ./..."
	// tests-pass PASSES (a non-independent failure would fail-fast and stop the run before the
	// scanner); the independent gosec scanner FAILS with a finding. This exercises both
	// adapters in one pass: a passing test stream → zero findings, a gosec hit → one finding.
	sb := &scriptedSandbox{id: "gate-sb", results: map[string]sandbox.ExecResult{
		testsCmd: {ExitCode: 0, Stdout: []byte(goTestJSONPass)},
		gosecCmd: {ExitCode: 1, Stdout: []byte(gosecJSON)},
	}}
	registry := Registry{core.CheckAcceptanceTests: testsCmd, checkGosec: gosecCmd}
	store := testStore(t)
	g := New(&fakeBackend{sb: sb}, registry, store, t.TempDir(), nil, nil, WithIndependentChecks([]string{checkGosec}))

	cand := Candidate{
		Repo:           "/repo",
		Ref:            core.CandidateBranch("issue-1"),
		Postconditions: []string{core.CheckAcceptanceTests, checkGosec},
		Profile:        "go-toolchain",
		Limits:         config.SandboxLimits{CPU: 2, Mem: "2Gi", Wall: config.Duration(time.Minute)},
	}
	report, err := g.Run(context.Background(), cand)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Passed {
		t.Fatal("report Passed = true, want false (gosec failed)")
	}
	if len(report.Checks) != 2 {
		t.Fatalf("checks = %d, want 2 (tests-pass passed + gosec aggregated)", len(report.Checks))
	}
	// tests-pass passed → the adapter ran but found nothing.
	if tp := report.Checks[0]; tp.Name != core.CheckAcceptanceTests || len(tp.Findings) != 0 {
		t.Errorf("tests-pass findings = %+v, want none (passing run)", tp.Findings)
	}
	// gosec: the scanner adapter parsed the G401 hit.
	gs := report.Checks[1]
	if gs.Name != checkGosec || len(gs.Findings) != 1 || gs.Findings[0].Rule != "G401" {
		t.Errorf("gosec findings = %+v, want one G401 finding", gs.Findings)
	}
	// The harvested verdict carries the findings (the verification view reads this record).
	v := decodeVerdict(t, store, report.Verdict.Hash)
	if len(v.Checks) != 2 || len(v.Checks[1].Findings) != 1 || v.Checks[1].Findings[0].Rule != "G401" {
		t.Fatalf("verdict gosec finding not carried: %+v", v.Checks)
	}
}

// TestRunFailingTestCheckCarriesFindings proves a failing acceptance-test check carries the
// parsed test findings (which test failed, the assertion) — the signal a retry Brief needs,
// not the raw dump. A non-independent failure fail-fasts, so this is the single-check case.
func TestRunFailingTestCheckCarriesFindings(t *testing.T) {
	const testsCmd = "go test -json ./..."
	sb := &scriptedSandbox{id: "gate-sb", results: map[string]sandbox.ExecResult{
		testsCmd: {ExitCode: 1, Stdout: []byte(goTestJSONFail)},
	}}
	registry := Registry{core.CheckAcceptanceTests: testsCmd}
	store := testStore(t)
	g := New(&fakeBackend{sb: sb}, registry, store, t.TempDir(), nil, nil)

	cand := Candidate{
		Repo:           "/repo",
		Ref:            core.CandidateBranch("issue-1"),
		Postconditions: []string{core.CheckAcceptanceTests},
		Profile:        "go-toolchain",
		Limits:         config.SandboxLimits{CPU: 2, Mem: "2Gi", Wall: config.Duration(time.Minute)},
	}
	report, err := g.Run(context.Background(), cand)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Passed || len(report.Checks) != 1 {
		t.Fatalf("report = %+v, want one failed check", report)
	}
	c := report.Checks[0]
	if len(c.Findings) != 1 || c.Findings[0].Rule != "TestAdd" || c.Findings[0].File != "add_test.go" {
		t.Errorf("findings = %+v, want one TestAdd finding anchored at add_test.go", c.Findings)
	}
}
