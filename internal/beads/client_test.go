package beads

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/core"
)

// --- Unit tests: decode/mapping against canned bd output via the run seam ---

// fakeClient returns a Client whose run returns the given output/err, recording the
// args it was called with.
func fakeClient(out string, err error) (*Client, *[]string) {
	var gotArgs []string
	c := New()
	c.run = func(_ context.Context, args []string) ([]byte, error) {
		gotArgs = args
		return []byte(out), err
	}
	return c, &gotArgs
}

// readyJSON mirrors the real `bd ready --json` shape (verified against bd 0.62.0):
// a JSON array of issue objects, role carried in the metadata object.
const readyJSON = `[
  {
    "id": "harness-abc",
    "title": "implement the loader",
    "description": "make the tests pass",
    "status": "open",
    "priority": 2,
    "labels": ["lang=go", "tier=high"],
    "metadata": {"role": "implement"}
  },
  {
    "id": "harness-def",
    "title": "no role set",
    "description": "",
    "status": "open"
  }
]`

func TestReadyMapsFields(t *testing.T) {
	c, args := fakeClient(readyJSON, nil)
	issues, err := c.Ready(context.Background())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if want := []string{"ready", "--json", "--limit", "0"}; strings.Join(*args, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", *args, want)
	}
	if len(issues) != 2 {
		t.Fatalf("got %d issues, want 2", len(issues))
	}
	got := issues[0]
	if got.ID != "harness-abc" || got.Title != "implement the loader" || got.Body != "make the tests pass" {
		t.Errorf("issue[0] = %+v", got)
	}
	if got.Role != "implement" {
		t.Errorf("issue[0] role = %q, want implement (from metadata)", got.Role)
	}
	// Labels decode into selector Tags (one key=value label each); the role lives in
	// metadata, the tags in labels, kept apart so they never collide.
	if got.Tags["lang"] != "go" || got.Tags["tier"] != "high" || len(got.Tags) != 2 {
		t.Errorf("issue[0] tags = %v, want {lang:go, tier:high}", got.Tags)
	}
	// An issue with no metadata.role maps to an empty Role, not an error.
	if issues[1].Role != "" {
		t.Errorf("issue[1] role = %q, want empty", issues[1].Role)
	}
	// An issue with no labels carries a nil tag map (the trivial 1:1 case).
	if issues[1].Tags != nil {
		t.Errorf("issue[1] tags = %v, want nil", issues[1].Tags)
	}
}

// TestDecodesDependencies proves the read path turns bd's inline `dependencies` array
// (the blocked-by edges bd already emits on `bd list --json`) into core.Issue.DependsOn,
// the edge source the control-room DAG (T4.6) reads instead of a separate `bd dep` query.
// An empty/absent array yields a nil slice; an edge with an empty depends_on_id is skipped.
func TestDecodesDependencies(t *testing.T) {
	const listJSON = `[
	  {
	    "id": "harness-child",
	    "title": "blocked work",
	    "status": "open",
	    "dependencies": [
	      {"issue_id": "harness-child", "depends_on_id": "harness-parent", "type": "blocks"},
	      {"issue_id": "harness-child", "depends_on_id": "harness-other", "type": "blocks"},
	      {"issue_id": "harness-child", "depends_on_id": "", "type": "blocks"}
	    ]
	  },
	  {
	    "id": "harness-parent",
	    "title": "no blockers",
	    "status": "open"
	  }
	]`
	c, _ := fakeClient(listJSON, nil)
	issues, err := c.List(context.Background(), "open")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("got %d issues, want 2", len(issues))
	}
	if want := []string{"harness-parent", "harness-other"}; strings.Join(issues[0].DependsOn, ",") != strings.Join(want, ",") {
		t.Errorf("issue[0].DependsOn = %v, want %v (empty depends_on_id dropped)", issues[0].DependsOn, want)
	}
	if issues[1].DependsOn != nil {
		t.Errorf("issue[1].DependsOn = %v, want nil (no blockers)", issues[1].DependsOn)
	}
}

// TestParseLabels pins the label<->tag encoding: key=value splits on the first '=', a
// label with no '=' is a valueless tag, an empty key is dropped, and no labels yields nil.
func TestParseLabels(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want map[string]string
	}{
		{"nil", nil, nil},
		{"empty", []string{}, nil},
		{"single", []string{"lang=go"}, map[string]string{"lang": "go"}},
		{"multi", []string{"lang=go", "tier=high"}, map[string]string{"lang": "go", "tier": "high"}},
		{"value with equals", []string{"expr=a=b"}, map[string]string{"expr": "a=b"}},
		{"bare label is valueless", []string{"urgent"}, map[string]string{"urgent": ""}},
		{"empty key dropped", []string{"=v"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseLabels(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("parseLabels(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("parseLabels(%v)[%q] = %q, want %q", tc.in, k, got[k], v)
				}
			}
		})
	}
}

