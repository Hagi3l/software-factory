package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/Loxstomper/harness/internal/artifact"
	"github.com/Loxstomper/harness/internal/broker"
	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/messaging"
	"github.com/Loxstomper/harness/internal/model"
	"github.com/Loxstomper/harness/internal/sandbox"
)

// --- fakes -------------------------------------------------------------------

type fakeSandbox struct {
	id       string
	tornDown atomic.Bool
}

func (s *fakeSandbox) ID() string { return s.id }
func (s *fakeSandbox) Exec(context.Context, sandbox.Command) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil
}
func (s *fakeSandbox) Teardown(context.Context) error {
	s.tornDown.Store(true)
	return nil
}

type fakeBackend struct {
	mu       sync.Mutex
	lastSpec sandbox.Spec
	sandbox  *fakeSandbox
	err      error
}

func (b *fakeBackend) Provision(_ context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastSpec = spec
	if b.err != nil {
		return nil, b.err
	}
	b.sandbox = &fakeSandbox{id: "sb-1"}
	return b.sandbox, nil
}

func (b *fakeBackend) spec() sandbox.Spec {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastSpec
}

// fakeAdapter is a no-op model.Adapter; the runner's broker relay resolves and holds
// it, but these lifecycle tests never make a model call. Relay behavior is covered in
// broker_handler_test.go.
type fakeAdapter struct{}

func (fakeAdapter) Complete(context.Context, model.Request, model.StreamHandler) (model.Response, error) {
	return model.Response{}, nil
}

// fakeResolver returns the same fake adapter for any model name.
type fakeResolver struct{}

func (fakeResolver) Adapter(string) (model.Adapter, error) { return fakeAdapter{}, nil }

// fakeInvoker records the briefs it ran and can fail a configurable number of times
// first (to exercise the Nak/redelivery lease path).
type fakeInvoker struct {
	mu        sync.Mutex
	got       []core.Brief
	gotSb     sandbox.Sandbox
	called    chan struct{}
	failFirst int32
	result    core.Result
	err       error
}

func (i *fakeInvoker) Invoke(_ context.Context, sb sandbox.Sandbox, brief core.Brief, _ sandbox.Endpoint) (core.Result, error) {
	i.mu.Lock()
	i.got = append(i.got, brief)
	i.gotSb = sb
	i.mu.Unlock()
	if i.called != nil {
		i.called <- struct{}{}
	}
	if atomic.AddInt32(&i.failFirst, -1) >= 0 {
		return core.Result{}, i.err
	}
	return i.result, nil
}

func (i *fakeInvoker) briefs() []core.Brief {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]core.Brief(nil), i.got...)
}

// harvestingInvoker dials the broker the runner stood up and makes one model completion,
// so the relay records a prompt + transcript for the runner to harvest — exercising the
// full provenance evidence path (plan T1.20).
type harvestingInvoker struct {
	called chan struct{}
	result core.Result
}

func (i *harvestingInvoker) Invoke(ctx context.Context, _ sandbox.Sandbox, _ core.Brief, ep sandbox.Endpoint) (core.Result, error) {
	c := broker.NewClient(ep.Network, ep.Address)
	if _, err := c.Complete(ctx, model.Request{
		System:   "you are an implementor",
		Messages: []model.Message{{Role: model.RoleUser, Text: "do the work"}},
	}); err != nil {
		return core.Result{}, err
	}
	if i.called != nil {
		i.called <- struct{}{}
	}
	return i.result, nil
}

// --- harness -----------------------------------------------------------------

func testBrief() core.Brief {
	return core.Brief{
		Issue: core.Issue{ID: "iss-1", Title: "do the thing", Role: "implement"},
		Base:  "main",
		Soul:  core.Soul{Name: "implementor-go", Role: "implement", Sandbox: "go-toolchain"},
	}
}

