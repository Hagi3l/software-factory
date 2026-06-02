package beads

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Loxstomper/harness/internal/core"
)

// --- Unit tests: argument shape and control flow via the run seam ---

// recordingClient returns a Client that records every run invocation's args and
// replies via the supplied reply func (keyed off the subcommand).
func recordingClient(reply func(args []string) ([]byte, error)) (*Client, *[][]string) {
	var calls [][]string
	c := New()
	c.run = func(_ context.Context, args []string) ([]byte, error) {
		calls = append(calls, args)
		return reply(args)
	}
	return c, &calls
}

func TestClaimWritesStatusAndLease(t *testing.T) {
	c, calls := recordingClient(func([]string) ([]byte, error) { return nil, nil })
	before := time.Now().UTC()
	until, err := c.Claim(context.Background(), "harness-1", 30*time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if until.Before(before.Add(30*time.Minute)) || until.After(time.Now().UTC().Add(31*time.Minute)) {
		t.Errorf("lease until = %v, want ~now+30m", until)
	}
	got := strings.Join((*calls)[0], " ")
	if !strings.Contains(got, "update harness-1 --status in_progress") {
		t.Errorf("claim args = %q, want status in_progress", got)
	}
	if !strings.Contains(got, "--set-metadata lease_until=") {
		t.Errorf("claim args = %q, want lease_until set-metadata", got)
	}
	// Claim also stamps state_entered_at atomically (the time-in-state anchor, T4.16), like
	// every status-changing write — see MetadataKeyStateEntered.
	if !strings.Contains(got, "--set-metadata "+MetadataKeyStateEntered+"=") {
		t.Errorf("claim args = %q, want state_entered_at set-metadata", got)
	}
	// The lease value must be RFC3339 so the sweep can parse it. Find it by key rather than
	// position (state_entered_at now follows it in the arg list).
	ts := metadataArg((*calls)[0], MetadataKeyLease)
	if _, perr := time.Parse(time.RFC3339, ts); perr != nil {
		t.Errorf("lease value %q is not RFC3339: %v", ts, perr)
	}
}

// metadataArg returns the value of the first `--set-metadata key=value` pair in a recorded bd
// call, or "" if absent — a position-independent helper now that several keys ride on one write.
func metadataArg(args []string, key string) string {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, key+"="); ok {
			return v
		}
	}
	return ""
}

func TestClaimRejectsBadInput(t *testing.T) {
	c, calls := recordingClient(func([]string) ([]byte, error) { return nil, nil })
	if _, err := c.Claim(context.Background(), "", time.Minute); err == nil {
		t.Error("Claim accepted empty id")
	}
	if _, err := c.Claim(context.Background(), "x", 0); err == nil {
		t.Error("Claim accepted zero ttl")
	}
	if len(*calls) != 0 {
		t.Errorf("Claim shelled out despite bad input: %v", *calls)
	}
}

func TestStatusTransitions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		call    func(c *Client) error
		wantSub string
	}{
		{"close", func(c *Client) error { return c.Close(context.Background(), "i") }, "update i --status closed"},
		{"block", func(c *Client) error { return c.Block(context.Background(), "i", "") }, "update i --status blocked"},
		{"block-reason", func(c *Client) error { return c.Block(context.Background(), "i", "agent escalated") }, "update i --status blocked --set-metadata dlq_reason=agent escalated"},
		{"release", func(c *Client) error { return c.Release(context.Background(), "i") }, "update i --status open --unset-metadata lease_until"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, calls := recordingClient(func([]string) ([]byte, error) { return nil, nil })
			if err := tc.call(c); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			// Every status-changing write now also stamps state_entered_at (T4.16, the
			// time-in-state anchor), appended after the status/extra args — so assert the
			// status prefix is intact and the stamp is present, rather than exact equality.
			got := strings.Join((*calls)[0], " ")
			if !strings.HasPrefix(got, tc.wantSub) {
				t.Errorf("args = %q, want prefix %q", got, tc.wantSub)
			}
			stamp := metadataArg((*calls)[0], MetadataKeyStateEntered)
			if stamp == "" {
				t.Errorf("args = %q, want a state_entered_at stamp", got)
			} else if _, perr := time.Parse(time.RFC3339, stamp); perr != nil {
				t.Errorf("state_entered_at %q is not RFC3339: %v", stamp, perr)
			}
		})
	}
}

