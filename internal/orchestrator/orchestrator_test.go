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
	inflight []core.Issue

	claimed     []string
	released    []string
	reissued    []string
	closed      []string
	blocked     []string
	parked      []string
	approvedRef []string
	approvedBy  map[string]string
	applied     []core.Proposal
	pinned      map[string]string
	transcripts map[string]string
	seq         int

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

func (f *fakeBeads) Block(_ context.Context, id, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocked = append(f.blocked, id)
	// Mirror the real client: the dead-letter reason is durable on the issue, so a later Get
	// (the DLQ / Resolve read path) sees it.
	if is, ok := f.issues[id]; ok {
		is.Status = "blocked"
		is.DeadLetterReason = reason
		f.issues[id] = is
	}
	return nil
}

func (f *fakeBeads) AwaitApproval(_ context.Context, id, candidateRef, parkedProv string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parked = append(f.parked, id)
	// Mirror the real client: the issue is blocked and durably carries the parked candidate
	// and provenance, so a later Get (the approval handler's) sees the awaiting-approval state.
	if is, ok := f.issues[id]; ok {
		is.Status = "blocked"
		is.CandidateRef = candidateRef
		is.ParkedProvenance = parkedProv
		f.issues[id] = is
	}
	return nil
}

func (f *fakeBeads) RecordApproval(_ context.Context, id, approvedRef, approver string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.approvedBy == nil {
		f.approvedBy = map[string]string{}
	}
	f.approvedBy[id] = approver
	f.approvedRef = append(f.approvedRef, approvedRef)
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

func (f *fakeBeads) InProgress(context.Context) ([]core.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.Issue(nil), f.inflight...), nil
}

func (f *fakeBeads) Reissue(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reissued = append(f.reissued, id)
	// Mirror the real client: status -> open and the pin is cleared, so a later Get sees it.
	if is, ok := f.issues[id]; ok {
		is.Status = "open"
		is.SpecHash = ""
		f.issues[id] = is
	}
	return nil
}

func (f *fakeBeads) PinSpecHash(_ context.Context, id, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pinned == nil {
		f.pinned = map[string]string{}
	}
	f.pinned[id] = hash
	// Mirror the real client: the pin is durable on the issue, so a later Get sees it.
	if is, ok := f.issues[id]; ok {
		is.SpecHash = hash
		f.issues[id] = is
	}
	return nil
}

func (f *fakeBeads) ListAll(context.Context) ([]core.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]core.Issue, 0, len(f.issues))
	for _, is := range f.issues {
		out = append(out, is)
	}
	return out, nil
}

func (f *fakeBeads) StampClosingSpend(_ context.Context, id string, tokens int, usd float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Mirror the real client: the marginal is durable on the issue, so a later ListAll (the
	// epic-budget aggregate read) sees it.
	if is, ok := f.issues[id]; ok {
		is.ClosingTokens = tokens
		is.ClosingUSD = usd
		f.issues[id] = is
	}
	return nil
}

func (f *fakeBeads) StampTranscript(_ context.Context, id, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.transcripts == nil {
		f.transcripts = map[string]string{}
	}
	f.transcripts[id] = hash
	// Mirror the real client: the hash is durable on the issue, so a later Get sees it.
	if is, ok := f.issues[id]; ok {
		is.Transcript = hash
		f.issues[id] = is
	}
	return nil
}

func (f *fakeBeads) StampSouls(_ context.Context, id, testsSoul, implementSoul string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Mirror the real client: only non-empty values are written, and the stamp is durable on
	// the issue so a later Get/ListAll sees the producing soul (the verification view's read).
	if is, ok := f.issues[id]; ok {
		if testsSoul != "" {
			is.TestsSoul = testsSoul
		}
		if implementSoul != "" {
			is.ImplementSoul = implementSoul
		}
		f.issues[id] = is
	}
	return nil
}

func (f *fakeBeads) StampGateVerdict(_ context.Context, id, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if hash == "" {
		return nil
	}
	if is, ok := f.issues[id]; ok {
		is.GateVerdict = hash
		f.issues[id] = is
	}
	return nil
}

// snapshot accessors (copy under lock).
func (f *fakeBeads) snap() (claimed, released, closed, blocked []string, applied []core.Proposal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.claimed...), append([]string(nil), f.released...),
		append([]string(nil), f.closed...), append([]string(nil), f.blocked...),
		append([]core.Proposal(nil), f.applied...)
}

// fakeGate returns a canned report (or error). reportFn, when set, computes the report from
// the candidate instead — used to make the initial branch gate and the integrate re-gate
// (which grade different refs) return different verdicts in one test.
type fakeGate struct {
	mu       sync.Mutex
	report   gate.Report
	err      error
	reportFn func(c gate.Candidate) (gate.Report, error)
	calls    []gate.Candidate
}

