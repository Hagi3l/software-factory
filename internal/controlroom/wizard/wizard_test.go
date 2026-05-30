package wizard_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/controlroom/wizard"
	"github.com/Loxstomper/harness/internal/model"
	"github.com/Loxstomper/harness/internal/model/modeltest"
	"github.com/Loxstomper/harness/internal/model/registry"
)

// fakeAdapter is a minimal model.Adapter the busy/blank tests drive directly (no network):
// it blocks on release until the test lets the reply land, so the in-flight window is
// deterministic — which the modeltest round-trip is too fast to observe.
type fakeAdapter struct {
	release chan struct{}
	reply   string
	err     error
	calls   int
}

func (a *fakeAdapter) Complete(_ context.Context, _ model.Request, onEvent model.StreamHandler) (model.Response, error) {
	a.calls++
	if a.release != nil {
		<-a.release
	}
	if a.err != nil {
		return model.Response{}, a.err
	}
	if onEvent != nil {
		onEvent(model.StreamEvent{TextDelta: a.reply})
	}
	return model.Response{Text: a.reply, Stop: model.StopEndTurn}, nil
}

// newCompatAdapter builds the real OpenAI adapter pointed at a modeltest server, exercising
// the production model layer end-to-end (the same posture the spine test takes) so the
// streaming path is the genuine wire contract, not a stub.
func newCompatAdapter(t *testing.T, url string) model.Adapter {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "test-key")
	reg, err := registry.New(map[string]config.ModelProvider{
		"fake": {Provider: config.ProviderOpenAICompat, Endpoint: url},
	})
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	a, err := reg.Adapter("fake")
	if err != nil {
		t.Fatalf("registry.Adapter: %v", err)
	}
	return a
}

