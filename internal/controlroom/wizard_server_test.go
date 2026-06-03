package controlroom

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Loxstomper/harness/internal/controlroom/wizard"
	"github.com/Loxstomper/harness/internal/model"
)

// scriptedAdapter is a trivial model.Adapter for the wizard server tests: it streams one
// fixed reply. It keeps the handler tests free of a network round-trip while still driving
// the real wizard conversation loop.
type scriptedAdapter struct{ reply string }

func (a scriptedAdapter) Complete(_ context.Context, _ model.Request, onEvent model.StreamHandler) (model.Response, error) {
	if onEvent != nil {
		onEvent(model.StreamEvent{TextDelta: a.reply})
	}
	return model.Response{Text: a.reply, Stop: model.StopEndTurn}, nil
}

func wizardServer(t *testing.T, reply string) (*httptest.Server, *wizard.Planner) {
	t.Helper()
	p := wizard.NewPlanner(scriptedAdapter{reply: reply}, "persona", wizard.WithTurnTimeout(5*time.Second))
	s := New(Options{Planner: p})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, p
}

// TestCreateNotConfigured proves the wizard degrades gracefully with no planner wired (a
// standalone `harness serve`, or a config without requirements_planner): the page renders a
// notice inside the chrome (200, never a dead form) and the data endpoints answer 503/4xx
// rather than hanging or 500ing.
func TestCreateNotConfigured(t *testing.T) {
	s := New(Options{})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	r := get(t, ts, "/create")
	if r.status != http.StatusOK {
		t.Fatalf("/create status = %d, want 200", r.status)
	}
	if !strings.Contains(r.body, "not configured") {
		t.Errorf("/create missing not-configured notice, got: %s", r.body)
	}
	if !strings.Contains(r.body, `href="/static/app.css"`) {
		t.Errorf("/create not wrapped in the base layout")
	}

	if frag := get(t, ts, "/create/messages/anything"); frag.status != http.StatusServiceUnavailable {
		t.Errorf("/create/messages status = %d, want 503", frag.status)
	}
	if stream := get(t, ts, "/create/stream/anything"); stream.status != http.StatusServiceUnavailable {
		t.Errorf("/create/stream status = %d, want 503", stream.status)
	}
	if led := get(t, ts, "/create/ledger/anything"); led.status != http.StatusServiceUnavailable {
		t.Errorf("/create/ledger status = %d, want 503", led.status)
	}
}

// TestCreateRendersPageAndSession proves a wired wizard renders the conversation page with a
// live SSE-connected transcript bound to a concrete session, and the empty-state prompt.
func TestCreateRendersPageAndSession(t *testing.T) {
	ts, _ := wizardServer(t, "a reply")

	r := get(t, ts, "/create")
	if r.status != http.StatusOK {
		t.Fatalf("/create status = %d, want 200", r.status)
	}
	for _, want := range []string{
		`hx-ext="sse"`,         // the live SSE wiring
		"sse-connect=",         // bound to this session's stream
		`hx-post="/create/message"`, // the turn form
		"No messages yet",      // the empty-state prompt
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("/create missing %q\nbody: %s", want, r.body)
		}
	}
}

