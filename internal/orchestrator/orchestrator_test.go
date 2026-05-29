package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/gate"
	"github.com/Loxstomper/harness/internal/messaging"
)

// --- fakes -------------------------------------------------------------------

// fakeBeads is an in-memory Beads. It records the single-writer calls the
// orchestrator makes and serves Get from a map so accept/route paths see the issues
// they created. Guarded by a mutex because the Result consumer and the tick loop run
// concurrently.
type fakeBeads struct {
	mu       sync.Mutex
	ready    []core.Issue
	issues   map[string]core.Issue
	stranded []string

	claimed  []string
	released []string
	closed   []string
	blocked  []string
	applied  []core.Proposal
	seq      int

	claimErr error
	applyErr error
	getErr   error
}

func newFakeBeads() *fakeBeads { return &fakeBeads{issues: map[string]core.Issue{}} }

func (f *fakeBeads) put(is core.Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues[is.ID] = is
}

func (f *fakeBeads) Ready(context.Context) ([]core.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.Issue(nil), f.ready...), nil
}

func (f *fakeBeads) Get(_ context.Context, id string) (core.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return core.Issue{}, f.getErr
	}
	is, ok := f.issues[id]
	if !ok {
		return core.Issue{}, fmt.Errorf("no such issue %q", id)
	}
	return is, nil
}

func (f *fakeBeads) Claim(_ context.Context, id string, _ time.Duration) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return time.Time{}, f.claimErr
	}
	f.claimed = append(f.claimed, id)
	return time.Now().Add(time.Hour), nil
}

func (f *fakeBeads) Release(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, id)
	return nil
}

func (f *fakeBeads) Close(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, id)
	return nil
}

func (f *fakeBeads) Block(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocked = append(f.blocked, id)
	return nil
}

func (f *fakeBeads) Apply(_ context.Context, proposals []core.Proposal) ([]core.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	created := make([]core.Issue, 0, len(proposals))
	for _, p := range proposals {
		f.seq++
		is := p.Issue
		is.ID = fmt.Sprintf("new-%d", f.seq)
		f.issues[is.ID] = is
		created = append(created, is)
		f.applied = append(f.applied, p)
	}
	return created, nil
}

func (f *fakeBeads) ListStranded(context.Context, time.Time) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stranded...), nil
}

// snapshot accessors (copy under lock).
func (f *fakeBeads) snap() (claimed, released, closed, blocked []string, applied []core.Proposal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.claimed...), append([]string(nil), f.released...),
		append([]string(nil), f.closed...), append([]string(nil), f.blocked...),
		append([]core.Proposal(nil), f.applied...)
}

// fakeGate returns a canned report (or error).
type fakeGate struct {
	mu     sync.Mutex
	report gate.Report
	err    error
	calls  []gate.Candidate
}

func (g *fakeGate) Run(_ context.Context, c gate.Candidate) (gate.Report, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, c)
	return g.report, g.err
}

func (g *fakeGate) called() []gate.Candidate {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]gate.Candidate(nil), g.calls...)
}

// fakeMerger records the candidates it was asked to merge and the provenance passed
// with each, so a test can assert the merge trailer is populated from config + evidence.
type fakeMerger struct {
	mu    sync.Mutex
	refs  []string
	provs []Provenance
	err   error
}

func (m *fakeMerger) Merge(_ context.Context, _, ref string, prov Provenance) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refs = append(m.refs, ref)
	m.provs = append(m.provs, prov)
	if m.err != nil {
		return "", m.err
	}
	return "deadbeef", nil
}

func (m *fakeMerger) merged() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.refs...)
}

func (m *fakeMerger) provenance() []Provenance {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Provenance(nil), m.provs...)
}

// --- helpers -----------------------------------------------------------------

