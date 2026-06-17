package orchestrator

import (
	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
)

// epicBranch is the short branch name an epic's children integrate onto: epic/<epic_id>. It is
// "main" for child-level integration under integration.mode: epic — the real main advances only
// at the epic's terminal merge (specs/integration.md, T7.3). Keyed by the epic id (the root
// seed's id, threaded forward onto every issue of the epic via core.EpicOf), so all children of
// one feature share one branch and a different feature never collides with it.
func epicBranch(epicID string) string { return core.EpicBranch(epicID) }

// epicMode reports whether the run lands verified work atomically per epic (integration.mode:
// epic) rather than per item (the kernel default). It reads the validated config the
// orchestrator schedules from; an absent block reads as per-item via Harness.Mode().
func (o *Orchestrator) epicMode() bool {
	return o.opts.Config != nil && o.opts.Config.Harness != nil &&
		o.opts.Config.Harness.Mode() == config.IntegrationEpic
}

// integrationBranchName is the SHORT branch name an issue's verified candidate integrates onto:
// o.base (main) in per-item mode, or the issue's epic branch epic/<epic_id> in epic mode
// (specs/integration.md, T7.3). The short form is what the Brief surfaces to the merge-resolver
// soul as its rebase target — DWIM-resolvable in the agent's sandbox clone, exactly like the
// gate's candidate/<id> — and is the human-facing branch name. The integration *target* the
// merge queue's git plumbing operates on is its fully-qualified sibling, integrationTargetRef.
func (o *Orchestrator) integrationBranchName(issue core.Issue) string {
	if o.epicMode() {
		return epicBranch(epicOf(issue))
	}
	return o.base
}

// integrationTargetRef is the FULLY-QUALIFIED ref the merge queue integrates an issue's
// candidate onto and advances: refs/heads/main, or refs/heads/epic/<epic_id> in epic mode. The
// fully-qualified form is what the merge queue's update-ref plumbing needs (it does not DWIM);
// the merger creates the epic branch off main on first use, so the orchestrator only names it.
func (o *Orchestrator) integrationTargetRef(issue core.Issue) string {
	return "refs/heads/" + o.integrationBranchName(issue)
}