// TestCreateMessageRoundTrip proves the action surface works end to end: POSTing a message
// records it (the returned transcript fragment shows the human's prompt at once, bare with no
// chrome for the htmx swap), an unknown session 404s, and the per-session SSE stream delivers
// the reply's `delta` and `turn` events to the connected browser.
func TestCreateMessageRoundTrip(t *testing.T) {
	const reply = "Should it reject <empty> rows? Give one example."
	ts, p := wizardServer(t, reply)
	sess := p.New()

	// Open the session's SSE stream first so no event is missed.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/create/stream/"+sess.ID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}
	waitFor(t, func() bool { return sess.Hub().Len() == 1 }, "browser did not subscribe to the stream")

	// POST a message; the fragment must echo the user's prompt, bare (no full-page chrome).
	form := url.Values{"session": {sess.ID}, "text": {"import a CSV of orders"}}
	pr, err := http.PostForm(ts.URL+"/create/message", form)
	if err != nil {
		t.Fatalf("POST /create/message: %v", err)
	}
	data, err := io.ReadAll(pr.Body)
	_ = pr.Body.Close()
	if err != nil {
		t.Fatalf("read POST body: %v", err)
	}
	body := string(data)
	if pr.StatusCode != http.StatusOK {
		t.Fatalf("POST status = %d, want 200", pr.StatusCode)
	}
	if strings.Contains(strings.ToLower(body), "<!doctype html>") {
		t.Errorf("message fragment should be bare, not a full page: %s", body)
	}
	if !strings.Contains(body, "import a CSV of orders") {
		t.Errorf("fragment missing the just-sent user message: %s", body)
	}

	// The reply streams over the session stream: collect frames until the `turn` event.
	names := collectSSE(t, resp, "turn", 5*time.Second)
	if !names["delta"] {
		t.Error("no delta event on the session stream — the reply did not stream")
	}
	if !names["turn"] {
		t.Error("no turn event on the session stream — the reply did not finalize")
	}

	// Unknown session is a 404 on both data endpoints.
	if u := get(t, ts, "/create/messages/deadbeef"); u.status != http.StatusNotFound {
		t.Errorf("/create/messages unknown session = %d, want 404", u.status)
	}
	if u := get(t, ts, "/create/stream/deadbeef"); u.status != http.StatusNotFound {
		t.Errorf("/create/stream unknown session = %d, want 404", u.status)
	}

	// After the turn, the transcript fragment carries the finalized assistant reply.
	waitFor(t, func() bool { return !sess.Busy() }, "turn did not complete")
	frag := get(t, ts, "/create/messages/"+sess.ID)
	if frag.status != http.StatusOK {
		t.Fatalf("/create/messages status = %d, want 200", frag.status)
	}
	if !strings.Contains(frag.body, "import a CSV of orders") {
		t.Errorf("transcript missing the user message: %s", frag.body)
	}
	// templ escapes the assistant text at render time, so the angle brackets appear escaped.
	if !strings.Contains(frag.body, "reject &lt;empty&gt; rows") {
		t.Errorf("transcript missing the finalized (escaped) assistant reply: %s", frag.body)
	}
}

// TestCreateLedgerRendersPanel proves GET /create/ledger/{session} renders the alignment-
// ledger fragment: the empty state before any turn, and — after driving a ledger-bearing turn
// through the planner — the titled panel with the question and its option chips. An unknown
// session 404s.
func TestCreateLedgerRendersPanel(t *testing.T) {
	const reply = "Where we stand.\n```ledger\n" +
		`[{"question":"Which datastore?","status":"open","rationale":"Driven by query shape.",` +
		`"options":[{"label":"Postgres","tradeoff":"mature ops","selected":false},` +
		`{"label":"SQLite","tradeoff":"single-node","selected":false}]}]` +
		"\n```"
	ts, p := wizardServer(t, reply)
	sess := p.New()

	// Empty state before any turn.
	empty := get(t, ts, "/create/ledger/"+sess.ID)
	if empty.status != http.StatusOK {
		t.Fatalf("ledger status = %d, want 200", empty.status)
	}
	if !strings.Contains(empty.body, "decisions appear here") {
		t.Errorf("empty ledger missing the invitation, got: %s", empty.body)
	}

	// Drive a ledger-bearing turn, then the panel shows the parsed item + chips.
	if !sess.Send("pick a datastore") {
		t.Fatal("Send returned false")
	}
	waitFor(t, func() bool { return !sess.Busy() && len(sess.Ledger()) == 1 }, "ledger not seeded")

	frag := get(t, ts, "/create/ledger/"+sess.ID)
	if frag.status != http.StatusOK {
		t.Fatalf("ledger status = %d, want 200", frag.status)
	}
	for _, want := range []string{
		"Alignment ledger",        // the titled panel
		"Which datastore?",        // the question
		"Postgres",                // a chip label
		"SQLite",                  // the other chip
		`hx-post="/create/ledger/answer"`, // the batch form funnels through the planner
		`name="opt-0"`,            // the per-fork chip radios
		`name="text-0"`,           // the first-class free-text box
		`name="discuss-0"`,        // the "let's discuss" flag
		"Submit answers",          // the batch submit button
	} {
		if !strings.Contains(frag.body, want) {
			t.Errorf("ledger panel missing %q\nbody: %s", want, frag.body)
		}
	}

	if u := get(t, ts, "/create/ledger/deadbeef"); u.status != http.StatusNotFound {
		t.Errorf("/create/ledger unknown session = %d, want 404", u.status)
	}
}

