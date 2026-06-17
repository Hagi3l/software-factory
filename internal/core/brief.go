package core

// Brief is the task envelope handed into a sandbox. Because an agent is stateless
// and sandboxed, the Brief is its entire knowledge of the world — there is no
// ambient context to fall back on (see specs/components/agent.md).
//
// Spec is the bounded spec slice (the referenced file plus its linked neighbors
// to a configured depth), deliberately not the whole specs/ tree, which would
// dilute focus and blow the context window (see specs/specs-process.md). SpecHash
// pins that slice's content address so the exact spec version is recorded; the
// orchestrator also stores it on the issue for spec-drift detection (T3.6/T3.7).
type Brief struct {
	Issue    Issue    // the work item
	Spec     string   // the resolved, bounded spec slice
	SpecHash string   // content address of Spec (see internal/spec.Hash); the pinned spec version
	Base     string   // git ref to branch from
	Criteria []string // postconditions this node must satisfy
	Soul     Soul     // the identity that will execute this Brief
	// IntegrationBase is the short branch name the verified candidate will be integrated onto
	// by the trusted merge queue: `main` in per-item mode, or the epic branch `epic/<epic_id>`
	// in epic mode (specs/integration.md, T7.3). It is the rebase target the merge-resolver
	// soul must rebase a conflicting candidate onto — distinct from Base (where the candidate
	// branched from). The short form is DWIM-resolvable in the agent's sandbox clone (only the
	// default branch is a local ref there; an epic branch is reachable as origin/epic/<id>),
	// exactly like the gate's candidate/<id> ref. Empty defaults to `main` at render.
	IntegrationBase string
}

// IntegrationBaseOrMain returns the branch the candidate integrates onto, defaulting to `main`
// when unset so a brief built before epic mode (or by a pure-per-item path) renders identically
// to the historical behavior. It is the rebase target surfaced to the merge-resolver soul.
func (b Brief) IntegrationBaseOrMain() string {
	if b.IntegrationBase == "" {
		return "main"
	}
	return b.IntegrationBase
}
