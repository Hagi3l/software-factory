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
	release   chan struct{}
	reply     string
	err       error
	calls     int
	gotSystem string // the System prompt of the last request (so a test can assert grounding)
}

func (a *fakeAdapter) Complete(_ context.Context, req model.Request, onEvent model.StreamHandler) (model.Response, error) {
	a.calls++
	a.gotSystem = req.System
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

// TestTurnElapsedTracksInFlightTurn proves the live activity-line timer source: TurnElapsed is 0
// when no turn is in flight (before the first send and after a turn completes) and counts up the
// in-flight turn's whole seconds while busy. The view anchors the client-ticked "mm:ss" clock off
// this, so a slow turn visibly progresses ("it's working, not hung") — the contract that matters
// is the 0-when-idle / nonzero-while-running split.
func TestTurnElapsedTracksInFlightTurn(t *testing.T) {
	a := &fakeAdapter{release: make(chan struct{}), reply: "a reply long enough to stream"}
	sess := wizard.NewPlanner(a, "persona").New()

	if got := sess.TurnElapsed(); got != 0 {
		t.Errorf("TurnElapsed before any send = %d, want 0", got)
	}
	if !sess.Send("first message") {
		t.Fatal("Send returned false")
	}
	// The turn is blocked on the fake adapter's release; let just over a second pass so the elapsed
	// counter is unambiguously nonzero, then assert it reflects the running turn.
	time.Sleep(1100 * time.Millisecond)
	if got := sess.TurnElapsed(); got < 1 {
		t.Errorf("TurnElapsed while busy ~1.1s = %d, want >= 1", got)
	}

	close(a.release) // let the turn complete
	waitFor(t, func() bool { return !sess.Busy() }, "turn did not complete")
	if got := sess.TurnElapsed(); got != 0 {
		t.Errorf("TurnElapsed after the turn completed = %d, want 0 (no turn in flight)", got)
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

// TestLedgerTurnParsesAndStreamsClean proves a turn whose scripted reply carries a trailing
// ```ledger block: the parsed ledger is stored on the session, the recorded transcript holds
// only the clean prose (no JSON), and the streamed delta never contains the fence — so the raw
// ledger JSON never reaches the browser. It runs through the real adapter via modeltest.
func TestLedgerTurnParsesAndStreamsClean(t *testing.T) {
	const reply = "Here is where we stand.\n\n```ledger\n" +
		`[{"question":"Which datastore?","status":"open","rationale":"Driven by query shape.",` +
		`"options":[{"label":"Postgres","tradeoff":"mature ops","selected":false},` +
		`{"label":"SQLite","tradeoff":"single-node","selected":false}]}]` +
		"\n```"
	srv := modeltest.NewServer(t, []modeltest.Turn{{Text: reply}})
	p := wizard.NewPlanner(newCompatAdapter(t, srv.URL()), "persona", wizard.WithTurnTimeout(10*time.Second))
	sess := p.New()

	sub, cancel := sess.Hub().Subscribe()
	defer cancel()

	if !sess.Send("build me a CSV importer") {
		t.Fatal("Send returned false")
	}

	var lastDelta string
	deadline := time.After(5 * time.Second)
	for {
		done := false
		select {
		case ev := <-sub:
			switch ev.Name {
			case "delta":
				lastDelta = ev.Data
			case "turn":
				done = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for the turn to complete")
		}
		if done {
			break
		}
	}

	// The streamed delta must never carry the fence or the raw JSON.
	if strings.Contains(lastDelta, "ledger") || strings.Contains(lastDelta, "Postgres") || strings.Contains(lastDelta, "```") {
		t.Errorf("delta leaked the ledger block: %q", lastDelta)
	}

	waitFor(t, func() bool { return !sess.Busy() }, "turn did not complete")

	msgs := sess.Messages()
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	if msgs[1].Text != "Here is where we stand." {
		t.Errorf("assistant message = %q, want the clean prose with the block stripped", msgs[1].Text)
	}

	led := sess.Ledger()
	if len(led) != 1 {
		t.Fatalf("ledger = %d items, want 1", len(led))
	}
	if led[0].Question != "Which datastore?" || len(led[0].Options) != 2 {
		t.Errorf("ledger item wrong: %+v", led[0])
	}
}

// TestAnswerBatchFunnelsThroughPlanner proves a batch of fork answers (T4.27) is funneled back
// through the planner as ONE user turn enumerating each answered fork by number (a chip pick, a
// free-text answer, and a "let's discuss" flag together), and dispatches a fresh planner turn.
// Out-of-range / empty answers are skipped. The first turn seeds a multi-fork ledger.
func TestAnswerBatchFunnelsThroughPlanner(t *testing.T) {
	const seed = "Where we stand.\n```ledger\n" +
		`[{"question":"Which datastore?","status":"open","options":[` +
		`{"label":"Postgres","tradeoff":"mature ops","selected":false},` +
		`{"label":"SQLite","tradeoff":"single-node","selected":false}]},` +
		`{"question":"Auth in v1?","status":"open","options":[]},` +
		`{"question":"Rate limiting?","status":"open","options":[]}]` +
		"\n```"
	const after = "Got it.\n```ledger\n" +
		`[{"question":"Which datastore?","status":"agreed","options":[{"label":"Postgres","selected":true}]}]` +
		"\n```"
	srv := modeltest.NewServer(t, []modeltest.Turn{{Text: seed}, {Text: after}})
	p := wizard.NewPlanner(newCompatAdapter(t, srv.URL()), "persona", wizard.WithTurnTimeout(10*time.Second))
	sess := p.New()

	if !sess.Send("design the orders service") {
		t.Fatal("first Send returned false")
	}
	waitFor(t, func() bool { return !sess.Busy() && len(sess.Ledger()) == 3 }, "ledger was not seeded")

	// An empty / all-out-of-range batch is a no-op (no new turn, no new message).
	before := len(sess.Messages())
	if got := sess.Answer(nil); got != "" {
		t.Errorf("empty batch returned %q, want empty", got)
	}
	if got := sess.Answer([]wizard.ForkAnswer{{ItemIdx: 9, OptIdx: 0}}); got != "" {
		t.Errorf("out-of-range batch returned %q, want empty", got)
	}
	if len(sess.Messages()) != before {
		t.Errorf("a no-op Answer recorded a message")
	}

	// A real batch: chip pick on fork 1, free text on fork 2, "let's discuss" on fork 3.
	msg := sess.Answer([]wizard.ForkAnswer{
		{ItemIdx: 0, OptIdx: 0},
		{ItemIdx: 1, OptIdx: -1, Text: "Use the existing OAuth provider."},
		{ItemIdx: 2, OptIdx: -1, Discuss: true, Note: "unsure of the target"},
	})
	for _, want := range []string{
		"Here are my answers to the open forks:",
		`1. "Which datastore?" → I choose: Postgres.`,
		`2. "Auth in v1?" → Use the existing OAuth provider.`,
		`3. "Rate limiting?" → let's discuss: unsure of the target`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("batch message missing %q\ngot: %s", want, msg)
		}
	}
	waitFor(t, func() bool { return !sess.Busy() && len(sess.Messages()) == 4 }, "the batch did not drive a planner turn")

	msgs := sess.Messages()
	if msgs[2].Role != "user" || msgs[2].Text != msg {
		t.Errorf("message[2] = %+v, want the enumerated batch turn", msgs[2])
	}
	if msgs[3].Role != "assistant" || !strings.Contains(msgs[3].Text, "Got it") {
		t.Errorf("message[3] = %+v, want the planner's follow-up", msgs[3])
	}
	if srv.Requests() != 2 {
		t.Errorf("model requests = %d, want 2 (seed + batch)", srv.Requests())
	}
}

// TestDraftTurnParsesAndStreamsClean proves a turn whose reply carries a trailing ```draft block
// (here alongside a ```ledger block): the parsed draft is stored on the session, a `draft` SSE
// nudge fires, the transcript holds only the clean prose, and neither structured block leaks into
// the streamed delta. It runs through the real adapter via modeltest.
func TestDraftTurnParsesAndStreamsClean(t *testing.T) {
	const reply = "I think this is ready to build.\n\n" +
		"```ledger\n" + `[{"question":"Scope?","status":"agreed","rationale":"Export only."}]` + "\n```\n" +
		"```draft\n" +
		`{"summary":"CSV export","specs":[{"path":"specs/export.md","content":"# Export\n\nSpec body.\n"}],` +
		`"issues":[{"title":"Add CSV export","body":"Build it.","spec":"specs/export.md"}]}` +
		"\n```"
	// NB: the \n inside the spec content above are literal backslash-n (raw string), so the
	// embedded JSON is valid — a real newline inside a JSON string would not be.
	srv := modeltest.NewServer(t, []modeltest.Turn{{Text: reply}})
	p := wizard.NewPlanner(newCompatAdapter(t, srv.URL()), "persona", wizard.WithTurnTimeout(10*time.Second))
	sess := p.New()

	sub, cancel := sess.Hub().Subscribe()
	defer cancel()

	if !sess.Send("build me a CSV exporter") {
		t.Fatal("Send returned false")
	}

	// The draft nudge is broadcast last (after turn), so wait for it specifically — observing
	// `turn` first and stopping there would miss the buffered `draft` event.
	var lastDelta string
	var sawTurn, sawDraft bool
	deadline := time.After(5 * time.Second)
	for !sawDraft {
		select {
		case ev := <-sub:
			switch ev.Name {
			case "delta":
				lastDelta = ev.Data
			case "turn":
				sawTurn = true
			case "draft":
				sawDraft = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for the draft nudge")
		}
	}
	if !sawTurn {
		t.Error("turn nudge did not precede the draft nudge")
	}

	// Neither block may leak into the live stream.
	if strings.Contains(lastDelta, "draft") || strings.Contains(lastDelta, "export.md") || strings.Contains(lastDelta, "```") {
		t.Errorf("delta leaked a structured block: %q", lastDelta)
	}

	waitFor(t, func() bool { return !sess.Busy() }, "turn did not complete")

	msgs := sess.Messages()
	if len(msgs) != 2 || msgs[1].Text != "I think this is ready to build." {
		t.Fatalf("assistant message not clean prose: %+v", msgs)
	}

	d := sess.Draft()
	if d.Empty() {
		t.Fatal("draft not stored on the session")
	}
	if d.Summary != "CSV export" || len(d.Specs) != 1 || d.Specs[0].Path != "specs/export.md" {
		t.Errorf("draft specs wrong: %+v", d)
	}
	if len(d.Issues) != 1 || d.Issues[0].Title != "Add CSV export" {
		t.Errorf("draft issues wrong: %+v", d.Issues)
	}

	// The transcript is the replayable JSON of the user/assistant turns (with blocks stripped).
	tr := string(sess.Transcript())
	if !strings.Contains(tr, "build me a CSV exporter") || !strings.Contains(tr, "ready to build") {
		t.Errorf("transcript missing conversation turns: %s", tr)
	}
	if strings.Contains(tr, "export.md") || strings.Contains(tr, "ledger") {
		t.Errorf("transcript leaked a structured block: %s", tr)
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