// TestCreateLedgerAnswerRecordsTurn proves POST /create/ledger/answer funnels a batch of fork
// answers through the planner (T4.27) — recording one enumerated user turn and returning the
// transcript fragment — and that an unknown session 404s.
func TestCreateLedgerAnswerRecordsTurn(t *testing.T) {
	const reply = "Where we stand.\n```ledger\n" +
		`[{"question":"Which datastore?","status":"open","options":[` +
		`{"label":"Postgres","tradeoff":"mature ops","selected":false}]},` +
		`{"question":"Auth in v1?","status":"open","options":[]}]` +
		"\n```"
	ts, p := wizardServer(t, reply)
	sess := p.New()

	// Seed the ledger so there are forks to answer.
	if !sess.Send("design it") {
		t.Fatal("Send returned false")
	}
	waitFor(t, func() bool { return !sess.Busy() && len(sess.Ledger()) == 2 }, "ledger not seeded")

	// A batch: chip pick on fork 0, free text on fork 1.
	form := url.Values{"session": {sess.ID}, "opt-0": {"0"}, "text-1": {"Use OAuth."}}
	pr, err := http.PostForm(ts.URL+"/create/ledger/answer", form)
	if err != nil {
		t.Fatalf("POST answer: %v", err)
	}
	data, _ := io.ReadAll(pr.Body)
	_ = pr.Body.Close()
	if pr.StatusCode != http.StatusOK {
		t.Fatalf("answer status = %d, want 200", pr.StatusCode)
	}
	body := string(data)
	if !strings.Contains(body, "I choose: Postgres") || !strings.Contains(body, "Use OAuth.") {
		t.Errorf("transcript fragment missing the batch answers: %s", body)
	}
	waitFor(t, func() bool { return !sess.Busy() }, "answer turn did not complete")

	// Unknown session 404s.
	bad := url.Values{"session": {"deadbeef"}, "opt-0": {"0"}}
	ur, err := http.PostForm(ts.URL+"/create/ledger/answer", bad)
	if err != nil {
		t.Fatalf("POST answer unknown: %v", err)
	}
	_ = ur.Body.Close()
	if ur.StatusCode != http.StatusNotFound {
		t.Errorf("answer unknown session = %d, want 404", ur.StatusCode)
	}
}

// fakeSeeder is a wizard.Seeder stub for the approve-handler tests: it records the request it
// was handed and returns a canned result or error, so the handler is tested without touching git,
// beads, or the artifact store (those are exercised by the cmd-side seeder integration test).
type fakeSeeder struct {
	got *wizard.SeedRequest
	res wizard.SeedResult
	err error
}

func (f *fakeSeeder) Seed(_ context.Context, req wizard.SeedRequest) (wizard.SeedResult, error) {
	f.got = &req
	return f.res, f.err
}

// draftReply is a scripted planner turn that proposes a spec + one seed issue (the JSON uses
// raw-string \n so the embedded block is valid JSON).
const draftReply = "Ready to build.\n```draft\n" +
	`{"summary":"CSV export","specs":[{"path":"specs/export.md","content":"# Export\n\nBody.\n"}],` +
	`"issues":[{"title":"Add CSV export","body":"Build it.","spec":"specs/export.md"}]}` +
	"\n```"

// discussReply carries both a draft and a ledger with an item the human flagged `discussing` —
// the soft approval gate (T4.27) must refuse to commit while it stands.
const discussReply = "Let's settle this.\n```ledger\n" +
	`[{"question":"Which datastore?","status":"discussing","rationale":"Need the scale target."}]` +
	"\n```\n```draft\n" +
	`{"summary":"X","specs":[{"path":"specs/x.md","content":"# X\n\nB.\n"}],"issues":[{"title":"Do X","spec":"specs/x.md"}]}` +
	"\n```"

