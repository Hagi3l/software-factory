package core

import "testing"

func TestFindingsFormatEmptyIsBlank(t *testing.T) {
	if got := Findings(nil).Format(); got != "" {
		t.Fatalf("empty findings should format to \"\", got %q", got)
	}
	if got := (Findings{}).Format(); got != "" {
		t.Fatalf("zero-length findings should format to \"\", got %q", got)
	}
}

func TestFindingsFormatRendersComponentsCompactly(t *testing.T) {
	cases := []struct {
		name string
		f    Finding
		want string
	}{
		{
			name: "full",
			f:    Finding{File: "internal/foo/bar.go", Line: 42, Severity: "high", Rule: "G401", Message: "weak crypto"},
			want: "internal/foo/bar.go:42 [high] G401: weak crypto",
		},
		{
			name: "no location",
			f:    Finding{Severity: "error", Rule: "build", Message: "undefined: Foo"},
			want: "[error] build: undefined: Foo",
		},
		{
			name: "file without line",
			f:    Finding{File: "go.mod", Rule: "GO-2024-0001", Message: "vulnerable dependency"},
			want: "go.mod GO-2024-0001: vulnerable dependency",
		},
		{
			name: "message only (a plain test failure)",
			f:    Finding{Message: "TestThing failed"},
			want: "TestThing failed",
		},
		{
			name: "rule without message",
			f:    Finding{File: "a.go", Line: 3, Rule: "SA4006"},
			want: "a.go:3 SA4006",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Findings{tc.f}).Format(); got != tc.want {
				t.Fatalf("Format() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFindingsFormatIndentsMultiLineDetail(t *testing.T) {
	f := Finding{
		File: "calc_test.go", Line: 12, Rule: "TestAdd",
		Message: "assertion failed",
		Detail:  "want: 5\ngot:  4",
	}
	want := "calc_test.go:12 TestAdd: assertion failed\n    want: 5\n    got:  4"
	if got := (Findings{f}).Format(); got != want {
		t.Fatalf("Format() = %q, want %q", got, want)
	}
}

// TestFindingsFormatIsCacheStable is the load-bearing property: the rendering must be
// byte-identical regardless of the order the parser emitted the findings in, so an
// unchanged re-run keeps the conversation prefix cacheable (specs/verification.md).
func TestFindingsFormatIsCacheStable(t *testing.T) {
	a := Finding{File: "z.go", Line: 1, Rule: "A", Message: "first"}
	b := Finding{File: "a.go", Line: 9, Rule: "B", Message: "second"}
	c := Finding{File: "a.go", Line: 2, Rule: "C", Message: "third"}

	one := Findings{a, b, c}.Format()
	two := Findings{c, a, b}.Format()
	three := Findings{b, c, a}.Format()

	if one != two || two != three {
		t.Fatalf("Format() is order-sensitive:\n%q\n%q\n%q", one, two, three)
	}
	// And the canonical order is file, then line: a.go:2, a.go:9, z.go:1.
	want := "a.go:2 C: third\na.go:9 B: second\nz.go:1 A: first"
	if one != want {
		t.Fatalf("Format() = %q, want canonical order %q", one, want)
	}
}

func TestCheckStatusOf(t *testing.T) {
	if got := CheckStatusOf(true); got != CheckStatusPassed {
		t.Fatalf("CheckStatusOf(true) = %q, want %q", got, CheckStatusPassed)
	}
	if got := CheckStatusOf(false); got != CheckStatusFailed {
		t.Fatalf("CheckStatusOf(false) = %q, want %q", got, CheckStatusFailed)
	}
}