func (g *fakeGate) Run(_ context.Context, c gate.Candidate) (gate.Report, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, c)
	if g.reportFn != nil {
		return g.reportFn(c)
	}
	return g.report, g.err
}

func (g *fakeGate) called() []gate.Candidate {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]gate.Candidate(nil), g.calls...)
}

// fakeMerger records the candidates it was asked to merge and the provenance passed
// with each, so a test can assert the merge trailer is populated from config + evidence.
// When regateRef is set it invokes the supplied ReGate with that ref (simulating a rebase
// that published the result there), so a test can drive the orchestrator's re-gate path;
// the provenance recorded is then the one the re-gate returned, mirroring production.
type fakeMerger struct {
	mu        sync.Mutex
	refs      []string
	provs     []core.Provenance
	err       error
	regateRef string
}

func (m *fakeMerger) Merge(ctx context.Context, _, ref string, prov core.Provenance, regate ReGate, progress MergeProgress) (string, error) {
	m.mu.Lock()
	m.refs = append(m.refs, ref)
	m.mu.Unlock()
	if m.err != nil {
		return "", m.err
	}
	// Mirror the real merger's step announcements when a re-gate is simulated: a configured
	// regateRef means main moved, so the candidate is rebased then re-gated before landing.
	if m.regateRef != "" && regate != nil && progress != nil {
		progress(core.MergeStateRebasing)
		progress(core.MergeStateReGating)
	}
	if m.regateRef != "" && regate != nil {
		regated, accepted, err := regate(ctx, m.regateRef)
		if err != nil {
			return "", err
		}
		if !accepted {
			return "", errReGateFailed
		}
		prov = regated
	}
	m.mu.Lock()
	m.provs = append(m.provs, prov)
	m.mu.Unlock()
	return "deadbeef", nil
}

func (m *fakeMerger) merged() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.refs...)
}

func (m *fakeMerger) provenance() []core.Provenance {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]core.Provenance(nil), m.provs...)
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

// planConfig returns a DAG whose entry is a plan stage (kind plan, role planner) that
// produces author-tests, plus downstream agent stages so a planner's proposals resolve to
// real agent roles. It is the fixture for the ungated decomposition path.
func planConfig(maxRetries int) *config.Config {
	return &config.Config{
		Harness: &config.Harness{
			DAG: map[string]config.Stage{
				"plan": {
					Kind:      config.StageKindPlan,
					Role:      "planner",
					OnFailure: "plan",
					Produces:  []string{"author-tests"},
				},
				"author-tests": {Role: "test-author", Postcondition: []string{"tests-red"}, OnFailure: "author-tests", Produces: []string{"implement"}},
				"implement":    {Role: "implementor", Postcondition: []string{"tests-pass"}, OnFailure: "implement", Produces: []string{"integrate"}},
				"integrate":    {Kind: config.StageKindTrustedMerge},
			},
			Policy: config.Policy{MaxRetries: maxRetries},
		},
		Souls: []core.Soul{
			{Name: "planner-go", Role: "planner", Sandbox: "go-toolchain"},
			{Name: "test-author-go", Role: "test-author", Sandbox: "go-toolchain"},
			{Name: "implementor-go", Role: "implementor", Sandbox: "go-toolchain"},
		},
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
	if err := messaging.SetupStreams(context.Background(), js, messaging.StreamOptions{}); err != nil {
		t.Fatalf("setup streams: %v", err)
	}
	o, err := New(Options{Config: cfg, Repo: "/repo", Limits: config.SandboxLimits{CPU: 1, Mem: "1Gi", Wall: config.Duration(time.Minute)}}, bd, g, m, nc, js)
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
	_, err := New(Options{}, nil, nil, nil, nil, nil)
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

// TestHandleResultIntegrateConflictDeadLetters: the fallback path when no resolve stage is
// configured (the kernel DAG). When a verified candidate cannot be rebased onto the current
// main (a merge-queue conflict with a branch that landed first) the merger reports
// errRebaseConflict; absent a resolve stage to spawn a sandboxed resolution into, the
// orchestrator escalates the issue to the DLQ — it does not retry the same candidate (a
// conflict is deterministic) and does not close the blocked issue. With a resolve stage it
// instead spawns a resolution issue (TestHandleResultIntegrateConflictSpawnsResolution).
func TestHandleResultIntegrateConflictDeadLetters(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	m := &fakeMerger{err: errRebaseConflict}
	o, nc := newOrch(t, kernelConfig(2), bd, &fakeGate{report: gate.Report{Passed: true}}, m)

	sub, err := nc.SubscribeSync(messaging.SubjectDLQ)
	if err != nil {
		t.Fatalf("subscribe dlq: %v", err)
	}

	res := core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: core.CandidateBranch("iss-1")}}
	transient, err := o.handleResult(context.Background(), res)
	if err != nil || transient {
		t.Fatalf("handleResult = (%v,%v), want (false,nil) — a conflict is dead-lettered, not retried", transient, err)
	}
	if _, derr := sub.NextMsg(time.Second); derr != nil {
		t.Errorf("no DLQ alert published for the integrate conflict: %v", derr)
	}
	_, _, closed, blocked, _ := bd.snap()
	if len(blocked) != 1 || blocked[0] != "iss-1" {
		t.Errorf("blocked = %v, want [iss-1] (conflict escalated)", blocked)
	}
	if len(closed) != 0 {
		t.Errorf("closed = %v, want none (a dead-lettered issue is blocked, not closed)", closed)
	}
}

