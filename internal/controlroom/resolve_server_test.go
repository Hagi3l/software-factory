package controlroom

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/controlroom/query"
	"github.com/Loxstomper/harness/internal/controlroom/wizard"
	"github.com/Loxstomper/harness/internal/core"
)

// fakeResolver is a wizard.Resolver stub for the resolve-handler tests: it records the request it
// was handed and returns a canned result or error, so the handler is tested without touching git,
// beads, or the artifact store (those are exercised by the cmd-side resolver integration test).
type fakeResolver struct {
	got *wizard.ResolveRequest
	res wizard.ResolveResult
	err error
}

func (f *fakeResolver) Resolve(_ context.Context, req wizard.ResolveRequest) (wizard.ResolveResult, error) {
	f.got = &req
	return f.res, f.err
}

// resolveRepo writes a spec file into a fresh temp repo so the slice the Resolve page pre-loads
// (and the blast radius) resolves from disk, and returns the repo root.
func resolveRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	full := filepath.Join(repo, "specs", "export.md")
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("# Export\n\nThe acceptance criteria are ambiguous.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

// blockedIssue is a dead-lettered issue the Resolve wizard unsticks.
func blockedIssue() core.Issue {
	return core.Issue{
		ID: "harness-9", Title: "stuck export", Status: "blocked", Role: "implementor",
		Spec: "specs/export.md", DeadLetterReason: "agent escalated: needs-spec-clarification",
		Transcript: "sha256:tx", Attempt: 1,
	}
}

// TestResolveNotConfigured proves Resolve degrades gracefully: no planner → a wizard-disabled
// notice (page) and 503 (data endpoints); a planner but no reader → a not-attached notice; a
// planner but no resolver → an approval-unavailable notice (never a dead form or a 500).
func TestResolveNotConfigured(t *testing.T) {
	// No planner at all.
	s := New(Options{})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	if r := get(t, ts, "/resolve/harness-9"); r.status != http.StatusOK || !strings.Contains(r.body, "not configured") {
		t.Errorf("/resolve no-planner = %d %q, want 200 with not-configured notice", r.status, r.body)
	}
	if r := get(t, ts, "/resolve/blast/x"); r.status != http.StatusServiceUnavailable {
		t.Errorf("/resolve/blast no-planner = %d, want 503", r.status)
	}

	// Planner but no reader → not attached.
	p := wizard.NewPlanner(scriptedAdapter{reply: "hi"}, "persona")
	s2 := New(Options{Planner: p})
	ts2 := httptest.NewServer(s2.Handler())
	t.Cleanup(ts2.Close)
	if r := get(t, ts2, "/resolve/harness-9"); r.status != http.StatusOK || !strings.Contains(r.body, "Not attached") {
		t.Errorf("/resolve no-reader = %d %q, want 200 with not-attached notice", r.status, r.body)
	}
}

// TestResolveRendersPage proves a wired Resolve page pre-loads the escalation (id + reason), the
// governing spec slice, and the conversation wiring — reusing the per-session create endpoints for
// the stream/transcript and the resolve-specific blast panel.
func TestResolveRendersPage(t *testing.T) {
	p := wizard.NewPlanner(scriptedAdapter{reply: "let's look at this"}, "persona")
	reader := query.NewReader(&fakeIssues{all: []core.Issue{blockedIssue()}}, fakeArts{}, fakeProv{})
	s := New(Options{Planner: p, Reader: reader, Resolver: &fakeResolver{}, Repo: resolveRepo(t), SpecDepth: 0})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	r := get(t, ts, "/resolve/harness-9")
	if r.status != http.StatusOK {
		t.Fatalf("/resolve status = %d, want 200", r.status)
	}
	for _, want := range []string{
		"harness-9",                          // the escalation identity
		"needs-spec-clarification",           // the orchestrator's reason
		"acceptance criteria are ambiguous",  // the resolved spec slice content
		`sse-connect="/create/stream/`,       // the shared per-session live stream
		`hx-get="/resolve/blast/`,            // the resolve-specific blast panel
		`hx-post="/create/message"`,          // the shared turn form
		`href="/issue/harness-9"`,            // the drill-through to forensics
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("/resolve page missing %q\nbody: %s", want, r.body)
		}
	}
}