// deferReply carries a draft and a ledger with one agreed + one still-open fork — approval must
// succeed, auto-deferring the open fork and recording both in the decisions handed to the Seeder.
const deferReply = "Ready.\n```ledger\n" +
	`[{"question":"Auth in v1?","status":"agreed","rationale":"Out of scope."},` +
	`{"question":"Caching?","status":"open","rationale":"Punt to v2."}]` +
	"\n```\n```draft\n" +
	`{"summary":"X","specs":[{"path":"specs/x.md","content":"# X\n\nB.\n"}],"issues":[{"title":"Do X","spec":"specs/x.md"}]}` +
	"\n```"

// TestCreateDraftRendersPanel proves GET /create/draft/{session} renders the draft fragment: the
// empty invitation before the planner proposes anything, and — after a draft-bearing turn — the
// proposed spec, the seed issue, and the Approve button. An unknown session 404s.
func TestCreateDraftRendersPanel(t *testing.T) {
	p := wizard.NewPlanner(scriptedAdapter{reply: draftReply}, "persona", wizard.WithTurnTimeout(5*time.Second))
	s := New(Options{Planner: p, Seeder: &fakeSeeder{}})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	sess := p.New()

	empty := get(t, ts, "/create/draft/"+sess.ID)
	if empty.status != http.StatusOK {
		t.Fatalf("draft status = %d, want 200", empty.status)
	}
	if !strings.Contains(empty.body, "appear here") {
		t.Errorf("empty draft missing the invitation: %s", empty.body)
	}

	if !sess.Send("build a CSV exporter") {
		t.Fatal("Send returned false")
	}
	waitFor(t, func() bool { return !sess.Busy() && !sess.Draft().Empty() }, "draft not produced")

	frag := get(t, ts, "/create/draft/"+sess.ID)
	if frag.status != http.StatusOK {
		t.Fatalf("draft status = %d, want 200", frag.status)
	}
	for _, want := range []string{
		"specs/export.md",        // the proposed spec path
		"Add CSV export",         // the seed issue title
		`hx-post="/create/approve"`, // the consent-gate form
		"Approve",                // the button
	} {
		if !strings.Contains(frag.body, want) {
			t.Errorf("draft panel missing %q\nbody: %s", want, frag.body)
		}
	}

	if u := get(t, ts, "/create/draft/deadbeef"); u.status != http.StatusNotFound {
		t.Errorf("/create/draft unknown session = %d, want 404", u.status)
	}
}

// TestCreateApproveSuccess proves the consent gate commits the SERVER-SIDE draft through the
// Seeder and renders the outcome: the created issue (linking to its detail page) and the commit.
// The Seeder receives exactly the planner's drafted spec + issues — never browser content.
func TestCreateApproveSuccess(t *testing.T) {
	seeder := &fakeSeeder{res: wizard.SeedResult{
		Commit: "abc123def4567",
		Issues: []wizard.SeededIssue{{ID: "harness-7", Title: "Add CSV export", Role: "planner"}},
	}}
	p := wizard.NewPlanner(scriptedAdapter{reply: draftReply}, "persona", wizard.WithTurnTimeout(5*time.Second))
	s := New(Options{Planner: p, Seeder: seeder})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	sess := p.New()

	if !sess.Send("build a CSV exporter") {
		t.Fatal("Send returned false")
	}
	waitFor(t, func() bool { return !sess.Busy() && !sess.Draft().Empty() }, "draft not produced")

	pr, err := http.PostForm(ts.URL+"/create/approve", url.Values{"session": {sess.ID}})
	if err != nil {
		t.Fatalf("POST approve: %v", err)
	}
	data, _ := io.ReadAll(pr.Body)
	_ = pr.Body.Close()
	if pr.StatusCode != http.StatusOK {
		t.Fatalf("approve status = %d, want 200", pr.StatusCode)
	}
	body := string(data)
	if !strings.Contains(body, "work seeded") || !strings.Contains(body, "harness-7") || !strings.Contains(body, `/issue/harness-7`) {
		t.Errorf("approve result missing the seeded issue link: %s", body)
	}

	// The Seeder was handed the planner's drafted spec + issue, verbatim.
	if seeder.got == nil {
		t.Fatal("Seeder was not called")
	}
	if len(seeder.got.Specs) != 1 || seeder.got.Specs[0].Path != "specs/export.md" {
		t.Errorf("Seeder got wrong specs: %+v", seeder.got.Specs)
	}
	if len(seeder.got.Issues) != 1 || seeder.got.Issues[0].Title != "Add CSV export" {
		t.Errorf("Seeder got wrong issues: %+v", seeder.got.Issues)
	}
	if len(seeder.got.Transcript) == 0 {
		t.Error("Seeder got no transcript")
	}
}