// conflictResolveConfig is kernelConfig plus a resolve stage (kind: resolve, role
// merge-resolver) and a soul fulfilling it, so the conflict path spawns a sandboxed
// resolution issue instead of dead-lettering. implement -> integrate; a conflict at
// integrate spawns a merge-resolver issue that itself produces integrate.
func conflictResolveConfig(maxRetries int) *config.Config {
	cfg := kernelConfig(maxRetries)
	cfg.Harness.DAG["resolve"] = config.Stage{
		Kind:          config.StageKindResolve,
		Role:          "merge-resolver",
		Postcondition: []string{"tests-pass"},
		OnFailure:     "resolve",
		Produces:      []string{"integrate"},
	}
	cfg.Souls = append(cfg.Souls, core.Soul{Name: "merge-resolver-go", Role: "merge-resolver", Sandbox: "go-toolchain"})
	return cfg
}

// TestHandleResultIntegrateConflictSpawnsResolution: with a resolve stage configured, an
// integrate rebase conflict spawns a sandboxed conflict-resolution issue rather than
// dead-lettering. The new issue carries the resolve role, is seeded at the CONFLICTING
// candidate (Base) so the resolver rebases that branch onto main, and counts as the next
// attempt; the conflicted issue is closed (its work lives on in the resolution), and nothing
// is dead-lettered. (specs/integration.md step 2: spawn a resolution issue, block, loop.)
func TestHandleResultIntegrateConflictSpawnsResolution(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	m := &fakeMerger{err: errRebaseConflict}
	o, nc := newOrch(t, conflictResolveConfig(2), bd, &fakeGate{report: gate.Report{Passed: true}}, m)

	sub, err := nc.SubscribeSync(messaging.SubjectDLQ)
	if err != nil {
		t.Fatalf("subscribe dlq: %v", err)
	}

	res := core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: core.CandidateBranch("iss-1")}}
	transient, err := o.handleResult(context.Background(), res)
	if err != nil || transient {
		t.Fatalf("handleResult = (%v,%v), want (false,nil) — a conflict spawns a resolution, not an error", transient, err)
	}

	_, _, closed, blocked, applied := bd.snap()
	if len(blocked) != 0 {
		t.Errorf("blocked = %v, want none (a resolvable conflict is not dead-lettered)", blocked)
	}
	if len(closed) != 1 || closed[0] != "iss-1" {
		t.Errorf("closed = %v, want [iss-1] (the conflicted issue is closed in favor of the resolution)", closed)
	}
	if len(applied) != 1 {
		t.Fatalf("applied = %+v, want one conflict-resolution issue", applied)
	}
	got := applied[0].Issue
	if got.Role != "merge-resolver" {
		t.Errorf("resolution role = %q, want merge-resolver", got.Role)
	}
	if got.Base != core.CandidateBranch("iss-1") {
		t.Errorf("resolution base = %q, want %q (rebase the conflicting candidate)", got.Base, core.CandidateBranch("iss-1"))
	}
	if got.Attempt != 1 {
		t.Errorf("resolution attempt = %d, want 1 (the conflict loop counts against the retry cap)", got.Attempt)
	}
	// No DLQ alert for a spawned resolution.
	if _, derr := sub.NextMsg(200 * time.Millisecond); derr == nil {
		t.Error("a DLQ alert was published; a spawned resolution must not dead-letter")
	}
}

