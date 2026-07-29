package wizard

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/Loxstomper/software-factory/internal/model"
)

// The draft (T4.14, specs/control-room.md "The wizard") is the second structured artifact
// the requirements planner maintains alongside the conversation and the alignment ledger:
// the concrete spec markdown + seed issues it proposes to write once intent has converged.
// Like the ledger it is a latest-wins snapshot the planner re-emits each turn as the
// schema-validated arguments of a `propose_draft` tool call (T4.29); the engine harvests it
// (parseDraftArgs), stores it on the Session, and the view renders it with an APPROVE button.
// Nothing here is written
// anywhere — the draft is purely what *would* be created. The consent boundary is the human
// clicking APPROVE: only then does the trusted Seeder (implemented by the composition root,
// where git/beads/the artifact store are in scope) commit the spec to git, write the
// decisions sidecar, store the conversation transcript, and create the seed issues through
// the single-writer beads path. That ordering — propose, review, then APPROVE — is exactly
// the consent gate the spec requires: "the human reviews the drafted spec + the seed issues
// and approves before anything is written."
type Draft struct {
	// Summary is a one-line description of the whole change — the seed epic's headline. It
	// becomes the commit subject and the decisions-sidecar title.
	Summary string
	// Specs are the spec markdown files to author (new files or replacements). Their content
	// is the planner's drafted prose; the wizard owns link-integrity across them (every
	// cross-link resolves; every spec is referenced by at least one seed issue).
	Specs []DraftSpec
	// Issues are the seed work items to create at the pipeline entry stage. Each carries the
	// structured spec reference (Spec) the orchestrator resolves a Brief slice from.
	Issues []DraftIssue
}

// DraftSpec is one spec file the planner proposes to write: a repo-relative markdown path
// and its full content. Content is preserved verbatim (not trimmed) so the markdown's
// formatting survives to disk.
type DraftSpec struct {
	Path    string
	Content string
}

// DraftIssue is one seed work item the planner proposes. Role is the pipeline entry stage it
// enters at (empty defaults to the DAG entry role on approval); Spec is its structured spec
// reference; Key/DependsOn express inter-sibling blocked-by edges for a batch the way
// core.Proposal does (resolved by beads.Apply when the issues are created).
type DraftIssue struct {
	Title     string
	Body      string
	Role      string
	Spec      string
	Key       string
	DependsOn []string
}

// Empty reports whether the draft proposes nothing — no spec files and no seed issues. The
// view shows a muted invitation in that state, and APPROVE refuses it (there is nothing to
// consent to).
func (d Draft) Empty() bool { return len(d.Specs) == 0 && len(d.Issues) == 0 }

func (d Draft) clone() Draft {
	out := Draft{Summary: d.Summary}
	out.Specs = slices.Clone(d.Specs)
	out.Issues = make([]DraftIssue, len(d.Issues))
	for i, is := range d.Issues {
		is.DependsOn = slices.Clone(is.DependsOn)
		out.Issues[i] = is
	}
	return out
}

// toolProposeDraft is the name of the output tool the planner calls to propose the draft (the
// spec files + seed issues) as schema-validated arguments (T4.29).
const toolProposeDraft = "propose_draft"

// draftWire is the JSON wire shape of the propose_draft call's arguments — kept separate from the
// rendered Draft so the presentation types carry no json concerns (mirrors ledgerWire).
type draftWire struct {
	Summary string `json:"summary"`
	Specs   []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	} `json:"specs"`
	Issues []struct {
		Title     string   `json:"title"`
		Body      string   `json:"body"`
		Role      string   `json:"role"`
		Spec      string   `json:"spec"`
		Key       string   `json:"key"`
		DependsOn []string `json:"depends_on"`
	} `json:"issues"`
}