// TestSendStreamsAndRecords is the conversation loop's core contract: a sent message drives
// one trusted model turn whose growing reply is broadcast as a `delta` SSE event (HTML-
// escaped, so untrusted-looking model markup is inert on the wire) and finalized with a
// `turn` nudge, after which the transcript carries the user prompt and the assistant reply
// verbatim. It runs against the real OpenAI adapter via modeltest, so it pins the actual
// streaming behavior the canonical model layer provides.
func TestSendStreamsAndRecords(t *testing.T) {
	const reply = "Good — should it reject <empty> input & malformed payloads? Give one example."
	srv := modeltest.NewServer(t, []modeltest.Turn{{Text: reply}})
	p := wizard.NewPlanner(newCompatAdapter(t, srv.URL()), "you are the requirements planner",
		wizard.WithTurnTimeout(10*time.Second))
	sess := p.New()

	// Subscribe before sending so no streamed event can be missed.
	sub, cancel := sess.Hub().Subscribe()
	defer cancel()

	if !sess.Send("build me a CSV importer") {
		t.Fatal("Send returned false for a fresh, non-blank message")
	}

	var sawDelta, sawTurn bool
	var lastDelta string
	deadline := time.After(5 * time.Second)
	for !sawTurn {
		select {
		case ev := <-sub:
			switch ev.Name {
			case "delta":
				sawDelta = true
				lastDelta = ev.Data
			case "turn":
				sawTurn = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for the reply to stream and complete")
		}
	}

	if !sawDelta {
		t.Error("no delta event observed — the reply did not stream")
	}
	// The streamed delta is HTML-escaped (it is swapped into the DOM as innerHTML).
	if !strings.Contains(lastDelta, "&lt;empty&gt;") || !strings.Contains(lastDelta, "&amp;") {
		t.Errorf("delta is not HTML-escaped: %q", lastDelta)
	}

	msgs := sess.Messages()
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (user + assistant): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || msgs[0].Text != "build me a CSV importer" {
		t.Errorf("first message = %+v, want the user prompt", msgs[0])
	}
	// The recorded transcript keeps the RAW model text (templ escapes at render time); the
	// escaping is only on the SSE wire above.
	if msgs[1].Role != "assistant" || msgs[1].Text != reply {
		t.Errorf("second message = %+v, want the assistant reply verbatim", msgs[1])
	}
	if sess.Busy() {
		t.Error("session still busy after the turn completed")
	}
	if srv.Requests() != 1 {
		t.Errorf("model requests = %d, want exactly 1 turn", srv.Requests())
	}
}

// TestSendRejectsConcurrentTurn proves at most one reply turn runs at a time: a second Send
// while one is in flight records nothing and returns false, so a human cannot interleave
// turns and corrupt the conversation order. A blocking adapter makes the in-flight window
// deterministic.
func TestSendRejectsConcurrentTurn(t *testing.T) {
	a := &fakeAdapter{release: make(chan struct{}), reply: "a reply long enough to stream"}
	p := wizard.NewPlanner(a, "persona")
	sess := p.New()

	if !sess.Send("first message") {
		t.Fatal("first Send returned false")
	}
	if !sess.Busy() {
		t.Fatal("session not marked busy after the first Send")
	}
	if sess.Send("second message while busy") {
		t.Error("second Send returned true while a turn was in flight")
	}
	if got := len(sess.Messages()); got != 1 {
		t.Errorf("messages = %d, want 1 (the rejected message must not be recorded)", got)
	}

	close(a.release) // let the first turn complete
	waitFor(t, func() bool { return !sess.Busy() }, "first turn did not complete")

	msgs := sess.Messages()
	if len(msgs) != 2 || msgs[1].Role != "assistant" {
		t.Fatalf("after completion want [user, assistant], got %+v", msgs)
	}
	if a.calls != 1 {
		t.Errorf("adapter called %d times, want 1 (the rejected turn must not dispatch)", a.calls)
	}
}

// TestSendIgnoresBlank proves a blank or whitespace-only message is a no-op: it neither
// records a turn nor dispatches the model, so an accidental empty submit does nothing.
func TestSendIgnoresBlank(t *testing.T) {
	a := &fakeAdapter{reply: "unused"}
	sess := wizard.NewPlanner(a, "persona").New()
	if sess.Send("") || sess.Send("   \n\t ") {
		t.Error("Send accepted a blank message")
	}
	if len(sess.Messages()) != 0 {
		t.Error("a blank message was recorded")
	}
	if a.calls != 0 {
		t.Error("a blank message dispatched the model")
	}
}

// TestErrorTurnDoesNotWedge proves a failed model turn still finalizes: busy clears, a `turn`
// nudge fires, and the transcript carries an assistant error note rather than leaving the
// session stuck mid-turn forever.
func TestErrorTurnDoesNotWedge(t *testing.T) {
	a := &fakeAdapter{err: errors.New("provider unavailable")}
	p := wizard.NewPlanner(a, "persona", wizard.WithTurnTimeout(5*time.Second))
	sess := p.New()
	sub, cancel := sess.Hub().Subscribe()
	defer cancel()

	if !sess.Send("anything") {
		t.Fatal("Send returned false")
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-sub:
			if ev.Name == "turn" {
				goto done
			}
		case <-deadline:
			t.Fatal("no turn nudge after a failed model turn — the session wedged")
		}
	}
done:
	waitFor(t, func() bool { return !sess.Busy() }, "session stayed busy after a failed turn")
	msgs := sess.Messages()
	if len(msgs) != 2 || msgs[1].Role != "assistant" || !strings.Contains(strings.ToLower(msgs[1].Text), "error") {
		t.Fatalf("want an assistant error note after a failed turn, got %+v", msgs)
	}
}

// TestNewSessionsUniqueAndBounded proves session ids are distinct and the in-memory map is
// bounded: past the cap the oldest session is evicted (best-effort working state, not a
// durable record — the durable transcript lands on APPROVE in T4.14).
func TestNewSessionsUniqueAndBounded(t *testing.T) {
	a := &fakeAdapter{reply: "x"}
	p := wizard.NewPlanner(a, "persona", wizard.WithMaxSessions(2))
	s1 := p.New()
	s2 := p.New()
	s3 := p.New()

	if s1.ID == s2.ID || s2.ID == s3.ID || s1.ID == s3.ID {
		t.Errorf("session ids collided: %q %q %q", s1.ID, s2.ID, s3.ID)
	}
	if p.Get(s1.ID) != nil {
		t.Error("oldest session was not evicted past the cap")
	}
	if p.Get(s2.ID) == nil || p.Get(s3.ID) == nil {
		t.Error("a within-cap session was lost")
	}
	if p.Get("never-created") != nil {
		t.Error("Get returned a session for an unknown id")
	}
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met: %s", msg)
}