// TestResolveBlastPanel proves the blast-radius fragment renders the proposed spec edit and the
// in-flight work the edit would reissue (its slice includes the edited path). An unknown session
// 404s.
func TestResolveBlastPanel(t *testing.T) {
	p := wizard.NewPlanner(scriptedAdapter{reply: draftReply}, "persona")
	reader := query.NewReader(&fakeIssues{all: []core.Issue{
		blockedIssue(),
		// an in-flight issue whose slice (specs/export.md) includes the edited path → reissued.
		{ID: "harness-3", Status: "in_progress", Role: "implementor", Spec: "specs/export.md", SpecHash: "sha256:pin"},
	}}, fakeArts{}, fakeProv{})
	s := New(Options{Planner: p, Reader: reader, Resolver: &fakeResolver{}, Repo: resolveRepo(t), SpecDepth: 0})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	// NewResolve auto-opens a turn; the scripted draftReply gives the session a spec edit.
	sess := p.NewResolve(wizard.ResolveSeed{IssueID: "harness-9", Spec: "specs/export.md"})
	waitFor(t, func() bool { return !sess.Busy() && !sess.Draft().Empty() }, "resolve draft not produced")

	r := get(t, ts, "/resolve/blast/"+sess.ID)
	if r.status != http.StatusOK {
		t.Fatalf("/resolve/blast status = %d, want 200", r.status)
	}
	for _, want := range []string{
		"specs/export.md",  // the proposed spec edit
		"Blast radius",     // the preview heading
		"harness-3",        // the in-flight item that would be reissued
		`hx-post="/resolve/approve"`, // the resolve consent gate
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("blast panel missing %q\nbody: %s", want, r.body)
		}
	}

	if u := get(t, ts, "/resolve/blast/deadbeef"); u.status != http.StatusNotFound {
		t.Errorf("/resolve/blast unknown session = %d, want 404", u.status)
	}
}

// TestResolveApprove proves the Resolve consent gate commits the SERVER-SIDE draft against the
// issue the server bound at mint (sess.ResolveIssue) and renders the outcome (the reopened issue
// link), plus the degenerate guards.
func TestResolveApprove(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		resolver := &fakeResolver{res: wizard.ResolveResult{Commit: "abc123def4567", ReopenedIssue: "harness-9"}}
		p := wizard.NewPlanner(scriptedAdapter{reply: draftReply}, "persona")
		reader := query.NewReader(&fakeIssues{all: []core.Issue{blockedIssue()}}, fakeArts{}, fakeProv{})
		s := New(Options{Planner: p, Reader: reader, Resolver: resolver, Repo: resolveRepo(t)})
		ts := httptest.NewServer(s.Handler())
		t.Cleanup(ts.Close)

		sess := p.NewResolve(wizard.ResolveSeed{IssueID: "harness-9", Spec: "specs/export.md"})
		waitFor(t, func() bool { return !sess.Busy() && !sess.Draft().Empty() }, "resolve draft not produced")

		pr, err := http.PostForm(ts.URL+"/resolve/approve", url.Values{"session": {sess.ID}})
		if err != nil {
			t.Fatalf("POST resolve/approve: %v", err)
		}
		data, _ := io.ReadAll(pr.Body)
		_ = pr.Body.Close()
		if pr.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", pr.StatusCode)
		}
		if !strings.Contains(string(data), "spec refined") || !strings.Contains(string(data), `/issue/harness-9`) {
			t.Errorf("resolve result missing the reopened-issue link: %s", string(data))
		}
		// The resolver got the server-bound issue id and the planner's drafted spec — never browser content.
		if resolver.got == nil || resolver.got.IssueID != "harness-9" {
			t.Fatalf("resolver got wrong issue id: %+v", resolver.got)
		}
		if len(resolver.got.Specs) != 1 || resolver.got.Specs[0].Path != "specs/export.md" {
			t.Errorf("resolver got wrong specs: %+v", resolver.got.Specs)
		}
	})

	t.Run("no resolver", func(t *testing.T) {
		p := wizard.NewPlanner(scriptedAdapter{reply: draftReply}, "persona")
		reader := query.NewReader(&fakeIssues{all: []core.Issue{blockedIssue()}}, fakeArts{}, fakeProv{})
		s := New(Options{Planner: p, Reader: reader, Repo: resolveRepo(t)}) // no Resolver
		ts := httptest.NewServer(s.Handler())
		t.Cleanup(ts.Close)
		sess := p.NewResolve(wizard.ResolveSeed{IssueID: "harness-9", Spec: "specs/export.md"})
		waitFor(t, func() bool { return !sess.Busy() }, "opening turn never settled")

		pr, _ := http.PostForm(ts.URL+"/resolve/approve", url.Values{"session": {sess.ID}})
		data, _ := io.ReadAll(pr.Body)
		_ = pr.Body.Close()
		if pr.StatusCode != http.StatusOK || !strings.Contains(string(data), "Resolve is unavailable") {
			t.Errorf("no-resolver = %d %q, want 200 with resolve-unavailable notice", pr.StatusCode, string(data))
		}
	})

	t.Run("unknown session", func(t *testing.T) {
		p := wizard.NewPlanner(scriptedAdapter{reply: draftReply}, "persona")
		s := New(Options{Planner: p, Resolver: &fakeResolver{}})
		ts := httptest.NewServer(s.Handler())
		t.Cleanup(ts.Close)
		pr, _ := http.PostForm(ts.URL+"/resolve/approve", url.Values{"session": {"deadbeef"}})
		_ = pr.Body.Close()
		if pr.StatusCode != http.StatusNotFound {
			t.Errorf("unknown session = %d, want 404", pr.StatusCode)
		}
	})
}
