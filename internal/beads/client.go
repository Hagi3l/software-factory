package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Loxstomper/harness/internal/core"
)

// MetadataKeyRole is the beads metadata key under which an issue's harness role
// (its DAG stage, e.g. "implement") is stored. Role lives in metadata, not in
// labels: labels are the issue's tags, matched against a soul's selector to pick
// which soul fulfills a role (see specs/configuration.md), whereas the role is the
// stage binding that decides the work subject an issue is dispatched to. Keeping
// them separate keeps tag-based soul selection from colliding with stage routing.
// The orchestrator's single-writer transitions (T1.4) write this key; the read
// path here is the only thing that interprets it.
const MetadataKeyRole = "role"

// Client is the read interface to the beads (bd) work-item store. It shells out to
// the bd CLI rather than linking a library because bd is the canonical store and
// owns its own database/versioning; funneling every access through this one package
// is what makes the single-writer invariant enforceable (see
// specs/components/orchestrator.md). This type currently reads; the orchestrator's
// single-writer transitions are added to it in T1.4.
type Client struct {
	bin string
	dir string
	run runFunc
}

// runFunc executes a bd subcommand and returns its stdout. It is a seam so the
// decode/mapping logic can be exercised against canned output and error paths in
// unit tests; the default implementation execs the real bd binary.
type runFunc func(ctx context.Context, args []string) ([]byte, error)

// Option configures a Client.
type Option func(*Client)

// WithBinary sets the bd executable to invoke (default "bd", resolved on PATH).
func WithBinary(path string) Option { return func(c *Client) { c.bin = path } }

// WithDir sets the working directory bd runs in, which is how it auto-discovers the
// .beads database. Defaults to the process working directory.
func WithDir(dir string) Option { return func(c *Client) { c.dir = dir } }

// New builds a Client. With no options it invokes "bd" from the current directory.
func New(opts ...Option) *Client {
	c := &Client{bin: "bd"}
	for _, o := range opts {
		o(c)
	}
	if c.run == nil {
		c.run = c.execRun
	}
	return c
}

// Ready returns the issues that are claimable now: open, with no active blocker
// (bd's ready semantics apply the blocked-by precondition for us). Predicate
// preconditions beyond blocker closure are evaluated by the orchestrator in a
// sandbox (T1.19), not here. The default limit is dropped (--limit 0) so the
// orchestrator sees the whole ready set, not just the first page.
func (c *Client) Ready(ctx context.Context) ([]core.Issue, error) {
	out, err := c.run(ctx, []string{"ready", "--json", "--limit", "0"})
	if err != nil {
		return nil, fmt.Errorf("beads: query ready work: %w", err)
	}
	return decodeIssues(out)
}

// Get reads a single issue's fields by ID.
func (c *Client) Get(ctx context.Context, id string) (core.Issue, error) {
	if id == "" {
		return core.Issue{}, fmt.Errorf("beads: empty issue id")
	}
	out, err := c.run(ctx, []string{"show", id, "--json"})
	if err != nil {
		return core.Issue{}, fmt.Errorf("beads: show issue %s: %w", id, err)
	}
	issues, err := decodeIssues(out)
	if err != nil {
		return core.Issue{}, err
	}
	if len(issues) == 0 {
		return core.Issue{}, fmt.Errorf("beads: issue %s not found", id)
	}
	return issues[0], nil
}

// InProgress returns every issue currently in_progress, fully decoded. It is the input to
// the orchestrator's spec-drift sweep (recompileSpecDelta): an in-flight issue carries the
// content hash of the spec slice it was briefed against (SpecHash), which the sweep
// re-resolves and compares to detect work made stale by a spec edit (see
// specs/specs-process.md). ListStranded reads the same in_progress set but for lease expiry
// and needs only IDs plus the lease metadata; this returns whole core.Issue values because
// the drift comparison needs Spec, SpecHash, and Role. The default limit is dropped so the
// whole in-flight set is seen, not just the first page.
func (c *Client) InProgress(ctx context.Context) ([]core.Issue, error) {
	out, err := c.run(ctx, []string{"list", "--status", "in_progress", "--json", "--limit", "0"})
	if err != nil {
		return nil, fmt.Errorf("beads: list in_progress: %w", err)
	}
	return decodeIssues(out)
}