// TestCreateApproveGuards proves the consent gate's degenerate paths: no Seeder → an
// approval-unavailable notice (not a 500); an empty draft → a "nothing to approve" notice
// (Seeder never called); a Seeder error → the failure surfaced in-fragment; an unknown session
// 404s.
func TestCreateApproveGuards(t *testing.T) {
	t.Run("no seeder", func(t *testing.T) {
		ts, p := wizardServer(t, draftReply) // wizardServer wires no Seeder
		sess := p.New()
		pr, err := http.PostForm(ts.URL+"/create/approve", url.Values{"session": {sess.ID}})
		if err != nil {
			t.Fatalf("POST approve: %v", err)
		}
		data, _ := io.ReadAll(pr.Body)
		_ = pr.Body.Close()
		if pr.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", pr.StatusCode)
		}
		if !strings.Contains(string(data), "Approval is unavailable") {
			t.Errorf("missing approval-unavailable notice: %s", string(data))
		}
	})

	t.Run("empty draft", func(t *testing.T) {
		seeder := &fakeSeeder{}
		p := wizard.NewPlanner(scriptedAdapter{reply: "just chatting"}, "persona", wizard.WithTurnTimeout(5*time.Second))
		s := New(Options{Planner: p, Seeder: seeder})
		ts := httptest.NewServer(s.Handler())
		t.Cleanup(ts.Close)
		sess := p.New() // no draft turn driven

		pr, err := http.PostForm(ts.URL+"/create/approve", url.Values{"session": {sess.ID}})
		if err != nil {
			t.Fatalf("POST approve: %v", err)
		}
		data, _ := io.ReadAll(pr.Body)
		_ = pr.Body.Close()
		if !strings.Contains(string(data), "Nothing to approve") {
			t.Errorf("missing nothing-to-approve notice: %s", string(data))
		}
		if seeder.got != nil {
			t.Error("Seeder was called for an empty draft")
		}
	})

	t.Run("seeder error", func(t *testing.T) {
		seeder := &fakeSeeder{err: errSeed}
		p := wizard.NewPlanner(scriptedAdapter{reply: draftReply}, "persona", wizard.WithTurnTimeout(5*time.Second))
		s := New(Options{Planner: p, Seeder: seeder})
		ts := httptest.NewServer(s.Handler())
		t.Cleanup(ts.Close)
		sess := p.New()
		if !sess.Send("build it") {
			t.Fatal("Send returned false")
		}
		waitFor(t, func() bool { return !sess.Busy() && !sess.Draft().Empty() }, "draft not produced")

		pr, err := http.PostForm(ts.URL+"/create/approve", url.Values{"session": {sess.ID}})
		if err != nil {
			t.Fatalf("POST approve: %v", err)
		}
		data, _ := io.ReadAll(pr.Body)
		_ = pr.Body.Close()
		if !strings.Contains(string(data), "Could not commit") || !strings.Contains(string(data), "broken link") {
			t.Errorf("missing surfaced seeder error: %s", string(data))
		}
	})

	t.Run("unknown session", func(t *testing.T) {
		p := wizard.NewPlanner(scriptedAdapter{reply: draftReply}, "persona")
		s := New(Options{Planner: p, Seeder: &fakeSeeder{}})
		ts := httptest.NewServer(s.Handler())
		t.Cleanup(ts.Close)
		pr, err := http.PostForm(ts.URL+"/create/approve", url.Values{"session": {"deadbeef"}})
		if err != nil {
			t.Fatalf("POST approve: %v", err)
		}
		_ = pr.Body.Close()
		if pr.StatusCode != http.StatusNotFound {
			t.Errorf("unknown session = %d, want 404", pr.StatusCode)
		}
	})
}