// kernelConfig returns the bootstrap DAG: implement -> integrate (trusted-merge), one
// soul per role, a retry cap of 2. mutate lets a test adjust it (e.g. produce qa).
func kernelConfig(maxRetries int) *config.Config {
	return &config.Config{
		Harness: &config.Harness{
			DAG: map[string]config.Stage{
				"implement": {
					Role:          "implement",
					Postcondition: []string{"tests-pass"},
					OnFailure:     "implement",
					Produces:      []string{"integrate"},
				},
				"integrate": {Kind: config.StageKindTrustedMerge},
			},
			Policy: config.Policy{MaxRetries: maxRetries},
		},
		Souls: []core.Soul{{Name: "implementor-go", Role: "implement", Sandbox: "go-toolchain"}},
	}
}

func newOrch(t *testing.T, cfg *config.Config, bd Beads, g Gate, m Merger) (*Orchestrator, *nats.Conn) {
	t.Helper()
	srv, err := messaging.NewEmbeddedServer(messaging.ServerConfig{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("embedded server: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	nc, err := srv.Connect()
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := messaging.JetStream(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := messaging.SetupStreams(context.Background(), js); err != nil {
		t.Fatalf("setup streams: %v", err)
	}
	o, err := New(Options{Config: cfg, Repo: "/repo", Limits: config.SandboxLimits{CPU: 1, Mem: "1Gi", Wall: config.Duration(time.Minute)}}, bd, g, m, js)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return o, nc
}

func inProgress(id, role string, attempt int) core.Issue {
	return core.Issue{ID: id, Title: "t", Body: "b", Role: role, Status: statusInProgress, Attempt: attempt}
}

// --- New validation ----------------------------------------------------------

func TestNewValidatesOptions(t *testing.T) {
	_, err := New(Options{}, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("New accepted empty options")
	}
}

// --- scheduleReady -----------------------------------------------------------

func TestScheduleReadyClaimsAndDispatches(t *testing.T) {
	bd := newFakeBeads()
	bd.ready = []core.Issue{{ID: "iss-1", Title: "do it", Role: "implement"}}
	o, nc := newOrch(t, kernelConfig(2), bd, &fakeGate{}, &fakeMerger{})

	sub, err := nc.SubscribeSync(messaging.WorkSubject("implement"))
	if err != nil {
		t.Fatalf("subscribe work: %v", err)
	}

	o.scheduleReady(context.Background())

	claimed, _, _, _, _ := bd.snap()
	if len(claimed) != 1 || claimed[0] != "iss-1" {
		t.Errorf("claimed = %v, want [iss-1]", claimed)
	}
	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no work published: %v", err)
	}
	var brief core.Brief
	if err := json.Unmarshal(msg.Data, &brief); err != nil {
		t.Fatalf("decode brief: %v", err)
	}
	if brief.Issue.ID != "iss-1" || brief.Soul.Name != "implementor-go" || brief.Base != "main" {
		t.Errorf("brief = %+v", brief)
	}
	if len(brief.Criteria) != 1 || brief.Criteria[0] != "tests-pass" {
		t.Errorf("brief criteria = %v, want [tests-pass]", brief.Criteria)
	}
}

func TestScheduleReadyReleasesOnPublishFailure(t *testing.T) {
	bd := newFakeBeads()
	bd.ready = []core.Issue{{ID: "iss-1", Role: "implement"}}
	o, nc := newOrch(t, kernelConfig(2), bd, &fakeGate{}, &fakeMerger{})
	nc.Close() // force the publish to fail

	o.scheduleReady(context.Background())

	claimed, released, _, _, _ := bd.snap()
	if len(claimed) != 1 {
		t.Fatalf("claimed = %v, want one claim", claimed)
	}
	if len(released) != 1 || released[0] != "iss-1" {
		t.Errorf("released = %v, want [iss-1] after publish failure", released)
	}
}

func TestScheduleReadySkipsUnknownRole(t *testing.T) {
	bd := newFakeBeads()
	bd.ready = []core.Issue{{ID: "iss-x", Role: "nonexistent"}}
	o, _ := newOrch(t, kernelConfig(2), bd, &fakeGate{}, &fakeMerger{})

	o.scheduleReady(context.Background())

	if claimed, _, _, _, _ := bd.snap(); len(claimed) != 0 {
		t.Errorf("claimed %v, want none for an unknown role", claimed)
	}
}

// --- accept (gate pass) ------------------------------------------------------

func TestHandleResultAcceptMergesAndCloses(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	g := &fakeGate{report: gate.Report{Passed: true}}
	m := &fakeMerger{}
	o, _ := newOrch(t, kernelConfig(2), bd, g, m)

	res := core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: core.CandidateBranch("iss-1")}}
	transient, err := o.handleResult(context.Background(), res)
	if err != nil || transient {
		t.Fatalf("handleResult = (%v,%v), want (false,nil)", transient, err)
	}

	// The gate grades the candidate against the producing stage's declared
	// postconditions (T2.1), not a hardcoded set, so they must reach the Candidate.
	if got := g.called(); len(got) != 1 || got[0].Ref != "candidate/iss-1" || got[0].Profile != "go-toolchain" ||
		!reflect.DeepEqual(got[0].Postconditions, []string{"tests-pass"}) {
		t.Errorf("gate candidate = %+v", got)
	}
	if got := m.merged(); len(got) != 1 || got[0] != "candidate/iss-1" {
		t.Errorf("merged = %v, want [candidate/iss-1]", got)
	}
	_, _, closed, _, _ := bd.snap()
	if len(closed) != 1 || closed[0] != "iss-1" {
		t.Errorf("closed = %v, want [iss-1]", closed)
	}
}

func TestHandleResultAcceptBuildsProvenance(t *testing.T) {
	cfg := kernelConfig(2)
	// Give the implement soul a model so the trailer's Model field is populated.
	cfg.Souls = []core.Soul{{Name: "implementor-go", Role: "implement", Sandbox: "go-toolchain", Model: "claude-opus-4-7"}}
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	g := &fakeGate{report: gate.Report{Passed: true, Checks: []gate.CheckResult{
		{Name: "build", Passed: true},
		{Name: "test", Passed: true},
	}}}
	m := &fakeMerger{}
	o, _ := newOrch(t, cfg, bd, g, m)

	res := core.Result{
		IssueID:  "iss-1",
		Status:   core.StatusDone,
		Branch:   core.Branch{Ref: core.CandidateBranch("iss-1")},
		Evidence: core.Evidence{PromptSHA: "sha256:9af"},
	}
	if _, err := o.handleResult(context.Background(), res); err != nil {
		t.Fatalf("handleResult: %v", err)
	}

	provs := m.provenance()
	if len(provs) != 1 {
		t.Fatalf("provenance recorded %d times, want 1", len(provs))
	}
	p := provs[0]
	if p.Soul != "implementor-go" || p.Model != "claude-opus-4-7" || p.Issue != "iss-1" || p.PromptSHA != "sha256:9af" {
		t.Errorf("provenance = %+v, want soul/model/issue/prompt populated", p)
	}
	if len(p.Verified) != 2 || p.Verified[0] != "build" || p.Verified[1] != "test" {
		t.Errorf("provenance Verified = %v, want [build test]", p.Verified)
	}
	// The rendered trailer matches the spec's exact two-line format.
	want := "Soul: implementor-go | Model: claude-opus-4-7\nIssue: iss-1 | Prompt-SHA: sha256:9af | Verified: build,test"
	if got := p.Trailer(); got != want {
		t.Errorf("trailer =\n%q\nwant\n%q", got, want)
	}
}

func TestHandleResultAcceptProducesAgentStage(t *testing.T) {
	cfg := kernelConfig(2)
	// implement now produces an agent stage qa instead of integrate.
	cfg.Harness.DAG["implement"] = config.Stage{Role: "implement", Produces: []string{"qa"}, OnFailure: "implement"}
	cfg.Harness.DAG["qa"] = config.Stage{Role: "qa", Produces: []string{"integrate"}}
	cfg.Souls = append(cfg.Souls, core.Soul{Name: "qa-soul", Role: "qa", Sandbox: "go-toolchain"})
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	o, _ := newOrch(t, cfg, bd, &fakeGate{report: gate.Report{Passed: true}}, &fakeMerger{})

	_, err := o.handleResult(context.Background(), core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: "candidate/iss-1"}})
	if err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	_, _, closed, _, applied := bd.snap()
	if len(applied) != 1 || applied[0].Issue.Role != "qa" {
		t.Errorf("applied = %+v, want one qa proposal", applied)
	}
	if len(closed) != 1 || closed[0] != "iss-1" {
		t.Errorf("closed = %v", closed)
	}
}

