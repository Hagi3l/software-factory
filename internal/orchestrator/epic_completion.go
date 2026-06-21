package orchestrator

import (
	"context"
	"sort"

	"github.com/Loxstomper/harness/internal/core"
)

// sweepEpicCompletion detects drained epics and lands each one atomically (T7.4,
// specs/integration.md "Atomic feature integration"). It runs only under integration.mode: epic,
// on the SLOW sweep cadence alongside recompileMergedDelta — never the dispatch hot path — for the
// same reason epic_budget is read there: it is a full-table aggregate over every issue of an epic,
// and epic completion is a minute-granularity event, so pacing it slower keeps the heavy ListAll
// off the latency-sensitive loop and cuts the Dolt read pressure that causes write-visibility lag.
//
// In epic mode children integrate onto the epic branch (epic/<epic_id>) and the real main is held
// quiescent (T7.3); main advances exactly once, here, when the feature is complete. "Complete" is
// drain: every issue sharing the epic id is closed (integrated onto the epic branch) AND nothing in
// the subtree is in flight. The in-flight clause is load-bearing — only a running invocation can
// request a subtask, so an empty in-flight set with no open/blocked issues means the subtree can no
// longer grow, which is what makes it safe to declare done. This is read as an epic_id AGGREGATE
// (the same grouping core.EpicOf gives the epic budget), not a status threaded down a line, because
// an epic is a fan-out DAG.
//
// All-or-nothing falls out of the drain test: a child that dead-letters is blocked, not closed, so
// the subtree never drains clean, the terminal merge never fires, and the epic branch is abandoned
// (left for triage, main untouched). A feature lands whole or not at all.
//
// The sweep is idempotent. After a terminal merge the epic's root issue stays closed, so a later
// sweep still sees the epic drained and calls MergeEpic again — which detects its own prior merge
// commit on main (the epic id in the trailer) and no-ops, reporting merged=false. So the steady
// state of a landed epic is a cheap, silent re-check every slow tick, never a second merge.
func (o *Orchestrator) sweepEpicCompletion(ctx context.Context) {
	if !o.epicMode() {
		return
	}
	all, err := o.bd.ListAll(ctx)
	if err != nil {
		o.log.ErrorContext(ctx, "orchestrator: list all for epic-completion sweep", "err", err)
		return
	}

	// Group every issue by its epic (core.EpicOf: a descendant's threaded EpicID, or a root seed's
	// own id), so the drain test is a per-feature aggregate rather than a per-issue check.
	byEpic := map[string][]core.Issue{}
	for _, is := range all {
		byEpic[epicOf(is)] = append(byEpic[epicOf(is)], is)
	}

	// An epic with any in-flight member cannot be declared drained even if ListAll's
	// (eventually-consistent) snapshot shows every issue closed: a running invocation may still
	// request a subtask, growing the subtree. The in-flight projection is the single writer's
	// read-your-writes record, so it never lags its own claims — it is the authoritative "nothing
	// in flight" signal, closing the window where a just-spawned child is not yet visible in
	// ListAll but its in-flight parent is in the projection (specs/integration.md "Completion").
	inflightEpics := map[string]bool{}
	for _, is := range o.inflight.issues() {
		inflightEpics[epicOf(is)] = true
	}

	// Deterministic order so logging and tests are stable across map iteration order.
	epics := make([]string, 0, len(byEpic))
	for epic := range byEpic {
		epics = append(epics, epic)
	}
	sort.Strings(epics)

	for _, epic := range epics {
		if inflightEpics[epic] {
			continue
		}
		drained := true
		var root core.Issue
		haveRoot := false
		for _, is := range byEpic[epic] {
			if is.Status != statusClosed {
				drained = false
				break
			}
			// The root issue (ID == the epic id) supplies the merge subject (its title) and the
			// trailer's durable epic reference. It folds into its own epic group via EpicOf.
			if is.ID == epic {
				root = is
				haveRoot = true
			}
		}
		// haveRoot guards the eventual-consistency case where descendants are visible but the root
		// is not yet in this ListAll snapshot: without the root we know neither the feature title
		// nor that the whole subtree is present, so wait for the next sweep rather than merge blind.
		if !drained || !haveRoot {
			continue
		}
		o.terminalMerge(ctx, root)
	}
}

// terminalMerge lands one drained epic on main via the merger's two-parent terminal merge and
// announces it. The whole-feature provenance layer carries the epic id as its durable Issue
// reference and the root's title as the subject (so main's first-parent history reads one commit
// per feature); the per-child provenance stays reachable under the merge's second parent. A
// merged=false return means the merge already landed (idempotent re-check) or there was nothing to
// land, so it neither announces nor logs — only a fresh landing is observable. An error is logged
// and the sweep moves on; the next slow tick retries (the epic stays drained), so a transient git
// fault cannot strand a finished feature.
func (o *Orchestrator) terminalMerge(ctx context.Context, root core.Issue) {
	epic := epicOf(root)
	epicRef := "refs/heads/" + epicBranch(epic)
	target := "refs/heads/" + o.base
	prov := core.Provenance{Issue: epic, Subject: root.Title}
	commit, merged, err := o.merger.MergeEpic(ctx, o.opts.Repo, epicRef, target, prov)
	if err != nil {
		o.log.ErrorContext(ctx, "orchestrator: epic terminal merge", "epic", epic, "epic_ref", epicRef, "err", err)
		return
	}
	if !merged {
		return
	}
	// Surface the atomic landing on the merge-queue view, keyed by the epic root, exactly as a
	// per-item candidate's landed step is (T4.24). commit is the new main tip the merge produced.
	o.announceMergeState(root, core.MergeStateLanded, commit)
	o.log.InfoContext(ctx, "orchestrator: epic landed atomically (terminal merge)",
		"epic", epic, "commit", commit, "subject", root.Title)
}
