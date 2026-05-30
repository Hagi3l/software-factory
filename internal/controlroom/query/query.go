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
	"time"

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
	DiffByIssue(ctx context.Context, issueID string) (string, bool, error)
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

// DeadLetter is the dead-letter queue's projection of a blocked issue — one escalation a
// human must triage, the control room's action surface. Beyond the board card's identity
// fields it surfaces the cumulative budget burn (SpentTokens/SpentUSD) and the retry
// generation (Attempt), because the two non-escalation dead-letter causes are exactly a
// budget breach and an exhausted retry cap (specs/workflow.md) — so a glance at spend and
// attempt tells the human *why* the work is stuck without opening it. Spec is the path the
// human refines to resolve it (the human re-entry invariant: stuck work is fixed by
// refining the spec, never by editing code — specs/specs-process.md). The full forensic
// trail (transcript, gate evidence, provenance) lives on the issue-detail page (T4.7) each
// entry links into. The dead-letter *reason* is deliberately not synthesized here: it is
// not recorded as a first-class field on the issue (the orchestrator only flips the status
// to blocked), so inferring it would mean guessing against policy caps — the honest move is
// to show the evidence (spend, attempt, spec) and let the detail page carry the rest.
type DeadLetter struct {
	ID          string
	Title       string
	Role        string
	Spec        string
	Attempt     int
	SpentTokens int
	SpentUSD    float64
}

// DeadLetters returns the blocked issues — the escalations awaiting a human, the control
// room's primary action surface (specs/control-room.md, specs/workflow.md). They are the
// blocked beads status: work the orchestrator dead-lettered on a budget breach, an
// exhausted retry cap, or a needs-spec-clarification escalation. Ordered by id for a
// stable render.
func (r *Reader) DeadLetters(ctx context.Context) ([]DeadLetter, error) {
	issues, err := r.issues.List(ctx, "blocked")
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
		})
	}
	sort.Slice(dls, func(a, b int) bool { return dls[a].ID < dls[b].ID })
	return dls, nil
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
		tokens := i.SpentTokens + i.ClosingTokens
		usd := i.SpentUSD + i.ClosingUSD
		out.Issues = append(out.Issues, IssueBudgetRow{
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
		})
	}
	sort.Slice(out.Issues, func(x, y int) bool { return issueBudgetLess(out.Issues[x], out.Issues[y]) })
	return out, nil
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