// --- route (gate fail / failed) ----------------------------------------------

func TestHandleResultGateFailRetries(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	o, _ := newOrch(t, kernelConfig(2), bd, &fakeGate{report: gate.Report{Passed: false}}, &fakeMerger{})

	_, err := o.handleResult(context.Background(), core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: "candidate/iss-1"}})
	if err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	_, _, closed, blocked, applied := bd.snap()
	if len(applied) != 1 || applied[0].Issue.Role != "implement" || applied[0].Issue.Attempt != 1 {
		t.Errorf("applied = %+v, want one implement fix issue at attempt 1", applied)
	}
	if len(closed) != 1 || closed[0] != "iss-1" {
		t.Errorf("closed = %v, want original closed", closed)
	}
	if len(blocked) != 0 {
		t.Errorf("blocked = %v, want none (still retryable)", blocked)
	}
}

func TestHandleResultFailedStatusRoutes(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	g := &fakeGate{}
	o, _ := newOrch(t, kernelConfig(2), bd, g, &fakeMerger{})

	_, err := o.handleResult(context.Background(), core.Result{IssueID: "iss-1", Status: core.StatusFailed})
	if err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	if len(g.called()) != 0 {
		t.Error("gate ran for a failed (no-candidate) result")
	}
	_, _, _, _, applied := bd.snap()
	if len(applied) != 1 || applied[0].Issue.Attempt != 1 {
		t.Errorf("applied = %+v, want retry at attempt 1", applied)
	}
}

