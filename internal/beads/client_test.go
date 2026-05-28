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
    "labels": ["lang-go"],
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
	// An issue with no metadata.role maps to an empty Role, not an error.
	if issues[1].Role != "" {
		t.Errorf("issue[1] role = %q, want empty", issues[1].Role)
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
