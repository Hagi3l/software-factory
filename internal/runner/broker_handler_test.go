package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Loxstomper/harness/internal/broker"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/model"
	"github.com/Loxstomper/harness/internal/sandbox"
)

// --- relay fakes -------------------------------------------------------------

// recordingAdapter streams the configured deltas, returns the configured response (or
// error), and counts calls so usage-tally accumulation across calls is observable.
type recordingAdapter struct {
	deltas    []string
	reasoning []string
	resp      model.Response
	err       error
	calls     int
}

func (a *recordingAdapter) Complete(_ context.Context, _ model.Request, onEvent model.StreamHandler) (model.Response, error) {
	a.calls++
	if a.err != nil {
		return model.Response{}, a.err
	}
	for _, d := range a.deltas {
		if onEvent != nil {
			onEvent(model.StreamEvent{TextDelta: d})
		}
	}
	for _, d := range a.reasoning {
		if onEvent != nil {
			onEvent(model.StreamEvent{ReasoningDelta: d})
		}
	}
	return a.resp, nil
}

// recordingPublisher captures every published (subject, data) so token/event fan-out
// can be asserted.
type recordingPublisher struct {
	mu   sync.Mutex
	subj []string
	data [][]byte
	err  error
}

func (p *recordingPublisher) Publish(subject string, data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subj = append(p.subj, subject)
	p.data = append(p.data, append([]byte(nil), data...))
	return p.err
}

func (p *recordingPublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.subj)
}

// bundleSandbox records the Exec command and returns a canned ExecResult, standing in
// for the in-sandbox `git bundle` without a real container.
type bundleSandbox struct {
	gotCmd  sandbox.Command
	result  sandbox.ExecResult
	execErr error
}

func (s *bundleSandbox) ID() string { return "sb-relay" }
func (s *bundleSandbox) Exec(_ context.Context, cmd sandbox.Command) (sandbox.ExecResult, error) {
	s.gotCmd = cmd
	return s.result, s.execErr
}
func (s *bundleSandbox) Teardown(context.Context) error { return nil }

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testRelay(adapter model.Adapter, pub Publisher, sb sandbox.Sandbox) *relay {
	return newRelay(adapter, pub, sb, relayConfig{
		eventSubject:  "harness.agent.inv-1.events",
		issueID:       "iss-1",
		role:          "implementor",
		repo:          "/repo",
		allowedBranch: "candidate/iss-1",
		log:           discardLogger(),
	})
}

// decodeEvent unwraps one published agent-event wire payload: the issue/role-stamped
// envelope (core.AgentEventEnvelope) the relay publishes, returning the envelope and the
// opaque inner event bytes. Every published event funnels through the same envelope, so
// the tests assert the stamping here and decode the inner event from the returned payload.
func decodeEvent(t *testing.T, data []byte) (core.AgentEventEnvelope, []byte) {
	t.Helper()
	var env core.AgentEventEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("unmarshal event envelope: %v", err)
	}
	if env.IssueID != "iss-1" || env.Role != "implementor" {
		t.Errorf("envelope binding = {issue:%q role:%q}, want {iss-1 implementor}", env.IssueID, env.Role)
	}
	return env, env.Payload
}

// --- Complete ----------------------------------------------------------------

func TestRelayCompleteStreamsDeltasAndTalliesUsage(t *testing.T) {
	adapter := &recordingAdapter{
		deltas: []string{"hel", "lo"},
		resp:   model.Response{Text: "hello", Stop: model.StopEndTurn, Usage: model.Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 2}},
	}
	pub := &recordingPublisher{}
	r := testRelay(adapter, pub, &bundleSandbox{})

	resp, err := r.Complete(context.Background(), model.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "hello" {
		t.Errorf("resp.Text = %q, want hello", resp.Text)
	}

	// Each non-empty text delta is published to the invocation's event subject.
	if pub.count() != 2 {
		t.Fatalf("published events = %d, want 2", pub.count())
	}
	if pub.subj[0] != "harness.agent.inv-1.events" {
		t.Errorf("event subject = %q, want harness.agent.inv-1.events", pub.subj[0])
	}
	_, payload := decodeEvent(t, pub.data[0])
	var ev tokenEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if ev.Type != "token" || ev.Delta != "hel" {
		t.Errorf("event = %+v, want {token hel}", ev)
	}

	// A second completion accumulates onto the running usage tally (the budget input).
	if _, err := r.Complete(context.Background(), model.Request{}); err != nil {
		t.Fatalf("second Complete: %v", err)
	}
	u := r.Usage()
	if u.InputTokens != 20 || u.OutputTokens != 10 || u.CacheReadTokens != 4 {
		t.Errorf("tallied usage = %+v, want input=20 output=10 cacheRead=4", u)
	}
}