func TestHandleResultRetryCapDeadLetters(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 2)) // MaxRetries=2, so attempt 2 is spent
	o, nc := newOrch(t, kernelConfig(2), bd, &fakeGate{report: gate.Report{Passed: false}}, &fakeMerger{})
	sub, err := nc.SubscribeSync(messaging.SubjectDLQ)
	if err != nil {
		t.Fatalf("subscribe dlq: %v", err)
	}

	_, err = o.handleResult(context.Background(), core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: "candidate/iss-1"}})
	if err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	_, _, _, blocked, applied := bd.snap()
	if len(blocked) != 1 || blocked[0] != "iss-1" {
		t.Errorf("blocked = %v, want [iss-1] (retry cap exhausted)", blocked)
	}
	if len(applied) != 0 {
		t.Errorf("applied = %+v, want no new fix issue past the retry cap", applied)
	}
	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no dlq alert: %v", err)
	}
	var alert dlqAlert
	if err := json.Unmarshal(msg.Data, &alert); err != nil {
		t.Fatalf("decode alert: %v", err)
	}
	if alert.IssueID != "iss-1" || alert.Attempt != 2 {
		t.Errorf("alert = %+v", alert)
	}
}

// --- escalation / proposals / idempotency ------------------------------------

func TestHandleResultEscalationDeadLetters(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	o, nc := newOrch(t, kernelConfig(2), bd, &fakeGate{}, &fakeMerger{})
	sub, _ := nc.SubscribeSync(messaging.SubjectDLQ)

	_, err := o.handleResult(context.Background(), core.Result{IssueID: "iss-1", Status: core.StatusNeedsSpecClarification})
	if err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	if _, _, _, blocked, _ := bd.snap(); len(blocked) != 1 {
		t.Errorf("blocked = %v, want the escalated issue dead-lettered", blocked)
	}
	if _, err := sub.NextMsg(2 * time.Second); err != nil {
		t.Errorf("no dlq alert for escalation: %v", err)
	}
}

