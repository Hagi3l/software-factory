package gotest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/core"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// TestParsePassYieldsNoFindings: a clean run is zero findings — the whole signal-density
// win is that a green run contributes nothing to the agent's context.
func TestParsePassYieldsNoFindings(t *testing.T) {
	got := Parse(readFixture(t, "pass.json"))
	if len(got) != 0 {
		t.Fatalf("expected no findings for a passing run, got %d:\n%s", len(got), got.Format())
	}
}

// TestParseTestFailure: a t.Errorf failure becomes one finding anchored at the test file
// and line, with the assertion text as the message and the failing test as the rule. The
// passing sibling test contributes nothing.
func TestParseTestFailure(t *testing.T) {
	got := Parse(readFixture(t, "fail.json"))
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 finding (one failing test, one passing), got %d:\n%s", len(got), got.Format())
	}
	f := got[0]
	if f.Rule != "TestAdd" {
		t.Errorf("Rule = %q, want TestAdd", f.Rule)
	}
	if f.File != "fail_test.go" || f.Line != 9 {
		t.Errorf("location = %s:%d, want fail_test.go:9", f.File, f.Line)
	}
	if want := "Add() = 4, want 5"; f.Message != want {
		t.Errorf("Message = %q, want %q", f.Message, want)
	}
}

// TestParseCompileFailureStructured: a build-fail in the stream surfaces the *compiler
// error* as a finding (the signal), not a green or a bare "build failed".
func TestParseCompileFailureStructured(t *testing.T) {
	got := Parse(readFixture(t, "compile.json"))
	if len(got) == 0 {
		t.Fatal("expected a build-failure finding, got none")
	}
	var build core.Finding
	for _, f := range got {
		if f.Rule == "build" {
			build = f
		}
	}
	if build.Rule != "build" {
		t.Fatalf("no build finding among %d findings:\n%s", len(got), got.Format())
	}
	if build.File != "compile_test.go" || build.Line != 6 {
		t.Errorf("build location = %s:%d, want compile_test.go:6", build.File, build.Line)
	}
	if !strings.Contains(build.Message, "undefined: Undefined") {
		t.Errorf("Message = %q, want it to name the compiler error", build.Message)
	}
}

// TestParseRawNonJSONNeverCrashes: a raw, non-test2json build dump (a `# pkg` block on
// stdout) must parse to a finding rather than panicking — the case CLAUDE.md sends a human
// to the .stderr file for.
func TestParseRawNonJSONNeverCrashes(t *testing.T) {
	raw := []byte("# scratch [scratch.test]\n./x.go:3:1: undefined: Foo\nFAIL\tscratch [build failed]\n")
	got := Parse(raw)
	if len(got) != 1 {
		t.Fatalf("expected 1 build finding from raw output, got %d:\n%s", len(got), got.Format())
	}
	f := got[0]
	if f.Rule != "build" {
		t.Errorf("Rule = %q, want build", f.Rule)
	}
	if f.File != "x.go" || f.Line != 3 {
		t.Errorf("location = %s:%d, want x.go:3", f.File, f.Line)
	}
	if !strings.Contains(f.Message, "undefined: Foo") {
		t.Errorf("Message = %q, want it to carry the compiler error", f.Message)
	}
}

// TestParseDataRaceKeepsStanzaVerbatim: the race finding keeps the WARNING: DATA RACE
// stanza in Detail verbatim — the interleaving is the evidence.
func TestParseDataRace(t *testing.T) {
	got := Parse(readFixture(t, "race.json"))
	if len(got) != 1 {
		t.Fatalf("expected 1 race finding, got %d:\n%s", len(got), got.Format())
	}
	f := got[0]
	if f.Rule != raceRule {
		t.Errorf("Rule = %q, want %q", f.Rule, raceRule)
	}
	if !strings.Contains(f.Detail, "WARNING: DATA RACE") {
		t.Errorf("Detail missing the verbatim race warning:\n%s", f.Detail)
	}
	if !strings.Contains(f.Detail, "Previous write at") {
		t.Errorf("Detail missing the race interleaving:\n%s", f.Detail)
	}
	// The goroutine-stack noise of a normal failure must not have been mistaken for the
	// race body: the stanza is bounded by its ==== rules.
	if strings.Contains(f.Detail, "_testmain.go") {
		t.Errorf("Detail leaked content beyond the race stanza:\n%s", f.Detail)
	}
}

// TestParsePanicDropsGoroutineDump: a panic keeps the message + triggering test but drops
// the goroutine dump (noise).
func TestParsePanic(t *testing.T) {
	got := Parse(readFixture(t, "panic.json"))
	if len(got) != 1 {
		t.Fatalf("expected 1 panic finding, got %d:\n%s", len(got), got.Format())
	}
	f := got[0]
	if f.Rule != "TestPanics" {
		t.Errorf("Rule = %q, want TestPanics", f.Rule)
	}
	if !strings.HasPrefix(f.Message, "panic:") {
		t.Errorf("Message = %q, want it to start with panic:", f.Message)
	}
	if f.Detail != "" {
		t.Errorf("Detail should be empty (goroutine dump dropped), got:\n%s", f.Detail)
	}
	if strings.Contains(f.Message, "goroutine") {
		t.Errorf("Message leaked the goroutine dump: %q", f.Message)
	}
}

// TestParseTimeoutDropsGoroutineDump: a `panic: test timed out` keeps the headline + test,
// drops the dump.
func TestParseTimeout(t *testing.T) {
	got := Parse(readFixture(t, "timeout.json"))
	if len(got) != 1 {
		t.Fatalf("expected 1 timeout finding, got %d:\n%s", len(got), got.Format())
	}
	f := got[0]
	if f.Rule != "TestSlow" {
		t.Errorf("Rule = %q, want TestSlow", f.Rule)
	}
	if !strings.Contains(f.Message, "test timed out") {
		t.Errorf("Message = %q, want it to mention the timeout", f.Message)
	}
	if f.Detail != "" {
		t.Errorf("Detail should be empty (goroutine dump dropped), got:\n%s", f.Detail)
	}
}

// TestParseIsCacheStable: feeding the same fixture twice yields byte-identical rendered
// findings — the load-bearing jitter-free property. Covers every fixture, including the
// ones whose raw stream contains timestamps and elapsed times.
func TestParseIsCacheStable(t *testing.T) {
	for _, name := range []string{"pass.json", "fail.json", "compile.json", "race.json", "panic.json", "timeout.json"} {
		t.Run(name, func(t *testing.T) {
			in := readFixture(t, name)
			a := Parse(in).Format()
			b := Parse(in).Format()
			if a != b {
				t.Fatalf("non-deterministic findings for %s:\n--- first ---\n%s\n--- second ---\n%s", name, a, b)
			}
		})
	}
}

// TestParseEmptyInput: no input is no findings, no panic.
func TestParseEmptyInput(t *testing.T) {
	if got := Parse(nil); len(got) != 0 {
		t.Fatalf("expected no findings for empty input, got %d", len(got))
	}
	if got := Parse([]byte("\n\n  \n")); len(got) != 0 {
		t.Fatalf("expected no findings for blank input, got %d", len(got))
	}
}
