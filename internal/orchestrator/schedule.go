package orchestrator

import (
	"context"
	"encoding/json"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/messaging"
	"github.com/Loxstomper/harness/internal/spec"
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
		soul, ok := o.selectSoul(issue)
		if !ok {
			// validate guarantees every agent role resolves to >=1 soul; reaching here means
			// either that (defense-in-depth) or a role with several souls none of whose
			// selector the issue's tags satisfy. Skip and log; the issue stays ready and is
			// retried — a persistently unmatched issue is a planner/config fault for a human.
			o.log.Error("orchestrator: no soul matches issue; cannot dispatch", "issue", issue.ID, "role", issue.Role, "tags", issue.Tags)
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

// buildBrief assembles the task envelope handed into the sandbox. The spec slice is the
// bounded context horizon (see specs/specs-process.md): the orchestrator resolves the
// issue's referenced spec file plus its linked neighbors to the configured depth from
// the integration repo and embeds it, so the agent gets exactly the contract it needs in
// context rather than slurping the whole specs/ tree. An issue with no spec reference (a
// seed without --spec) gets an empty slice and falls back to the tree in its worktree.
// Resolution is best-effort: a missing/unreadable spec is logged loudly and the brief
// dispatches with an empty slice rather than wedging the issue — degraded context, not a
// dead pipeline (the same discipline harvest uses). Base is the ref the candidate branches
// from: an issue produced by a preceding agent stage carries its predecessor's verified
// candidate (issue.Base), so e.g. implement branches from the author-tests candidate that
// holds the failing tests; a freshly seeded issue carries none and falls back to the
// pipeline base (o.base, main). Criteria carries the stage's postconditions so the agent
// knows what it must satisfy (the orchestrator independently re-checks them at the gate).
func (o *Orchestrator) buildBrief(issue core.Issue, stage config.Stage, soul core.Soul) core.Brief {
	base := o.base
	if issue.Base != "" {
		base = issue.Base
	}
	specSlice := ""
	if issue.Spec != "" {
		s, err := spec.Resolve(o.opts.Repo, issue.Spec, o.opts.Config.Harness.SpecDepth)
		if err != nil {
			o.log.Error("orchestrator: resolve spec slice; dispatching with empty slice",
				"issue", issue.ID, "spec", issue.Spec, "err", err)
		} else {
			specSlice = s
		}
	}
	return core.Brief{
		Issue:    issue,
		Spec:     specSlice,
		Base:     base,
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

// selectSoul picks the soul that fulfills an issue's role. A role may map to a *set* of
// souls (stage != soul); the choice among them is by matching the issue's tags against
// each candidate's Selector (see core.Soul.Matches, specs/configuration.md):
//
//   - no soul fulfills the role -> (zero, false): not dispatchable.
//   - exactly one soul -> use it unconditionally. The trivial 1:1 case needs no tags or
//     selector ceremony, so the kernel (one soul per role, issues untagged) keeps working
//     even though the shipped souls declare a selector.
//   - several souls -> keep those whose Selector the issue's tags satisfy and pick the
//     most specific (largest Selector); an empty selector is a catch-all default, so a
//     specialized soul beats it. If none match -> (zero, false).
//
// Selection is deterministic: Config.Souls is loaded sorted by Name, so iterating it and
// taking strictly-larger selectors breaks specificity ties by lowest Name. Validation
// rejects two souls fulfilling one role with identical selectors, so no real ambiguity
// reaches here. runGate re-selects by the same issue and gets the same soul, so the
// verification sandbox profile matches the producer's (producer != verifier still holds —
// a fresh sandbox, same toolchain).
func (o *Orchestrator) selectSoul(issue core.Issue) (core.Soul, bool) {
	var candidates []core.Soul
	for _, s := range o.opts.Config.Souls {
		if s.Role == issue.Role {
			candidates = append(candidates, s)
		}
	}
	switch len(candidates) {
	case 0:
		return core.Soul{}, false
	case 1:
		return candidates[0], true
	}
	best := -1
	var chosen core.Soul
	for _, s := range candidates {
		if s.Matches(issue.Tags) && len(s.Selector) > best {
			best = len(s.Selector)
			chosen = s
		}
	}
	if best < 0 {
		return core.Soul{}, false
	}
	return chosen, true
}

// roleIsAgentStage reports whether a role corresponds to a dispatchable agent stage —
// used to validate agent-proposed child issues before they are written.
func (o *Orchestrator) roleIsAgentStage(role string) bool {
	_, ok := o.stageForRole(role)
	return ok
}