// TestHandleResultIntegrateConflictResolveCapExhausted: a conflict whose issue has already
// exhausted the retry cap dead-letters even with a resolve stage configured — the conflict
// loop is bounded by the same termination guarantee as the fix loop, so a conflict no rebase
// resolves cannot spin forever.
func TestHandleResultIntegrateConflictResolveCapExhausted(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 2)) // at the cap (maxRetries=2)
	m := &fakeMerger{err: errRebaseConflict}
	o, nc := newOrch(t, conflictResolveConfig(2), bd, &fakeGate{report: gate.Report{Passed: true}}, m)

	sub, err := nc.SubscribeSync(messaging.SubjectDLQ)
	if err != nil {
		t.Fatalf("subscribe dlq: %v", err)
	}

	res := core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: core.CandidateBranch("iss-1")}}
	if transient, err := o.handleResult(context.Background(), res); err != nil || transient {
		t.Fatalf("handleResult = (%v,%v), want (false,nil)", transient, err)
	}
	if _, derr := sub.NextMsg(time.Second); derr != nil {
		t.Errorf("no DLQ alert for a cap-exhausted conflict: %v", derr)
	}
	_, _, closed, blocked, applied := bd.snap()
	if len(blocked) != 1 || blocked[0] != "iss-1" {
		t.Errorf("blocked = %v, want [iss-1] (cap exhausted -> dead-letter)", blocked)
	}
	if len(applied) != 0 {
		t.Errorf("applied = %+v, want none (no resolution spawned past the cap)", applied)
	}
	if len(closed) != 0 {
		t.Errorf("closed = %v, want none (a dead-lettered issue is blocked, not closed)", closed)
	}
}

// TestHandleResultIntegrateReGatePasses: when a candidate rebases onto a moved main, the
// merger re-gates the rebased result before advancing main (specs/integration.md step 3).
// On a passing re-gate the issue merges and closes, and the provenance recorded cites the
// RE-GATE's checks — the combination's verification is the truth that landed, not the
// branch gate's. (The fakeMerger drives the ReGate with regateRef, standing in for the
// temp ref a real rebase publishes.)
func TestHandleResultIntegrateReGatePasses(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	// Branch gate (ref candidate/iss-1) and re-gate (ref integration/iss-1) both pass, but
	// cite different checks so the test can tell which verdict reached the trailer.
	g := &fakeGate{reportFn: func(c gate.Candidate) (gate.Report, error) {
		if c.Ref == "integration/iss-1" {
			return gate.Report{Passed: true, Checks: []gate.CheckResult{{Name: "regate-check", Passed: true}}}, nil
		}
		return gate.Report{Passed: true, Checks: []gate.CheckResult{{Name: "branch-check", Passed: true}}}, nil
	}}
	m := &fakeMerger{regateRef: "integration/iss-1"}
	o, _ := newOrch(t, kernelConfig(2), bd, g, m)

	res := core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: core.CandidateBranch("iss-1")}}
	if transient, err := o.handleResult(context.Background(), res); err != nil || transient {
		t.Fatalf("handleResult = (%v,%v), want (false,nil)", transient, err)
	}

	// The gate ran twice: once on the branch, once on the rebased result.
	if got := g.called(); len(got) != 2 {
		t.Fatalf("gate called %d times, want 2 (branch + re-gate)", len(got))
	}
	provs := m.provenance()
	if len(provs) != 1 {
		t.Fatalf("provenance recorded %d times, want 1", len(provs))
	}
	if len(provs[0].Verified) != 1 || provs[0].Verified[0] != "regate-check" {
		t.Errorf("provenance Verified = %v, want [regate-check] (the re-gate's checks landed)", provs[0].Verified)
	}
	_, _, closed, _, _ := bd.snap()
	if len(closed) != 1 || closed[0] != "iss-1" {
		t.Errorf("closed = %v, want [iss-1]", closed)
	}
}

