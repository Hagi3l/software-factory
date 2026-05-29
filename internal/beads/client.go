package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

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

// issueJSON is the subset of bd's --json issue object the harness consumes. bd
// emits many more fields (priority, owner, timestamps, counts); only the facets an
// agent is handed are decoded. Metadata is decoded as raw values so a non-string
// entry written by another tool cannot fail the whole read.
type issueJSON struct {
	ID          string                     `json:"id"`
	Title       string                     `json:"title"`
	Description string                     `json:"description"`
	Status      string                     `json:"status"`
	Labels      []string                   `json:"labels"`
	Metadata    map[string]json.RawMessage `json:"metadata"`
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
		TraceMap: metaString(r.Metadata, MetadataKeyTraceMap),
		Tags:     parseLabels(r.Labels),
	}
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
	cmd := exec.CommandContext(ctx, c.bin, args...)
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
