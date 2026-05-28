package orchestrator

import (
	"context"
	"encoding/json"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/messaging"
)

// scheduleReady dispatches every ready issue whose role is an agent stage: it claims
// the issue (single-writer transition to in_progress with a lease) and publishes its
// Brief to the role's work subject. Claim-then-publish is the dispatch unit from the
// reconcile loop; the lease plus the runner's JetStream AckWait is what lets a dead
// runner's work be reclaimed (see specs/components/orchestrator.md). A failure on one
// issue never blocks the others — the loop logs and moves on, and the issue is retried
// on a later tick.
func (o *Orchestrator) scheduleReady(ctx context.Context) {
	ready, err := o.bd.Ready(ctx)
	if err != nil {
		o.log.Error("orchestrator: query ready work", "err", err)
		return
	}
	for _, issue := range ready {
		stage, ok := o.stageForRole(issue.Role)
		if !ok {
			// Not an agent stage (or an unknown role): nothing to dispatch. Non-agent
			// stages (e.g. integrate) are executed by the orchestrator on acceptance, never
			// dispatched to a runner, so a ready issue for one would be a config/seed error.
			o.log.Warn("orchestrator: ready issue has no agent stage for its role; skipping",
				"issue", issue.ID, "role", issue.Role)
			continue
		}
		soul, ok := o.soulForRole(issue.Role)
		if !ok {
			// validate guarantees every agent role resolves to a soul; defense-in-depth.
			o.log.Error("orchestrator: no soul fulfills role; cannot dispatch", "issue", issue.ID, "role", issue.Role)
			continue
		}

		if _, err := o.bd.Claim(ctx, issue.ID, o.leaseTTL); err != nil {
			o.log.Error("orchestrator: claim issue", "issue", issue.ID, "err", err)
			continue
		}
		brief := o.buildBrief(issue, stage, soul)
		if err := o.publishWork(ctx, issue.Role, brief); err != nil {
			o.log.Error("orchestrator: publish work, releasing claim", "issue", issue.ID, "err", err)
			// Undo the claim so the issue returns to ready and is redispatched promptly
			// rather than waiting for the lease to expire.
			if rerr := o.bd.Release(ctx, issue.ID); rerr != nil {
				o.log.Error("orchestrator: release after failed publish", "issue", issue.ID, "err", rerr)
			}
			continue
		}
		o.log.Info("orchestrator: dispatched", "issue", issue.ID, "role", issue.Role, "soul", soul.Name, "attempt", issue.Attempt)
	}
}

// buildBrief assembles the task envelope handed into the sandbox. In the bootstrap the
// spec slice is empty: the seeded worktree contains the whole specs/ tree, so the agent
// reads the spec it needs through its workspace tools; bounded spec-slice resolution is
// Phase 3 (see specs/specs-process.md). Base is the ref the candidate branches from,
// and Criteria carries the stage's postconditions so the agent knows what it must
// satisfy (the orchestrator independently re-checks them at the gate).
func (o *Orchestrator) buildBrief(issue core.Issue, stage config.Stage, soul core.Soul) core.Brief {
	return core.Brief{
		Issue:    issue,
		Spec:     "",
		Base:     o.base,
		Criteria: stage.Postcondition,
		Soul:     soul,
	}
}

// publishWork marshals the Brief and publishes it to the role's JetStream work subject
// for a runner to pull. The producer and consumer (the runner) share core.Brief, so
// the default field-name JSON encoding is the wire contract — no tags needed.
func (o *Orchestrator) publishWork(ctx context.Context, role string, brief core.Brief) error {
	data, err := json.Marshal(brief)
	if err != nil {
		return err
	}
	_, err = o.js.Publish(ctx, messaging.WorkSubject(role), data)
	return err
}

// stageForRole finds the DAG stage an agent role belongs to. In the bootstrap a stage
// is keyed by its own role name (the DAG collapses to implement -> gate -> integrate,
// one soul per role), so role and stage name coincide; resolving by the stage's Role
// field keeps the lookup correct even if a future config names them apart. Only agent
// stages (those with a Role) match; non-agent stages (kind: human/trusted-merge) never
// resolve here.
func (o *Orchestrator) stageForRole(role string) (config.Stage, bool) {
	if role == "" || o.opts.Config.Harness == nil {
		return config.Stage{}, false
	}
	for _, st := range o.opts.Config.Harness.DAG {
		if st.Role == role {
			return st, true
		}
	}
	return config.Stage{}, false
}

// soulForRole returns the first soul that fulfills the role. Selector-based choice
// among several souls for a role is Phase 3 (see specs/configuration.md); the kernel
// has one soul per role.
func (o *Orchestrator) soulForRole(role string) (core.Soul, bool) {
	for _, s := range o.opts.Config.Souls {
		if s.Role == role {
			return s, true
		}
	}
	return core.Soul{}, false
}

// roleIsAgentStage reports whether a role corresponds to a dispatchable agent stage —
// used to validate agent-proposed child issues before they are written.
func (o *Orchestrator) roleIsAgentStage(role string) bool {
	_, ok := o.stageForRole(role)
	return ok
}
