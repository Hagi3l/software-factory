// Package query is the control room's read model: render-ready projections over the three
// stores — beads (work state), git (provenance), and the artifact store (evidence) —
// stitched together and decoupled from the views that render them (specs/control-room.md,
// specs/observability.md). The views (T4.4+) call a Reader method and get a presentation
// struct; they never touch bd, git, or the store directly. Keeping the joins here (not in
// templ) is what lets the read shape be unit-tested without a browser and lets a view be a
// thin template over a typed value.
package query

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/model"
)

// IssueReader is the beads read surface the control room needs (a subset of
// *beads.Client). Defining it as an interface here keeps the query layer testable with a
// fake and free of a hard dependency on the bd CLI.
type IssueReader interface {
	Get(ctx context.Context, id string) (core.Issue, error)
	List(ctx context.Context, status string) ([]core.Issue, error)
	ListAll(ctx context.Context) ([]core.Issue, error)
}

// ArtifactReader is the read surface over the content-addressed artifact store (a subset of
// artifact.Store). Has backs availability badges; Get streams content to the evidence
// endpoint.
type ArtifactReader interface {
	Has(ctx context.Context, hash string) (bool, error)
	Get(ctx context.Context, hash string) (io.ReadCloser, error)
}

// ProvenanceReader reads merged-commit provenance from git (*GitProvenance satisfies it).
type ProvenanceReader interface {
	ByIssue(ctx context.Context, issueID string) (core.Provenance, bool, error)
	DiffByIssue(ctx context.Context, issueID string) (string, bool, error)
	Recent(ctx context.Context, limit int) ([]MergedCommit, error)
}

// Reader assembles the render-ready views. Its ports map to the three stores; a view method reads
// from whichever it needs and returns a presentation struct.
//
// It holds TWO issue readers, deliberately. live backs the LIVE work-state views — board, DAG,
// dead-letter, status bar, epic roll-up — which the spec routes through the orchestrator's
// work-graph projection (the consistent, in-memory read model that never lags the single writer and
// adds no `bd list` load — specs/observability.md "The live read model", T8.4). issues backs the
// FORENSIC pages — issue/invocation detail, replay, verification, budgets, merge queue — which the
// spec renders from beads, the durable truth (specs/control-room.md "Historical/forensic"). When
// co-located the two differ (live = projection, issues = beads); standalone, NewReader points both
// at the same beads reader so there is no behavior change without an attached orchestrator.
type Reader struct {
	live   IssueReader
	issues IssueReader
	arts   ArtifactReader
	prov   ProvenanceReader
}

// NewReader wires the read model to its stores with a single issue reader serving both the live and
// the forensic reads. It is the standalone shape (harness serve) and the default for tests: with no
// attached orchestrator there is no projection, so the live views read beads too — the same way the
// live SSE feed degrades there (specs/observability.md "The live read model").
func NewReader(issues IssueReader, arts ArtifactReader, prov ProvenanceReader) *Reader {
	return &Reader{live: issues, issues: issues, arts: arts, prov: prov}
}

// NewReaderWithLive wires the read model with a distinct LIVE reader for the work-state views (the
// projection-backed reader, NewProjectionIssueReader) and a durable beads reader for the forensic
// pages. It is the co-located shape (harness run --serve-addr), where the orchestrator's work-graph
// projection is the live read model (T8.4).
func NewReaderWithLive(live, issues IssueReader, arts ArtifactReader, prov ProvenanceReader) *Reader {
	return &Reader{live: live, issues: issues, arts: arts, prov: prov}
}

// IssueCard is the compact projection of an issue for the board and dead-letter lists —
// just what a card renders, not the full issue. StateEnteredAt/CreatedAt are the two anchors
// the board's client-ticked per-card timers advance from (T4.18, control-room.md "The board,
// in motion"): time-in-current-state and total-time-since-creation, respectively. They are
// raw time.Time values — the view emits them as epoch data-* attributes and the Alpine ticker
// does the per-second arithmetic, so the server never re-renders to tick.
type IssueCard struct {
	ID             string
	Title          string
	Status         string
	Role           string
	Attempt        int
	Spec           string
	StateEnteredAt time.Time
	CreatedAt      time.Time
	// WallCapped / WallPct / WallOver drive the live timer's budget.wall tint (control-room.md
	// "The board, in motion": the in-progress timer tints toward its budget.wall ceiling — a live
	// "about to breach" signal). They are the per-issue cumulative wall burn (core.Issue.SpentWall,
	// the cross-loop wall the orchestrator already enforces, T3.8b) against the configured per-issue
	// wall cap (caps.IssueWall), the same meterPct/meterOver the Budgets view uses — so the board
	// tint and the Budgets table can never disagree on a wall breach. WallCapped is false when no
	// budget.wall is configured (caps.IssueWall == 0), and the card then never tints (a percent of
	// no cap is meaningless), mirroring the Budgets view's uncapped-dimension behavior.
	WallCapped bool
	WallPct    int
	WallOver   bool
	// EpicID is the card's epic identity (core.EpicOf: the root seed's id, shared by every
	// issue of one feature). It is set whenever the issue belongs to a *multi-issue* epic — a
	// real decomposition fan-out (the epic's issue count > 1) — in per-item and epic modes
	// alike, because the grouping chrome it drives (a shared epic badge, a hue-hashed
	// left-border tint, and the lineage thread) is pure observability and is meaningful as soon
	// as a feature has more than one work item (T7.8, control-room.md "Epics on the board"). A
	// lone, directly-seeded issue (its own single-issue epic) leaves it empty — there the chrome
	// would only be noise.
	EpicID string
	// ParentID is the id of the card that *produced* this one — the lineage thread's edge
	// target (T7.8, control-room.md "Lineage thread"). It is derived with no new beads data:
	// a produced issue branches from its predecessor's verified candidate, so a Base of
	// candidate/<id> names the stage producer; a top-level decomposition child (no such base)
	// descends directly from the epic root. The root itself has none. Set only for cards in a
	// multi-issue epic (the same gate as EpicID), since a lone issue has no lineage to thread.
	ParentID string
	// Epic is the whole-feature summary, set only on an epic's *root* card (the hero) under
	// epic mode — nil on a child card and in per-item mode. It carries the progress indicator
	// (children integrated / total), the aggregate spend vs the epic_budget cap, and the
	// feature's integrating→done state, so the operator watches the feature land without
	// reading commits (T7.6, control-room.md "The root card is the hero"). It stays epic-mode-
	// only by design: its integrating→done lifecycle is git-derived and meaningless in per-item,
	// where the root closes at decomposition — so the grouping chrome above is decoupled from it.
	Epic *EpicSummary
}

func cardOf(i core.Issue) IssueCard {
	return IssueCard{
		ID:             i.ID,
		Title:          i.Title,
		Status:         i.Status,
		Role:           i.Role,
		Attempt:        i.Attempt,
		Spec:           i.Spec,
		StateEnteredAt: i.StateEnteredAt,
		CreatedAt:      i.CreatedAt,
	}
}

// Epic hero states (IssueCard.Epic.State, control-room.md "The root card is the hero"). The hero
// carries the feature through integrating while children finish and flips to done the moment the
// single terminal merge lands it on main — the board's read of the atomic feature landing.
const (
	// EpicStateIntegrating: the feature is still building/landing — at least one issue is not yet
	// integrated, or the subtree has drained but the terminal merge has not yet advanced main.
	EpicStateIntegrating = "integrating"
	// EpicStateDone: the epic's terminal merge has landed on main (a provenance commit citing the
	// epic id). This is the durable signal — read from git, not from issue status — so a drained
	// epic shows integrating until main actually moves, then flips to done.
	EpicStateDone = "done"
)