// TestHandleResultIntegrateReGateFailRoutesFix: a candidate that rebases cleanly but whose
// rebased result fails the re-gate (two branches green in isolation, broken together) is NOT
// merged. Unlike a conflict — which dead-letters, being deterministic — a re-gate failure is
// routed through the normal retry machinery (a different main may pass), closing the original
// and spawning a fix at on_failure. Nothing lands on main.
func TestHandleResultIntegrateReGateFailRoutesFix(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "implement", 0))
	g := &fakeGate{reportFn: func(c gate.Candidate) (gate.Report, error) {
		if c.Ref == "integration/iss-1" {
			return gate.Report{Passed: false}, nil // the combination is broken
		}
		return gate.Report{Passed: true}, nil // the branch alone is green
	}}
	m := &fakeMerger{regateRef: "integration/iss-1"}
	o, _ := newOrch(t, kernelConfig(2), bd, g, m)

	res := core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: core.CandidateBranch("iss-1")}}
	if transient, err := o.handleResult(context.Background(), res); err != nil || transient {
		t.Fatalf("handleResult = (%v,%v), want (false,nil) — a re-gate failure is routed, not surfaced", transient, err)
	}

	if got := m.provenance(); len(got) != 0 {
		t.Errorf("provenance recorded %v, want none (re-gate rejected the rebased result)", got)
	}
	_, _, closed, _, applied := bd.snap()
	if len(closed) != 1 || closed[0] != "iss-1" {
		t.Errorf("closed = %v, want [iss-1] (the routed issue is closed)", closed)
	}
	if len(applied) != 1 || applied[0].Issue.Role != "implement" || applied[0].Issue.Attempt != 1 {
		t.Errorf("applied = %+v, want one implement fix issue at attempt 1", applied)
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
		Evidence: core.Evidence{
			PromptSHA: "sha256:9af",
			// The runner harvested the agent conversation; its hash rides on Evidence and must
			// be cited in the trailer so the transcript stays reachable from the read stores.
			Artifacts: []core.ArtifactRef{{Kind: core.ArtifactKindTranscript, Hash: "sha256:tx"}},
		},
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
	if p.Transcript != "sha256:tx" {
		t.Errorf("provenance Transcript = %q, want sha256:tx (the harvested conversation)", p.Transcript)
	}
	// The rendered trailer matches the spec's exact two-line format. This issue carried no
	// threaded author-tests map (seeded directly at implement), so Traceability is (none) and —
	// with no author-tests stage in its lineage — Tests-Soul is (none) too; the harvested
	// transcript hash is cited.
	want := "Soul: implementor-go | Model: claude-opus-4-7 | Tests-Soul: (none)\nIssue: iss-1 | Prompt-SHA: sha256:9af | Verified: build,test | Traceability: (none) | Transcript: sha256:tx"
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
	// The produced issue branches from the predecessor's verified candidate, so the
	// downstream stage builds on the work already done rather than from main (this is
	// what carries an author-tests candidate's failing tests into the implementor's
	// worktree; see specs/workflow.md).
	if applied[0].Issue.Base != "candidate/iss-1" {
		t.Errorf("produced issue Base = %q, want candidate/iss-1 (the predecessor's candidate)", applied[0].Issue.Base)
	}
	if len(closed) != 1 || closed[0] != "iss-1" {
		t.Errorf("closed = %v", closed)
	}
}

// --- plan stage (ungated decomposition) --------------------------------------

// A plan stage is an agent stage but is not sandbox-gated: a planner's "done" result
// carries no candidate branch, only proposed children. The orchestrator accepts it
// structurally — it writes the proposals (emergent breadth) and closes the plan issue,
// running NO gate and merging nothing.
func TestHandleResultPlanAppliesProposalsNoGate(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("plan-1", "planner", 0))
	g := &fakeGate{report: gate.Report{Passed: true}}
	m := &fakeMerger{}
	o, _ := newOrch(t, planConfig(2), bd, g, m)

	res := core.Result{
		IssueID: "plan-1", Status: core.StatusDone, // a planner pushes no branch
		Proposes: []core.Proposal{
			{Issue: core.Issue{Title: "slice A", Role: "test-author"}, Key: "a"},
			{Issue: core.Issue{Title: "slice B", Role: "test-author"}, DependsOn: []string{"a"}},
		},
	}
	transient, err := o.handleResult(context.Background(), res)
	if err != nil || transient {
		t.Fatalf("handleResult = (%v,%v), want (false,nil)", transient, err)
	}
	if len(g.called()) != 0 {
		t.Error("the gate ran for an ungated plan stage")
	}
	if len(m.merged()) != 0 {
		t.Error("a plan stage merged something")
	}
	_, _, closed, blocked, applied := bd.snap()
	if len(applied) != 2 || applied[0].Issue.Role != "test-author" || applied[1].Issue.Role != "test-author" {
		t.Errorf("applied = %+v, want two author-tests children", applied)
	}
	if len(closed) != 1 || closed[0] != "plan-1" {
		t.Errorf("closed = %v, want [plan-1]", closed)
	}
	if len(blocked) != 0 {
		t.Errorf("blocked = %v, want none", blocked)
	}
}

// A planner that proposes nothing did not decompose the work; it routes via on_failure
// (a fresh plan attempt) rather than stalling the pipeline with no work to do.
func TestHandleResultPlanNoProposalsRoutes(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("plan-1", "planner", 0))
	o, _ := newOrch(t, planConfig(2), bd, &fakeGate{}, &fakeMerger{})

	res := core.Result{IssueID: "plan-1", Status: core.StatusDone} // no proposals
	if _, err := o.handleResult(context.Background(), res); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	_, _, closed, blocked, applied := bd.snap()
	if len(applied) != 1 || applied[0].Issue.Role != "planner" || applied[0].Issue.Attempt != 1 {
		t.Errorf("applied = %+v, want one planner retry at attempt 1", applied)
	}
	if len(closed) != 1 || closed[0] != "plan-1" {
		t.Errorf("closed = %v, want the original plan issue closed", closed)
	}
	if len(blocked) != 0 {
		t.Errorf("blocked = %v, want none (still retryable)", blocked)
	}
}