// TestStateEnteredRoundTrip pins the read side of the time-in-state anchor (T4.16): a status
// write stamps state_entered_at as RFC3339, and a subsequent read decodes it back into
// core.Issue.StateEnteredAt via metaTime (UTC, second-resolution). It uses a recording client
// to capture what Close stamped, then feeds that metadata through the issue decoder. A real bd
// round-trip is covered by the broader beads integration test; here the point is the
// decode-from-metadata contract the control-room board reads.
func TestStateEnteredRoundTrip(t *testing.T) {
	c, calls := recordingClient(func([]string) ([]byte, error) { return nil, nil })
	if err := c.Close(context.Background(), "i"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	stamp := metadataArg((*calls)[0], MetadataKeyStateEntered)
	if stamp == "" {
		t.Fatal("Close did not stamp state_entered_at")
	}
	meta := map[string]json.RawMessage{
		MetadataKeyStateEntered: json.RawMessage(`"` + stamp + `"`),
	}
	got := metaTime(meta, MetadataKeyStateEntered)
	if got.IsZero() {
		t.Fatalf("metaTime decoded %q to zero", stamp)
	}
	want, _ := time.Parse(time.RFC3339, stamp)
	if !got.Equal(want) {
		t.Errorf("decoded state_entered_at = %v, want %v", got, want)
	}
	// Absent / malformed metadata must decode to the zero time, never fail the read.
	if !metaTime(map[string]json.RawMessage{}, MetadataKeyStateEntered).IsZero() {
		t.Error("metaTime on absent key should be zero")
	}
	if !metaTime(map[string]json.RawMessage{MetadataKeyStateEntered: json.RawMessage(`"not-a-time"`)}, MetadataKeyStateEntered).IsZero() {
		t.Error("metaTime on malformed value should be zero")
	}
}

func TestStatusRejectsEmptyID(t *testing.T) {
	c, _ := recordingClient(func([]string) ([]byte, error) { return nil, nil })
	if err := c.Close(context.Background(), ""); err == nil {
		t.Error("Close accepted empty id")
	}
}

// TestApplyCreatesIssueWithLabels pins the write side of the tag<->label encoding: an
// issue's selector Tags are emitted as bd's comma-separated key=value --labels, sorted by
// key for a deterministic encoding (parseLabels is the inverse on the read path).
func TestApplyCreatesIssueWithLabels(t *testing.T) {
	c, calls := recordingClient(func(args []string) ([]byte, error) {
		if args[0] == "create" {
			return []byte(`{"id":"new-1"}`), nil
		}
		return nil, nil
	})
	proposals := []core.Proposal{
		{Issue: core.Issue{Title: "do it", Role: "implement", Tags: map[string]string{"tier": "high", "lang": "go"}}},
	}
	if _, err := c.Apply(context.Background(), proposals); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	create := strings.Join((*calls)[0], " ")
	if !strings.Contains(create, "--labels lang=go,tier=high") {
		t.Errorf("create args = %q, want sorted --labels lang=go,tier=high", create)
	}
}

// An issue with no tags emits no --labels flag, so an untagged issue's create stays clean.
func TestApplyCreatesIssueWithoutLabelsWhenUntagged(t *testing.T) {
	c, calls := recordingClient(func(args []string) ([]byte, error) {
		if args[0] == "create" {
			return []byte(`{"id":"new-1"}`), nil
		}
		return nil, nil
	})
	if _, err := c.Apply(context.Background(), []core.Proposal{{Issue: core.Issue{Title: "do it", Role: "implement"}}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if create := strings.Join((*calls)[0], " "); strings.Contains(create, "--labels") {
		t.Errorf("create args = %q, want no --labels for an untagged issue", create)
	}
}

func TestApplyCreatesIssuesAndEdges(t *testing.T) {
	// create returns a distinct id per call; the existence probe (show) finds the
	// external dependency; dep add succeeds.
	n := 0
	c, calls := recordingClient(func(args []string) ([]byte, error) {
		switch args[0] {
		case "create":
			n++
			return []byte(`{"id":"new-` + string(rune('0'+n)) + `"}`), nil
		case "show":
			return []byte(`[{"id":"existing-1","status":"open"}]`), nil
		}
		return nil, nil
	})
	proposals := []core.Proposal{
		{Issue: core.Issue{Title: "fix it", Body: "details", Role: "implement"}, DependsOn: []string{"existing-1"}},
		{Issue: core.Issue{Title: "qa it", Role: "qa"}},
	}
	created, err := c.Apply(context.Background(), proposals)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(created) != 2 || created[0].ID != "new-1" || created[1].ID != "new-2" {
		t.Fatalf("created = %+v", created)
	}
	if created[0].Role != "implement" {
		t.Errorf("created[0].Role = %q", created[0].Role)
	}
	// The external dependency's existence is checked first, before any create — so an
	// illegal proposal fails fast with nothing to roll back.
	if got := strings.Join((*calls)[0], " "); got != "show existing-1 --json" {
		t.Errorf("first call = %q, want the existence probe show existing-1 --json", got)
	}
	// First create carries description + role metadata. Apply is two-phase: it creates
	// every child first (so a sibling reference can resolve to an assigned id), then adds
	// the edges — so both creates precede the dep add.
	c1 := strings.Join((*calls)[1], " ")
	if !strings.Contains(c1, "create fix it --json") || !strings.Contains(c1, "--description details") || !strings.Contains(c1, `--metadata {"role":"implement"}`) {
		t.Errorf("create args = %q", c1)
	}
	if got := (*calls)[2]; got[0] != "create" {
		t.Errorf("third call = %q, want the second create (two-phase: all creates before edges)", got)
	}
	if got := strings.Join((*calls)[3], " "); got != "dep add new-1 existing-1" {
		t.Errorf("dep args = %q, want dep add new-1 existing-1 (added after all creates)", got)
	}
}

// A child may depend on a sibling proposed in the same batch by naming the sibling's
// local Key in DependsOn; Apply resolves it to the sibling's freshly assigned id (the
// edge a decomposition planner emits, since siblings have no id when proposed). Forward
// references resolve too, because every child is created before any edge is added.
func TestApplyResolvesSiblingKey(t *testing.T) {
	n := 0
	c, calls := recordingClient(func(args []string) ([]byte, error) {
		if args[0] == "create" {
			n++
			return []byte(`{"id":"new-` + string(rune('0'+n)) + `"}`), nil
		}
		return nil, nil
	})
	// Second child depends on the first via its key; first child also forward-references
	// nothing. Order the dependent first to exercise forward resolution.
	proposals := []core.Proposal{
		{Issue: core.Issue{Title: "validate", Role: "implement"}, DependsOn: []string{"order-type"}},
		{Issue: core.Issue{Title: "order type", Role: "implement"}, Key: "order-type"},
	}
	created, err := c.Apply(context.Background(), proposals)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(created) != 2 || created[0].ID != "new-1" || created[1].ID != "new-2" {
		t.Fatalf("created = %+v", created)
	}
	// new-1 ("validate") must be blocked by new-2 ("order type"), the resolved sibling key.
	var sawEdge bool
	for _, call := range *calls {
		if strings.Join(call, " ") == "dep add new-1 new-2" {
			sawEdge = true
		}
		if call[0] == "dep" && call[len(call)-1] == "order-type" {
			t.Errorf("dep edge used the unresolved key: %v", call)
		}
		// A sibling key has no id when proposed, so it must NOT be existence-checked — it
		// is satisfied by construction once Phase 1 creates it.
		if call[0] == "show" {
			t.Errorf("sibling-key dependency must not be existence-checked: %v", call)
		}
	}
	if !sawEdge {
		t.Errorf("did not add the resolved sibling edge dep add new-1 new-2; calls = %v", *calls)
	}
}

// A reused local key in one batch is ambiguous (which sibling does a reference mean?), so
// Apply rejects it before shelling out.
func TestApplyDuplicateKeyRejected(t *testing.T) {
	c, calls := recordingClient(func([]string) ([]byte, error) { return nil, nil })
	_, err := c.Apply(context.Background(), []core.Proposal{
		{Issue: core.Issue{Title: "a", Role: "implement"}, Key: "dup"},
		{Issue: core.Issue{Title: "b", Role: "implement"}, Key: "dup"},
	})
	if err == nil {
		t.Fatal("Apply accepted a duplicate local key")
	}
	if len(*calls) != 0 {
		t.Errorf("Apply shelled out despite a duplicate key: %v", *calls)
	}
}

func TestApplyValidation(t *testing.T) {
	c, calls := recordingClient(func([]string) ([]byte, error) { return nil, nil })
	cases := []core.Proposal{
		{Issue: core.Issue{Title: "  ", Role: "implement"}},                            // empty title
		{Issue: core.Issue{Title: "t"}},                                                // empty role
		{Issue: core.Issue{Title: "t", Role: "implement"}, DependsOn: []string{""}},    // empty dep id
	}
	for i, p := range cases {
		if _, err := c.Apply(context.Background(), []core.Proposal{p}); err == nil {
			t.Errorf("case %d accepted invalid proposal %+v", i, p)
		}
	}
	if len(*calls) != 0 {
		t.Errorf("Apply shelled out despite validation failure: %v", *calls)
	}
}

// On a failed dependency edge, Apply must delete every issue it created in the call
// so the orchestrator can retry against a clean graph.
func TestApplyRollsBackOnDepFailure(t *testing.T) {
	c, calls := recordingClient(func(args []string) ([]byte, error) {
		switch args[0] {
		case "create":
			return []byte(`{"id":"new-1"}`), nil
		case "show": // the dependency exists, so the failure is the edge, not the check
			return []byte(`[{"id":"existing-1","status":"open"}]`), nil
		case "dep":
			return nil, errors.New("adding dependency would create a cycle")
		}
		return nil, nil
	})
	_, err := c.Apply(context.Background(), []core.Proposal{
		{Issue: core.Issue{Title: "t", Role: "implement"}, DependsOn: []string{"existing-1"}},
	})
	if err == nil {
		t.Fatal("Apply ignored a dep failure")
	}
	var sawDelete bool
	for _, call := range *calls {
		if call[0] == "delete" && call[1] == "new-1" {
			sawDelete = true
		}
	}
	if !sawDelete {
		t.Errorf("Apply did not roll back created issue; calls = %v", *calls)
	}
}

func TestApplyCreateNoID(t *testing.T) {
	c, _ := recordingClient(func(args []string) ([]byte, error) {
		if args[0] == "create" {
			return []byte(`{}`), nil // no id field
		}
		return nil, nil
	})
	if _, err := c.Apply(context.Background(), []core.Proposal{
		{Issue: core.Issue{Title: "t", Role: "implement"}},
	}); err == nil {
		t.Fatal("Apply accepted a create response with no id")
	}
}

// A dependency on an issue that does not exist must fail the whole batch before any
// child is created. This closes the bd-1.0.4 foreign-prefix gap: `dep add` validates a
// same-prefix target but silently accepts a foreign-prefix id as an unchecked external
// reference, so an untrusted proposal could otherwise plant a dangling edge to a
// fabricated id. The harness verifies existence itself via the read path, prefix-blind
// (see specs/architecture.md, T3.2).
func TestApplyRejectsUnknownDependency(t *testing.T) {
	c, calls := recordingClient(func(args []string) ([]byte, error) {
		switch args[0] {
		case "show": // bd reports a miss with a nonzero exit; model that as an error
			return nil, errors.New(`no issue found matching "other-123"`)
		case "create":
			return []byte(`{"id":"new-1"}`), nil
		}
		return nil, nil
	})
	_, err := c.Apply(context.Background(), []core.Proposal{
		{Issue: core.Issue{Title: "t", Role: "implement"}, DependsOn: []string{"other-123"}},
	})
	if err == nil {
		t.Fatal("Apply accepted a dependency on a nonexistent issue")
	}
	// The check must fire before any create, so there is nothing to roll back: no create
	// and no delete may have been issued.
	for _, call := range *calls {
		if call[0] == "create" || call[0] == "delete" {
			t.Errorf("existence check did not fail fast before mutating; saw %v", call)
		}
	}
}

// A miss reported as an empty result (zero exit, empty array) — rather than a nonzero
// exit — must also be rejected, since bd's show output shape varies by version. Get
// turns an empty array into a not-found error, so Apply must still reject the batch.
func TestApplyRejectsUnknownDependencyEmptyResult(t *testing.T) {
	c, _ := recordingClient(func(args []string) ([]byte, error) {
		if args[0] == "show" {
			return []byte(`[]`), nil
		}
		return []byte(`{"id":"new-1"}`), nil
	})
	if _, err := c.Apply(context.Background(), []core.Proposal{
		{Issue: core.Issue{Title: "t", Role: "implement"}, DependsOn: []string{"other-123"}},
	}); err == nil {
		t.Fatal("Apply accepted a dependency whose show returned no issue")
	}
}

// --- Integration tests: real bd binary ---

func TestClaimIntegration(t *testing.T) {
	bdAvailable(t)
	dir := bdInit(t)
	id := quickCreate(t, dir, "claim me")
	runBD(t, dir, "update", id, "--metadata", `{"role":"implement"}`)

	c := New(WithDir(dir))
	until, err := c.Claim(context.Background(), id, 15*time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	out := runBD(t, dir, "show", id, "--json")
	if !strings.Contains(out, `"in_progress"`) {
		t.Errorf("status not in_progress after Claim: %s", out)
	}
	if !strings.Contains(out, "lease_until") {
		t.Errorf("lease_until not stamped: %s", out)
	}
	// Role must survive the metadata merge.
	if !strings.Contains(out, `"implement"`) {
		t.Errorf("role clobbered by Claim: %s", out)
	}
	if time.Until(until) > 16*time.Minute {
		t.Errorf("lease %v too far out", until)
	}
}

func TestTransitionsIntegration(t *testing.T) {
	bdAvailable(t)
	dir := bdInit(t)
	c := New(WithDir(dir))

	id := quickCreate(t, dir, "lifecycle")
	if _, err := c.Claim(context.Background(), id, time.Hour); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := c.Release(context.Background(), id); err != nil {
		t.Fatalf("Release: %v", err)
	}
	out := runBD(t, dir, "show", id, "--json")
	if !strings.Contains(out, `"open"`) {
		t.Errorf("Release did not reopen: %s", out)
	}
	if strings.Contains(out, "lease_until") {
		t.Errorf("Release did not clear lease: %s", out)
	}

	if err := c.Close(context.Background(), id); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if out := runBD(t, dir, "show", id, "--json"); !strings.Contains(out, `"closed"`) {
		t.Errorf("Close did not close: %s", out)
	}

	id2 := quickCreate(t, dir, "to block")
	if err := c.Block(context.Background(), id2, "agent escalated: needs-spec-clarification"); err != nil {
		t.Fatalf("Block: %v", err)
	}
	if out := runBD(t, dir, "show", id2, "--json"); !strings.Contains(out, `"blocked"`) {
		t.Errorf("Block did not block: %s", out)
	}
	// The reason rides in metadata so the DLQ / Resolve read path can show why (T4.15).
	got, err := c.Get(context.Background(), id2)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.DeadLetterReason != "agent escalated: needs-spec-clarification" {
		t.Errorf("DeadLetterReason = %q, want the blocked reason (round-tripped via metadata)", got.DeadLetterReason)
	}
}

func TestApplyIntegration(t *testing.T) {
	bdAvailable(t)
	dir := bdInit(t)
	existing := quickCreate(t, dir, "dependency")

	c := New(WithDir(dir))
	created, err := c.Apply(context.Background(), []core.Proposal{
		{Issue: core.Issue{Title: "child a", Body: "do a", Role: "implement"}, DependsOn: []string{existing}},
		{Issue: core.Issue{Title: "child b", Role: "qa"}},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(created) != 2 {
		t.Fatalf("created %d issues, want 2", len(created))
	}

	// Read the created child back: role round-trips via metadata, body via description.
	got, err := c.Get(context.Background(), created[0].ID)
	if err != nil {
		t.Fatalf("Get child: %v", err)
	}
	if got.Title != "child a" || got.Body != "do a" || got.Role != "implement" {
		t.Errorf("child a = %+v", got)
	}

	// child a depends on `existing`, so it must NOT be ready while existing is open;
	// child b (no deps) must be ready.
	ready, err := c.Ready(context.Background())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	readyIDs := map[string]bool{}
	for _, is := range ready {
		readyIDs[is.ID] = true
	}
	if readyIDs[created[0].ID] {
		t.Errorf("child a is ready despite an open blocker")
	}
	if !readyIDs[created[1].ID] {
		t.Errorf("child b (no deps) is not ready")
	}
}

// A dependency on a nonexistent issue must fail the whole Apply and leave nothing
// behind — proving the prefix-independent existence check (T3.2) against real bd. The
// check fires before any create, so the issue count is unchanged for want of anything
// to roll back.
func TestApplyRejectsUnknownDependencyIntegration(t *testing.T) {
	bdAvailable(t)
	dir := bdInit(t)
	c := New(WithDir(dir))

	before := len(allIssues(t, dir))
	_, err := c.Apply(context.Background(), []core.Proposal{
		{Issue: core.Issue{Title: "orphan", Role: "implement"}, DependsOn: []string{"harness-nonexistent"}},
	})
	if err == nil {
		t.Fatal("Apply accepted a dependency on a nonexistent issue")
	}
	if after := len(allIssues(t, dir)); after != before {
		t.Errorf("issue count changed %d -> %d; existence check created an orphan child", before, after)
	}
}

// A real edge failure (a cycle between two siblings, which all exist by construction so
// they clear the existence check) must roll back every issue created in the call,
// proving the all-or-nothing rollback path against real bd. bd rejects the second edge
// because it would close a cycle.
func TestApplyRollbackOnCycleIntegration(t *testing.T) {
	bdAvailable(t)
	dir := bdInit(t)
	c := New(WithDir(dir))

	before := len(allIssues(t, dir))
	_, err := c.Apply(context.Background(), []core.Proposal{
		{Issue: core.Issue{Title: "a", Role: "implement"}, Key: "a", DependsOn: []string{"b"}},
		{Issue: core.Issue{Title: "b", Role: "implement"}, Key: "b", DependsOn: []string{"a"}},
	})
	if err == nil {
		t.Fatal("Apply accepted a dependency cycle between siblings")
	}
	if after := len(allIssues(t, dir)); after != before {
		t.Errorf("issue count changed %d -> %d; rollback did not delete the created children", before, after)
	}
}

// ListStranded must return in_progress issues whose lease has expired (or is absent)
// and exclude those with a future lease, against the real bd metadata round-trip.
func TestListStrandedIntegration(t *testing.T) {
	bdAvailable(t)
	dir := bdInit(t)
	c := New(WithDir(dir))

	// expired: claimed with a tiny ttl that is already in the past by check time.
	expired := quickCreate(t, dir, "stranded")
	runBD(t, dir, "update", expired, "--status", "in_progress",
		"--set-metadata", "lease_until=2000-01-01T00:00:00Z")
	// fresh: a future lease — held by a live runner, not stranded.
	fresh := quickCreate(t, dir, "held")
	if _, err := c.Claim(context.Background(), fresh, time.Hour); err != nil {
		t.Fatalf("Claim fresh: %v", err)
	}
	// noLease: in_progress with no lease metadata at all — treated as stranded.
	noLease := quickCreate(t, dir, "no lease")
	runBD(t, dir, "update", noLease, "--status", "in_progress")

	stranded, err := c.ListStranded(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("ListStranded: %v", err)
	}
	got := map[string]bool{}
	for _, id := range stranded {
		got[id] = true
	}
	if !got[expired] {
		t.Errorf("expired issue %s not reported stranded; got %v", expired, stranded)
	}
	if !got[noLease] {
		t.Errorf("lease-less issue %s not reported stranded; got %v", noLease, stranded)
	}
	if got[fresh] {
		t.Errorf("freshly-leased issue %s reported stranded; got %v", fresh, stranded)
	}
}

// Reissue returns a spec-drifted in_progress issue to the ready pool and clears BOTH its
// lease and its pinned spec hash, against the real bd metadata round-trip. This is the
// re-derive half of "recompile the delta": the spec version changed, so unlike Release
// (dead-runner recovery, which keeps the pin) the pin must be dropped so the next dispatch
// re-resolves the edited slice and re-pins it (see specs/specs-process.md).
func TestReissueIntegration(t *testing.T) {
	bdAvailable(t)
	dir := bdInit(t)
	c := New(WithDir(dir))

	id := quickCreate(t, dir, "drifted work")
	if _, err := c.Claim(context.Background(), id, time.Hour); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := c.PinSpecHash(context.Background(), id, "sha256:abc123"); err != nil {
		t.Fatalf("PinSpecHash: %v", err)
	}

	if err := c.Reissue(context.Background(), id); err != nil {
		t.Fatalf("Reissue: %v", err)
	}

	got, err := c.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "open" {
		t.Errorf("Status = %q, want open (reissued back to ready)", got.Status)
	}
	if got.SpecHash != "" {
		t.Errorf("SpecHash = %q, want empty (the pin must be cleared so dispatch re-pins the edited slice)", got.SpecHash)
	}
}

// InProgress returns every in_progress issue fully decoded — including the pinned spec hash
// the drift sweep compares against — and excludes issues in any other status.
func TestInProgressIntegration(t *testing.T) {
	bdAvailable(t)
	dir := bdInit(t)
	c := New(WithDir(dir))

	working := quickCreate(t, dir, "working")
	if _, err := c.Claim(context.Background(), working, time.Hour); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := c.PinSpecHash(context.Background(), working, "sha256:pinned"); err != nil {
		t.Fatalf("PinSpecHash: %v", err)
	}
	_ = quickCreate(t, dir, "still open") // stays open; must not appear in the in_progress set

	got, err := c.InProgress(context.Background())
	if err != nil {
		t.Fatalf("InProgress: %v", err)
	}
	if len(got) != 1 || got[0].ID != working {
		t.Fatalf("InProgress = %+v, want only the claimed issue %s", got, working)
	}
	if got[0].SpecHash != "sha256:pinned" {
		t.Errorf("SpecHash = %q, want sha256:pinned (the version the drift sweep reads)", got[0].SpecHash)
	}
}

// The retry generation written via Apply (Issue.Attempt) must round-trip through bd
// metadata and decode back on Get — this counter is what the retry cap is enforced
// against.
func TestRetriesRoundTripIntegration(t *testing.T) {
	bdAvailable(t)
	dir := bdInit(t)
	c := New(WithDir(dir))

	created, err := c.Apply(context.Background(), []core.Proposal{
		{Issue: core.Issue{Title: "retry me", Role: "implement", Attempt: 3}},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := c.Get(context.Background(), created[0].ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Attempt != 3 {
		t.Errorf("Attempt = %d, want 3 (round-tripped via metadata)", got.Attempt)
	}
	if got.Role != "implement" {
		t.Errorf("Role = %q, want implement (must survive alongside retries)", got.Role)
	}
}

// TestBaseRoundTripIntegration proves an issue's Base — the git ref a produced issue's
// candidate branches from — survives the write to beads metadata and decodes back on
// Get. This is what carries the author-tests candidate (holding the failing tests) into
// the implementor's worktree; it must round-trip alongside Role and Attempt.
func TestBaseRoundTripIntegration(t *testing.T) {
	bdAvailable(t)
	dir := bdInit(t)
	c := New(WithDir(dir))

	created, err := c.Apply(context.Background(), []core.Proposal{
		{Issue: core.Issue{Title: "build on the candidate", Role: "implement", Attempt: 2, Base: "candidate/harness-1"}},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := c.Get(context.Background(), created[0].ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Base != "candidate/harness-1" {
		t.Errorf("Base = %q, want candidate/harness-1 (round-tripped via metadata)", got.Base)
	}
	if got.Role != "implement" || got.Attempt != 2 {
		t.Errorf("Role/Attempt = %q/%d, want implement/2 (must survive alongside base)", got.Role, got.Attempt)
	}
}

// TestTraceMapRoundTripIntegration proves an issue's TraceMap — the artifact-store hash of
// the author-tests test↔spec traceability map — survives the write to beads metadata and
// decodes back on Get. It is threaded forward like Base so it reaches the integrate stage
// where the merge provenance trailer cites it; it must round-trip alongside Role, Attempt,
// and Base (see specs/verification.md).
func TestTraceMapRoundTripIntegration(t *testing.T) {
	bdAvailable(t)
	dir := bdInit(t)
	c := New(WithDir(dir))

	created, err := c.Apply(context.Background(), []core.Proposal{
		{Issue: core.Issue{Title: "implement against traced tests", Role: "implement", Attempt: 1,
			Base: "candidate/harness-1", TraceMap: "sha256:abc123"}},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := c.Get(context.Background(), created[0].ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.TraceMap != "sha256:abc123" {
		t.Errorf("TraceMap = %q, want sha256:abc123 (round-tripped via metadata)", got.TraceMap)
	}
	if got.Base != "candidate/harness-1" || got.Role != "implement" || got.Attempt != 1 {
		t.Errorf("Base/Role/Attempt = %q/%q/%d, want candidate/harness-1/implement/1 (must survive alongside trace map)",
			got.Base, got.Role, got.Attempt)
	}
}

// TestSpentRoundTripIntegration proves an issue's cumulative spend — SpentTokens and
// SpentUSD, the tokens and dollars burned by earlier attempts in its on_failure retry
// chain — survives the write to beads metadata and decodes back on Get. The orchestrator
// threads these forward like Base/Attempt and enforces the per-issue budget against the
// running sum (T3.8), so they must round-trip; SpentUSD is fractional, exercising the
// metaFloat decode path (see specs/workflow.md).
func TestSpentRoundTripIntegration(t *testing.T) {
	bdAvailable(t)
	dir := bdInit(t)
	c := New(WithDir(dir))

	created, err := c.Apply(context.Background(), []core.Proposal{
		{Issue: core.Issue{Title: "expensive fix", Role: "implement", Attempt: 2,
			SpentTokens: 1_350_000, SpentUSD: 12.5}},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := c.Get(context.Background(), created[0].ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SpentTokens != 1_350_000 {
		t.Errorf("SpentTokens = %d, want 1350000 (round-tripped via metadata)", got.SpentTokens)
	}
	if got.SpentUSD != 12.5 {
		t.Errorf("SpentUSD = %v, want 12.5 (round-tripped via metadata, fractional)", got.SpentUSD)
	}
	if got.Role != "implement" || got.Attempt != 2 {
		t.Errorf("Role/Attempt = %q/%d, want implement/2 (must survive alongside spend)", got.Role, got.Attempt)
	}
}

// TestSpecRoundTripIntegration proves an issue's Spec — the repo-relative path of the spec
// file governing it — survives the write to beads metadata and decodes back on Get. It is
// the reference the orchestrator resolves the bounded spec slice from, threaded forward
// like Base, so it must round-trip alongside Role and Base (see internal/spec, T3.5).
func TestSpecRoundTripIntegration(t *testing.T) {
	bdAvailable(t)
	dir := bdInit(t)
	c := New(WithDir(dir))

	created, err := c.Apply(context.Background(), []core.Proposal{
		{Issue: core.Issue{Title: "implement orders", Role: "implement",
			Base: "candidate/harness-1", Spec: "specs/orders.md"}},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := c.Get(context.Background(), created[0].ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Spec != "specs/orders.md" {
		t.Errorf("Spec = %q, want specs/orders.md (round-tripped via metadata)", got.Spec)
	}
	if got.Base != "candidate/harness-1" || got.Role != "implement" {
		t.Errorf("Base/Role = %q/%q, want candidate/harness-1/implement (must survive alongside spec)", got.Base, got.Role)
	}
}

// TestPinSpecHashRoundTripIntegration proves PinSpecHash merges the spec-version hash into
// an issue's metadata without disturbing the role/spec/base written at creation, and that it
// decodes back on Get — the durable anchor T3.7 diffs against (T3.6, see internal/spec).
func TestPinSpecHashRoundTripIntegration(t *testing.T) {
	bdAvailable(t)
	dir := bdInit(t)
	c := New(WithDir(dir))
	ctx := context.Background()

	created, err := c.Apply(ctx, []core.Proposal{
		{Issue: core.Issue{Title: "implement orders", Role: "implement", Spec: "specs/orders.md"}},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	id := created[0].ID

	// An empty hash is a no-op (an issue naming no spec has nothing to pin).
	if err := c.PinSpecHash(ctx, id, ""); err != nil {
		t.Fatalf("PinSpecHash(empty): %v", err)
	}
	if got, _ := c.Get(ctx, id); got.SpecHash != "" {
		t.Errorf("empty hash must not pin, got SpecHash %q", got.SpecHash)
	}

	if err := c.PinSpecHash(ctx, id, "sha256:abc123"); err != nil {
		t.Fatalf("PinSpecHash: %v", err)
	}
	got, err := c.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SpecHash != "sha256:abc123" {
		t.Errorf("SpecHash = %q, want sha256:abc123 (round-tripped via metadata)", got.SpecHash)
	}
	if got.Role != "implement" || got.Spec != "specs/orders.md" {
		t.Errorf("Role/Spec = %q/%q, want implement/specs/orders.md (pin must not disturb them)", got.Role, got.Spec)
	}
}

// TestEpicAndWallRoundTripIntegration proves the T3.8b metadata round-trips through beads:
// the epic root id and the cumulative wall (a duration string) are stamped at creation and
// threaded forward like Base/SpentTokens, and the per-issue closing spend is stamped post-hoc
// by StampClosingSpend and decodes back for the epic-budget aggregate read. The wall exercises
// the metaDuration decode path; closing spend exercises a numeric --set-metadata round-trip
// (see specs/workflow.md "epic_budget").
func TestEpicAndWallRoundTripIntegration(t *testing.T) {
	bdAvailable(t)
	dir := bdInit(t)
	c := New(WithDir(dir))
	ctx := context.Background()

	created, err := c.Apply(ctx, []core.Proposal{
		{Issue: core.Issue{Title: "fix", Role: "implement", Attempt: 1,
			EpicID: "harness-1", SpentWall: 90 * time.Second}},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	id := created[0].ID

	got, err := c.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.EpicID != "harness-1" {
		t.Errorf("EpicID = %q, want harness-1 (round-tripped via metadata)", got.EpicID)
	}
	if got.SpentWall != 90*time.Second {
		t.Errorf("SpentWall = %s, want 1m30s (round-tripped as a duration string)", got.SpentWall)
	}
	// Closing spend is absent until stamped — the aggregate read treats it as 0.
	if got.ClosingTokens != 0 || got.ClosingUSD != 0 {
		t.Errorf("ClosingTokens/USD = %d/%v, want 0/0 before StampClosingSpend", got.ClosingTokens, got.ClosingUSD)
	}

	if err := c.StampClosingSpend(ctx, id, 12_000, 0.35); err != nil {
		t.Fatalf("StampClosingSpend: %v", err)
	}
	got, err = c.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after stamp: %v", err)
	}
	if got.ClosingTokens != 12_000 {
		t.Errorf("ClosingTokens = %d, want 12000 (numeric --set-metadata round-trip)", got.ClosingTokens)
	}
	if got.ClosingUSD != 0.35 {
		t.Errorf("ClosingUSD = %v, want 0.35 (fractional --set-metadata round-trip)", got.ClosingUSD)
	}
	// The stamp must not disturb the values written at creation.
	if got.EpicID != "harness-1" || got.SpentWall != 90*time.Second || got.Attempt != 1 {
		t.Errorf("EpicID/SpentWall/Attempt = %q/%s/%d, want harness-1/1m30s/1 (stamp must not disturb them)", got.EpicID, got.SpentWall, got.Attempt)
	}
}

// TestStampTranscriptRoundTripIntegration proves the most-recent invocation transcript hash
// survives StampTranscript into beads metadata and decodes back on Get — so the decision trail is
// reachable from the issue itself (for the Resolve wizard / replay of non-merged work, T4.15),
// not only from a merge trailer. An empty hash is a no-op (nothing to cite).
func TestStampTranscriptRoundTripIntegration(t *testing.T) {
	bdAvailable(t)
	dir := bdInit(t)
	c := New(WithDir(dir))
	ctx := context.Background()

	id := quickCreate(t, dir, "ran once")
	if got := mustGetIssue(t, c, id); got.Transcript != "" {
		t.Errorf("Transcript = %q, want empty before any stamp", got.Transcript)
	}
	// Empty hash is a no-op.
	if err := c.StampTranscript(ctx, id, ""); err != nil {
		t.Fatalf("StampTranscript(empty): %v", err)
	}
	if got := mustGetIssue(t, c, id); got.Transcript != "" {
		t.Errorf("Transcript = %q, want still empty after empty-hash no-op", got.Transcript)
	}
	if err := c.StampTranscript(ctx, id, "sha256:deadbeef"); err != nil {
		t.Fatalf("StampTranscript: %v", err)
	}
	if got := mustGetIssue(t, c, id); got.Transcript != "sha256:deadbeef" {
		t.Errorf("Transcript = %q, want sha256:deadbeef (round-tripped via metadata)", mustGetIssue(t, c, id).Transcript)
	}
}

// TestSoulsRoundTripIntegration proves the producing souls (TestsSoul / ImplementSoul) thread
// through Apply into beads metadata and decode back on Get — the threading half of T4.22, which
// keeps producer ≠ verifier readable across an epic's stages. They must survive alongside the
// other threaded facets (TraceMap/Base).
func TestSoulsRoundTripIntegration(t *testing.T) {
	bdAvailable(t)
	dir := bdInit(t)
	c := New(WithDir(dir))

	created, err := c.Apply(context.Background(), []core.Proposal{
		{Issue: core.Issue{Title: "implement against traced tests", Role: "implement",
			TraceMap: "sha256:tm", TestsSoul: "test-author-go", ImplementSoul: "implementor-go"}},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := mustGetIssue(t, c, created[0].ID)
	if got.TestsSoul != "test-author-go" || got.ImplementSoul != "implementor-go" {
		t.Errorf("TestsSoul/ImplementSoul = %q/%q, want test-author-go/implementor-go (round-tripped)", got.TestsSoul, got.ImplementSoul)
	}
	if got.TraceMap != "sha256:tm" {
		t.Errorf("TraceMap = %q, want sha256:tm (must survive alongside the souls)", got.TraceMap)
	}
}

// TestStampSoulsRoundTripIntegration proves StampSouls records a stage's producing soul post-hoc
// without disturbing other keys, writes only non-empty values, and is a no-op when both are empty.
func TestStampSoulsRoundTripIntegration(t *testing.T) {
	bdAvailable(t)
	dir := bdInit(t)
	c := New(WithDir(dir))
	ctx := context.Background()

	id := quickCreate(t, dir, "author-tests stage")
	// Both empty is a no-op.
	if err := c.StampSouls(ctx, id, "", ""); err != nil {
		t.Fatalf("StampSouls(empty): %v", err)
	}
	if got := mustGetIssue(t, c, id); got.TestsSoul != "" || got.ImplementSoul != "" {
		t.Errorf("souls = %q/%q, want both empty after the no-op", got.TestsSoul, got.ImplementSoul)
	}
	// Stamping only the tests soul leaves ImplementSoul untouched.
	if err := c.StampSouls(ctx, id, "test-author-go", ""); err != nil {
		t.Fatalf("StampSouls(tests): %v", err)
	}
	got := mustGetIssue(t, c, id)
	if got.TestsSoul != "test-author-go" || got.ImplementSoul != "" {
		t.Errorf("souls = %q/%q, want test-author-go/empty (only the tests soul written)", got.TestsSoul, got.ImplementSoul)
	}
}

// TestStampGateVerdictRoundTripIntegration proves the gate-verdict record hash survives
// StampGateVerdict into beads metadata and decodes back on Get — so a rejected candidate's
// verdict is reachable from the issue for the verification view (T4.22). An empty hash is a no-op.
func TestStampGateVerdictRoundTripIntegration(t *testing.T) {
	bdAvailable(t)
	dir := bdInit(t)
	c := New(WithDir(dir))
	ctx := context.Background()

	id := quickCreate(t, dir, "gated candidate")
	if err := c.StampGateVerdict(ctx, id, ""); err != nil {
		t.Fatalf("StampGateVerdict(empty): %v", err)
	}
	if got := mustGetIssue(t, c, id); got.GateVerdict != "" {
		t.Errorf("GateVerdict = %q, want empty after the empty-hash no-op", got.GateVerdict)
	}
	if err := c.StampGateVerdict(ctx, id, "sha256:verdict"); err != nil {
		t.Fatalf("StampGateVerdict: %v", err)
	}
	if got := mustGetIssue(t, c, id); got.GateVerdict != "sha256:verdict" {
		t.Errorf("GateVerdict = %q, want sha256:verdict (round-tripped via metadata)", got.GateVerdict)
	}
}

// mustGetIssue is a small Get helper for the round-trip assertions.
func mustGetIssue(t *testing.T, c *Client, id string) core.Issue {
	t.Helper()
	got, err := c.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return got
}

// allIssues returns every issue id in the db (including closed) for count assertions.
func allIssues(t *testing.T, dir string) []string {
	t.Helper()
	out := runBD(t, dir, "list", "--all", "--json", "--limit", "0")
	if strings.TrimSpace(out) == "" || strings.TrimSpace(out) == "[]" {
		return nil
	}
	issues, err := decodeIssues([]byte(out))
	if err != nil {
		t.Fatalf("decode list: %v", err)
	}
	ids := make([]string, len(issues))
	for i, is := range issues {
		ids[i] = is.ID
	}
	return ids
}