// EpicSummary is the whole-feature roll-up the board's hero (epic root) card renders under
// integration.mode: epic (T7.6, control-room.md "Epics on the board"). It needs no new data —
// it is an aggregate over the issues sharing an epic_id (core.EpicOf), the same grouping the
// epic budget enforces and the Budgets view shows: Integrated/Total is the integrated-vs-active
// child count (counting the durable `integrated` marker, NOT any closed status — a bead closes
// for several reasons, only one of which is integration), Tokens/USD is the summed marginal
// spend against the epic_budget cap, and State is the integrating→done lifecycle. So the hero
// shows "bounded autonomy" — progress and spend vs cap — as the feature builds.
type EpicSummary struct {
	EpicID     string
	Integrated int     // children marked integrated (the durable marker, not any closed status)
	Total      int     // integrated children + still-active children, excluding root + superseded beads
	State      string  // EpicStateIntegrating | EpicStateDone
	Tokens     int     // summed marginal token spend across the epic (matches authorizeEpic)
	TokenCap   int     // configured epic token cap, 0 when uncapped
	TokenPct   int     // burn as a clamped 0..100% of cap for a meter; 0 when uncapped
	USD        float64 // summed marginal USD spend across the epic
	USDCap     float64 // configured epic USD cap, 0 when uncapped
	USDPct     int     // burn as a clamped 0..100% of cap; 0 when uncapped
}

// epicSummaries rolls every issue up into its epic (core.EpicOf) for the board's hero cards.
// It mirrors the Budgets view's epic aggregation (marginal Closing* spend, never cumulative
// Spent*, which a fan-out would double-count) so the hero and the budget view can never
// disagree on a feature's burn. State is integrating by default and flips to done only when the
// terminal merge has landed the epic on main — read from provenance (landedOnMain), not from
// issue status, because the subtree drains (all issues closed) a slow-sweep tick *before* main
// actually advances; using the durable git signal keeps the hero honest across that window.
func (r *Reader) epicSummaries(ctx context.Context, issues []core.Issue, caps BudgetCaps) map[string]*EpicSummary {
	sums := make(map[string]*EpicSummary)
	for _, i := range issues {
		ep := core.EpicOf(i)
		s := sums[ep]
		if s == nil {
			s = &EpicSummary{EpicID: ep, State: EpicStateIntegrating}
			sums[ep] = s
		}
		// Spend aggregates over EVERY issue of the epic — the root, superseded retries, and every
		// intermediate stage all burned tokens — so the hero's burn matches the Budgets view /
		// authorizeEpic exactly (the same marginal Closing* sum). The progress *counts* below
		// filter; the spend does not.
		s.Tokens += i.ClosingTokens
		s.USD += i.ClosingUSD
		// Progress counts only the feature's "real" children: a bead reaches closed for several
		// reasons, so `closed` cannot mean "contributed to the feature" (specs/integration.md
		// "Integrated vs. closed"). Exclude the epic root (it closes at decomposition; its id is
		// the epic id) and any closed-but-not-integrated bead — a superseded on_failure retry or an
		// advanced intermediate stage. What remains is one frontier bead per lineage: its terminal
		// integrated bead, or its current active (open/in_progress/blocked) stage. Integrated counts
		// the durable marker (T8.3), never `closed`. So a feature split into two children that has
		// landed neither reads 0/2, never 1/4 from counting the closed root and a failed attempt.
		if i.ID == ep {
			continue
		}
		if i.Status == statusClosed && !i.Integrated {
			continue
		}
		s.Total++
		if i.Integrated {
			s.Integrated++
		}
	}
	for ep, s := range sums {
		s.TokenCap = caps.EpicTokens
		s.USDCap = caps.EpicUSD
		s.TokenPct = meterPct(float64(s.Tokens), float64(caps.EpicTokens))
		s.USDPct = meterPct(s.USD, caps.EpicUSD)
		if r.landedOnMain(ctx, ep) {
			s.State = EpicStateDone
		}
	}
	return sums
}

// landedOnMain reports whether an epic's terminal merge has landed on main — a provenance
// commit on main citing the epic id (the root's id). In epic mode children integrate onto the
// epic branch, so main carries only the single terminal merge; ByIssue (which greps main)
// therefore returns merged=true exactly when the feature has atomically landed. Best-effort: a
// missing provenance port or a git fault reads as "not landed" (the hero stays integrating)
// rather than failing the board — the same posture as the status bar's last-merge lookup.
func (r *Reader) landedOnMain(ctx context.Context, epicID string) bool {
	if r.prov == nil {
		return false
	}
	_, merged, err := r.prov.ByIssue(ctx, epicID)
	return err == nil && merged
}

// BoardColumn is one stage's cards on the kanban board. Focus marks the single column the
// board auto-scrolls to — the work frontier (control-room.md "Follow the frontier"): the
// leftmost column holding incomplete work, else the rightmost when everything is closed. It
// is a property of board state, so the query layer (not the view) is its single source of
// truth; exactly one column is Focus across a non-empty board.
type BoardColumn struct {
	Stage string
	Cards []IssueCard
	Focus bool
}

// Board is the kanban projection: every issue grouped by stage (its role), columns in the
// pipeline order the caller supplies. EpicMode echoes whether epic chrome was assembled (the
// cards carry EpicID/Epic) so the view can render the legend/affordance without re-reading
// config — the same reason Budgets echoes its Caps.
type Board struct {
	Columns  []BoardColumn
	Total    int
	EpicMode bool
}

// unassignedStage is the column for issues with no role set (a freshly seeded epic before
// the planner stamps stages). Named so it sorts and renders rather than vanishing.
const unassignedStage = "(unassigned)"

// Board groups every issue by stage for the kanban view. stageOrder is the pipeline order
// (e.g. the configured DAG: requirements→plan→…→integrate) so columns read left-to-right
// like the flow. Every declared stage gets a column whether or not it currently holds work
// (an empty stage renders as a count-0 column, not an absent one) so the board shows the
// shape of the whole pipeline at rest and never reflows as work flows through it
// (specs/control-room.md). Any stage present in the data but absent from stageOrder — an
// ad-hoc role the config never declared — is appended in stable alphabetical order, with
// unassigned work last; those materialize only when they actually hold cards. Cards within
// a column are ordered by id for a stable render.
// The grouping chrome — each card's epic identity (the hue-hashed badge/tint) and its lineage
// parent (the thread edge) — is driven by the *data*, not the mode: a card gets it whenever its
// issue belongs to a multi-issue epic (a real decomposition fan-out), in per-item and epic modes
// alike (T7.8, control-room.md "Epics on the board"). A lone, directly-seeded issue (its own
// single-issue epic) stays bare — the chrome would only be noise. The whole-feature EpicSummary
// hero roll-up, by contrast, is set only under integration.mode: epic (epicMode) on each epic's
// root card and measured against the supplied epic budget caps (T7.6) — its integrating→done
// lifecycle is git-derived and meaningless in per-item, where the root closes at decomposition.
// caps is the same query.BudgetCaps the Budgets view uses, so the hero's spend agrees with that
// view; it is ignored in per-item mode.
func (r *Reader) Board(ctx context.Context, stageOrder []string, epicMode bool, caps BudgetCaps) (Board, error) {
	issues, err := r.live.ListAll(ctx)
	if err != nil {
		return Board{}, fmt.Errorf("query: board: %w", err)
	}
	// Count issues per epic so the grouping chrome gates on a genuine multi-issue fan-out
	// (epic count > 1), independent of integration.mode.
	epicCounts := make(map[string]int)
	for _, i := range issues {
		epicCounts[core.EpicOf(i)]++
	}
	var summaries map[string]*EpicSummary
	if epicMode {
		summaries = r.epicSummaries(ctx, issues, caps)
	}
	byStage := make(map[string][]IssueCard)
	for _, i := range issues {
		stage := i.Role
		if stage == "" {
			stage = unassignedStage
		}
		c := cardOf(i)
		// budget.wall tint for the live timer: the cumulative wall burn the orchestrator enforces
		// against the per-issue wall cap, computed with the same meters as the Budgets view. Only
		// when a wall cap is configured (uncapped → no tint, like the Budgets view).
		if caps.IssueWall > 0 {
			c.WallCapped = true
			c.WallPct = meterPct(float64(i.SpentWall), float64(caps.IssueWall))
			c.WallOver = meterOver(float64(i.SpentWall), float64(caps.IssueWall))
		}
		ep := core.EpicOf(i)
		// Grouping chrome rides a real multi-issue epic, in either mode: the shared badge/tint
		// (EpicID) and the lineage thread's edge target (ParentID).
		if epicCounts[ep] > 1 {
			c.EpicID = ep
			c.ParentID = parentOf(i, ep)
		}
		// The hero is epic-mode-only and rides the epic root card (its own id == the epic id);
		// children carry just the shared grouping chrome above.
		if epicMode && i.ID == ep {
			c.Epic = summaries[ep]
		}
		byStage[stage] = append(byStage[stage], c)
	}

	board := Board{Total: len(issues), EpicMode: epicMode}
	for _, stage := range orderedStages(stageOrder, byStage) {
		cards := byStage[stage]
		sort.Slice(cards, func(a, b int) bool { return cards[a].ID < cards[b].ID })
		board.Columns = append(board.Columns, BoardColumn{Stage: stage, Cards: cards})
	}
	if idx := frontierColumn(board.Columns); idx >= 0 {
		board.Columns[idx].Focus = true
	}
	return board, nil
}