// A planner may only produce work targeting a role its stage declares it produces
// (author-tests). A proposal that names a valid agent role outside that set — here
// implementor, which would skip author-tests — is an illegal proposal and dead-letters,
// so an untrusted planner cannot inject stage-skipping work.
func TestHandleResultPlanIllegalTargetDeadLetters(t *testing.T) {
	bd := newFakeBeads()
	bd.put(inProgress("plan-1", "planner", 0))
	o, nc := newOrch(t, planConfig(2), bd, &fakeGate{}, &fakeMerger{})
	sub, _ := nc.SubscribeSync(messaging.SubjectDLQ)

	res := core.Result{
		IssueID: "plan-1", Status: core.StatusDone,
		Proposes: []core.Proposal{{Issue: core.Issue{Title: "skip ahead", Role: "implementor"}}},
	}
	if _, err := o.handleResult(context.Background(), res); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	_, _, _, blocked, applied := bd.snap()
	if len(blocked) != 1 || blocked[0] != "plan-1" {
		t.Errorf("blocked = %v, want the plan issue dead-lettered", blocked)
	}
	if len(applied) != 0 {
		t.Errorf("applied = %+v, want no children written for an illegal decomposition", applied)
	}
	if _, err := sub.NextMsg(2 * time.Second); err != nil {
		t.Errorf("no dlq alert for the illegal proposal: %v", err)
	}
}

// TestScheduleReadyThreadsProducedBase proves a produced issue's Base flows into the
// Brief: an issue carrying a predecessor's candidate branch is dispatched with that ref
// as the worktree base, not the pipeline default (main). This is the seam that lets
// implement branch from the author-tests candidate.
func TestScheduleReadyThreadsProducedBase(t *testing.T) {
	bd := newFakeBeads()
	bd.ready = []core.Issue{{ID: "iss-2", Role: "implement", Base: "candidate/iss-1"}}
	o, nc := newOrch(t, kernelConfig(2), bd, &fakeGate{}, &fakeMerger{})

	sub, err := nc.SubscribeSync(messaging.WorkSubject("implement"))
	if err != nil {
		t.Fatalf("subscribe work: %v", err)
	}
	o.scheduleReady(context.Background())

	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("no work published: %v", err)
	}
	var brief core.Brief
	if err := json.Unmarshal(msg.Data, &brief); err != nil {
		t.Fatalf("decode brief: %v", err)
	}
	if brief.Base != "candidate/iss-1" {
		t.Errorf("brief Base = %q, want candidate/iss-1 (threaded from the issue)", brief.Base)
	}
}

// TestRunGateThreadsIssueBaseAsBaseRef proves implement's red→green proof verifies against
// the candidate's threaded base — the author-tests candidate that holds the failing tests
// but no implementation — rather than the pipeline base (main). This is the T2.5 wiring:
// runGate must pass issue.Base as the gate's BaseRef, so the proof's red half runs where
// the tests are present but the impl is absent (red on the base, green on the candidate).
// Were it left at main, the tests would be absent on the base and the proof would be
// meaningless. An issue carrying no threaded base falls back to the pipeline base.
func TestRunGateThreadsIssueBaseAsBaseRef(t *testing.T) {
	cfg := kernelConfig(2)
	cfg.Harness.DAG["implement"] = config.Stage{
		Role:          "implement",
		Postcondition: []string{core.PostconditionRedGreen},
		OnFailure:     "implement",
		Produces:      []string{"integrate"},
	}

	t.Run("threaded base", func(t *testing.T) {
		bd := newFakeBeads()
		iss := inProgress("iss-1", "implement", 0)
		iss.Base = "candidate/iss-0" // the author-tests candidate, threaded by advance
		bd.put(iss)
		g := &fakeGate{report: gate.Report{Passed: true}}
		o, _ := newOrch(t, cfg, bd, g, &fakeMerger{})

		if _, err := o.handleResult(context.Background(), core.Result{
			IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: core.CandidateBranch("iss-1")},
		}); err != nil {
			t.Fatalf("handleResult: %v", err)
		}
		got := g.called()
		if len(got) != 1 {
			t.Fatalf("gate called %d times, want 1", len(got))
		}
		if got[0].BaseRef != "candidate/iss-0" {
			t.Errorf("gate BaseRef = %q, want candidate/iss-0 (the issue's threaded base)", got[0].BaseRef)
		}
		if !reflect.DeepEqual(got[0].Postconditions, []string{core.PostconditionRedGreen}) {
			t.Errorf("gate Postconditions = %v, want [tests-red-then-green]", got[0].Postconditions)
		}
	})

	t.Run("fallback to pipeline base", func(t *testing.T) {
		bd := newFakeBeads()
		bd.put(inProgress("iss-2", "implement", 0)) // no threaded Base (freshly seeded)
		g := &fakeGate{report: gate.Report{Passed: true}}
		o, _ := newOrch(t, cfg, bd, g, &fakeMerger{})

		if _, err := o.handleResult(context.Background(), core.Result{
			IssueID: "iss-2", Status: core.StatusDone, Branch: core.Branch{Ref: core.CandidateBranch("iss-2")},
		}); err != nil {
			t.Fatalf("handleResult: %v", err)
		}
		got := g.called()
		if len(got) != 1 || got[0].BaseRef != "main" {
			t.Errorf("gate BaseRef = %+v, want main (the pipeline-base fallback)", got)
		}
	})
}

