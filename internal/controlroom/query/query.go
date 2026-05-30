// Package query is the control room's read model: render-ready projections over the three
// stores — beads (work state), git (provenance), and the artifact store (evidence) —
// stitched together and decoupled from the views that render them (specs/control-room.md,
// specs/observability.md). The views (T4.4+) call a Reader method and get a presentation
// struct; they never touch bd, git, or the store directly. Keeping the joins here (not in
// templ) is what lets the read shape be unit-tested without a browser and lets a view be a
// thin template over a typed value.
package query

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Loxstomper/harness/internal/core"
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
	Recent(ctx context.Context, limit int) ([]MergedCommit, error)
}

// Reader assembles the render-ready views. Its three ports map one-to-one to the three
// stores; a view method reads from whichever it needs and returns a presentation struct.
type Reader struct {
	issues IssueReader
	arts   ArtifactReader
	prov   ProvenanceReader
}

// NewReader wires the read model to its three stores.
func NewReader(issues IssueReader, arts ArtifactReader, prov ProvenanceReader) *Reader {
	return &Reader{issues: issues, arts: arts, prov: prov}
}

// IssueCard is the compact projection of an issue for the board and dead-letter lists —
// just what a card renders, not the full issue.
type IssueCard struct {
	ID      string
	Title   string
	Status  string
	Role    string
	Attempt int
	Spec    string
}

func cardOf(i core.Issue) IssueCard {
	return IssueCard{ID: i.ID, Title: i.Title, Status: i.Status, Role: i.Role, Attempt: i.Attempt, Spec: i.Spec}
}

// BoardColumn is one stage's cards on the kanban board.
type BoardColumn struct {
	Stage string
	Cards []IssueCard
}

// Board is the kanban projection: every issue grouped by stage (its role), columns in the
// pipeline order the caller supplies.
type Board struct {
	Columns []BoardColumn
	Total   int
}

// unassignedStage is the column for issues with no role set (a freshly seeded epic before
// the planner stamps stages). Named so it sorts and renders rather than vanishing.
const unassignedStage = "(unassigned)"

// Board groups every issue by stage for the kanban view. stageOrder is the pipeline order
// (e.g. the configured DAG: requirements→plan→…→integrate) so columns read left-to-right
// like the flow; any stage present in the data but absent from stageOrder is appended in
// stable alphabetical order (so an ad-hoc role still shows), with unassigned work last.
// Cards within a column are ordered by id for a stable render.
func (r *Reader) Board(ctx context.Context, stageOrder []string) (Board, error) {
	issues, err := r.issues.ListAll(ctx)
	if err != nil {
		return Board{}, fmt.Errorf("query: board: %w", err)
	}
	byStage := make(map[string][]IssueCard)
	for _, i := range issues {
		stage := i.Role
		if stage == "" {
			stage = unassignedStage
		}
		byStage[stage] = append(byStage[stage], cardOf(i))
	}

	board := Board{Total: len(issues)}
	for _, stage := range orderedStages(stageOrder, byStage) {
		cards := byStage[stage]
		sort.Slice(cards, func(a, b int) bool { return cards[a].ID < cards[b].ID })
		board.Columns = append(board.Columns, BoardColumn{Stage: stage, Cards: cards})
	}
	return board, nil
}

// orderedStages returns the stages present in byStage, ordered by stageOrder first, then
// any remaining stages alphabetically, with the unassigned column always last. A stage in
// stageOrder with no issues is skipped (no empty columns) — the board reflects the data.
func orderedStages(stageOrder []string, byStage map[string][]IssueCard) []string {
	var out []string
	seen := make(map[string]bool)
	for _, s := range stageOrder {
		if _, ok := byStage[s]; ok && !seen[s] {
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

// DeadLetters returns the blocked issues — the escalations awaiting a human, the control
// room's primary action surface (specs/control-room.md, specs/workflow.md). They are the
// blocked beads status: work the orchestrator dead-lettered on a budget breach or a
// needs-spec-clarification escalation.
func (r *Reader) DeadLetters(ctx context.Context) ([]IssueCard, error) {
	issues, err := r.issues.List(ctx, "blocked")
	if err != nil {
		return nil, fmt.Errorf("query: dead-letters: %w", err)
	}
	cards := make([]IssueCard, 0, len(issues))
	for _, i := range issues {
		cards = append(cards, cardOf(i))
	}
	sort.Slice(cards, func(a, b int) bool { return cards[a].ID < cards[b].ID })
	return cards, nil
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
// been merged to main), and the evidence artifacts derivable from that provenance, each
// resolved against the store for availability.
type IssueDetail struct {
	Issue      core.Issue
	Merged     bool
	Provenance core.Provenance
	Evidence   []ArtifactLink
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
	} else if issue.TraceMap != "" {
		detail.Evidence = []ArtifactLink{r.link(ctx, "Traceability", core.ArtifactKindTraceabilityMap, issue.TraceMap)}
	}
	return detail, nil
}

// evidenceFromProvenance turns a provenance record into resolved artifact links: the
// prompt, the traceability map (preferring the trailer's hash, falling back to the issue's
// threaded one), and each passing gate check. A check cited as a bare name (its evidence
// could not be persisted) is kept with an empty hash and Available=false so the view shows
// the check ran rather than hiding it.
func (r *Reader) evidenceFromProvenance(ctx context.Context, prov core.Provenance, issue core.Issue) []ArtifactLink {
	var links []ArtifactLink
	if prov.PromptSHA != "" {
		links = append(links, r.link(ctx, "Prompt", core.ArtifactKindPrompt, prov.PromptSHA))
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
type MergedCommit struct {
	Commit     string
	Provenance core.Provenance
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