// Metadata written by another tool with a non-string value must not fail the whole
// read — the role just comes back empty.
func TestReadyToleratesNonStringMetadata(t *testing.T) {
	c, _ := fakeClient(`[{"id":"x","title":"t","metadata":{"role":42}}]`, nil)
	issues, err := c.Ready(context.Background())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if issues[0].Role != "" {
		t.Errorf("role = %q, want empty for non-string metadata", issues[0].Role)
	}
}

func TestReadyEmptyArray(t *testing.T) {
	c, _ := fakeClient(`[]`, nil)
	issues, err := c.Ready(context.Background())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(issues) != 0 {
		t.Errorf("got %d issues, want 0", len(issues))
	}
}

func TestReadyRunError(t *testing.T) {
	c, _ := fakeClient("", errors.New("bd exploded"))
	if _, err := c.Ready(context.Background()); err == nil {
		t.Fatal("Ready accepted a run error, want failure")
	}
}

func TestReadyMalformedJSON(t *testing.T) {
	c, _ := fakeClient("not json", nil)
	if _, err := c.Ready(context.Background()); err == nil {
		t.Fatal("Ready accepted malformed json, want failure")
	}
}

func TestGetMapsSingleIssue(t *testing.T) {
	c, args := fakeClient(`[{"id":"harness-abc","title":"t","description":"b","metadata":{"role":"qa"}}]`, nil)
	got, err := c.Get(context.Background(), "harness-abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if want := []string{"show", "harness-abc", "--json"}; strings.Join(*args, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", *args, want)
	}
	if got.ID != "harness-abc" || got.Role != "qa" {
		t.Errorf("issue = %+v", got)
	}
}

func TestGetEmptyID(t *testing.T) {
	c := New()
	called := false
	c.run = func(_ context.Context, _ []string) ([]byte, error) { called = true; return nil, nil }
	if _, err := c.Get(context.Background(), ""); err == nil {
		t.Fatal("Get accepted an empty id, want failure")
	}
	if called {
		t.Error("Get shelled out to bd for an empty id; it should reject before running")
	}
}

func TestGetNotFoundEmptyArray(t *testing.T) {
	c, _ := fakeClient(`[]`, nil)
	if _, err := c.Get(context.Background(), "ghost"); err == nil {
		t.Fatal("Get accepted an empty result, want not-found error")
	}
}

func TestListPassesStatusAndDecodes(t *testing.T) {
	c, args := fakeClient(readyJSON, nil)
	issues, err := c.List(context.Background(), "blocked")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if want := []string{"list", "--status", "blocked", "--json", "--flat", "--limit", "0"}; strings.Join(*args, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", *args, want)
	}
	if len(issues) != 2 || issues[0].ID != "harness-abc" {
		t.Errorf("decoded %d issues = %+v", len(issues), issues)
	}
}

func TestListEmptyStatusRejected(t *testing.T) {
	c := New()
	called := false
	c.run = func(_ context.Context, _ []string) ([]byte, error) { called = true; return nil, nil }
	if _, err := c.List(context.Background(), ""); err == nil {
		t.Fatal("List accepted an empty status, want failure")
	}
	if called {
		t.Error("List shelled out to bd for an empty status; it should reject before running")
	}
}

func TestListRunError(t *testing.T) {
	c, _ := fakeClient("", errors.New("bd exploded"))
	if _, err := c.List(context.Background(), "open"); err == nil {
		t.Fatal("List accepted a run error, want failure")
	}
}

func TestListAllPassesArgs(t *testing.T) {
	c, args := fakeClient(`[]`, nil)
	if _, err := c.ListAll(context.Background()); err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if want := []string{"list", "--all", "--json", "--flat", "--limit", "0"}; strings.Join(*args, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", *args, want)
	}
}

func TestListAllRunError(t *testing.T) {
	c, _ := fakeClient("", errors.New("bd exploded"))
	if _, err := c.ListAll(context.Background()); err == nil {
		t.Fatal("ListAll accepted a run error, want failure")
	}
}

// --- Integration tests: drive the real bd binary ---

// bdAvailable skips the test if the real bd CLI is not installed. bd is a hard
// dependency of the harness, but the unit tests above cover the mapping without it;
// these integration tests prove the wrapper speaks the real CLI's contract.
func bdAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not on PATH; skipping beads integration test")
	}
}

