package orchestrator

import (
	"context"
	"fmt"
	"sort"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/spec"
)

// recompileSpecDelta is the orchestrator's response to a spec edit — "the factory recompiles
// the delta" (see specs/specs-process.md). A spec file is a human's only lever, and it may be
// refined while work is in flight. The agents cannot see that change: each issue's spec slice
// is materialized once, at dispatch, so keeping in-flight work aligned with an edited spec
// must be structural, not a prompt instruction. Each in_progress issue pinned the content
// hash of the slice it was briefed against (T3.6, scheduleReady/PinSpecHash); this sweep
// re-resolves that slice from the integration repo, re-hashes it, and compares it to the pin.
// A mismatch means the spec version underneath the work changed, so the issue is reissued —
// returned to the ready pool to be re-dispatched against the new slice — and the in-flight
// attempt's eventual Result is ignored because the issue is no longer in_progress, exactly as
// for a released stranded lease.
//
// Re-resolving every in_progress issue every tick subsumes "diff which issues referenced the
// edited file": an issue whose slice does not include the edited file re-hashes to the same
// value and is left untouched, so no explicit edit-event channel or membership test is needed.
// The in_progress set is small (bounded by concurrent dispatch) and resolution is a handful
// of local file reads, so this is cheap in the bootstrap; a future optimization could gate it
// on a cheap spec-tree change signal (T3.7b).
//
// Scope: only in_progress issues are swept here. A not-yet-dispatched issue carries no pin and
// will resolve the current slice when it dispatches; already-merged (closed) work is re-derived
// by the companion sweep recompileMergedDelta (T3.7b) — a closed issue is past re-dispatch, so
// realigning it means spawning fresh planning work, not reissuing. Re-resolution is best-effort,
// mirroring buildBrief's discipline: an issue with no spec or no pin is skipped, and a slice that
// fails to resolve (the file is mid-edit or was deleted) is logged and left alone rather than
// disrupting live work on an ambiguous signal.
func (o *Orchestrator) recompileSpecDelta(ctx context.Context) {
	inflight, err := o.bd.InProgress(ctx)
	if err != nil {
		o.log.Error("orchestrator: list in_progress for spec-drift sweep", "err", err)
		return
	}
	for _, issue := range inflight {
		// No spec reference (nothing to resolve) or no pin yet (dispatched before the slice
		// could be pinned — degraded, not drifted): there is no version to diff against.
		if issue.Spec == "" || issue.SpecHash == "" {
			continue
		}
		slice, err := spec.Resolve(o.opts.Repo, issue.Spec, o.opts.Config.Harness.SpecDepth)
		if err != nil {
			o.log.Error("orchestrator: resolve spec slice for drift check; leaving in-flight work untouched",
				"issue", issue.ID, "spec", issue.Spec, "err", err)
			continue
		}
		current := spec.Hash(slice)
		if current == issue.SpecHash {
			continue
		}
		if err := o.bd.Reissue(ctx, issue.ID); err != nil {
			o.log.Error("orchestrator: reissue spec-drifted issue", "issue", issue.ID, "err", err)
			continue
		}
		o.log.Info("orchestrator: spec drift detected; reissued stale in-flight work",
			"issue", issue.ID, "spec", issue.Spec, "pinned", issue.SpecHash, "current", current)
	}
}

