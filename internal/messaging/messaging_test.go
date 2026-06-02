package messaging

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

func TestSubjectHelpers(t *testing.T) {
	cases := []struct{ got, want string }{
		{WorkSubject("implement"), "harness.work.implement"},
		{ResultSubject("qa"), "harness.result.qa"},
		{AgentEventsSubject("abc123"), "harness.agent.abc123.events"},
		{ControlSubject("health"), "harness.control.health"},
		{SubjectDLQ, "harness.dlq"},
		{WorkStreamSubjects, "harness.work.>"},
		{ResultStreamSubjects, "harness.result.>"},
		{ControlSubjects, "harness.control.*"},
		{AgentEventsWildcard, "harness.agent.*.events"},
		{IssueStateSubject("abc123"), "harness.issue.abc123.state"},
		{IssueStateWildcard, "harness.issue.*.state"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("subject = %q, want %q", c.got, c.want)
		}
	}
}

// TestIssueIDFromStateSubject proves the inverse of IssueStateSubject round-trips and that
// malformed subjects (the wrong shape, the wildcard itself, an embedded separator, an empty
// id) yield "" — the T4.17 pump relies on this to label each tailed transition.
func TestIssueIDFromStateSubject(t *testing.T) {
	cases := []struct{ subj, want string }{
		{IssueStateSubject("abc123"), "abc123"},
		{"harness.issue.harness-7.state", "harness-7"},
		{"harness.agent.abc.events", ""},
		{"harness.issue..state", ""},
		{IssueStateWildcard, ""}, // the literal "*" carries no id
		{"harness.issue.a.b.state", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := IssueIDFromStateSubject(c.subj); got != c.want {
			t.Errorf("IssueIDFromStateSubject(%q) = %q, want %q", c.subj, got, c.want)
		}
	}
}

// TestAgentIDFromEventSubject proves the inverse of AgentEventsSubject round-trips and
// that malformed subjects (the wrong shape, the wildcard itself, an empty id) yield ""
// rather than a bogus id — the control room relies on this to label tailed events.
func TestAgentIDFromEventSubject(t *testing.T) {
	cases := []struct{ subj, want string }{
		{AgentEventsSubject("abc123"), "abc123"},
		{"harness.agent.deadbeef.events", "deadbeef"},
		{"harness.work.implement", ""},
		{"harness.agent..events", ""},
		{AgentEventsWildcard, ""}, // the literal "*" carries no id
		{"harness.agent.a.b.events", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := AgentIDFromEventSubject(c.subj); got != c.want {
			t.Errorf("AgentIDFromEventSubject(%q) = %q, want %q", c.subj, got, c.want)
		}
	}
}

// startTestServer spins up an embedded in-process server backed by a temp store and
// returns a JetStream handle, registering cleanup. Using the embedded server in
// tests is itself a guarantee that the in-process transport works.
func startTestServer(t *testing.T) jetstream.JetStream {
	t.Helper()
	srv, err := NewEmbeddedServer(ServerConfig{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewEmbeddedServer: %v", err)
	}
	t.Cleanup(srv.Shutdown)

	nc, err := srv.Connect()
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(nc.Close)

	js, err := JetStream(nc)
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	return js
}

func TestSetupStreamsIdempotent(t *testing.T) {
	js := startTestServer(t)
	ctx := context.Background()

	if err := SetupStreams(ctx, js); err != nil {
		t.Fatalf("SetupStreams: %v", err)
	}
	// The orchestrator calls SetupStreams on every start, so a second call must
	// succeed without error.
	if err := SetupStreams(ctx, js); err != nil {
		t.Fatalf("SetupStreams (2nd call): %v", err)
	}

	for name, subj := range map[string]string{
		StreamWork:   WorkStreamSubjects,
		StreamResult: ResultStreamSubjects,
		StreamDLQ:    SubjectDLQ,
	} {
		s, err := js.Stream(ctx, name)
		if err != nil {
			t.Fatalf("Stream(%s): %v", name, err)
		}
		info, err := s.Info(ctx)
		if err != nil {
			t.Fatalf("Stream(%s).Info: %v", name, err)
		}
		if len(info.Config.Subjects) != 1 || info.Config.Subjects[0] != subj {
			t.Errorf("stream %s subjects = %v, want [%q]", name, info.Config.Subjects, subj)
		}
	}
}

// TestWorkQueueRoundTrip proves the central mechanism: an assignment published to a
// role's work subject is pull-consumed and acked by a runner-style consumer.
func TestWorkQueueRoundTrip(t *testing.T) {
	js := startTestServer(t)
	ctx := context.Background()
	if err := SetupStreams(ctx, js); err != nil {
		t.Fatalf("SetupStreams: %v", err)
	}

	cons, err := EnsureWorkConsumer(ctx, js, "implement", 2*time.Second)
	if err != nil {
		t.Fatalf("EnsureWorkConsumer: %v", err)
	}

	if _, err := js.Publish(ctx, WorkSubject("implement"), []byte("brief-payload")); err != nil {
		t.Fatalf("Publish work: %v", err)
	}

	got := fetchOne(t, cons)
	if string(got.Data()) != "brief-payload" {
		t.Errorf("work payload = %q, want %q", got.Data(), "brief-payload")
	}
	if got.Subject() != WorkSubject("implement") {
		t.Errorf("work subject = %q, want %q", got.Subject(), WorkSubject("implement"))
	}
	if err := got.Ack(); err != nil {
		t.Fatalf("Ack: %v", err)
	}
}

// TestResultConsumer proves Result envelopes flow back and are consumable by the
// orchestrator's durable consumer.
func TestResultConsumer(t *testing.T) {
	js := startTestServer(t)
	ctx := context.Background()
	if err := SetupStreams(ctx, js); err != nil {
		t.Fatalf("SetupStreams: %v", err)
	}

	cons, err := EnsureResultConsumer(ctx, js)
	if err != nil {
		t.Fatalf("EnsureResultConsumer: %v", err)
	}
	if _, err := js.Publish(ctx, ResultSubject("implement"), []byte("result-envelope")); err != nil {
		t.Fatalf("Publish result: %v", err)
	}

	got := fetchOne(t, cons)
	if string(got.Data()) != "result-envelope" {
		t.Errorf("result payload = %q, want %q", got.Data(), "result-envelope")
	}
	if err := got.Ack(); err != nil {
		t.Fatalf("Ack: %v", err)
	}
}

func fetchOne(t *testing.T, cons jetstream.Consumer) jetstream.Msg {
	t.Helper()
	batch, err := cons.Fetch(1, jetstream.FetchMaxWait(3*time.Second))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	var got jetstream.Msg
	for m := range batch.Messages() {
		got = m
	}
	if err := batch.Error(); err != nil {
		t.Fatalf("batch error: %v", err)
	}
	if got == nil {
		t.Fatal("no message delivered within wait")
	}
	return got
}