func TestRelayCompletePublishesReasoningAndToolEvents(t *testing.T) {
	adapter := &recordingAdapter{
		deltas:    []string{"sure"},
		reasoning: []string{"let me ", "think"},
		resp: model.Response{
			Stop: model.StopToolUse,
			ToolCalls: []model.ToolCall{
				{ID: "1", Name: "write_file", Args: json.RawMessage(`{"path":"index.html","content":"<html>"}`)},
				{ID: "2", Name: "run_tests"},
			},
		},
	}
	pub := &recordingPublisher{}
	r := testRelay(adapter, pub, &bundleSandbox{})

	if _, err := r.Complete(context.Background(), model.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Published in stream order: the text delta, then the two reasoning deltas, then one
	// tool row per tool call (emitted from the assembled response after the turn).
	var got []tokenEvent
	for _, d := range pub.data {
		_, payload := decodeEvent(t, d)
		var ev tokenEvent
		if err := json.Unmarshal(payload, &ev); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		got = append(got, ev)
	}
	want := []tokenEvent{
		{Type: "token", Delta: "sure"},
		{Type: "reasoning", Delta: "let me "},
		{Type: "reasoning", Delta: "think"},
		{Type: "tool", Delta: "write_file index.html"},
		{Type: "tool", Delta: "run_tests"},
	}
	if len(got) != len(want) {
		t.Fatalf("published events = %d (%+v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestRelayCompletePropagatesErrorAndDoesNotTally(t *testing.T) {
	adapter := &recordingAdapter{err: errors.New("model API 503")}
	r := testRelay(adapter, &recordingPublisher{}, &bundleSandbox{})

	if _, err := r.Complete(context.Background(), model.Request{}); err == nil {
		t.Fatal("Complete: want error, got nil")
	}
	if u := r.Usage(); u != (model.Usage{}) {
		t.Errorf("usage = %+v, want zero (a failed call tallies nothing)", u)
	}
}

func TestRelayCapturesPromptAndTranscript(t *testing.T) {
	adapter := &recordingAdapter{resp: model.Response{Text: "ok", Stop: model.StopEndTurn}}
	r := testRelay(adapter, &recordingPublisher{}, &bundleSandbox{})

	// No model call yet: there is nothing to harvest.
	if _, ok := r.Prompt(); ok {
		t.Error("Prompt available before any completion")
	}
	if _, ok := r.Transcript(); ok {
		t.Error("Transcript available before any completion")
	}

	first := model.Request{System: "persona", Messages: []model.Message{{Role: model.RoleUser, Text: "first"}}}
	if _, err := r.Complete(context.Background(), first); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// A second turn must not overwrite the captured prompt (the first request).
	if _, err := r.Complete(context.Background(), model.Request{Messages: []model.Message{{Role: model.RoleUser, Text: "second"}}}); err != nil {
		t.Fatalf("second Complete: %v", err)
	}

	promptData, ok := r.Prompt()
	if !ok {
		t.Fatal("Prompt not captured after a completion")
	}
	var gotPrompt model.Request
	if err := json.Unmarshal(promptData, &gotPrompt); err != nil {
		t.Fatalf("unmarshal prompt: %v", err)
	}
	if gotPrompt.System != "persona" || len(gotPrompt.Messages) != 1 || gotPrompt.Messages[0].Text != "first" {
		t.Errorf("captured prompt = %+v, want the first request", gotPrompt)
	}

	transcriptData, ok := r.Transcript()
	if !ok {
		t.Fatal("Transcript not captured after completions")
	}
	var turns []model.TranscriptTurn
	if err := json.Unmarshal(transcriptData, &turns); err != nil {
		t.Fatalf("unmarshal transcript: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("transcript turns = %d, want 2", len(turns))
	}
	if turns[0].Request.Messages[0].Text != "first" || turns[1].Request.Messages[0].Text != "second" {
		t.Errorf("transcript did not record both turns in order: %+v", turns)
	}
}

// --- GitPush -----------------------------------------------------------------

func TestRelayGitPushRefusesNonTaskBranch(t *testing.T) {
	sb := &bundleSandbox{}
	r := testRelay(&recordingAdapter{}, &recordingPublisher{}, sb)
	pushed := false
	r.pushBundle = func(context.Context, string, string, []byte) (string, error) {
		pushed = true
		return "", nil
	}

	_, err := r.GitPush(context.Background(), broker.GitPushRequest{Branch: "main"})
	if err == nil {
		t.Fatal("GitPush onto main: want error, got nil")
	}
	if sb.gotCmd.Path != "" {
		t.Error("a refused branch must not exec inside the sandbox")
	}
	if pushed {
		t.Error("a refused branch must not reach pushBundle")
	}
}

func TestRelayGitPushExtractsBundleAndPushes(t *testing.T) {
	sb := &bundleSandbox{result: sandbox.ExecResult{ExitCode: 0, Stdout: []byte("BUNDLEBYTES")}}
	r := testRelay(&recordingAdapter{}, &recordingPublisher{}, sb)

	var gotRepo, gotBranch string
	var gotBundle []byte
	r.pushBundle = func(_ context.Context, repo, branch string, bundle []byte) (string, error) {
		gotRepo, gotBranch, gotBundle = repo, branch, bundle
		return "deadbeef", nil
	}

	res, err := r.GitPush(context.Background(), broker.GitPushRequest{Branch: "candidate/iss-1"})
	if err != nil {
		t.Fatalf("GitPush: %v", err)
	}
	if res.Commit != "deadbeef" {
		t.Errorf("commit = %q, want deadbeef", res.Commit)
	}

	// The branch is extracted as a git bundle on stdout from inside the sandbox.
	want := []string{"bundle", "create", "-", "candidate/iss-1"}
	if sb.gotCmd.Path != "git" || strings.Join(sb.gotCmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("exec = %s %v, want git %v", sb.gotCmd.Path, sb.gotCmd.Args, want)
	}
	if gotRepo != "/repo" || gotBranch != "candidate/iss-1" || string(gotBundle) != "BUNDLEBYTES" {
		t.Errorf("pushBundle got repo=%q branch=%q bundle=%q", gotRepo, gotBranch, string(gotBundle))
	}
}

func TestRelayGitPushFailsOnNonZeroBundleExit(t *testing.T) {
	sb := &bundleSandbox{result: sandbox.ExecResult{ExitCode: 128, Stderr: []byte("not a valid object name")}}
	r := testRelay(&recordingAdapter{}, &recordingPublisher{}, sb)
	r.pushBundle = func(context.Context, string, string, []byte) (string, error) {
		t.Fatal("pushBundle must not run when bundle extraction fails")
		return "", nil
	}

	if _, err := r.GitPush(context.Background(), broker.GitPushRequest{Branch: "candidate/iss-1"}); err == nil {
		t.Fatal("GitPush with failing bundle: want error, got nil")
	}
}

// --- PublishEvent ------------------------------------------------------------

func TestRelayPublishEvent(t *testing.T) {
	pub := &recordingPublisher{}
	r := testRelay(&recordingAdapter{}, pub, &bundleSandbox{})

	err := r.PublishEvent(context.Background(), broker.PublishRequest{Type: "progress", Payload: json.RawMessage(`{"msg":"hi"}`)})
	if err != nil {
		t.Fatalf("PublishEvent: %v", err)
	}
	if pub.count() != 1 || pub.subj[0] != "harness.agent.inv-1.events" {
		t.Fatalf("published %d events on %v, want 1 on the agent event subject", pub.count(), pub.subj)
	}
	_, payload := decodeEvent(t, pub.data[0])
	var got broker.PublishRequest
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if got.Type != "progress" {
		t.Errorf("event type = %q, want progress", got.Type)
	}
}

// --- FetchPackage ------------------------------------------------------------

func TestRelayFetchPackageProxiesAndLogs(t *testing.T) {
	var gotPath string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("v1.0.0\nv1.1.0\n"))
	}))
	defer proxy.Close()

	r := newRelay(&recordingAdapter{}, &recordingPublisher{}, &bundleSandbox{}, relayConfig{
		eventSubject: "harness.agent.inv-1.events", issueID: "iss-1", role: "implementor",
		repo: "/repo", allowedBranch: "candidate/iss-1", log: discardLogger(),
		packageProxy: proxy.URL,
	})

	res, err := r.FetchPackage(context.Background(), broker.FetchPackageRequest{Path: "/github.com/pkg/errors/@v/list"})
	if err != nil {
		t.Fatalf("FetchPackage: %v", err)
	}
	if gotPath != "/github.com/pkg/errors/@v/list" {
		t.Errorf("proxy got path %q, want the request path joined onto the proxy base", gotPath)
	}
	if res.Status != 200 || string(res.Body) != "v1.0.0\nv1.1.0\n" {
		t.Errorf("result = %+v, want status 200 and the proxied body", res)
	}
	if !strings.HasPrefix(res.ContentType, "text/plain") {
		t.Errorf("content-type = %q, want the upstream text/plain echoed", res.ContentType)
	}
}