// List returns every issue in the given beads status, fully decoded. status is one of
// bd's stored statuses (open, in_progress, blocked, closed) and may be a comma-separated
// set (e.g. "open,in_progress"). Unlike Ready it applies no blocker precondition, so it is
// the read the control room uses to populate the board and the dead-letter queue (blocked)
// across the whole status — not just dispatchable work (see specs/control-room.md). --flat
// forces a flat JSON array (the tree layout is for human terminals); --limit 0 drops bd's
// default page cap so the whole set is returned.
func (c *Client) List(ctx context.Context, status string) ([]core.Issue, error) {
	if status == "" {
		return nil, fmt.Errorf("beads: empty status")
	}
	out, err := c.run(ctx, []string{"list", "--status", status, "--json", "--flat", "--limit", "0"})
	if err != nil {
		return nil, fmt.Errorf("beads: list status %s: %w", status, err)
	}
	return decodeIssues(out)
}

// ListAll returns every issue regardless of status, including closed ones (bd hides closed
// issues from `list` by default; --all overrides that). It backs the control room's board
// and provenance views, which must show completed work, not only what is in flight. The
// --flat/--limit 0 flags mean the same as in List.
func (c *Client) ListAll(ctx context.Context) ([]core.Issue, error) {
	out, err := c.run(ctx, []string{"list", "--all", "--json", "--flat", "--limit", "0"})
	if err != nil {
		return nil, fmt.Errorf("beads: list all: %w", err)
	}
	return decodeIssues(out)
}

// issueJSON is the subset of bd's --json issue object the harness consumes. bd
// emits many more fields (priority, owner, timestamps, counts); only the facets an
// agent is handed are decoded. Metadata is decoded as raw values so a non-string
// entry written by another tool cannot fail the whole read.
type issueJSON struct {
	ID           string                     `json:"id"`
	Title        string                     `json:"title"`
	Description  string                     `json:"description"`
	Status       string                     `json:"status"`
	Labels       []string                   `json:"labels"`
	Metadata     map[string]json.RawMessage `json:"metadata"`
	Dependencies []depJSON                  `json:"dependencies"`
	CreatedAt    string                     `json:"created_at"`
}

// depJSON is one dependency edge bd emits inline on `bd list --json` (the `dependencies`
// array). Only depends_on_id is consumed — the id of the issue this one is blocked by; the
// edge's own issue_id is the enclosing record's id, and the type (e.g. "blocks") is not
// needed for the DAG read. It is decoded into core.Issue.DependsOn (blocked-by targets),
// the read-side edge source for the control room's dependency graph (T4.6), so the harness
// need not issue a separate `bd dep` query.
type depJSON struct {
	DependsOnID string `json:"depends_on_id"`
}

