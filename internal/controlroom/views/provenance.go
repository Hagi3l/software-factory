package views

import "strings"

// verifiedName / verifiedHash split one Provenance.Verified entry, stored as "name@<hash>"
// when the gate evidence was persisted (else a bare check name). The provenance view links
// the name to its evidence artifact when a hash is present and renders a plain badge when it
// is not — so a check that ran but whose evidence could not be stored still shows it passed
// rather than vanishing. Kept as text helpers (no class strings) per views/budgets.go.
func verifiedName(v string) string {
	name, _, _ := strings.Cut(v, "@")
	return name
}

func verifiedHash(v string) string {
	_, hash, _ := strings.Cut(v, "@")
	return hash
}