func TestRelayFetchPackageForwardsUpstreamStatus(t *testing.T) {
	// 404/410 must be echoed, not swallowed: go reads them as "not found, try the next proxy".
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer proxy.Close()

	r := newRelay(&recordingAdapter{}, &recordingPublisher{}, &bundleSandbox{}, relayConfig{
		eventSubject: "e", issueID: "iss-1", role: "implementor", repo: "/repo",
		allowedBranch: "candidate/iss-1", log: discardLogger(), packageProxy: proxy.URL,
	})

	res, err := r.FetchPackage(context.Background(), broker.FetchPackageRequest{Path: "/x/@v/v9.9.9.info"})
	if err != nil {
		t.Fatalf("FetchPackage: %v", err)
	}
	if res.Status != http.StatusGone {
		t.Errorf("status = %d, want 410 echoed from upstream", res.Status)
	}
}

func TestRelayFetchPackageNoProxyConfigured(t *testing.T) {
	r := newRelay(&recordingAdapter{}, &recordingPublisher{}, &bundleSandbox{}, relayConfig{
		eventSubject: "e", issueID: "iss-1", role: "implementor", repo: "/repo",
		allowedBranch: "candidate/iss-1", log: discardLogger(), // packageProxy empty
	})
	if _, err := r.FetchPackage(context.Background(), broker.FetchPackageRequest{Path: "/x/@v/list"}); err == nil {
		t.Fatal("FetchPackage with no proxy configured must error, got nil")
	}
}