func (r issueJSON) toCore() core.Issue {
	return core.Issue{
		ID:       r.ID,
		Title:    r.Title,
		Body:     r.Description,
		Role:     metaString(r.Metadata, MetadataKeyRole),
		Status:   r.Status,
		Attempt:  metaInt(r.Metadata, MetadataKeyRetries),
		Base:     metaString(r.Metadata, MetadataKeyBase),
		Spec:     metaString(r.Metadata, MetadataKeySpec),
		SpecHash: metaString(r.Metadata, MetadataKeySpecHash),
		TraceMap: metaString(r.Metadata, MetadataKeyTraceMap),
		// Cumulative spend of the on_failure retry chain so far, threaded forward by the
		// orchestrator (the create() write path stamps them when nonzero). Absent metadata
		// decodes back to 0 — a fresh or next-stage issue carries no spend.
		SpentTokens: metaInt(r.Metadata, MetadataKeySpentTokens),
		SpentUSD:    metaFloat(r.Metadata, MetadataKeySpentUSD),
		// Cumulative wall of the on_failure chain (a duration string), and the epic root id,
		// both threaded forward by the orchestrator like the token/dollar spend and Base.
		SpentWall: metaDuration(r.Metadata, MetadataKeySpentWall),
		EpicID:    metaString(r.Metadata, MetadataKeyEpicID),
		// This issue's OWN invocation marginal, stamped post-hoc by StampClosingSpend; summed
		// across an epic for the aggregate epic-budget read. Absent decodes to 0.
		ClosingTokens: metaInt(r.Metadata, MetadataKeyClosingTokens),
		ClosingUSD:    metaFloat(r.Metadata, MetadataKeyClosingUSD),
		// Approval-gate state (T2.10), written by AwaitApproval when an integrate is parked
		// for human approval; absent (empty) on every issue that is not parked.
		CandidateRef:     metaString(r.Metadata, MetadataKeyCandidateRef),
		ParkedProvenance: metaString(r.Metadata, MetadataKeyParkedProv),
		// Most-recent invocation transcript hash (stamped post-hoc by StampTranscript) and the
		// dead-letter reason (stamped by Block when the issue is dead-lettered); both empty until
		// the orchestrator processes a Result / blocks the issue. They make the decision trail and
		// the escalation reachable from the issue for the Resolve wizard (T4.15).
		Transcript:       metaString(r.Metadata, MetadataKeyTranscript),
		DeadLetterReason: metaString(r.Metadata, MetadataKeyDLQReason),
		// The producing souls of the author-tests/implement stages (threaded forward like
		// TraceMap) and the hash of this issue's gate-verdict record (stamped post-hoc per gate
		// run). Both empty until the relevant stage has run; they make the producer≠verifier
		// split and the gate verdict renderable for in-flight and dead-lettered work (T4.22).
		TestsSoul:     metaString(r.Metadata, MetadataKeyTestsSoul),
		ImplementSoul: metaString(r.Metadata, MetadataKeyImplementSoul),
		GateVerdict:   metaString(r.Metadata, MetadataKeyGateVerdict),
		TransformLog:  metaString(r.Metadata, MetadataKeyTransformLog),
		// When the issue last entered its current status, stamped atomically by every
		// status-changing write (setStatus/Claim); the board's time-in-state anchor. Zero
		// (absent/unparsable) on an issue that has not transitioned since the field landed.
		StateEnteredAt: metaTime(r.Metadata, MetadataKeyStateEntered),
		// The claim lease expiry an in_progress issue carries (stamped by Claim, cleared by
		// Release). The orchestrator seeds its in-flight projection from this on restart so the
		// in-memory lease sweep recovers stranded work on the original deadline (T3.13). Zero
		// (absent/unparsable) on an issue not in_progress; lenient like the other decoders.
		Lease: metaTime(r.Metadata, MetadataKeyLease),
		// Beads' own top-level created_at (not harness metadata) — the board's per-card
		// "total time" anchor (T4.18). Lenient like the metadata decoders: an absent or
		// malformed timestamp reads as the zero time, never failing the read.
		CreatedAt: parseRFC3339(r.CreatedAt),
		Tags:      parseLabels(r.Labels),
		// Blocked-by edge targets bd emits inline on the read (the `dependencies` array),
		// distinct from the write-side Proposal.DependsOn. Empty/absent decodes to nil.
		DependsOn: dependsOn(r.Dependencies),
	}
}