// proposeDraftToolDef is the output-tool definition the planner calls to propose the draft. Like
// update_ledger it is a pure-output tool — it records the proposal and commits nothing (the
// human's Approve is the consent gate) and never triggers a model round-trip. Spec content is one
// JSON string (markdown with `\n`-escaped newlines), which the tool channel carries cleanly — the
// fenced-block era's "newline inside a JSON string breaks the parse" footgun is gone.
func proposeDraftToolDef() model.ToolDef {
	return model.ToolDef{
		Name: toolProposeDraft,
		Description: "Propose the concrete deliverable — the spec markdown files and the seed work items — for the human to Approve. " +
			"Call only once intent has genuinely converged; re-emit the complete draft each turn it changes (latest wins). " +
			"This records the proposal only: it commits nothing (Approve is the human's consent gate) and does not end the conversation.",
		Params: json.RawMessage(`{
			"type": "object",
			"properties": {
				"summary": {"type": "string", "description": "One-line description of the whole change (becomes the commit subject)."},
				"specs": {
					"type": "array",
					"description": "The spec markdown files to author. Each spec must be referenced by at least one issue.",
					"items": {
						"type": "object",
						"properties": {
							"path": {"type": "string", "description": "Repo-relative path under specs/, a .md file, e.g. \"specs/orders-export.md\"."},
							"content": {"type": "string", "description": "The complete spec markdown as a single string (escape newlines as \\n)."}
						},
						"required": ["path", "content"]
					}
				},
				"issues": {
					"type": "array",
					"description": "The seed work items, kept coarse (one per coherent deliverable — the autonomous planner decomposes them).",
					"items": {
						"type": "object",
						"properties": {
							"title": {"type": "string"},
							"body": {"type": "string", "description": "What to build, in prose."},
							"role": {"type": "string", "description": "Pipeline entry stage; omit for the default entry role."},
							"spec": {"type": "string", "description": "The spec path this issue implements."},
							"key": {"type": "string", "description": "A local key other issues reference in depends_on."},
							"depends_on": {"type": "array", "items": {"type": "string"}, "description": "Keys of sibling issues that must land first."}
						},
						"required": ["title"]
					}
				}
			},
			"required": ["summary", "specs", "issues"]
		}`),
	}
}

// parseDraftArgs decodes a propose_draft call's arguments into a Draft. Like parseLedgerArgs it
// degrades to an error (never a panic): a payload that does not decode, or one proposing neither a
// spec nor an issue, returns an error the caller acks/logs rather than clobbering a prior draft.
// Spec content is preserved verbatim; everything else is trimmed, and an entry missing its
// essential field (a spec with no path/content, an issue with no title) is dropped.
func parseDraftArgs(args json.RawMessage) (Draft, error) {
	var w draftWire
	if err := json.Unmarshal(args, &w); err != nil {
		return Draft{}, err
	}
	d := draftFromWire(w)
	if d.Empty() {
		return Draft{}, errors.New("draft proposes neither a spec file nor a seed issue")
	}
	return d, nil
}

// draftFromWire normalizes a decoded draftWire into a rendered Draft: trims fields (spec content
// verbatim) and drops a spec with no path/content or an issue with no title.
func draftFromWire(w draftWire) Draft {
	d := Draft{Summary: strings.TrimSpace(w.Summary)}
	for _, s := range w.Specs {
		path := strings.TrimSpace(s.Path)
		if path == "" || strings.TrimSpace(s.Content) == "" {
			continue // a spec needs both a path and content to be written
		}
		d.Specs = append(d.Specs, DraftSpec{Path: path, Content: s.Content})
	}
	for _, is := range w.Issues {
		title := strings.TrimSpace(is.Title)
		if title == "" {
			continue // an issue with no title cannot be created
		}
		var deps []string
		for _, dep := range is.DependsOn {
			if d := strings.TrimSpace(dep); d != "" {
				deps = append(deps, d)
			}
		}
		d.Issues = append(d.Issues, DraftIssue{
			Title:     title,
			Body:      strings.TrimSpace(is.Body),
			Role:      strings.TrimSpace(is.Role),
			Spec:      strings.TrimSpace(is.Spec),
			Key:       strings.TrimSpace(is.Key),
			DependsOn: deps,
		})
	}
	return d
}

