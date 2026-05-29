package beads

import (
	"context"
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
	// The lease value must be RFC3339 so the sweep can parse it.
	leaseArg := (*calls)[0][len((*calls)[0])-1]
	ts := strings.TrimPrefix(leaseArg, "lease_until=")
	if _, perr := time.Parse(time.RFC3339, ts); perr != nil {
		t.Errorf("lease value %q is not RFC3339: %v", ts, perr)
	}
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
		{"block", func(c *Client) error { return c.Block(context.Background(), "i") }, "update i --status blocked"},
		{"release", func(c *Client) error { return c.Release(context.Background(), "i") }, "update i --status open --unset-metadata lease_until"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, calls := recordingClient(func([]string) ([]byte, error) { return nil, nil })
			if err := tc.call(c); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if got := strings.Join((*calls)[0], " "); got != tc.wantSub {
				t.Errorf("args = %q, want %q", got, tc.wantSub)
			}
		})
	}
}

func TestStatusRejectsEmptyID(t *testing.T) {
	c, _ := recordingClient(func([]string) ([]byte, error) { return nil, nil })
	if err := c.Close(context.Background(), ""); err == nil {
		t.Error("Close accepted empty id")
	}
}

func TestApplyCreatesIssuesAndEdges(t *testing.T) {
	// create returns a distinct id per call; dep add succeeds.
	n := 0
	c, calls := recordingClient(func(args []string) ([]byte, error) {
		if args[0] == "create" {
			n++
			return []byte(`{"id":"new-` + string(rune('0'+n)) + `"}`), nil
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
	// First create carries description + role metadata; the dep edge is added after.
	c0 := strings.Join((*calls)[0], " ")
	if !strings.Contains(c0, "create fix it --json") || !strings.Contains(c0, "--description details") || !strings.Contains(c0, `--metadata {"role":"implement"}`) {
		t.Errorf("create args = %q", c0)
	}
	if got := strings.Join((*calls)[1], " "); got != "dep add new-1 existing-1" {
		t.Errorf("dep args = %q, want dep add new-1 existing-1", got)
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
	if err := c.Block(context.Background(), id2); err != nil {
		t.Fatalf("Block: %v", err)
	}
	if out := runBD(t, dir, "show", id2, "--json"); !strings.Contains(out, `"blocked"`) {
		t.Errorf("Block did not block: %s", out)
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

// A dependency on a nonexistent issue must fail the whole Apply and leave no
// created issue behind — proving the rollback path against real bd.
func TestApplyRollbackIntegration(t *testing.T) {
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
		t.Errorf("issue count changed %d -> %d; rollback did not delete the created child", before, after)
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
