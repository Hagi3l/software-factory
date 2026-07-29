package scanners

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Loxstomper/software-factory/internal/core"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// parser is the shared shape of every adapter, letting the cross-cutting assertions
// (clean input, malformed input, cache stability) run table-driven over all four.
type parser func([]byte) core.Findings

// --- golangci-lint -----------------------------------------------------------

func TestParseGolangciLintFindings(t *testing.T) {
	got := ParseGolangciLint(readFixture(t, "golangci_findings.json"))
	want := core.Findings{
		{File: "bad.go", Line: 8, Rule: "forbidigo", Message: "use of `fmt.Printf` forbidden by pattern `^(fmt\\.Print(|f|ln)|print|println)$`"},
		{File: "bad.go", Line: 10, Rule: "nlreturn", Message: "return with no blank line before"},
		{File: "bad.go", Line: 1, Severity: "warning", Rule: "revive", Message: "package-comments: should have a package comment"},
	}
	assertFindings(t, want, got)
}

func TestParseGolangciLintClean(t *testing.T) {
	// The clean fixture carries the real trailing "0 issues." line after the JSON object;
	// the streaming decoder must read the object and ignore the trailing token.
	if got := ParseGolangciLint(readFixture(t, "golangci_clean.json")); len(got) != 0 {
		t.Fatalf("clean input: want 0 findings, got %d: %q", len(got), got.Format())
	}
}

// --- gosec -------------------------------------------------------------------

func TestParseGosecFindings(t *testing.T) {
	got := ParseGosec(readFixture(t, "gosec_findings.json"))
	want := core.Findings{
		{
			File: "internal/example/sec.go", Line: 11, Severity: "medium", Rule: "G401",
			Message: "Use of weak cryptographic primitive",
			Detail:  "10: func Weak(cmd string) {\n11: \th := md5.New()\n12: \tfmt.Println(h)",
		},
		{
			File: "internal/example/sec.go", Line: 4, Severity: "medium", Rule: "G501",
			Message: "Blocklisted import crypto/md5: weak cryptographic primitive",
			Detail:  "3: import (\n4: \t\"crypto/md5\"\n5: \t\"fmt\"",
		},
	}
	assertFindings(t, want, got)
}

func TestParseGosecClean(t *testing.T) {
	if got := ParseGosec(readFixture(t, "gosec_clean.json")); len(got) != 0 {
		t.Fatalf("clean input: want 0 findings, got %d: %q", len(got), got.Format())
	}
}

func TestParseGosecLineRange(t *testing.T) {
	// gosec emits a range ("11-13") for multi-line constructs; we keep the first line.
	raw := []byte(`{"Issues":[{"severity":"HIGH","rule_id":"G104","details":"d","file":"a.go","code":"x","line":"11-13"}]}`)
	got := ParseGosec(raw)
	if len(got) != 1 || got[0].Line != 11 {
		t.Fatalf("range line: want line 11, got %+v", got)
	}
}

// --- govulncheck -------------------------------------------------------------

func TestParseGovulncheckFindings(t *testing.T) {
	got := ParseGovulncheck(readFixture(t, "govuln_findings.json"))
	// Only GO-2021-0113 has a symbol-level (called) finding; GO-2022-1059 is informational
	// (module/package-level only) and is intentionally dropped.
	want := core.Findings{
		{
			File: "main.go", Line: 6, Severity: "high", Rule: "GO-2021-0113",
			Message: "Out-of-bounds read in golang.org/x/text/language",
			Detail: "aliases: CVE-2021-38561, GHSA-ppp9-7jff-5vj2\n" +
				"fixed in: v0.3.7\n" +
				"call path: vulnpkg.main -> golang.org/x/text/language.Parse",
		},
	}
	assertFindings(t, want, got)
}

func TestParseGovulncheckClean(t *testing.T) {
	if got := ParseGovulncheck(readFixture(t, "govuln_clean.json")); len(got) != 0 {
		t.Fatalf("clean input: want 0 findings, got %d: %q", len(got), got.Format())
	}
}