// DecisionRecord is one finalized decision destined for the git decisions sidecar — a settled
// point and its one-line rationale. It is derived from the resolved items of the alignment
// ledger (FinalizedDecisions), keeping the ledger the single source of the "why" rather than
// asking the planner to re-state decisions in a second place. Deferred marks a fork that was
// knowingly left open rather than decided (T4.27): the sidecar records it as "deliberately left
// open" — pre-context for the needs-spec-clarification escalation a defer may later raise.
type DecisionRecord struct {
	Point     string
	Rationale string
	Deferred  bool
}

// FinalizedDecisions projects the resolved items of an alignment ledger into the decisions
// sidecar's bullet list (T4.13/T4.27): each `agreed` point (with the chosen fork option folded
// into the point text) as a settled decision, and each `deferred` point as an explicitly-recorded
// open item (Deferred=true) — so the sidecar carries both what was decided *and* what was
// knowingly left for later. Still-`open`/`discussing` items are excluded — only resolved forks
// are "finalized" (the approval gate, ApprovalDecisions, auto-defers plain opens before this runs
// so an approved ledger has none left). This is what makes the ledger, not a parallel block, the
// source of the sidecar.
func FinalizedDecisions(items []LedgerItem) []DecisionRecord {
	var out []DecisionRecord
	for _, it := range items {
		switch it.Status {
		case ledgerStatusAgreed:
			point := it.Question
			for _, o := range it.Options {
				if o.Selected {
					point = it.Question + " → " + o.Label
					break
				}
			}
			out = append(out, DecisionRecord{Point: point, Rationale: it.Rationale})
		case ledgerStatusDeferred:
			// A deferred fork is recorded by its question alone — it was *not* decided, so no
			// option is folded in. The rationale (if any) explains why it was left open.
			out = append(out, DecisionRecord{Point: it.Question, Rationale: it.Rationale, Deferred: true})
		}
	}
	return out
}

// SeedRequest is the fully-assembled, human-approved unit of intent the wizard hands to the
// Seeder on APPROVE: the drafted spec files, the seed issues, the finalized decisions for the
// sidecar, and the conversation transcript to retain as provenance. The server builds it from
// the session's latest draft + ledger + transcript at the moment of consent — never from
// browser-supplied content, so the human approves exactly the trusted planner's last draft.
type SeedRequest struct {
	Summary    string
	Specs      []DraftSpec
	Issues     []DraftIssue
	Decisions  []DecisionRecord
	Transcript []byte
}

// SeedResult is what the Seeder reports back after committing an approved draft: the spec
// commit, the artifact-store hash the transcript was stored under, and the created seed
// issues (with their assigned ids) so the view can link straight to them.
type SeedResult struct {
	Commit        string
	TranscriptRef string
	Issues        []SeededIssue
}

// SeededIssue is one created seed issue, flattened for the view (no core import in the
// presentation path).
type SeededIssue struct {
	ID    string
	Title string
	Role  string
}

// Seeder commits an approved draft. It is the consent-gated write seam: it writes the spec
// files to git (keeping link-integrity and the README index consistent), writes the decisions
// sidecar, stores the conversation transcript in the artifact store, and creates the seed
// issues through the orchestrator's single-writer beads path (validated, never written
// directly). It is implemented by the composition root — the wizard package deliberately does
// not import git, beads, the artifact store, or config, so the conversation engine stays a
// self-contained unit; the Seeder is the one interface across that boundary. nil (a standalone
// `software-factory serve`, or a control room with no repo to write) disables APPROVE.
type Seeder interface {
	Seed(ctx context.Context, req SeedRequest) (SeedResult, error)
}
