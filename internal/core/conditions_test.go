package core

import "testing"

// ParseMetricComparison splits a well-formed comparison into metric/op/threshold and
// rejects malformed ones, so config validation and the gate agree on what is a metric
// comparison versus a bare identifier (a reserved proof or a command-check name).
func TestParseMetricComparison(t *testing.T) {
	cases := []struct {
		in        string
		metric    string
		op        string
		threshold float64
		ok        bool
	}{
		// ">=" must be matched before ">" (longest-first), so the threshold is "0.8", not "=0.8".
		{"mutation>=0.8", "mutation", ">=", 0.8, true},
		{"mutation>0.8", "mutation", ">", 0.8, true},
		{"mutation<=0.5", "mutation", "<=", 0.5, true},
		{"coverage==1", "coverage", "==", 1, true},
		{" mutation >= 0.8 ", "mutation", ">=", 0.8, true}, // surrounding/internal spaces trimmed
		// Not comparisons: bare identifiers fall through to the caller.
		{"tests-pass", "", "", 0, false},
		{"tests-red-then-green", "", "", 0, false},
		// Malformed: no metric, or a non-numeric threshold.
		{">=0.8", "", "", 0, false},
		{"mutation>=high", "", "", 0, false},
		{"mutation>=", "", "", 0, false},
	}
	for _, c := range cases {
		metric, op, threshold, ok := ParseMetricComparison(c.in)
		if ok != c.ok || metric != c.metric || op != c.op || threshold != c.threshold {
			t.Errorf("ParseMetricComparison(%q) = (%q, %q, %v, %v), want (%q, %q, %v, %v)",
				c.in, metric, op, threshold, ok, c.metric, c.op, c.threshold, c.ok)
		}
	}
}

// CompareMetric evaluates every recognized operator and fails closed on an unknown one,
// so a metric check that cannot be evaluated is never treated as passing.
func TestCompareMetric(t *testing.T) {
	cases := []struct {
		value     float64
		op        string
		threshold float64
		want      bool
	}{
		{0.85, ">=", 0.8, true},
		{0.80, ">=", 0.8, true},  // boundary: >= includes equality
		{0.80, ">", 0.8, false},  // boundary: > excludes equality
		{0.50, ">=", 0.8, false}, // below threshold fails the gate
		{0.50, "<=", 0.8, true},
		{0.80, "==", 0.8, true},
		{0.81, "==", 0.8, false},
		{0.5, "??", 0.8, false}, // unknown operator fails closed
	}
	for _, c := range cases {
		if got := CompareMetric(c.value, c.op, c.threshold); got != c.want {
			t.Errorf("CompareMetric(%v, %q, %v) = %v, want %v", c.value, c.op, c.threshold, got, c.want)
		}
	}
}