func newRunner(t *testing.T, b *fakeBackend, inv Invoker, mod ...func(*Options)) (*Runner, *nats.Conn) {
	t.Helper()
	srv, err := messaging.NewEmbeddedServer(messaging.ServerConfig{})
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

	store, err := artifact.NewFilesStore(t.TempDir())
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	opts := Options{
		Roles:     []string{"implement"},
		Repo:      "/repo",
		SocketDir: t.TempDir(),
		Limits:    config.SandboxLimits{CPU: 1, Mem: "1Gi", Wall: config.Duration(time.Minute)},
		Allowlist: []string{"llm-api"},
		AckWait:   2 * time.Second,
		ResolveImage: func(profile string) string {
			return "harness/" + profile + "@sha256:test"
		},
	}
	for _, m := range mod {
		m(&opts)
	}
	r, err := New(opts, b, fakeResolver{}, nc, inv, store, js)
	if err != nil {
		t.Fatalf("New runner: %v", err)
	}
	return r, nc
}

func publishWork(t *testing.T, nc *nats.Conn, brief core.Brief) {
	t.Helper()
	js, err := messaging.JetStream(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	data, err := json.Marshal(brief)
	if err != nil {
		t.Fatalf("marshal brief: %v", err)
	}
	if _, err := js.Publish(context.Background(), messaging.WorkSubject(brief.Issue.Role), data); err != nil {
		t.Fatalf("publish work: %v", err)
	}
}

// --- tests -------------------------------------------------------------------

func TestRunProvisionsInvokesPublishesAndAcks(t *testing.T) {
	b := &fakeBackend{}
	inv := &fakeInvoker{
		called: make(chan struct{}, 4),
		result: core.Result{Status: core.StatusDone, Branch: core.Branch{Ref: "candidate/iss-1"}},
	}
	r, nc := newRunner(t, b, inv)

	// Subscribe to the result subject before publishing work so we catch the harvest.
	resultSub, err := nc.SubscribeSync(messaging.ResultSubject("implement"))
	if err != nil {
		t.Fatalf("subscribe results: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	publishWork(t, nc, testBrief())

	select {
	case <-inv.called:
	case <-time.After(5 * time.Second):
		t.Fatal("invoker was never called")
	}

	// The harvested Result is published on the role's result subject.
	msg, err := resultSub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("waiting for published result: %v", err)
	}
	var got core.Result
	if err := json.Unmarshal(msg.Data, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if got.Status != core.StatusDone || got.Branch.Ref != "candidate/iss-1" {
		t.Errorf("published result = %+v, want StatusDone candidate/iss-1", got)
	}

	// The Spec carried the Brief's base ref, soul sandbox profile, configured repo,
	// limits, and a unix broker endpoint.
	spec := b.spec()
	if spec.Profile != "go-toolchain" {
		t.Errorf("spec profile = %q, want go-toolchain", spec.Profile)
	}
	// The logical profile was resolved to a concrete image via Options.ResolveImage and
	// carried on the spec for the backend to boot (and for provenance).
	if spec.Image != "harness/go-toolchain@sha256:test" {
		t.Errorf("spec image = %q, want resolved concrete image", spec.Image)
	}
	if spec.Workspace.BaseRef != "main" || spec.Workspace.Repo != "/repo" {
		t.Errorf("spec workspace = %+v, want repo=/repo base=main", spec.Workspace)
	}
	if spec.Broker.Network != "unix" || spec.Broker.Address == "" {
		t.Errorf("spec broker = %+v, want unix socket", spec.Broker)
	}
	if spec.Limits.CPU != 1 {
		t.Errorf("spec limits = %+v, want CPU=1", spec.Limits)
	}

	// The sandbox was reaped after the invocation.
	b.mu.Lock()
	sb := b.sandbox
	b.mu.Unlock()
	if sb == nil || !waitTrue(func() bool { return sb.tornDown.Load() }) {
		t.Error("sandbox was not torn down after invocation")
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned error: %v", err)
	}
}

func TestHarvestStampsEvidence(t *testing.T) {
	b := &fakeBackend{}
	inv := &harvestingInvoker{
		called: make(chan struct{}, 2),
		result: core.Result{Status: core.StatusDone, Branch: core.Branch{Ref: "candidate/iss-1"}},
	}
	r, nc := newRunner(t, b, inv)

	resultSub, err := nc.SubscribeSync(messaging.ResultSubject("implement"))
	if err != nil {
		t.Fatalf("subscribe results: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	publishWork(t, nc, testBrief())
	select {
	case <-inv.called:
	case <-time.After(5 * time.Second):
		t.Fatal("invoker was never called")
	}

	msg, err := resultSub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("waiting for published result: %v", err)
	}
	var got core.Result
	if err := json.Unmarshal(msg.Data, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// The harvested prompt's content address is stamped as the Prompt-SHA, and both the
	// prompt and the transcript are recorded as artifacts pointing into the store.
	if got.Evidence.PromptSHA == "" {
		t.Error("Result.Evidence.PromptSHA was not stamped from the harvested prompt")
	}
	var kinds []string
	for _, a := range got.Evidence.Artifacts {
		kinds = append(kinds, a.Kind)
		if has, _ := r.store.Has(ctx, a.Hash); !has {
			t.Errorf("artifact %s (%s) not present in the store", a.Kind, a.Hash)
		}
	}
	if !containsStr(kinds, core.ArtifactKindPrompt) || !containsStr(kinds, core.ArtifactKindTranscript) {
		t.Errorf("harvested artifact kinds = %v, want prompt + transcript", kinds)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned error: %v", err)
	}
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// exploringInvoker dials the broker and makes BOTH a parent completion and an explorer-tagged
// completion (via the explore sub-completer), so the relay records a parent transcript AND a
// separate explore transcript for the runner to harvest — exercising the T12.4 evidence path.
type exploringInvoker struct {
	called chan struct{}
	result core.Result
}

func (i *exploringInvoker) Invoke(ctx context.Context, _ sandbox.Sandbox, _ core.Brief, ep sandbox.Endpoint) (core.Result, error) {
	c := broker.NewClient(ep.Network, ep.Address)
	if _, err := c.Complete(ctx, model.Request{Messages: []model.Message{{Role: model.RoleUser, Text: "parent"}}}); err != nil {
		return core.Result{}, err
	}
	if _, err := c.ExploreCompleter("s1").Complete(ctx, model.Request{Messages: []model.Message{{Role: model.RoleUser, Text: "explore"}}}); err != nil {
		return core.Result{}, err
	}
	if i.called != nil {
		i.called <- struct{}{}
	}
	return i.result, nil
}

// TestHarvestStampsExploreEvidence: when the explore sub-loop ran, the runner harvests the
// explore transcript as its OWN artifact (kind explore-transcript, distinct from the main
// transcript) and stamps the pinned explorer model onto the Result — the provenance signal the
// merge trailer records (specs/components/agent.md rule 5, specs/models.md "Helper souls").
func TestHarvestStampsExploreEvidence(t *testing.T) {
	b := &fakeBackend{}
	inv := &exploringInvoker{
		called: make(chan struct{}, 2),
		result: core.Result{Status: core.StatusDone, Branch: core.Branch{Ref: "candidate/iss-1"}},
	}
	r, nc := newRunner(t, b, inv)

	resultSub, err := nc.SubscribeSync(messaging.ResultSubject("implement"))
	if err != nil {
		t.Fatalf("subscribe results: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// A brief with an explorer soul pinned makes the runner build the second (explorer) adapter,
	// so an explorer-tagged completion is relayed and metered rather than failing closed.
	brief := testBrief()
	brief.Explorer = &core.Soul{Name: "explorer-cheap", Role: config.RoleExplorer, Model: "cheap-model", Sandbox: "go-toolchain"}
	brief.ExploreBudget = core.ExploreBudget{Tokens: 10000}
	publishWork(t, nc, brief)

	select {
	case <-inv.called:
	case <-time.After(5 * time.Second):
		t.Fatal("invoker was never called")
	}

	msg, err := resultSub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("waiting for published result: %v", err)
	}
	var got core.Result
	if err := json.Unmarshal(msg.Data, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// The pinned explorer model is stamped from the relay's authoritative record.
	if got.ExploreModel != "cheap-model" {
		t.Errorf("Result.ExploreModel = %q, want cheap-model", got.ExploreModel)
	}
	// Both transcripts are harvested as SEPARATE artifacts.
	var kinds []string
	for _, a := range got.Evidence.Artifacts {
		kinds = append(kinds, a.Kind)
		if has, _ := r.store.Has(ctx, a.Hash); !has {
			t.Errorf("artifact %s (%s) not present in the store", a.Kind, a.Hash)
		}
	}
	if !containsStr(kinds, core.ArtifactKindTranscript) || !containsStr(kinds, core.ArtifactKindExploreTranscript) {
		t.Errorf("harvested artifact kinds = %v, want both transcript + explore-transcript", kinds)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned error: %v", err)
	}
}

// A Result carrying a test↔spec traceability map (the author-tests stage produces one) is
// harvested to the artifact store under the traceability-map kind, and the structured form
// is cleared from the envelope so the bulky map travels by hash, not inline (see
// specs/components/artifact-store.md). The stored bytes are the deterministic rendering.
func TestHarvestStoresTraceabilityMap(t *testing.T) {
	entries := []core.TraceEntry{
		{Test: "TestRejectsNegative", Spec: "orders.md", Heading: "Quantities", Sentence: "reject negative quantities with a 400"},
		{Test: "TestHappyPath", Heading: "Quantities", Sentence: "accept positive quantities"},
	}
	b := &fakeBackend{}
	inv := &harvestingInvoker{
		called: make(chan struct{}, 2),
		result: core.Result{Status: core.StatusDone, Branch: core.Branch{Ref: "candidate/iss-1"}, Trace: entries},
	}
	r, nc := newRunner(t, b, inv)

	resultSub, err := nc.SubscribeSync(messaging.ResultSubject("implement"))
	if err != nil {
		t.Fatalf("subscribe results: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	publishWork(t, nc, testBrief())
	select {
	case <-inv.called:
	case <-time.After(5 * time.Second):
		t.Fatal("invoker was never called")
	}

	msg, err := resultSub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("waiting for published result: %v", err)
	}
	var got core.Result
	if err := json.Unmarshal(msg.Data, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if len(got.Trace) != 0 {
		t.Errorf("Result.Trace = %+v, want cleared after harvest (map travels by hash)", got.Trace)
	}
	var mapHash string
	for _, a := range got.Evidence.Artifacts {
		if a.Kind == core.ArtifactKindTraceabilityMap {
			mapHash = a.Hash
		}
	}
	if mapHash == "" {
		t.Fatalf("no %s artifact on Evidence; got %+v", core.ArtifactKindTraceabilityMap, got.Evidence.Artifacts)
	}
	rc, err := r.store.Get(ctx, mapHash)
	if err != nil {
		t.Fatalf("get traceability map: %v", err)
	}
	defer rc.Close()
	stored, _ := io.ReadAll(rc)
	if !bytes.Equal(stored, formatTraceabilityMap(entries)) {
		t.Errorf("stored map =\n%s\nwant\n%s", stored, formatTraceabilityMap(entries))
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned error: %v", err)
	}
}

// formatTraceabilityMap renders a stable, human-readable document, one block per test in
// emission order, omitting the optional spec line when absent. Determinism is what lets
// the same map content-address to one hash and the provenance citation be reproducible.
func TestFormatTraceabilityMap(t *testing.T) {
	out := string(formatTraceabilityMap([]core.TraceEntry{
		{Test: "TestA", Spec: "orders.md", Heading: "Quantities", Sentence: "reject negatives"},
		{Test: "TestB", Heading: "Auth", Sentence: "require a token"},
	}))
	want := "# Test ↔ spec traceability map\n" +
		"\ntest: TestA\nspec: orders.md\nheading: Quantities\nsentence: reject negatives\n" +
		"\ntest: TestB\nheading: Auth\nsentence: require a token\n"
	if out != want {
		t.Errorf("formatTraceabilityMap =\n%q\nwant\n%q", out, want)
	}
}

func TestHarvestStoresTransformLog(t *testing.T) {
	records := []core.TransformRecord{
		{Tool: "rename", Target: "a.go:3:6 → hello", Mechanism: core.TransformMechanismSemantic, Files: 2, Edits: 4},
		{Tool: "rename", Target: "greet → hi at b.go:1:1", Mechanism: core.TransformMechanismText, Files: 1, Edits: 3, Note: "3 match(es) across 1 file(s); 1 inside comments or string literals (heuristic) — review them"},
	}
	b := &fakeBackend{}
	inv := &harvestingInvoker{
		called: make(chan struct{}, 2),
		result: core.Result{Status: core.StatusDone, Branch: core.Branch{Ref: "candidate/iss-1"}, Transforms: records},
	}
	r, nc := newRunner(t, b, inv)

	resultSub, err := nc.SubscribeSync(messaging.ResultSubject("implement"))
	if err != nil {
		t.Fatalf("subscribe results: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	publishWork(t, nc, testBrief())
	select {
	case <-inv.called:
	case <-time.After(5 * time.Second):
		t.Fatal("invoker was never called")
	}

	msg, err := resultSub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("waiting for published result: %v", err)
	}
	var got core.Result
	if err := json.Unmarshal(msg.Data, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	if len(got.Transforms) != 0 {
		t.Errorf("Result.Transforms = %+v, want cleared after harvest (log travels by hash)", got.Transforms)
	}
	var logHash string
	for _, a := range got.Evidence.Artifacts {
		if a.Kind == core.ArtifactKindTransformLog {
			logHash = a.Hash
		}
	}
	if logHash == "" {
		t.Fatalf("no %s artifact on Evidence; got %+v", core.ArtifactKindTransformLog, got.Evidence.Artifacts)
	}
	rc, err := r.store.Get(ctx, logHash)
	if err != nil {
		t.Fatalf("get transform log: %v", err)
	}
	defer rc.Close()
	stored, _ := io.ReadAll(rc)
	// The log is harvested as JSON (the canonical []core.TransformRecord), so the verification
	// view can read it back structurally — the same content-addressed discipline the gate verdict
	// uses. Assert it round-trips exactly to the records the invoker produced.
	var decoded []core.TransformRecord
	if err := json.Unmarshal(stored, &decoded); err != nil {
		t.Fatalf("stored transform log is not valid JSON: %v\n%s", err, stored)
	}
	if !reflect.DeepEqual(decoded, records) {
		t.Errorf("decoded log = %+v\nwant %+v", decoded, records)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned error: %v", err)
	}
}

func TestNakOnInvokeErrorRedelivers(t *testing.T) {
	b := &fakeBackend{}
	inv := &fakeInvoker{
		called:    make(chan struct{}, 8),
		failFirst: 1, // fail the first delivery, succeed the redelivery
		err:       context.DeadlineExceeded,
		result:    core.Result{Status: core.StatusDone},
	}
	r, nc := newRunner(t, b, inv)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	publishWork(t, nc, testBrief())

	// First invoke fails (Nak), second (redelivery) succeeds: expect ≥2 calls.
	for i := 0; i < 2; i++ {
		select {
		case <-inv.called:
		case <-time.After(8 * time.Second):
			t.Fatalf("expected redelivery, only got %d invoke call(s)", i)
		}
	}
	if got := len(inv.briefs()); got < 2 {
		t.Errorf("invoke calls = %d, want ≥2 (Nak should redeliver)", got)
	}

	cancel()
	<-done
}

func TestTeardownRunsEvenWhenInvokeErrors(t *testing.T) {
	b := &fakeBackend{}
	inv := &fakeInvoker{
		called:    make(chan struct{}, 8),
		failFirst: 100, // always fail
		err:       context.DeadlineExceeded,
	}
	r, nc := newRunner(t, b, inv)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	publishWork(t, nc, testBrief())
	select {
	case <-inv.called:
	case <-time.After(5 * time.Second):
		t.Fatal("invoker was never called")
	}

	b.mu.Lock()
	sb := b.sandbox
	b.mu.Unlock()
	if sb == nil || !waitTrue(func() bool { return sb.tornDown.Load() }) {
		t.Error("sandbox must be torn down even when the invocation errors")
	}

	cancel()
	<-done
}

func TestTermOnPoisonMessage(t *testing.T) {
	b := &fakeBackend{}
	inv := &fakeInvoker{
		called: make(chan struct{}, 4),
		result: core.Result{Status: core.StatusDone},
	}
	r, nc := newRunner(t, b, inv)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// Publish undecodable bytes, then a valid brief.
	js, _ := messaging.JetStream(nc)
	if _, err := js.Publish(context.Background(), messaging.WorkSubject("implement"), []byte("{not json")); err != nil {
		t.Fatalf("publish poison: %v", err)
	}
	publishWork(t, nc, testBrief())

	// The valid brief must be processed; the poison one must not block or loop the runner.
	select {
	case <-inv.called:
	case <-time.After(5 * time.Second):
		t.Fatal("valid brief was not processed after a poison message")
	}
	if got := len(inv.briefs()); got != 1 {
		t.Errorf("invoke calls = %d, want exactly 1 (poison message should not invoke)", got)
	}

	cancel()
	<-done
}

func TestNewValidatesOptions(t *testing.T) {
	_, err := New(Options{}, &fakeBackend{}, fakeResolver{}, (*nats.Conn)(nil), &fakeInvoker{}, nil, nil)
	if err == nil {
		t.Fatal("New with empty options: want error, got nil")
	}
}

// blockingInvoker announces each invocation on entered, then blocks on release — so a test can
// observe how many invocations of one role run at the same time.
type blockingInvoker struct {
	entered chan core.Brief
	release chan struct{}
	result  core.Result
}

func (i *blockingInvoker) Invoke(_ context.Context, _ sandbox.Sandbox, brief core.Brief, _ sandbox.Endpoint) (core.Result, error) {
	i.entered <- brief
	<-i.release
	return i.result, nil
}

// With MaxConcurrency=2 a role's runner serves two same-role siblings at once: both
// invocations enter before either is released. At the default (serial) concurrency the second
// would not start until the first returned, so the test would time out at one. This is the
// lever that fans out a wide decomposition instead of serializing it on the slowest stage.
func TestMaxConcurrencyRunsSameRoleSiblingsInParallel(t *testing.T) {
	inv := &blockingInvoker{
		entered: make(chan core.Brief, 2),
		release: make(chan struct{}),
		result:  core.Result{Status: core.StatusDone},
	}
	r, nc := newRunner(t, &fakeBackend{}, inv, func(o *Options) { o.MaxConcurrency = 2 })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	b1 := testBrief()
	b1.Issue.ID = "iss-1"
	b2 := testBrief()
	b2.Issue.ID = "iss-2"
	publishWork(t, nc, b1)
	publishWork(t, nc, b2)

	// Both siblings must ENTER before either is released — only possible if they run at once.
	for n := 0; n < 2; n++ {
		select {
		case <-inv.entered:
		case <-time.After(3 * time.Second):
			close(inv.release) // unblock any in-flight invoker so the runner can drain and Run returns
			cancel()
			<-done
			t.Fatalf("only %d of 2 same-role siblings ran concurrently — runner serialized the work", n)
		}
	}
	close(inv.release) // let both complete and ack

	cancel()
	<-done
}

// waitTrue polls cond for up to a second; teardown happens in a deferred goroutine
// path so it may lag the invoke-call signal slightly.
func waitTrue(cond func() bool) bool {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}