// bdInit creates a fresh beads database in a temp dir and returns the dir. It pins
// the issue prefix to "harness" so id strings are deterministic across tests: bd
// 1.0.4 resolves a dependency id whose prefix differs from the db's as an external
// (federation) reference and does NOT check it exists, whereas a same-prefix id is
// validated. A stable prefix keeps TestApplyRollbackIntegration's "harness-nonexistent"
// a genuinely-nonexistent local id (so the rollback path is exercised), rather than a
// silently-accepted foreign ref. (See IMPLEMENTATION_PLAN.md bd-version findings.)
func bdInit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runBD(t, dir, "init", "--prefix", "harness")
	return dir
}

// runBD runs a bd command in dir, failing the test on error. Used to arrange
// fixture state (the read client under test is exercised separately).
func runBD(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("bd", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bd %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// quickCreate creates an issue via `bd q` (outputs only the ID) and returns the ID.
func quickCreate(t *testing.T, dir, title string) string {
	t.Helper()
	return strings.TrimSpace(runBD(t, dir, "q", title))
}

func TestReadyIntegration(t *testing.T) {
	bdAvailable(t)
	dir := bdInit(t)

	work := quickCreate(t, dir, "do the work")
	runBD(t, dir, "update", work, "--metadata", `{"role":"implement"}`, "--description", "make it pass")
	blocker := quickCreate(t, dir, "must come first")
	// blocker blocks work => work is not ready, blocker is.
	runBD(t, dir, "dep", blocker, "--blocks", work)

	c := New(WithDir(dir))
	ready, err := c.Ready(context.Background())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}

	ids := map[string]core.Issue{}
	for _, is := range ready {
		ids[is.ID] = is
	}
	if _, ok := ids[work]; ok {
		t.Errorf("blocked issue %s appeared in ready set %v", work, ready)
	}
	if _, ok := ids[blocker]; !ok {
		t.Errorf("unblocked issue %s missing from ready set %v", blocker, ready)
	}
}

func TestGetIntegration(t *testing.T) {
	bdAvailable(t)
	dir := bdInit(t)

	id := quickCreate(t, dir, "read me")
	runBD(t, dir, "update", id, "--metadata", `{"role":"qa"}`, "--description", "the body")

	c := New(WithDir(dir))
	got, err := c.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != id || got.Title != "read me" || got.Body != "the body" || got.Role != "qa" {
		t.Errorf("issue = %+v (want title=read me, body=the body, role=qa)", got)
	}
}

func TestGetNotFoundIntegration(t *testing.T) {
	bdAvailable(t)
	dir := bdInit(t)
	c := New(WithDir(dir))
	if _, err := c.Get(context.Background(), "harness-nonexistent"); err == nil {
		t.Fatal("Get accepted a nonexistent id, want error")
	}
}

func TestReadyEmptyIntegration(t *testing.T) {
	bdAvailable(t)
	dir := bdInit(t)
	c := New(WithDir(dir))
	ready, err := c.Ready(context.Background())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(ready) != 0 {
		t.Errorf("fresh db ready set = %v, want empty", ready)
	}
}

// TestListIntegration proves the new read methods speak the real bd CLI contract: a closed
// issue is excluded from a status-filtered open list but surfaces under List("closed") and
// ListAll. This is the property the control room relies on to show completed work (closed
// issues are hidden from bd's default list) — exactly the gap List/ListAll close.
func TestListIntegration(t *testing.T) {
	bdAvailable(t)
	dir := bdInit(t)

	openID := quickCreate(t, dir, "still open")
	doneID := quickCreate(t, dir, "all finished")
	runBD(t, dir, "close", doneID)

	c := New(WithDir(dir))
	ctx := context.Background()

	ids := func(issues []core.Issue) map[string]bool {
		m := map[string]bool{}
		for _, is := range issues {
			m[is.ID] = true
		}
		return m
	}

	openList, err := c.List(ctx, "open")
	if err != nil {
		t.Fatalf("List(open): %v", err)
	}
	if got := ids(openList); !got[openID] || got[doneID] {
		t.Errorf("List(open) = %v, want open=%s present and closed=%s absent", openList, openID, doneID)
	}

	closedList, err := c.List(ctx, "closed")
	if err != nil {
		t.Fatalf("List(closed): %v", err)
	}
	if got := ids(closedList); !got[doneID] || got[openID] {
		t.Errorf("List(closed) = %v, want closed=%s present and open=%s absent", closedList, doneID, openID)
	}

	all, err := c.ListAll(ctx)
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if got := ids(all); !got[openID] || !got[doneID] {
		t.Errorf("ListAll = %v, want both %s and %s present", all, openID, doneID)
	}
}