// parentOf derives a card's lineage parent — the card that produced it — with no new beads
// data (T7.8, control-room.md "Lineage thread"). A produced issue branches from its
// predecessor's verified candidate, so a Base of candidate/<id> names the stage producer
// directly; a child without such a base (a top-level decomposition issue, or a freshly seeded
// one) descends from the epic root, so it threads back to ep. The root itself (its own id == the
// epic id) has no parent. The thread it backs is a clean producer tree — sibling-ordering
// blocked-by edges are deliberately not drawn (control-room.md).
func parentOf(i core.Issue, ep string) string {
	if pid, ok := core.IssueIDFromCandidateBranch(i.Base); ok {
		return pid
	}
	if i.ID != ep {
		return ep
	}
	return ""
}

// frontierColumn returns the index of the board's work frontier — the column the view
// auto-scrolls into view (control-room.md "Follow the frontier"). The rule is purely
// positional over the rendered columns: the leftmost column holding any incomplete card
// (anything not closed — open/in_progress/blocked all count, so a blocked card pulls focus),
// else the rightmost column when every card is closed (the factory is done, so the view rests
// at the finish line). Returns -1 for a board with no columns. Column ordering — including
// where the ad-hoc/unassigned columns sit — is the caller's stageOrder concern, not this
// rule's; "leftmost" simply means lowest index.
func frontierColumn(cols []BoardColumn) int {
	if len(cols) == 0 {
		return -1
	}
	for i := range cols {
		for _, c := range cols[i].Cards {
			if c.Status != statusClosed {
				return i
			}
		}
	}
	return len(cols) - 1
}