// TestCreateApproveSoftGate proves the T4.27 soft approval gate: an item still `discussing`
// blocks the commit (the Seeder is never called and the flagged question is named), while plain
// `open` forks are auto-deferred and recorded — the Seeder receives both the agreed decision and
// the auto-deferred open fork (marked Deferred), nothing silently dropped.
func TestCreateApproveSoftGate(t *testing.T) {
	t.Run("discussing blocks", func(t *testing.T) {
		seeder := &fakeSeeder{}
		p := wizard.NewPlanner(scriptedAdapter{reply: discussReply}, "persona", wizard.WithTurnTimeout(5*time.Second))
		s := New(Options{Planner: p, Seeder: seeder})
		ts := httptest.NewServer(s.Handler())
		t.Cleanup(ts.Close)
		sess := p.New()
		if !sess.Send("go") {
			t.Fatal("Send returned false")
		}
		waitFor(t, func() bool { return !sess.Busy() && !sess.Draft().Empty() && len(sess.Ledger()) == 1 }, "draft/ledger not produced")

		pr, err := http.PostForm(ts.URL+"/create/approve", url.Values{"session": {sess.ID}})
		if err != nil {
			t.Fatalf("POST approve: %v", err)
		}
		data, _ := io.ReadAll(pr.Body)
		_ = pr.Body.Close()
		body := string(data)
		if !strings.Contains(body, "under discussion") || !strings.Contains(body, "Which datastore?") {
			t.Errorf("missing discussing-blocked notice naming the fork: %s", body)
		}
		if seeder.got != nil {
			t.Error("Seeder was called despite a discussing item blocking approval")
		}
	})

	t.Run("auto-defers open", func(t *testing.T) {
		seeder := &fakeSeeder{res: wizard.SeedResult{Commit: "abc1234"}}
		p := wizard.NewPlanner(scriptedAdapter{reply: deferReply}, "persona", wizard.WithTurnTimeout(5*time.Second))
		s := New(Options{Planner: p, Seeder: seeder})
		ts := httptest.NewServer(s.Handler())
		t.Cleanup(ts.Close)
		sess := p.New()
		if !sess.Send("go") {
			t.Fatal("Send returned false")
		}
		waitFor(t, func() bool { return !sess.Busy() && !sess.Draft().Empty() && len(sess.Ledger()) == 2 }, "draft/ledger not produced")

		pr, err := http.PostForm(ts.URL+"/create/approve", url.Values{"session": {sess.ID}})
		if err != nil {
			t.Fatalf("POST approve: %v", err)
		}
		data, _ := io.ReadAll(pr.Body)
		_ = pr.Body.Close()
		if pr.StatusCode != http.StatusOK {
			t.Fatalf("approve status = %d, want 200", pr.StatusCode)
		}
		if !strings.Contains(string(data), "work seeded") {
			t.Errorf("approve with only open forks should succeed (soft gate): %s", string(data))
		}
		if seeder.got == nil {
			t.Fatal("Seeder was not called")
		}
		var agreed, deferred bool
		for _, d := range seeder.got.Decisions {
			if d.Point == "Auth in v1?" && !d.Deferred {
				agreed = true
			}
			if d.Point == "Caching?" && d.Deferred {
				deferred = true
			}
		}
		if !agreed || !deferred {
			t.Errorf("decisions = %+v, want the agreed fork + the auto-deferred open fork", seeder.got.Decisions)
		}
	})
}

// errSeed is a stand-in seeder failure carrying a representative validation message.
var errSeed = errSeedErr("spec \"specs/x.md\" has a broken link to \"specs/missing.md\"")

type errSeedErr string

func (e errSeedErr) Error() string { return string(e) }

// collectSSE reads SSE `event:` lines from a stream until the named terminal event is seen
// or the deadline passes, returning the set of event names observed.
func collectSSE(t *testing.T, resp *http.Response, until string, timeout time.Duration) map[string]bool {
	t.Helper()
	names := make(map[string]bool)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r := bufio.NewReader(resp.Body)
		for {
			line, err := r.ReadString('\n')
			if name, ok := strings.CutPrefix(strings.TrimSpace(line), "event:"); ok {
				n := strings.TrimSpace(name)
				names[n] = true
				if n == until {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for SSE %q event; saw %v", until, names)
	}
	return names
}