func TestHandleResultIllegalProposalDeadLetters(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	o, _ := newOrch(t, kernelConfig(2), bd, &fakeGate{report: gate.Report{Passed: true}}, &fakeMerger{})

	res := core.Result{
		IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: "candidate/iss-1"},
		Proposes: []core.Proposal{{Issue: core.Issue{Title: "child", Role: "bogus-role"}}},
	}
	_, err := o.handleResult(context.Background(), res)
	if err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	if _, _, _, blocked, _ := bd.snap(); len(blocked) != 1 {
		t.Errorf("blocked = %v, want dead-letter on illegal proposal", blocked)
	}
}

func TestHandleResultStaleIssueIgnored(t *testing.T) {
	bd := newFakeBeads()
	bd.put(core.Issue{ID: "iss-1", Role: "implement", Status: "closed"})
	g := &fakeGate{report: gate.Report{Passed: true}}
	m := &fakeMerger{}
	o, _ := newOrch(t, kernelConfig(2), bd, g, m)

	transient, err := o.handleResult(context.Background(), core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: "candidate/iss-1"}})
	if err != nil || transient {
		t.Fatalf("handleResult = (%v,%v), want (false,nil)", transient, err)
	}
	if len(g.called()) != 0 || len(m.merged()) != 0 {
		t.Error("a stale (not-in-progress) result was acted on")
	}
	if _, _, closed, _, _ := bd.snap(); len(closed) != 0 {
		t.Errorf("closed = %v, want none for a stale result", closed)
	}
}

func TestHandleResultGateInfraErrorIsTransient(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	g := &fakeGate{err: errors.New("sandbox died")}
	o, _ := newOrch(t, kernelConfig(2), bd, g, &fakeMerger{})

	transient, err := o.handleResult(context.Background(), core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: "candidate/iss-1"}})
	if err == nil || !transient {
		t.Fatalf("handleResult = (%v,%v), want (true, err)", transient, err)
	}
	if _, _, closed, blocked, _ := bd.snap(); len(closed) != 0 || len(blocked) != 0 {
		t.Error("a transient gate error changed issue state; it must be left for retry")
	}
}

// --- sweep -------------------------------------------------------------------

func TestSweepReleasesStranded(t *testing.T) {
	bd := newFakeBeads()
	bd.stranded = []string{"iss-a", "iss-b"}
	o, _ := newOrch(t, kernelConfig(2), bd, &fakeGate{}, &fakeMerger{})

	o.sweepLeases(context.Background())

	_, released, _, _, _ := bd.snap()
	if len(released) != 2 {
		t.Errorf("released = %v, want both stranded issues", released)
	}
}

// --- consumeResults over real NATS -------------------------------------------

func TestConsumeResultsProcessesAndAcks(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	g := &fakeGate{report: gate.Report{Passed: true}}
	m := &fakeMerger{}
	o, nc := newOrch(t, kernelConfig(2), bd, g, m)

	js, err := messaging.JetStream(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	res := core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: "candidate/iss-1"}}
	data, _ := json.Marshal(res)
	if _, err := js.Publish(context.Background(), messaging.ResultSubject("implement"), data); err != nil {
		t.Fatalf("publish result: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cons, err := messaging.EnsureResultConsumer(ctx, js)
	if err != nil {
		t.Fatalf("ensure consumer: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- o.consumeResults(ctx, cons) }()

	// Wait for the merge to land, then shut the consumer down cleanly.
	deadline := time.After(5 * time.Second)
	for len(m.merged()) == 0 {
		select {
		case <-deadline:
			t.Fatal("result was not processed")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("consumeResults returned %v, want nil on shutdown", err)
	}
}
