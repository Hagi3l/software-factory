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
}