// recompileMergedDelta extends "recompile the delta" to ALREADY-MERGED work (T3.7b). The
// in-flight sweep (recompileSpecDelta) re-dispatches stale live work, but a spec a human edits
// after an epic has merged also needs to flow back into the code — the merged implementation may
// now diverge from the refined contract. A closed issue is past re-dispatch, so realignment is not
// reissuing it but spawning fresh planning work that decomposes the delta against the merged code.
//
// The unit is (epic, spec-path), NOT the individual closed issue: one spec edit typically touches
// many closed issues of an epic that share a path, and re-deriving per issue would fan out a
// redundant pass for each. Keying on (epicOf, Spec) dedupes that to a single re-derivation per
// epic per edited path. On a mismatch between the re-resolved slice and the group's pinned hash
// the sweep spawns ONE fresh plan issue for the (epic, path) — re-entry at planning, not
// author-tests, because a spec change can add/remove/alter work items, which only the
// decomposition planner can express; reading the edited spec against the already-merged code, it
// decomposes only the delta. The new plan carries the epic id and the epic's selector tags and
// branches from the epic's merged tip (the merged work is on main, so an empty Base falls back to
// the pipeline base, exactly as a freshly seeded plan issue does).
//
// Two idempotency mechanisms keep one edit from spawning an unbounded stream of plans. (1) The
// spawn is SKIPPED when a planning pass for that (epic, path) is already open — any non-closed
// plan-role issue for the group, a prior re-derivation still in flight. (2) After spawning, every
// closed member is RE-PINNED to the new slice (the latch): once the spawned plan settles and its
// open marker clears, the members already match the current hash, so the next sweep sees the group
// settled and does not respawn. Best-effort throughout, like the in-flight sweep: a group whose
// slice fails to resolve is logged and left untouched, and a closed member with no pin carries no
// version to diff and contributes no drift signal. Known coarseness (see specs/specs-process.md):
// a provably-localized single-criterion edit still triggers a full planning pass.
func (o *Orchestrator) recompileMergedDelta(ctx context.Context) {
	// Re-derivation re-enters at the plan stage. Without one (the kernel's single-stage
	// pipeline) there is nowhere to re-derive into, so the sweep is a no-op and we skip the
	// ListAll entirely.
	plan := o.planRoles()
	if plan.spawn == "" {
		return
	}

	all, err := o.bd.ListAll(ctx)
	if err != nil {
		o.log.Error("orchestrator: list all for merged spec-drift sweep", "err", err)
		return
	}

	// Group closed work by (epic, spec-path) and note which groups already have a planning pass
	// open, so the spawn below can dedupe one edit across an epic's many closed issues.
	type groupKey struct{ epic, spec string }
	closed := map[groupKey][]core.Issue{}
	planOpen := map[groupKey]bool{}
	for _, issue := range all {
		if issue.Spec == "" {
			continue
		}
		k := groupKey{epic: epicOf(issue), spec: issue.Spec}
		if issue.Status == statusClosed {
			closed[k] = append(closed[k], issue)
			continue
		}
		// A non-closed plan-role issue for this (epic, path) means a planning pass — the epic's
		// initial decomposition or a prior re-derivation — is in flight; don't pile on another.
		if plan.roles[issue.Role] {
			planOpen[k] = true
		}
	}

	// Deterministic iteration so logging (and tests) are stable across map-order runs.
	keys := make([]groupKey, 0, len(closed))
	for k := range closed {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].epic != keys[j].epic {
			return keys[i].epic < keys[j].epic
		}
		return keys[i].spec < keys[j].spec
	})

	for _, k := range keys {
		members := closed[k]
		slice, err := spec.Resolve(o.opts.Repo, k.spec, o.opts.Config.Harness.SpecDepth)
		if err != nil {
			o.log.Error("orchestrator: resolve spec slice for merged drift check; leaving merged work untouched",
				"epic", k.epic, "spec", k.spec, "err", err)
			continue
		}
		current := spec.Hash(slice)
		// Drift iff a closed member's pin differs from the re-resolved slice. A member with no
		// pin carries no version to diff and is ignored as a signal (mirroring the in-flight sweep).
		drifted := false
		for _, m := range members {
			if m.SpecHash != "" && m.SpecHash != current {
				drifted = true
				break
			}
		}
		if !drifted {
			continue
		}
		// Idempotency (1): a planning pass for this (epic, path) is already open — don't spawn a
		// second. Leave the pins as-is; the next sweep re-checks once that pass settles.
		if planOpen[k] {
			continue
		}

		// Carry the epic's selector tags forward so the re-derivation routes to the same souls the
		// epic used (a `lang=go` epic stays on go souls); the members all share them.
		var tags map[string]string
		for _, m := range members {
			if len(m.Tags) > 0 {
				tags = m.Tags
				break
			}
		}
		title := fmt.Sprintf("Re-derive %s after spec change", k.spec)
		body := fmt.Sprintf("The governing spec %s changed since this epic's work merged. Re-plan "+
			"against the already-merged code, decomposing only the delta the edit introduces; do not "+
			"redo work that still satisfies the updated spec.", k.spec)
		created, err := o.bd.Apply(ctx, []core.Proposal{{
			Issue: core.Issue{Title: title, Body: body, Role: plan.spawn, Spec: k.spec, Tags: tags, EpicID: k.epic},
		}})
		if err != nil {
			o.log.Error("orchestrator: spawn re-derivation plan issue", "epic", k.epic, "spec", k.spec, "err", err)
			continue
		}
		// Idempotency (2): re-pin every closed member to the new slice (the latch). Once the
		// spawned plan settles, the members already match current, so a later sweep sees the group
		// settled and does not respawn. Best-effort: a pin failure logs and the spawn still stands
		// (the open-plan marker keeps the next sweep from double-spawning meanwhile).
		for _, m := range members {
			if err := o.bd.PinSpecHash(ctx, m.ID, current); err != nil {
				o.log.Error("orchestrator: re-pin merged issue spec hash", "issue", m.ID, "err", err)
			}
		}
		for _, c := range created {
			o.log.Info("orchestrator: spec drift on merged work; spawned re-derivation plan",
				"epic", k.epic, "spec", k.spec, "plan", c.ID, "current", current)
		}
	}
}

// planRoleSet captures the decomposition plan stage(s) for the merged-delta sweep. roles holds
// every plan-stage role (used to detect an in-flight planning pass for a group); spawn is the
// single role a re-derivation plan is created at — the lexicographically-first plan role, which
// is deterministic and, in every real config, the one plan stage. spawn is "" when no plan stage
// is configured (the kernel's single-stage pipeline), the signal recompileMergedDelta no-ops on.
type planRoleSet struct {
	roles map[string]bool
	spawn string
}

func (o *Orchestrator) planRoles() planRoleSet {
	out := planRoleSet{roles: map[string]bool{}}
	var roles []string
	for _, st := range o.opts.Config.Harness.DAG {
		if st.Kind == config.StageKindPlan && st.Role != "" {
			out.roles[st.Role] = true
			roles = append(roles, st.Role)
		}
	}
	sort.Strings(roles)
	if len(roles) > 0 {
		out.spawn = roles[0]
	}
	return out
}