// orderedStages returns the board's columns in render order: every declared stage first, in
// stageOrder, then any remaining stages present only in the data alphabetically, with the
// unassigned column always last. A declared stage with no issues is *kept* (rendered as an
// empty count-0 column) so the board always shows the full pipeline; only undeclared stages
// are data-driven (they appear when they hold work) — see specs/control-room.md.
func orderedStages(stageOrder []string, byStage map[string][]IssueCard) []string {
	var out []string
	seen := make(map[string]bool)
	for _, s := range stageOrder {
		if !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	var rest []string
	for s := range byStage {
		if !seen[s] && s != unassignedStage {
			rest = append(rest, s)
		}
	}
	sort.Strings(rest)
	out = append(out, rest...)
	if _, ok := byStage[unassignedStage]; ok {
		out = append(out, unassignedStage)
	}
	return out
}

// DeadLetter is the dead-letter queue's projection of a blocked issue — one escalation a
// human must triage, the control room's action surface. Beyond the board card's identity
// fields it surfaces the cumulative budget burn (SpentTokens/SpentUSD) and the retry
// generation (Attempt), because the two non-escalation dead-letter causes are exactly a
// budget breach and an exhausted retry cap (specs/workflow.md) — so a glance at spend and
// attempt tells the human *why* the work is stuck without opening it. Spec is the path the
// human refines to resolve it (the human re-entry invariant: stuck work is fixed by
// refining the spec, never by editing code — specs/specs-process.md). The full forensic
// trail (transcript, gate evidence, provenance) lives on the issue-detail page (T4.7) each
// entry links into. Reason is the orchestrator's own one-line classification of why the work
// terminated, stamped on the issue when it was blocked (core.Issue.DeadLetterReason, T4.15) —
// so the queue now states the cause rather than leaving the human to infer it from spend and
// attempt alone. It is empty for an older issue blocked before the reason was recorded.
type DeadLetter struct {
	ID          string
	Title       string
	Role        string
	Spec        string
	Attempt     int
	SpentTokens int
	SpentUSD    float64
	Reason      string
}

// DeadLetters returns the blocked issues — the escalations awaiting a human, the control
// room's primary action surface (specs/control-room.md, specs/workflow.md). They are the
// blocked beads status: work the orchestrator dead-lettered on a budget breach, an
// exhausted retry cap, or a needs-spec-clarification escalation. Ordered by id for a
// stable render.
func (r *Reader) DeadLetters(ctx context.Context) ([]DeadLetter, error) {
	issues, err := r.live.List(ctx, "blocked")
	if err != nil {
		return nil, fmt.Errorf("query: dead-letters: %w", err)
	}
	dls := make([]DeadLetter, 0, len(issues))
	for _, i := range issues {
		dls = append(dls, DeadLetter{
			ID:          i.ID,
			Title:       i.Title,
			Role:        i.Role,
			Spec:        i.Spec,
			Attempt:     i.Attempt,
			SpentTokens: i.SpentTokens,
			SpentUSD:    i.SpentUSD,
			Reason:      i.DeadLetterReason,
		})
	}
	sort.Slice(dls, func(a, b int) bool { return dls[a].ID < dls[b].ID })
	return dls, nil
}

// MergeRow is one candidate in the serialized merge train, for the merge-queue view (T4.25):
// the live merge step (from the typed merge-state event) joined to the issue's title/role/spec
// (from beads), so the row reads as more than a bare id. State is one of the core.MergeState*
// steps (queued/rebasing/re-gating/landed/conflicted/regate-failed); Terminal marks a row that
// has left the train (landed or failed) and Failed marks the two interesting terminal outcomes
// — conflicted / regate-failed — which correlate to the dead-letter or fix issue the same
// transition routes. Commit is the landed main tip (set only on landed) so the row can link
// onward to Provenance.
type MergeRow struct {
	ID       string
	Title    string
	Role     string
	Spec     string
	State    string
	Commit   string
	Terminal bool
	Failed   bool
}

// MergeQueue projects the live merge-train snapshot (the merge-state buffer the pump feeds —
// plan T4.25) into render-ready rows, enriching each candidate with its beads title/role/spec
// via a single ListAll. The event order is preserved (the buffer already holds the train's
// arrival order), so the view reads top-to-bottom as the queue is processed. A candidate whose
// issue is no longer in beads (raced past, or evicted) still renders from the event alone — the
// merge step is the point of this view, the title is enrichment. The merge step itself is never
// reconstructed from beads (beads holds no per-step state); it comes only from the events, which
// is why the buffer, not a beads read, is the source of membership and step.
func (r *Reader) MergeQueue(ctx context.Context, events []core.MergeStateEvent) ([]MergeRow, error) {
	if len(events) == 0 {
		return nil, nil
	}
	issues, err := r.issues.ListAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("query: merge-queue: %w", err)
	}
	byID := make(map[string]core.Issue, len(issues))
	for _, i := range issues {
		byID[i.ID] = i
	}
	rows := make([]MergeRow, 0, len(events))
	for _, ev := range events {
		row := MergeRow{
			ID:       ev.ID,
			Role:     ev.Role,
			State:    ev.State,
			Commit:   ev.Commit,
			Terminal: isTerminalMergeState(ev.State),
			Failed:   isFailedMergeState(ev.State),
		}
		if iss, ok := byID[ev.ID]; ok {
			row.Title = iss.Title
			row.Spec = iss.Spec
			if row.Role == "" {
				row.Role = iss.Role
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// isTerminalMergeState reports whether a merge step has left the train — landed (merged to
// main) or one of the two failure outcomes — so the view can tint it apart from an in-flight row.
func isTerminalMergeState(state string) bool {
	switch state {
	case core.MergeStateLanded, core.MergeStateConflicted, core.MergeStateRegateFailed:
		return true
	default:
		return false
	}
}

// isFailedMergeState reports whether a merge step is one of the two interesting terminal
// failures — a rebase conflict or a re-gate failure — which the spec calls out as the rows that
// correlate to a dead-letter or fix issue (specs/control-room.md "The merge-queue view").
func isFailedMergeState(state string) bool {
	return state == core.MergeStateConflicted || state == core.MergeStateRegateFailed
}

// ArtifactLink is a named, content-addressed evidence pointer for the issue-detail view —
// a hash plus a human label plus whether it is still resolvable in the store.
type ArtifactLink struct {
	Label     string // "Prompt", "Traceability", or a gate check's name
	Kind      string // core artifact kind (prompt / traceability-map / gate-evidence)
	Hash      string // content address ("sha256:…"); empty for a degraded citation with no persisted evidence
	Available bool   // store.Has(hash): whether the content can actually be fetched
}

// IssueDetail is the full single-issue view: the issue itself, its provenance (if it has
// been merged to main), the candidate diff that landed it, and the evidence artifacts
// derivable from that provenance, each resolved against the store for availability.
type IssueDetail struct {
	Issue      core.Issue
	Merged     bool
	Provenance core.Provenance
	Evidence   []ArtifactLink
	Diff       string // the unified diff the integration commit landed; empty until merged
}

// IssueDetail stitches all three stores for one issue: beads for the issue, git for its
// provenance (present once merged), and the artifact store to confirm each cited artifact
// is still fetchable. For a merged issue the evidence comes from the provenance trailer
// (prompt, traceability map, and each verified gate check cited as name@<hash>); for
// in-flight work it falls back to the traceability map hash threaded onto the issue. The
// store check is best-effort — a Has error marks an item unavailable rather than failing
// the whole view, so a flaky store never blanks the page.
func (r *Reader) IssueDetail(ctx context.Context, id string) (IssueDetail, error) {
	issue, err := r.issues.Get(ctx, id)
	if err != nil {
		return IssueDetail{}, fmt.Errorf("query: issue detail %s: %w", id, err)
	}
	detail := IssueDetail{Issue: issue}

	prov, merged, err := r.prov.ByIssue(ctx, id)
	if err != nil {
		return IssueDetail{}, fmt.Errorf("query: issue detail %s provenance: %w", id, err)
	}
	detail.Merged = merged
	detail.Provenance = prov

	if merged {
		detail.Evidence = r.evidenceFromProvenance(ctx, prov, issue)
		// The candidate diff is supplementary forensic context, not the spine of the page,
		// so a git fault reading it leaves the diff empty rather than blanking the whole
		// view — the same best-effort posture as the store availability check above.
		if diff, ok, derr := r.prov.DiffByIssue(ctx, id); derr == nil && ok {
			detail.Diff = diff
		}
	} else if issue.TraceMap != "" {
		detail.Evidence = []ArtifactLink{r.link(ctx, "Traceability", core.ArtifactKindTraceabilityMap, issue.TraceMap)}
	}
	return detail, nil
}

// evidenceFromProvenance turns a provenance record into resolved artifact links: the
// prompt, the transcript (the full agent conversation — the replayable decision trail),
// the traceability map (preferring the trailer's hash, falling back to the issue's threaded
// one), and each passing gate check. A check cited as a bare name (its evidence could not be
// persisted) is kept with an empty hash and Available=false so the view shows the check ran
// rather than hiding it.
func (r *Reader) evidenceFromProvenance(ctx context.Context, prov core.Provenance, issue core.Issue) []ArtifactLink {
	var links []ArtifactLink
	if prov.PromptSHA != "" {
		links = append(links, r.link(ctx, "Prompt", core.ArtifactKindPrompt, prov.PromptSHA))
	}
	if prov.Transcript != "" {
		links = append(links, r.link(ctx, "Transcript", core.ArtifactKindTranscript, prov.Transcript))
	}
	trace := prov.Traceability
	if trace == "" {
		trace = issue.TraceMap
	}
	if trace != "" {
		links = append(links, r.link(ctx, "Traceability", core.ArtifactKindTraceabilityMap, trace))
	}
	for _, v := range prov.Verified {
		name, hash, _ := strings.Cut(v, "@")
		links = append(links, r.link(ctx, name, core.ArtifactKindGateEvidence, hash))
	}
	return links
}

// link builds an ArtifactLink, resolving availability against the store. An empty hash (a
// degraded citation) is unavailable by definition and skips the store call.
func (r *Reader) link(ctx context.Context, label, kind, hash string) ArtifactLink {
	l := ArtifactLink{Label: label, Kind: kind, Hash: hash}
	if hash != "" && r.arts != nil {
		if has, err := r.arts.Has(ctx, hash); err == nil {
			l.Available = has
		}
	}
	return l
}

// MergedCommit is one integration commit with its parsed provenance, for the provenance
// view's "trace a merged commit back to issue→soul→model→prompt→evidence" timeline.
// Signature is the verify-on-read verdict (T5.10): whether main's tip commit carries a
// valid harness signature, so the human auditing provenance sees not just what produced a
// change but that the trusted layer cryptographically vouches for the record.
type MergedCommit struct {
	Commit     string
	Provenance core.Provenance
	Signature  SignatureStatus
}

// SignatureStatus is the result of verifying a merged commit's signature against the
// configured allowed-signers file (T5.10, specs/security.md). It mirrors git's %G?
// pretty-format codes, collapsed to the four states the provenance view distinguishes.
type SignatureStatus string

const (
	// SignatureUnchecked: verification was not attempted (no allowed-signers file
	// configured on the reader). The default — signing is an opt-in deployment posture.
	SignatureUnchecked SignatureStatus = ""
	// SignatureVerified: git %G? = G — a good signature by a key present in the
	// allowed-signers file (the harness identity). The trusted, attributable state.
	SignatureVerified SignatureStatus = "verified"
	// SignatureUnsigned: git %G? = N — the commit carries no signature (an unsigned
	// deployment, or a pre-signing commit in history).
	SignatureUnsigned SignatureStatus = "unsigned"
	// SignatureUntrusted: git %G? is anything else (U/B/E/X/Y/R) — signed, but not by a
	// key the allowed-signers file recognizes, or a bad/uncheckable signature. Surfaced
	// distinctly because it is the one state that should alarm: a commit signed by an
	// unknown key is more suspicious than an unsigned one.
	SignatureUntrusted SignatureStatus = "untrusted"
)

// signatureStatusFromGitCode maps git's %G? pretty-format code to a SignatureStatus.
func signatureStatusFromGitCode(code string) SignatureStatus {
	switch strings.TrimSpace(code) {
	case "G":
		return SignatureVerified
	case "N", "":
		return SignatureUnsigned
	default: // U (unknown validity), B (bad), E (cannot check), X/Y/R (expired/revoked)
		return SignatureUntrusted
	}
}

// RecentProvenance returns the most recent integration commits with parsed provenance,
// newest first — the provenance view's backing read.
func (r *Reader) RecentProvenance(ctx context.Context, limit int) ([]MergedCommit, error) {
	commits, err := r.prov.Recent(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("query: recent provenance: %w", err)
	}
	return commits, nil
}

// BudgetCaps are the configured ceilings the budget view measures burn against — the
// termination-guarantee limits from config.Harness.Policy (specs/workflow.md): the per-issue
// cumulative Budget, the per-epic aggregate EpicBudget, and the retry cap. A zero field means
// that dimension is uncapped. They are passed in by the caller (the server, from config) so
// the query layer stays free of a config dependency — mirroring how Board takes stageOrder.
type BudgetCaps struct {
	IssueTokens int
	IssueUSD    float64
	IssueWall   time.Duration
	EpicTokens  int
	EpicUSD     float64
	MaxRetries  int
}

// EpicBudgetRow is one epic's aggregate burn against the epic cap. Burn is the sum of each
// member issue's own marginal spend (ClosingTokens/ClosingUSD) over all issues sharing the
// epic — exactly what the orchestrator's authorizeEpic sums (T3.8b), so the view shows the
// same number enforcement acts on. (Marginal, never cumulative Spent*, because summing the
// cumulative across a fan-out would double-count shared ancestry.) The epic budget has no
// wall dimension — wall is per-issue cumulative only (config.Policy comment).
type EpicBudgetRow struct {
	EpicID    string
	Issues    int
	Tokens    int
	TokenCap  int
	TokenPct  int  // burn as a clamped 0..100% of cap for the meter; 0 when uncapped
	TokenOver bool // a breach: burn exceeds a configured cap
	USD       float64
	USDCap    float64
	USDPct    int
	USDOver   bool
}

// IssueBudgetRow is one issue's cumulative burn against the per-issue caps. Token/USD burn is
// the chain-cumulative Spent* (inherited across the on_failure retry chain) plus this issue's
// own marginal Closing* — i.e. the total spend the issue's chain has reached at this attempt,
// the quantity the orchestrator's per-issue budget bounds (T3.8). Wall is the cumulative
// SpentWall. Retry burn is Attempt against MaxRetries — an exhausted retry cap is one of the
// two non-escalation dead-letter causes, so it sits alongside spend (specs/workflow.md).
type IssueBudgetRow struct {
	ID         string
	Role       string
	Spec       string
	Status     string
	Attempt    int
	MaxRetries int
	RetryPct   int
	RetryOver  bool
	Tokens     int
	TokenCap   int
	TokenPct   int
	TokenOver  bool
	USD        float64
	USDCap     float64
	USDPct     int
	USDOver    bool
	Wall       time.Duration
	WallCap    time.Duration
	WallPct    int
	WallOver   bool
}

// Budgets is the budget view's projection: epic aggregates and per-issue burn, each measured
// against the configured caps (specs/control-room.md, "token/$/wall-clock burn vs. caps, per
// epic/issue"). The numbers come from beads' stamped cumulative/closing spend — the same
// values the orchestrator enforces budgets on — not from the OTel metric backend, so the view
// is self-contained and exact (the OTel metrics are the buy/Grafana cost-over-time surface;
// see specs/observability.md "build vs buy"). Caps is echoed for the header.
type Budgets struct {
	Epics  []EpicBudgetRow
	Issues []IssueBudgetRow
	Caps   BudgetCaps
}

// Budgets aggregates every issue's stamped spend into the epic and per-issue burn-vs-cap
// view. Epics are grouped by core.EpicOf (the single source the orchestrator's epic budget
// also groups by) and ordered by USD burn descending so the heaviest epics surface first;
// issues likewise, so the top spenders and any breaches read at a glance. A breach (burn over
// a configured cap, or Attempt at the retry cap) is flagged per dimension for the view to tint.
func (r *Reader) Budgets(ctx context.Context, caps BudgetCaps) (Budgets, error) {
	issues, err := r.issues.ListAll(ctx)
	if err != nil {
		return Budgets{}, fmt.Errorf("query: budgets: %w", err)
	}
	return aggregateBudgets(issues, caps), nil
}

// aggregateBudgets folds a set of issues into the epic and per-issue burn-vs-cap view. It is the
// shared core of the Budgets view (which feeds it beads' durable spend — the forensic cost surface)
// and the Status bar (which feeds it the live projection's issues, so the bar's queue depth /
// escalation / budget-health glance agrees with the live board, T8.4). Keeping the math in one place
// is why the bar and the budget table can never disagree on a breach.
func aggregateBudgets(issues []core.Issue, caps BudgetCaps) Budgets {
	// Epic aggregation: sum each member's marginal Closing* under its epic id.
	type epicAgg struct {
		tokens int
		usd    float64
		issues int
	}
	byEpic := make(map[string]*epicAgg)
	for _, i := range issues {
		ep := core.EpicOf(i)
		a := byEpic[ep]
		if a == nil {
			a = &epicAgg{}
			byEpic[ep] = a
		}
		a.tokens += i.ClosingTokens
		a.usd += i.ClosingUSD
		a.issues++
	}

	out := Budgets{Caps: caps}
	for ep, a := range byEpic {
		out.Epics = append(out.Epics, EpicBudgetRow{
			EpicID:    ep,
			Issues:    a.issues,
			Tokens:    a.tokens,
			TokenCap:  caps.EpicTokens,
			TokenPct:  meterPct(float64(a.tokens), float64(caps.EpicTokens)),
			TokenOver: meterOver(float64(a.tokens), float64(caps.EpicTokens)),
			USD:       a.usd,
			USDCap:    caps.EpicUSD,
			USDPct:    meterPct(a.usd, caps.EpicUSD),
			USDOver:   meterOver(a.usd, caps.EpicUSD),
		})
	}
	sort.Slice(out.Epics, func(x, y int) bool { return epicLess(out.Epics[x], out.Epics[y]) })

	for _, i := range issues {
		out.Issues = append(out.Issues, buildIssueBudgetRow(i, caps))
	}
	sort.Slice(out.Issues, func(x, y int) bool { return issueBudgetLess(out.Issues[x], out.Issues[y]) })
	return out
}

// buildIssueBudgetRow projects one issue's cumulative burn against the per-issue caps. It is the
// single source for an issue's budget meter — the Budgets table maps it over every issue and the
// live-invocation view (T4.21) calls it for one — so the two surfaces can never disagree on what
// an issue has spent. Token/USD burn is the chain-cumulative Spent* plus this issue's own marginal
// Closing* (the quantity the per-issue budget bounds, T3.8); a breach is flagged per dimension.
func buildIssueBudgetRow(i core.Issue, caps BudgetCaps) IssueBudgetRow {
	tokens := i.SpentTokens + i.ClosingTokens
	usd := i.SpentUSD + i.ClosingUSD
	return IssueBudgetRow{
		ID:         i.ID,
		Role:       i.Role,
		Spec:       i.Spec,
		Status:     i.Status,
		Attempt:    i.Attempt,
		MaxRetries: caps.MaxRetries,
		RetryPct:   meterPct(float64(i.Attempt), float64(caps.MaxRetries)),
		RetryOver:  caps.MaxRetries > 0 && i.Attempt >= caps.MaxRetries,
		Tokens:     tokens,
		TokenCap:   caps.IssueTokens,
		TokenPct:   meterPct(float64(tokens), float64(caps.IssueTokens)),
		TokenOver:  meterOver(float64(tokens), float64(caps.IssueTokens)),
		USD:        usd,
		USDCap:     caps.IssueUSD,
		USDPct:     meterPct(usd, caps.IssueUSD),
		USDOver:    meterOver(usd, caps.IssueUSD),
		Wall:       i.SpentWall,
		WallCap:    caps.IssueWall,
		WallPct:    meterPct(float64(i.SpentWall), float64(caps.IssueWall)),
		WallOver:   meterOver(float64(i.SpentWall), float64(caps.IssueWall)),
	}
}

// Invocation is the live-invocation view's projection (T4.21): the header identity of one issue's
// current invocation (id/title/role/stage/status) plus its cumulative budget burn against the
// per-issue caps, so the page shows a meter advancing toward the wall/token ceiling. Terminal is
// true once the issue is no longer in flight (closed or blocked) — the page then stops presenting
// itself as live and hands off to the forensic Replay (ReplayAvailable) of the same invocation.
// The scoped activity feed is NOT here: it is read from the live in-memory buffer (which the query
// layer has no access to) and supplied to the view by the server, filtered on the issue id the
// runner stamps on every agent event (T4.20).
type Invocation struct {
	ID              string
	Title           string
	Role            string
	Status          string
	Spec            string
	Body            string
	Terminal        bool
	ReplayAvailable bool // a transcript is resolvable (merge trailer or issue stamp) — the /replay drill lands
	Budget          IssueBudgetRow
}

// Invocation assembles the live-invocation view for one issue: its header identity and the
// budget meter (the same per-issue burn the Budgets view shows, via the shared row builder), plus
// whether it is terminal and whether Replay can reconstruct its decision trail. The replay handoff
// surfaces wherever Replay will land: a merge trailer with a transcript, OR the hash stamped on the
// issue itself (every disposition) — so a dead-lettered invocation, the most useful handoff target,
// links through too, not only merged work.
func (r *Reader) Invocation(ctx context.Context, id string, caps BudgetCaps) (Invocation, error) {
	issue, err := r.issues.Get(ctx, id)
	if err != nil {
		return Invocation{}, fmt.Errorf("query: invocation %s: %w", id, err)
	}
	inv := Invocation{
		ID:       issue.ID,
		Title:    issue.Title,
		Role:     issue.Role,
		Status:   issue.Status,
		Spec:     issue.Spec,
		Body:     issue.Body,
		Terminal: issue.Status == statusClosed || issue.Status == statusBlocked,
		Budget:   buildIssueBudgetRow(issue, caps),
	}
	// Best-effort, like IssueDetail's provenance read: a git fault leaves the trailer source
	// unresolved rather than failing the page. ByIssue returns merged=false cheaply for in-flight
	// work. The issue stamp is the merge-independent fallback, so a dead-lettered run still links.
	if prov, merged, perr := r.prov.ByIssue(ctx, id); perr == nil && merged && prov.Transcript != "" {
		inv.ReplayAvailable = true
	} else if issue.Transcript != "" {
		inv.ReplayAvailable = true
	}
	return inv, nil
}

// Status is the layout status-bar projection — the "is the factory healthy?" glance that rides
// every page (specs/control-room.md, "A thin status bar rides every page … queue depth · active
// agents · open escalations · budget-health dot · last merge"). It is assembled from the same
// beads/provenance reads that back the board, dead-letter, and budget views — no new beads query —
// so the bar agrees with those views by construction. ActiveAgents is deliberately NOT here: it
// derives from the in-memory live.Activity buffer (distinct agent ids seen in a recent window),
// which the read model has no access to, so the server handler fills it in alongside this read.
type Status struct {
	// QueueDepth is the work still in flight: issues that are neither terminal (closed) nor
	// escalated (blocked) — i.e. open/ready or in_progress — the depth the factory has yet to drain.
	QueueDepth int
	// OpenEscalations is the dead-letter queue depth (blocked issues), the human's action surface.
	OpenEscalations int
	// BudgetHealth is the worst per-dimension burn state across all issues and epics:
	// StatusHealthOK / StatusHealthWarn (any dimension ≥ the warn threshold) / StatusHealthBreach
	// (any dimension over a configured cap) — the same thresholds the Budgets view tints by.
	BudgetHealth string
	// LastMergeIssue is the issue id of the most recent integration commit, "" if none merged yet.
	LastMergeIssue string
}

// Budget-health levels for Status.BudgetHealth, worst-wins. They mirror the Budgets view's
// tinting (rose breach / amber ≥80% / emerald) so the status dot and the budget meters agree.
const (
	StatusHealthOK     = "ok"
	StatusHealthWarn   = "warn"
	StatusHealthBreach = "breach"
	// statusWarnPct is the burn percentage (of any configured cap) at which the health dot turns
	// amber, matching the Budgets view's ≥80% amber band.
	statusWarnPct = 80
)

// statusBlocked is the beads status of a dead-lettered (escalated) issue. statusClosed (the
// terminal status) is declared alongside the other status literals in resolve.go.
const statusBlocked = "blocked"

// Status assembles the layout status bar from a single Budgets read (one beads ListAll, reusing
// the exact burn/breach math the budget view enforces on) plus the newest provenance commit. It
// derives queue depth and escalations from the per-issue rows' status, and budget health from the
// rows' breach/percentage flags — so a status that drifts from the board or budget view is
// impossible. The last-merge lookup is best-effort: a git fault leaves LastMergeIssue empty rather
// than failing the whole bar (the bar is a glance, not a gate).
func (r *Reader) Status(ctx context.Context, caps BudgetCaps) (Status, error) {
	// The status bar is a LIVE view, so it reads the projection (r.live), not beads — its queue
	// depth / escalation count / budget-health dot are exactly the live board's numbers, derived
	// through the same burn math (aggregateBudgets) the forensic Budgets view uses, so the two can
	// never disagree (T8.4, specs/observability.md "The live read model").
	issues, err := r.live.ListAll(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("query: status: %w", err)
	}
	b := aggregateBudgets(issues, caps)
	st := Status{BudgetHealth: StatusHealthOK}
	for _, row := range b.Issues {
		switch row.Status {
		case statusBlocked:
			st.OpenEscalations++
		case statusClosed:
			// Terminal: merged or otherwise closed, no longer in flight.
		default:
			// open/ready, in_progress, or any non-terminal status is work the factory still owes.
			st.QueueDepth++
		}
		st.BudgetHealth = worseHealth(st.BudgetHealth, issueRowHealth(row))
	}
	for _, e := range b.Epics {
		st.BudgetHealth = worseHealth(st.BudgetHealth, epicRowHealth(e))
	}
	// Last merge is best-effort: a git fault — or no provenance port at all — leaves it empty
	// rather than failing the whole bar (the bar is a glance, not a gate).
	if r.prov != nil {
		if recent, rerr := r.prov.Recent(ctx, 1); rerr == nil && len(recent) > 0 {
			st.LastMergeIssue = recent[0].Provenance.Issue
		}
	}
	return st, nil
}

// issueRowHealth reduces a per-issue budget row to a health level: a breach on any dimension
// (token/USD/wall burn over cap, or the retry cap reached) is StatusHealthBreach; otherwise any
// dimension at/above the warn band is StatusHealthWarn; else OK.
func issueRowHealth(row IssueBudgetRow) string {
	if row.TokenOver || row.USDOver || row.WallOver || row.RetryOver {
		return StatusHealthBreach
	}
	if row.TokenPct >= statusWarnPct || row.USDPct >= statusWarnPct ||
		row.WallPct >= statusWarnPct || row.RetryPct >= statusWarnPct {
		return StatusHealthWarn
	}
	return StatusHealthOK
}

// epicRowHealth is issueRowHealth's epic-aggregate analog (token/USD only — epics have no wall or
// retry cap).
func epicRowHealth(row EpicBudgetRow) string {
	if row.TokenOver || row.USDOver {
		return StatusHealthBreach
	}
	if row.TokenPct >= statusWarnPct || row.USDPct >= statusWarnPct {
		return StatusHealthWarn
	}
	return StatusHealthOK
}

// worseHealth returns the more severe of two health levels (breach > warn > ok).
func worseHealth(a, b string) string {
	rank := func(h string) int {
		switch h {
		case StatusHealthBreach:
			return 2
		case StatusHealthWarn:
			return 1
		default:
			return 0
		}
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// meterPct is burn as a whole-number percent of cap, clamped to [0,100] for the meter fill.
// An uncapped dimension (cap <= 0) has no meaningful fill, so it reports 0.
func meterPct(used, cap float64) int {
	if cap <= 0 {
		return 0
	}
	p := int(used / cap * 100)
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// meterOver reports a breach: burn strictly past a configured (non-zero) cap.
func meterOver(used, cap float64) bool { return cap > 0 && used > cap }

// epicLess orders epics by USD burn desc, then tokens desc, then id, for a stable
// heaviest-first render.
func epicLess(a, b EpicBudgetRow) bool {
	if a.USD != b.USD {
		return a.USD > b.USD
	}
	if a.Tokens != b.Tokens {
		return a.Tokens > b.Tokens
	}
	return a.EpicID < b.EpicID
}

// issueBudgetLess orders issues by USD burn desc, then tokens desc, then id.
func issueBudgetLess(a, b IssueBudgetRow) bool {
	if a.USD != b.USD {
		return a.USD > b.USD
	}
	if a.Tokens != b.Tokens {
		return a.Tokens > b.Tokens
	}
	return a.ID < b.ID
}

// Artifact streams a single artifact's content by hash — the backing read for the evidence
// endpoint (a transcript, a gate output, the prompt). The caller closes the reader.
func (r *Reader) Artifact(ctx context.Context, hash string) (io.ReadCloser, error) {
	if r.arts == nil {
		return nil, fmt.Errorf("query: no artifact store configured")
	}
	rc, err := r.arts.Get(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("query: artifact %s: %w", hash, err)
	}
	return rc, nil
}

// GateVerdictView is the verification view's projection (T4.23): the assembled gate-verdict
// record of one gate run plus the producer≠verifier soul split, the trust argument rendered
// as a forensic snapshot (specs/verification.md "The gate verdict is recorded"). TestsSoul is
// the independent test author and ImplementSoul the implementor — the two producing souls
// whose independence the design hinges on; the qa gate has no verifier soul (it runs in the
// orchestrator-controlled clean verification sandbox, its independence carried structurally).
//
// Available is false when no verdict record could be reconstructed: Hash == "" means none was
// stamped (the issue's candidate has not been gated, or the gate could not persist one), while
// a set Hash with Available == false means the cited record could not be fetched or decoded.
// Either way the view degrades to a notice (offering the raw-bytes link when a hash is known)
// rather than failing the page — the same best-effort posture as Replay.
type GateVerdictView struct {
	Issue         core.Issue
	Merged        bool
	TestsSoul     string // the author-tests producing soul (the independent test author)
	ImplementSoul string // the implement producing soul (the implementor)
	Hash          string // the gate-verdict artifact's content address, when one is stamped
	Available     bool   // the record resolved + parsed from the store
	Verdict       core.GateVerdict
	// Trace is the test↔spec traceability map cited as evidence — the only window into how
	// the author read the prose (specs/verification.md). It is read from the issue's own
	// threaded stamp (issue.TraceMap), the same principled source as the souls; an empty hash
	// renders as "not recorded" rather than a dead link.
	Trace ArtifactLink
	// Transforms is this issue's transformation log (T6.3), read back from the hash stamped on
	// the issue (issue.TransformLog) — one entry per semantic write tool recording the MECHANISM
	// it ran through (semantic vs the degraded text floor). It lets the verification view weigh
	// the text-fallback transformations — the imprecise ones that can rewrite comments and string
	// literals — alongside the gate verdict. Nil when the invocation ran no semantic write tools
	// (the common case) or the log could not be fetched/parsed; TransformLog.Available reflects
	// which, and the view renders the transformations section only when there is a hash.
	Transforms []core.TransformRecord
	// TransformLog is the raw-bytes link to the harvested log (Available = fetched + parsed), so a
	// human can open the canonical record behind the rendered rows. Hash empty when none was stamped.
	TransformLog ArtifactLink
}

// GateVerdict reconstructs one issue's verification verdict: the assembled gate-verdict record
// the gate harvested and the two producing souls, read back for the verification view. The
// souls come from the issue's own stamps (TestsSoul / ImplementSoul) — the principled,
// stage-keyed source the orchestrator threads forward like TraceMap. The merge trailer is NOT
// preferred for them: its Soul is whichever stage produced the *landed* candidate (qa, or a
// merge-resolver), not necessarily the implementor, and it carries no implement-specific field —
// the issue stamps are the single source the trailer's Tests-Soul itself is derived from. The
// verdict record is resolved from the hash the orchestrator stamps onto the issue for *every*
// disposition — so a rejected candidate's verdict renders too, not only a merged one (T4.22).
//
// Only an unreadable issue or provenance is fatal; a missing/unfetchable/corrupt verdict record
// yields a view with Available=false and a notice, mirroring the detail and replay pages'
// best-effort posture (a flaky store never blanks the page).
func (r *Reader) GateVerdict(ctx context.Context, id string) (GateVerdictView, error) {
	issue, err := r.issues.Get(ctx, id)
	if err != nil {
		return GateVerdictView{}, fmt.Errorf("query: gate verdict %s: %w", id, err)
	}
	view := GateVerdictView{
		Issue:         issue,
		TestsSoul:     issue.TestsSoul,
		ImplementSoul: issue.ImplementSoul,
		Hash:          issue.GateVerdict,
		Trace:         r.link(ctx, "Traceability map", core.ArtifactKindTraceabilityMap, issue.TraceMap),
		TransformLog:  ArtifactLink{Label: "Transformation log", Kind: core.ArtifactKindTransformLog, Hash: issue.TransformLog},
	}

	// The transformation log (T6.3) is resolved independently of the verdict: an issue can carry
	// one without a verdict (and vice versa). A successful fetch+parse marks the link Available
	// and supplies the structured rows the view weighs; a degraded read leaves just the link.
	if recs, ok := r.readTransformLog(ctx, issue.TransformLog); ok {
		view.Transforms = recs
		view.TransformLog.Available = true
	}

	// Merged is a presentation flag (has this landed?); the souls come from the issue stamps
	// above, not the trailer. A provenance read fault leaves Merged false rather than failing
	// the page — best-effort, like the detail page.
	if _, merged, perr := r.prov.ByIssue(ctx, id); perr == nil {
		view.Merged = merged
	}

	if issue.GateVerdict == "" || r.arts == nil {
		return view, nil // nothing to resolve — the view renders the no-verdict notice
	}
	// Best-effort from here: a store fault or a corrupt record degrades to "couldn't load"
	// (with the raw-bytes link still offered via Hash) rather than blanking the header.
	rc, err := r.arts.Get(ctx, issue.GateVerdict)
	if err != nil {
		return view, nil
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return view, nil
	}
	var rec core.GateVerdict
	if err := json.Unmarshal(data, &rec); err != nil {
		return view, nil
	}
	view.Available = true
	view.Verdict = rec
	return view, nil
}

// readTransformLog fetches and decodes an issue's transformation log (T6.3) — the JSON
// []core.TransformRecord the runner harvested under ArtifactKindTransformLog. Best-effort: an
// empty hash, no store, a fetch fault, or a corrupt record yields ok=false (the verification
// view then degrades to the raw-bytes link), never an error — the same posture as the verdict read.
func (r *Reader) readTransformLog(ctx context.Context, hash string) ([]core.TransformRecord, bool) {
	if hash == "" || r.arts == nil {
		return nil, false
	}
	rc, err := r.arts.Get(ctx, hash)
	if err != nil {
		return nil, false
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, false
	}
	var recs []core.TransformRecord
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, false
	}
	return recs, true
}

// ReplayToolCall is one tool the model asked to invoke on a turn: the tool name and its
// arguments, pretty-printed for legibility (the wire form is compact JSON). ID ties it to
// the matching tool result fed back on the next turn.
type ReplayToolCall struct {
	ID   string
	Name string
	Args string // the model's arguments, indented JSON (falls back to the raw bytes if not JSON)
}

// ReplayToolResult is one tool result fed back to the model: the textual outcome and
// whether the tool reported an error (so the trail shows a failure the model had to
// recover from, not a silently-hidden one).
type ReplayToolResult struct {
	ToolCallID string
	Content    string
	IsError    bool
}

// ReplayMessage is one message the model newly saw at the start of a turn — the opening
// brief on turn 0, or the prior turn's tool results afterwards. Assistant echoes are
// deliberately excluded (see buildReplay): the assistant's own output is rendered once, as
// the turn's response, not again as inbound history.
type ReplayMessage struct {
	Role        string
	Text        string
	ToolResults []ReplayToolResult
}

// ReplayTurn is one llm-turn of the decision trail: what the model newly saw (Inbound),
// what it produced (Text + ToolCalls), why it stopped, and the tokens the turn cost. It
// maps one-to-one to the llm-turn span in the invocation trace (specs/observability.md).
type ReplayTurn struct {
	Index        int
	Inbound      []ReplayMessage
	Text         string
	ToolCalls    []ReplayToolCall
	Stop         string
	InputTokens  int
	OutputTokens int
	CacheRead    int
}

// Replay is the reconstructed decision trail for one invocation (specs/observability.md,
// "Replayability — the differentiator"): the persona the model ran under (System) and each
// turn it took, parsed from the broker-captured transcript in the artifact store. It is the
// forensic answer to "why did the agent do that" — exactly what the LLM saw and did, step
// by step.
//
// Available is false when no trail could be reconstructed: Hash == "" means no transcript is
// reachable for this issue (it has not merged, or none was harvested — the hash is only
// retained on the merge trailer, like the transcript evidence link, T4.7b), while a set Hash
// with Available == false means the cited transcript could not be fetched or decoded from the
// store. Either way the view degrades to a notice (offering the raw-bytes link when a hash is
// known) rather than failing the page.
type Replay struct {
	Issue       core.Issue
	Merged      bool
	Available   bool
	Hash        string // the transcript artifact's content address, when one is cited
	System      string // the persona/system prompt the invocation ran under (from the first turn)
	Turns       []ReplayTurn
	TotalInput  int
	TotalOutput int
}

// Replay reconstructs an invocation's decision trail from its broker-captured transcript.
// It resolves the transcript hash — preferring the merge trailer (the immutable, authoritative
// record of what landed), then falling back to the hash stamped on the issue itself
// (core.Issue.Transcript) — streams the JSON []model.TranscriptTurn from the artifact store,
// and folds it into the per-turn presentation the replay view renders. The issue stamp is the
// load-bearing fallback: the orchestrator records the most-recent invocation's transcript on
// the issue for *every* disposition (accepted, routed, dead-lettered), so a failed or escalated
// run is replayable too — the case where the forensic trail matters most, not only merged work.
// The issue itself is the only hard dependency — a missing/unharvested transcript or a flaky
// store yields a rendered page with Available=false and a notice, never an error, mirroring the
// detail page's best-effort posture. Only an inability to read the issue or its provenance is fatal.
func (r *Reader) Replay(ctx context.Context, id string) (Replay, error) {
	issue, err := r.issues.Get(ctx, id)
	if err != nil {
		return Replay{}, fmt.Errorf("query: replay %s: %w", id, err)
	}
	rep := Replay{Issue: issue}

	prov, merged, err := r.prov.ByIssue(ctx, id)
	if err != nil {
		return Replay{}, fmt.Errorf("query: replay %s provenance: %w", id, err)
	}
	rep.Merged = merged

	// Prefer the merge trailer's hash so merged replay is unchanged; fall back to the issue's
	// own stamp so unmerged work (in-flight, or dead-lettered after an escalation) replays too.
	hash := prov.Transcript
	if hash == "" {
		hash = issue.Transcript
	}
	if hash == "" || r.arts == nil {
		return rep, nil // nothing to replay — the view renders the no-transcript notice
	}
	rep.Hash = hash

	// Best-effort from here: the transcript is the spine of the page, but a store fault or a
	// corrupt artifact degrades to "couldn't load" (with the raw-bytes link still offered)
	// rather than blanking the issue header and notice.
	rc, err := r.arts.Get(ctx, hash)
	if err != nil {
		return rep, nil
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return rep, nil
	}
	var turns []model.TranscriptTurn
	if err := json.Unmarshal(data, &turns); err != nil {
		return rep, nil
	}

	rep.Available = true
	rep.System, rep.Turns, rep.TotalInput, rep.TotalOutput = buildReplay(turns)
	return rep, nil
}

// buildReplay folds the raw transcript turns into the render-ready trail. The persona is
// taken from the first turn (it is constant across an invocation — the soul's identity).
// For each turn, Inbound is the messages new to that turn's request versus the previous
// turn's: turn 0 carries the opening brief; later turns carry the prior turn's tool results.
// The agent loop appends (assistant turn + tool results) to the history each iteration, so
// the message slice is append-only and the suffix beyond the previous length is exactly what
// the model newly saw — and the leading assistant echo in that suffix is dropped, because the
// assistant's output is already rendered as the previous turn's response. A non-monotonic
// history (which the append-only loop never produces) falls back to showing the whole slice
// so the trail is never silently truncated.
func buildReplay(turns []model.TranscriptTurn) (system string, out []ReplayTurn, totalIn, totalOut int) {
	if len(turns) == 0 {
		return "", nil, 0, 0
	}
	system = turns[0].Request.System
	prevLen := 0
	for i, t := range turns {
		msgs := t.Request.Messages
		start := prevLen
		if start > len(msgs) {
			start = 0 // history isn't a clean superset — show all rather than truncate
		}
		rt := ReplayTurn{
			Index:        i,
			Text:         t.Response.Text,
			Stop:         string(t.Response.Stop),
			InputTokens:  t.Response.Usage.InputTokens,
			OutputTokens: t.Response.Usage.OutputTokens,
			CacheRead:    t.Response.Usage.CacheReadTokens,
		}
		for _, m := range msgs[start:] {
			if m.Role == model.RoleAssistant {
				continue // already rendered as the prior turn's response
			}
			rt.Inbound = append(rt.Inbound, replayMessage(m))
		}
		for _, tc := range t.Response.ToolCalls {
			rt.ToolCalls = append(rt.ToolCalls, replayToolCall(tc))
		}
		prevLen = len(msgs)
		totalIn += rt.InputTokens
		totalOut += rt.OutputTokens
		out = append(out, rt)
	}
	return system, out, totalIn, totalOut
}

// replayMessage projects a canonical message into the view's flattened form, keeping the
// query layer the single place the model types are decoded (the views stay free of the
// model package).
func replayMessage(m model.Message) ReplayMessage {
	rm := ReplayMessage{Role: string(m.Role), Text: m.Text}
	for _, tr := range m.ToolResults {
		rm.ToolResults = append(rm.ToolResults, ReplayToolResult{
			ToolCallID: tr.ToolCallID,
			Content:    tr.Content,
			IsError:    tr.IsError,
		})
	}
	return rm
}

// replayToolCall projects a tool call, pretty-printing its raw-JSON arguments for the trail.
func replayToolCall(tc model.ToolCall) ReplayToolCall {
	return ReplayToolCall{ID: tc.ID, Name: tc.Name, Args: prettyJSON(tc.Args)}
}

// prettyJSON indents a raw JSON value for legible display, falling back to the raw bytes
// when they are not valid JSON (an agent can emit malformed arguments — the trail shows them
// verbatim rather than hiding the fact).
func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}