func TestRelayFetchPackageRejectsMalformedPath(t *testing.T) {
	// The proxy must never be dialed for a malformed path; a hit here is a failure.
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("malformed path must be rejected before any egress")
	}))
	defer proxy.Close()

	r := newRelay(&recordingAdapter{}, &recordingPublisher{}, &bundleSandbox{}, relayConfig{
		eventSubject: "e", issueID: "iss-1", role: "implementor", repo: "/repo",
		allowedBranch: "candidate/iss-1", log: discardLogger(), packageProxy: proxy.URL,
	})

	for _, bad := range []string{"", "no-leading-slash", "/has/../traversal", "https://evil.example/x", "/has space"} {
		if _, err := r.FetchPackage(context.Background(), broker.FetchPackageRequest{Path: bad}); err == nil {
			t.Errorf("path %q: want rejection, got nil error", bad)
		}
	}
}

// --- host-side bundle apply (real git, no docker) ----------------------------

// TestPushBundleToRepoIntegration drives the real host-side git path: it builds a
// candidate branch in one repo, bundles it exactly as the in-sandbox exec would, and
// asserts pushBundleToRepo lands that branch+commit in a separate source repo. This is
// what makes a candidate reachable to the gate/merge without a bind mount or copy-out.
func TestPushBundleToRepoIntegration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()

	// Source repo the candidate is pushed into (mirrors r.opts.Repo).
	srcRepo := t.TempDir()
	mustGit(t, srcRepo, "init", "-q")
	mustGit(t, srcRepo, "config", "user.email", "t@example.com")
	mustGit(t, srcRepo, "config", "user.name", "t")
	writeFile(t, filepath.Join(srcRepo, "base.txt"), "base")
	mustGit(t, srcRepo, "add", ".")
	mustGit(t, srcRepo, "commit", "-qm", "base")

	// Candidate repo (mirrors the sandbox worktree): clone src, branch, commit, bundle.
	candRepo := t.TempDir()
	mustGit(t, "", "clone", "-q", srcRepo, candRepo)
	mustGit(t, candRepo, "config", "user.email", "t@example.com")
	mustGit(t, candRepo, "config", "user.name", "t")
	mustGit(t, candRepo, "checkout", "-q", "-b", "candidate/iss-1")
	writeFile(t, filepath.Join(candRepo, "feature.txt"), "feature")
	mustGit(t, candRepo, "add", ".")
	mustGit(t, candRepo, "commit", "-qm", "feature")
	wantSHA := strings.TrimSpace(mustGit(t, candRepo, "rev-parse", "candidate/iss-1"))
	bundle := []byte(mustGitRaw(t, candRepo, "bundle", "create", "-", "candidate/iss-1"))

	commit, err := pushBundleToRepo(ctx, srcRepo, "candidate/iss-1", bundle)
	if err != nil {
		t.Fatalf("pushBundleToRepo: %v", err)
	}
	if commit != wantSHA {
		t.Errorf("returned commit = %q, want %q", commit, wantSHA)
	}
	// The branch now exists in the source repo at the candidate head.
	gotSHA := strings.TrimSpace(mustGit(t, srcRepo, "rev-parse", "refs/heads/candidate/iss-1"))
	if gotSHA != wantSHA {
		t.Errorf("source repo branch head = %q, want %q", gotSHA, wantSHA)
	}
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return mustGitRaw(t, dir, args...)
}

func mustGitRaw(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
