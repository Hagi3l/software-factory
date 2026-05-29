package core

import "testing"

// TestSoulMatches pins the selector subset semantics that drive soul selection: a soul
// fulfills an issue when every selector key matches a tag of the same value, an empty
// selector is a catch-all, and a missing/mismatched tag fails the match (see
// specs/configuration.md, orchestrator.selectSoul).
func TestSoulMatches(t *testing.T) {
	cases := []struct {
		name     string
		selector map[string]string
		tags     map[string]string
		want     bool
	}{
		{"empty selector matches anything", nil, map[string]string{"lang": "go"}, true},
		{"empty selector matches no tags", nil, nil, true},
		{"exact single match", map[string]string{"lang": "go"}, map[string]string{"lang": "go"}, true},
		{"subset of tags matches", map[string]string{"lang": "go"}, map[string]string{"lang": "go", "tier": "high"}, true},
		{"value mismatch fails", map[string]string{"lang": "go"}, map[string]string{"lang": "rust"}, false},
		{"missing key fails", map[string]string{"lang": "go"}, map[string]string{"tier": "high"}, false},
		{"multi-key all must match", map[string]string{"lang": "go", "tier": "high"}, map[string]string{"lang": "go", "tier": "high"}, true},
		{"multi-key one mismatch fails", map[string]string{"lang": "go", "tier": "high"}, map[string]string{"lang": "go", "tier": "low"}, false},
		{"non-empty selector vs no tags fails", map[string]string{"lang": "go"}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Soul{Selector: tc.selector}
			if got := s.Matches(tc.tags); got != tc.want {
				t.Errorf("Matches(%v) with selector %v = %v, want %v", tc.tags, tc.selector, got, tc.want)
			}
		})
	}
}