// dependsOn collects the non-empty depends_on_id values from bd's inline dependency edges
// into the blocked-by target list. An empty id is skipped (defensive against a malformed
// edge); no edges yields nil so an issue with no blockers carries a nil slice.
func dependsOn(deps []depJSON) []string {
	if len(deps) == 0 {
		return nil
	}
	out := make([]string, 0, len(deps))
	for _, d := range deps {
		if d.DependsOnID != "" {
			out = append(out, d.DependsOnID)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseLabels decodes beads labels into the issue's selector tags. A tag is encoded as
// one `key=value` label (the unambiguous, conventional form; bd round-trips it verbatim),
// split on the first `=`. This is the bridge from bd's flat label list to the
// map[string]string a soul's selector matches against (see core.Issue.Tags,
// core.Soul.Matches). A label with no `=` is kept as a valueless tag (key with empty
// value): it cannot satisfy a {k: v} selector but is preserved rather than dropped, and
// being lenient keeps the read path robust to labels the harness did not write. Returns
// nil for no labels so an untagged issue carries a nil map (the trivial 1:1 case).
func parseLabels(labels []string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	tags := make(map[string]string, len(labels))
	for _, l := range labels {
		k, v, _ := strings.Cut(l, "=")
		if k == "" {
			continue
		}
		tags[k] = v
	}
	if len(tags) == 0 {
		return nil
	}
	return tags
}

// metaString returns the string value of a metadata key, or "" if absent or not a
// string. Being lenient here keeps the read path robust to metadata the harness did
// not write.
func metaString(m map[string]json.RawMessage, key string) string {
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// metaInt returns the integer value of a metadata key, or 0 if absent or not a
// number. Lenient for the same reason as metaString: foreign metadata must never
// fail a read.
func metaInt(m map[string]json.RawMessage, key string) int {
	raw, ok := m[key]
	if !ok {
		return 0
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0
	}
	return n
}

// metaFloat returns the float value of a metadata key, or 0 if absent or not a
// number. It backs the cumulative USD spend (spent_usd), which is fractional; lenient
// for the same reason as metaString/metaInt — foreign metadata must never fail a read.
func metaFloat(m map[string]json.RawMessage, key string) float64 {
	raw, ok := m[key]
	if !ok {
		return 0
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0
	}
	return f
}

// metaDuration returns the time.Duration parsed from a metadata key's Go-duration string
// value (e.g. "5m0s"), or 0 if absent, not a string, or unparsable. It backs the cumulative
// wall spend (spent_wall), written by create() as core.Issue.SpentWall.String(); lenient for
// the same reason as metaString/metaInt — foreign metadata must never fail a read.
func metaDuration(m map[string]json.RawMessage, key string) time.Duration {
	raw, ok := m[key]
	if !ok {
		return 0
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}

// metaTime returns the time.Time parsed from a metadata key's RFC3339 string value, or the
// zero Time if absent, not a string, or unparsable. It backs state_entered_at (written by the
// status-changing writes via stateEnteredNow) and lease_until (written by Claim); lenient for
// the same reason as metaString — foreign or malformed metadata must never fail a read, it just
// reads as "not stamped".
func metaTime(m map[string]json.RawMessage, key string) time.Time {
	raw, ok := m[key]
	if !ok {
		return time.Time{}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return time.Time{}
	}
	return parseRFC3339(s)
}

// parseRFC3339 parses an RFC3339 timestamp string to a UTC time.Time, or the zero Time when
// empty or unparsable. It backs beads' top-level created_at (decoded as a plain JSON string,
// not metadata), and is shared with metaTime; lenient for the same reason — a malformed or
// foreign timestamp must never fail the read, it just reads as "not stamped".
func parseRFC3339(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// decodeIssues parses bd's JSON array of issues. bd emits a JSON array for ready,
// list, and show (show returns a one-element array); empty output or an empty array
// yields no issues.
func decodeIssues(data []byte) ([]core.Issue, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}
	var raw []issueJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("beads: decode issue json: %w", err)
	}
	issues := make([]core.Issue, len(raw))
	for i, r := range raw {
		issues[i] = r.toCore()
	}
	return issues, nil
}

// execRun runs the bd binary, returning stdout. bd writes its --json payload to
// stdout and advisory warnings to stderr, so only stdout is parsed; stderr is folded
// into the error message on failure to make a bd error legible.
func (c *Client) execRun(ctx context.Context, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.bin, args...) // #nosec G204 -- c.bin is the configured bd binary; args are orchestrator-built, not untrusted agent input.
	cmd.Dir = c.dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}
