package checkfindings

import (
	"testing"

	"github.com/Loxstomper/harness/internal/core"
)

const goTestJSONFail = `{"Action":"run","Package":"x","Test":"TestAdd"}
{"Action":"output","Package":"x","Test":"TestAdd","Output":"    add_test.go:12: want 5, got 4\n"}
{"Action":"output","Package":"x","Test":"TestAdd","Output":"--- FAIL: TestAdd (0.00s)\n"}
{"Action":"fail","Package":"x","Test":"TestAdd","Elapsed":0}
`

const gosecJSON = `{"Issues":[{"severity":"HIGH","rule_id":"G401","details":"weak crypto","file":"c.go","line":"7"}]}`

func TestByNameSelectsKernelAdapters(t *testing.T) {
	for _, name := range []string{core.CheckAcceptanceTests, Gosec, Govulncheck, GolangciLint, LicenseScan} {
		if ByName(name) == nil {
			t.Errorf("ByName(%q) = nil, want an adapter", name)
		}
	}
	if ByName("bespoke-scanner") != nil {
		t.Error("ByName(unknown) returned an adapter, want nil (graceful fallback)")
	}
}

// TestGoTestGuardsOnNDJSON proves the go-test adapter only fires on ndjson — human-format
// test output yields no findings rather than a fabricated "build" finding.
func TestGoTestGuardsOnNDJSON(t *testing.T) {
	if fs := GoTest([]byte(goTestJSONFail)); len(fs) != 1 || fs[0].Rule != "TestAdd" {
		t.Fatalf("ndjson parse = %+v, want one TestAdd finding", fs)
	}
	if fs := GoTest([]byte("ok  \tx\t0.1s\n")); len(fs) != 0 {
		t.Fatalf("human output yielded findings: %+v", fs)
	}
	if fs := GoTest(nil); len(fs) != 0 {
		t.Fatalf("empty output yielded findings: %+v", fs)
	}
}

// TestParseFallsBackToStderr proves Parse uses stdout, falls back to stderr when stdout is
// empty, and returns nil for a name with no adapter.
func TestParseFallsBackToStderr(t *testing.T) {
	if fs := Parse(Gosec, []byte(gosecJSON), nil); len(fs) != 1 || fs[0].Rule != "G401" {
		t.Fatalf("stdout parse = %+v, want one G401", fs)
	}
	if fs := Parse(Gosec, nil, []byte(gosecJSON)); len(fs) != 1 {
		t.Fatalf("stderr fallback = %+v, want one finding", fs)
	}
	if fs := Parse("bespoke", []byte(gosecJSON), nil); fs != nil {
		t.Fatalf("no-adapter name yielded findings: %+v", fs)
	}
}