func TestParseGovulncheckTruncatedTailKeepsCompleteMessages(t *testing.T) {
	// A dump cut off mid-object must still surface the findings already fully read.
	raw := readFixture(t, "govuln_findings.json")
	truncated := append([]byte(nil), raw...)
	truncated = append(truncated, []byte(`{"finding":{"osv":"GO-2021-0113","trace":[{"module":"x","fun`)...)
	got := ParseGovulncheck(truncated)
	if len(got) != 1 || got[0].Rule != "GO-2021-0113" {
		t.Fatalf("truncated tail: want the 1 complete finding, got %d: %q", len(got), got.Format())
	}
}

// --- license-scan ------------------------------------------------------------

func TestParseLicenseScanFindings(t *testing.T) {
	got := ParseLicenseScan(readFixture(t, "license_findings.txt"))
	// The glog "E0622 ...] Failed to find license..." line is jitter and dropped; the three
	// verdict lines become findings.
	want := core.Findings{
		{Severity: "error", Rule: "github.com/example/copyleft", Message: "forbidden license: GPL-3.0"},
		{Severity: "error", Rule: "github.com/example/network-copyleft", Message: "license not allowed: AGPL-3.0"},
		{Severity: "error", Rule: "example.com/foo", Message: "unknown license type"},
	}
	assertFindings(t, want, got)
}

func TestParseLicenseScanClean(t *testing.T) {
	if got := ParseLicenseScan(readFixture(t, "license_clean.txt")); len(got) != 0 {
		t.Fatalf("clean input: want 0 findings, got %d: %q", len(got), got.Format())
	}
}

// --- cross-cutting: malformed degrades, cache stability ----------------------

func allParsers() map[string]parser {
	return map[string]parser{
		"golangci":    ParseGolangciLint,
		"gosec":       ParseGosec,
		"govulncheck": ParseGovulncheck,
		"license":     ParseLicenseScan,
	}
}

// TestMalformedInputDegradesGracefully feeds every parser a truncated/non-JSON/empty blob:
// it must never panic and must return what it can (here: nothing) rather than failing the
// caller. The gate still grades on the exit code, so a garbled report must degrade.
func TestMalformedInputDegradesGracefully(t *testing.T) {
	blobs := map[string][]byte{
		"empty":      {},
		"whitespace": []byte("   \n\t "),
		"not-json":   []byte("totally not json {{{"),
		"truncated":  []byte(`{"Issues":[{"rule_id":"G1`),
	}
	for pname, p := range allParsers() {
		for bname, blob := range blobs {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("%s panicked on %s blob: %v", pname, bname, r)
					}
				}()
				// The text-based license parser legitimately treats arbitrary text as "no
				// verdict lines" -> empty; the JSON parsers reject the blob -> empty. Either
				// way: no panic, and a deterministic Format().
				_ = p(blob).Format()
			}()
		}
	}
}

// TestCacheStability re-parses each tool's findings fixture twice and asserts the rendered
// findings are byte-identical — the load-bearing property for prefix caching and the
// "findings not shrinking across attempts" signal. Any leaked jitter (a version string, a
// map iteration leaking into order) would break this.
func TestCacheStability(t *testing.T) {
	fixtures := map[string]struct {
		p    parser
		file string
	}{
		"golangci":    {ParseGolangciLint, "golangci_findings.json"},
		"gosec":       {ParseGosec, "gosec_findings.json"},
		"govulncheck": {ParseGovulncheck, "govuln_findings.json"},
		"license":     {ParseLicenseScan, "license_findings.txt"},
	}
	for name, fx := range fixtures {
		raw := readFixture(t, fx.file)
		first := fx.p(raw).Format()
		for i := 0; i < 5; i++ {
			if got := fx.p(raw).Format(); got != first {
				t.Fatalf("%s: parse not cache-stable on re-run %d:\n--- first ---\n%s\n--- got ---\n%s", name, i, first, got)
			}
		}
		if first == "" {
			t.Fatalf("%s: findings fixture rendered empty — fixture should carry findings", name)
		}
	}
}

// assertFindings compares two findings sets by their canonical Format() rendering — the one
// renderer that reaches an agent's context — so the assertion tracks the observable output,
// not slice order.
func assertFindings(t *testing.T, want, got core.Findings) {
	t.Helper()
	if w, g := want.Format(), got.Format(); w != g {
		t.Fatalf("findings mismatch:\n--- want ---\n%s\n--- got ---\n%s", w, g)
	}
}
