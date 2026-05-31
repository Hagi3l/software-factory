package wizard

import (
	"context"
	"fmt"
	"strings"

	"github.com/Loxstomper/harness/internal/controlroom/live"
)

// ResolveSeed is the grounding the control room hands the wizard when a human opens Resolve
// mode on a dead-lettered issue (specs/control-room.md "Create and Resolve are the same
// component"). The composition root assembles it from the read model (query.ResolveContext),
// so the wizard package stays pure (model + live only): it receives already-rendered strings,
// not stores. Every field is optional — a thin escalation still opens a usable session.
type ResolveSeed struct {
	IssueID   string // the dead-lettered issue this session is unsticking
	Title     string // its title, for the grounding preamble
	Role      string // the stage that dead-lettered
	Spec      string // the governing spec path the human will refine
	Reason    string // the orchestrator's escalation reason (why it is stuck)
	SpecSlice string // the current resolved spec slice the human refines (bounded by spec_depth)
}

// resolveOpening is the first user turn NewResolve sends so the planner *opens* already engaged
// with the escalation — the "pre-loaded conversation" Resolve mode promises — rather than
// waiting on a blank prompt. The heavy grounding (escalation + spec slice) rides in the system
// prompt (resolveContext), so this stays a short, natural instruction.
const resolveOpening = "Let's resolve this escalation. Review the escalation and the current spec slice above, then help me refine the spec to remove the ambiguity that blocked the work. Propose the minimal spec edit."

// NewResolve mints a session pre-grounded in a dead-lettered issue and opens the conversation.
// Unlike New (a blank Create session) it folds the escalation + spec slice into the session's
// system prompt (so the planner reasons with that context without it cluttering the visible
// transcript), records the issue id the resolve consent gate will commit against, and kicks off
// an opening turn so the planner immediately analyses the escalation and proposes a direction.
// It obeys the same session-eviction discipline as New (via register).
func (p *Planner) NewResolve(seed ResolveSeed) *Session {
	s := p.register(&Session{
		ID:          newID(),
		hub:         live.NewHub(),
		adapter:     p.adapter,
		persona:     p.persona + "\n\n" + resolveContext(seed),
		maxTokens:   p.maxTokens,
		turnTimeout: p.turnTimeout,
		log:         p.log,
		issueID:     seed.IssueID,
	})
	// Open the conversation: the planner reads the grounding (system prompt) and the opener and
	// replies with its first analysis. Send is a no-op if the opener is blank (it never is here).
	s.Send(resolveOpening)
	return s
}

// resolveContext renders the Resolve grounding appended to the planner's persona: the escalation
// it is unsticking and the current spec slice it will refine. It is the system-channel context
// (not a visible message), so the transcript stays the human↔planner conversation while the
// planner still reasons against the full escalation. The agent's raw transcript is referenced
// (the human reads it via the control room), not inlined, to keep the prompt bounded.
func resolveContext(seed ResolveSeed) string {
	var b strings.Builder
	b.WriteString("RESOLVE MODE — a dead-lettered work item needs the spec refined to unstick it.\n")
	b.WriteString("Per the human re-entry invariant, stuck work is resolved by refining the spec, never by editing code.\n\n")
	b.WriteString("Escalation:\n")
	if seed.IssueID != "" || seed.Title != "" {
		fmt.Fprintf(&b, "- Issue: %s — %s\n", seed.IssueID, seed.Title)
	}
	if seed.Role != "" {
		fmt.Fprintf(&b, "- Stage: %s\n", seed.Role)
	}
	if seed.Reason != "" {
		fmt.Fprintf(&b, "- Why it is stuck: %s\n", seed.Reason)
	}
	if seed.Spec != "" {
		fmt.Fprintf(&b, "\nGoverning spec (%s):\n", seed.Spec)
		if seed.SpecSlice != "" {
			b.WriteString("```\n")
			b.WriteString(seed.SpecSlice)
			if !strings.HasSuffix(seed.SpecSlice, "\n") {
				b.WriteByte('\n')
			}
			b.WriteString("```\n")
		}
	}
	b.WriteString("\nHelp the human refine the spec slice above to remove the ambiguity/gap, then draft the " +
		"spec edit (the ```draft block) and maintain the alignment ledger as usual. Keep the edit minimal — " +
		"change only what resolves the escalation. Do not invent new scope. The same APPROVE consent gate " +
		"then commits the spec; the factory re-pins and reissues the affected work automatically.")
	return b.String()
}

// Resolver is the Resolve-mode consent-gate seam, the sibling of Seeder (T4.15). On APPROVE in
// Resolve mode the composition root's implementation commits the refined spec, stores the
// conversation provenance, returns the dead-lettered issue to the ready pool so it is
// re-dispatched against the now-clarified spec, and creates any new seed issues the draft added.
// Like Seeder it is the one boundary the wizard package does not implement itself — the wizard
// stays a pure conversation engine; the composition root owns git/beads/artifact writes.
type Resolver interface {
	Resolve(ctx context.Context, req ResolveRequest) (ResolveResult, error)
}

// ResolveRequest is the server-side draft handed to the Resolver on APPROVE in Resolve mode. It
// is the same shape as SeedRequest plus the IssueID to reopen — the spec edits, the agreed
// decisions, and the transcript are committed as provenance exactly as in Create, and IssueID
// names the dead-lettered issue the resolution unsticks. Like the Create gate, the server
// supplies the planner's latest server-side draft, never browser content.
type ResolveRequest struct {
	IssueID    string
	Summary    string
	Specs      []DraftSpec
	Decisions  []DecisionRecord
	Transcript []byte
}

// ResolveResult reports the outcome of a Resolve commit: the spec commit, the stored transcript,
// and the dead-lettered issue returned to the ready pool (ReopenedIssue, empty if it was not
// blocked or could not be reopened). Resolve refines the spec and unsticks the issue; the
// orchestrator's recompile sweep re-pins and reissues the rest of the affected work, and new
// scope is authored through Create — so Resolve creates no new seed issues itself.
type ResolveResult struct {
	Commit        string
	TranscriptRef string
	ReopenedIssue string
}