// --- route (gate fail / failed) ----------------------------------------------

func TestHandleResultGateFailRetries(t *testing.T) {
	bd := newFakeBeads()
	iss := inProgress("iss-1", "implement", 0)
	iss.Base = "candidate/iss-0" // produced from a predecessor; the fix must build on the same base
	bd.put(iss)
	o, _ := newOrch(t, kernelConfig(2), bd, &fakeGate{report: gate.Report{Passed: false}}, &fakeMerger{})

	_, err := o.handleResult(context.Background(), core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: "candidate/iss-1"}})
	if err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	_, _, closed, blocked, applied := bd.snap()
	if len(applied) != 1 || applied[0].Issue.Role != "implement" || applied[0].Issue.Attempt != 1 {
		t.Errorf("applied = %+v, want one implement fix issue at attempt 1", applied)
	}
	// The retry inherits the failed issue's base, so a fix attempt builds on the same
	// branch its predecessor did rather than reverting to main.
	if applied[0].Issue.Base != "candidate/iss-0" {
		t.Errorf("fix issue Base = %q, want candidate/iss-0 (preserved across the retry)", applied[0].Issue.Base)
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
	var alert core.DLQAlert
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

	_, err := o.handleResult(context.Background(), core.Result{
		IssueID: "iss-1", Status: core.StatusNeedsSpecClarification,
		// The escalation invocation harvested a transcript; the orchestrator stamps it onto the
		// issue for every disposition so the Resolve wizard can pre-load it (T4.15).
		Evidence: core.Evidence{Artifacts: []core.ArtifactRef{{Kind: core.ArtifactKindTranscript, Hash: "sha256:tx"}}},
	})
	if err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	if _, _, _, blocked, _ := bd.snap(); len(blocked) != 1 {
		t.Errorf("blocked = %v, want the escalated issue dead-lettered", blocked)
	}
	if _, err := sub.NextMsg(2 * time.Second); err != nil {
		t.Errorf("no dlq alert for escalation: %v", err)
	}
	// The orchestrator's reason classification and the harvested transcript are stamped on the
	// issue (T4.15), so the DLQ / Resolve read path can show *why* it stuck and pre-load the trail.
	got, err := bd.Get(context.Background(), "iss-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.DeadLetterReason != "agent escalated: needs-spec-clarification" {
		t.Errorf("DeadLetterReason = %q, want the escalation classification", got.DeadLetterReason)
	}
	if got.Transcript != "sha256:tx" {
		t.Errorf("Transcript = %q, want the harvested transcript stamped on the dead-lettered issue", got.Transcript)
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

// When an author-tests stage is accepted, the traceability map it harvested (carried as a
// traceability-map artifact on the Result) is threaded onto the produced implement issue,
// like Base — so it survives to the integrate stage where it is cited in provenance.
func TestHandleResultAcceptThreadsTraceMapFromResult(t *testing.T) {
	cfg := kernelConfig(2)
	cfg.Harness.DAG["author-tests"] = config.Stage{Role: "author-tests", Produces: []string{"implement"}, OnFailure: "author-tests"}
	cfg.Souls = append(cfg.Souls, core.Soul{Name: "test-author-go", Role: "author-tests", Sandbox: "go-toolchain"})
	bd := newFakeBeads()
	bd.put(inProgress("iss-1", "author-tests", 0))
	o, _ := newOrch(t, cfg, bd, &fakeGate{report: gate.Report{Passed: true}}, &fakeMerger{})

	res := core.Result{
		IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: "candidate/iss-1"},
		Evidence: core.Evidence{Artifacts: []core.ArtifactRef{{Kind: core.ArtifactKindTraceabilityMap, Hash: "sha256:map"}}},
	}
	if _, err := o.handleResult(context.Background(), res); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	_, _, _, _, applied := bd.snap()
	if len(applied) != 1 || applied[0].Issue.Role != "implement" {
		t.Fatalf("applied = %+v, want one implement proposal", applied)
	}
	if applied[0].Issue.TraceMap != "sha256:map" {
		t.Errorf("produced issue TraceMap = %q, want sha256:map (threaded from the result)", applied[0].Issue.TraceMap)
	}
}

// A later agent stage (implement) carries no traceability map of its own; accepting it
// propagates the map already threaded onto the issue forward to the next produced issue,
// so the author's interpretation reaches integrate regardless of how many stages sit
// between author-tests and the merge.
func TestHandleResultAcceptPropagatesThreadedTraceMap(t *testing.T) {
	cfg := kernelConfig(2)
	cfg.Harness.DAG["implement"] = config.Stage{Role: "implement", Produces: []string{"qa"}, OnFailure: "implement"}
	cfg.Harness.DAG["qa"] = config.Stage{Role: "qa", Produces: []string{"integrate"}}
	cfg.Souls = append(cfg.Souls, core.Soul{Name: "qa-soul", Role: "qa", Sandbox: "go-toolchain"})
	bd := newFakeBeads()
	is := inProgress("iss-1", "implement", 0)
	is.TraceMap = "sha256:map" // threaded earlier from author-tests
	bd.put(is)
	o, _ := newOrch(t, cfg, bd, &fakeGate{report: gate.Report{Passed: true}}, &fakeMerger{})

	// The implement result carries no traceability-map artifact of its own.
	res := core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: "candidate/iss-1"}}
	if _, err := o.handleResult(context.Background(), res); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	_, _, _, _, applied := bd.snap()
	if len(applied) != 1 || applied[0].Issue.Role != "qa" {
		t.Fatalf("applied = %+v, want one qa proposal", applied)
	}
	if applied[0].Issue.TraceMap != "sha256:map" {
		t.Errorf("produced issue TraceMap = %q, want sha256:map (propagated from the issue)", applied[0].Issue.TraceMap)
	}
}

