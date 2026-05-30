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