// A merged change whose implement issue carries a threaded traceability map cites it in the
// provenance record, so the human-auditable map is reachable from the commit on main.
func TestProvenanceCitesThreadedTraceMap(t *testing.T) {
	cfg := kernelConfig(2)
	bd := newFakeBeads()
	is := inProgress("iss-1", "implement", 0)
	is.TraceMap = "sha256:map"
	bd.put(is)
	m := &fakeMerger{}
	o, _ := newOrch(t, cfg, bd, &fakeGate{report: gate.Report{Passed: true}}, m)

	res := core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: core.CandidateBranch("iss-1")}}
	if _, err := o.handleResult(context.Background(), res); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	provs := m.provenance()
	if len(provs) != 1 {
		t.Fatalf("provenance recorded %d times, want 1", len(provs))
	}
	if provs[0].Traceability != "sha256:map" {
		t.Errorf("provenance Traceability = %q, want sha256:map (the threaded author-tests map)", provs[0].Traceability)
	}
}

// A rejected candidate routed via on_failure preserves the threaded traceability map onto
// the new fix issue, like Base — so a re-implemented candidate still traces back to the same
// author's interpretation when it eventually merges.
func TestRoutePreservesTraceMap(t *testing.T) {
	cfg := kernelConfig(2)
	bd := newFakeBeads()
	is := inProgress("iss-1", "implement", 0)
	is.TraceMap = "sha256:map"
	is.Base = "candidate/at-1"
	bd.put(is)
	o, _ := newOrch(t, cfg, bd, &fakeGate{report: gate.Report{Passed: false}}, &fakeMerger{})

	res := core.Result{IssueID: "iss-1", Status: core.StatusDone, Branch: core.Branch{Ref: "candidate/iss-1"}}
	if _, err := o.handleResult(context.Background(), res); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	_, _, _, _, applied := bd.snap()
	if len(applied) != 1 {
		t.Fatalf("applied = %+v, want one on_failure fix issue", applied)
	}
	fix := applied[0].Issue
	if fix.TraceMap != "sha256:map" {
		t.Errorf("fix issue TraceMap = %q, want sha256:map (preserved across the retry)", fix.TraceMap)
	}
	if fix.Attempt != 1 || fix.Base != "candidate/at-1" {
		t.Errorf("fix issue Attempt/Base = %d/%q, want 1/candidate/at-1 (preserved alongside trace map)", fix.Attempt, fix.Base)
	}
}
