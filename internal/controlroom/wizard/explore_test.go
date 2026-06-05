package wizard_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/controlroom/live"
	"github.com/Loxstomper/harness/internal/controlroom/wizard"
	"github.com/Loxstomper/harness/internal/model/modeltest"
	"github.com/Loxstomper/harness/internal/sandbox"
)

// fakeSandbox is a no-Docker stand-in for an exploration sandbox: every Exec is recorded and
// answered with canned stdout so the read tools (cat/ls/grep) "succeed", and Teardown flips a
// flag the eviction test asserts on. It does NOT implement sandbox.SessionOpener, so the LSP
// sessions stay degraded (semantic tools fall back to grep) — exactly the host-side posture.
type fakeSandbox struct {
	mu       sync.Mutex
	execs    []sandbox.Command
	tornDown bool
}

func (f *fakeSandbox) ID() string { return "fake-explore" }

func (f *fakeSandbox) Exec(_ context.Context, cmd sandbox.Command) (sandbox.ExecResult, error) {
	f.mu.Lock()
	f.execs = append(f.execs, cmd)
	f.mu.Unlock()
	return sandbox.ExecResult{ExitCode: 0, Stdout: []byte("module example\n\ngo 1.23\n")}, nil
}

func (f *fakeSandbox) Teardown(context.Context) error {
	f.mu.Lock()
	f.tornDown = true
	f.mu.Unlock()
	return nil
}

func (f *fakeSandbox) sawExec(path, argSubstr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.execs {
		if c.Path != path {
			continue
		}
		if strings.Contains(strings.Join(c.Args, " "), argSubstr) {
			return true
		}
	}
	return false
}

func (f *fakeSandbox) wasTornDown() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tornDown
}

// fakeBackend provisions fakeSandboxes and counts how many — the laziness/eviction tests assert
// on that count instead of booting Docker.
type fakeBackend struct {
	mu          sync.Mutex
	provisioned []*fakeSandbox
}

func (b *fakeBackend) Provision(_ context.Context, _ sandbox.Spec) (sandbox.Sandbox, error) {
	sb := &fakeSandbox{}
	b.mu.Lock()
	b.provisioned = append(b.provisioned, sb)
	b.mu.Unlock()
	return sb, nil
}

func (b *fakeBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.provisioned)
}

func (b *fakeBackend) first() *fakeSandbox {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.provisioned) == 0 {
		return nil
	}
	return b.provisioned[0]
}

// withSandboxOpt wires a fake-backed exploration config: repo is unused by the fake, but sockDir
// must be a real writable dir because buildExplorer binds a real deny-all broker socket there
// (the only piece not faked, mirroring the gate's unit tests).
func withSandboxOpt(t *testing.T, be *fakeBackend) wizard.Option {
	t.Helper()
	return wizard.WithSandbox(be, t.TempDir(), "prof", "img", "main", config.SandboxLimits{}, t.TempDir())
}

// TestExploreToolLoopGroundsAndParses is the core contract of T4.28: a planner reply that calls
// a read tool drives a second model turn whose results it fed back, the tool ran against the
// (fake) sandbox, only the FINAL prose lands in the transcript (the intermediate tool-call turn
// is suppressed), and that final turn's ledger + draft parse as usual.
func TestExploreToolLoopGroundsAndParses(t *testing.T) {
	final := "Here is the plan.\n\n```ledger\n" +
		`[{"question":"Datastore?","status":"open","rationale":"r","options":[]}]` + "\n```\n" +
		"```draft\n" + `{"summary":"s","specs":[{"path":"specs/x.md","content":"# X\n"}],"issues":[{"title":"T","spec":"specs/x.md"}]}` + "\n```"
	srv := modeltest.NewServer(t, []modeltest.Turn{
		{Text: "Let me check the module.", ToolCalls: []modeltest.ToolCall{{ID: "c1", Name: "read_file", Args: `{"path":"go.mod"}`}}},
		{Text: final},
	})
	be := &fakeBackend{}
	p := wizard.NewPlanner(newCompatAdapter(t, srv.URL()), "persona",
		wizard.WithTurnTimeout(10*time.Second), withSandboxOpt(t, be))
	defer p.Shutdown(context.Background())

	sess := p.New()
	sub, cancel := sess.Hub().Subscribe()
	defer cancel()
	if !sess.Send("build X") {
		t.Fatal("Send returned false")
	}
	awaitTurn(t, sub)

	if srv.Requests() != 2 {
		t.Errorf("model requests = %d, want 2 (tool turn + final)", srv.Requests())
	}
	if be.count() != 1 {
		t.Fatalf("provisioned %d sandboxes, want 1", be.count())
	}
	if !be.first().sawExec("cat", "go.mod") {
		t.Errorf("the read_file tool did not cat go.mod in the sandbox")
	}
	msgs := sess.Messages()
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (user + ONE assistant; the tool-call turn is suppressed): %+v", len(msgs), msgs)
	}
	if msgs[1].Role != "assistant" || !strings.Contains(msgs[1].Text, "Here is the plan.") {
		t.Errorf("final assistant message wrong: %+v", msgs[1])
	}
	if len(sess.Ledger()) != 1 {
		t.Errorf("ledger = %d items, want 1", len(sess.Ledger()))
	}
	if sess.Draft().Empty() {
		t.Errorf("draft did not parse from the final turn")
	}
}

// TestExploreDisabledUnchanged proves a planner with NO sandbox option behaves exactly as a
// pure-conversation planner: one model request, no tool loop, ledger/draft parse identically.
func TestExploreDisabledUnchanged(t *testing.T) {
	final := "Plain reply.\n```ledger\n" + `[{"question":"Q?","status":"open"}]` + "\n```"
	srv := modeltest.NewServer(t, []modeltest.Turn{{Text: final}})
	p := wizard.NewPlanner(newCompatAdapter(t, srv.URL()), "persona", wizard.WithTurnTimeout(10*time.Second))
	defer p.Shutdown(context.Background())

	sess := p.New()
	sub, cancel := sess.Hub().Subscribe()
	defer cancel()
	if !sess.Send("hi") {
		t.Fatal("Send returned false")
	}
	awaitTurn(t, sub)

	if srv.Requests() != 1 {
		t.Errorf("model requests = %d, want 1 (no tool loop when exploration is disabled)", srv.Requests())
	}
	if len(sess.Ledger()) != 1 {
		t.Errorf("ledger = %d items, want 1", len(sess.Ledger()))
	}
}

// TestExploreLazyNoProvision proves the advertise-cheap/provision-lazy refinement: a session
// with exploration ENABLED that never triggers a tool call boots no sandbox at all.
func TestExploreLazyNoProvision(t *testing.T) {
	final := "Just chatting.\n```ledger\n" + `[{"question":"Q?","status":"open"}]` + "\n```"
	srv := modeltest.NewServer(t, []modeltest.Turn{{Text: final}})
	be := &fakeBackend{}
	p := wizard.NewPlanner(newCompatAdapter(t, srv.URL()), "persona",
		wizard.WithTurnTimeout(10*time.Second), withSandboxOpt(t, be))
	defer p.Shutdown(context.Background())

	sess := p.New()
	sub, cancel := sess.Hub().Subscribe()
	defer cancel()
	if !sess.Send("hello") {
		t.Fatal("Send returned false")
	}
	awaitTurn(t, sub)

	if be.count() != 0 {
		t.Errorf("provisioned %d sandboxes, want 0 (a non-exploring turn must not boot a sandbox)", be.count())
	}
}

// TestExploreEvictionTearsDown proves the eviction hook reaps an evicted session's sandbox: with
// a cap of 1, provisioning session A then minting B tears A's sandbox down.
func TestExploreEvictionTearsDown(t *testing.T) {
	srv := modeltest.NewServer(t, []modeltest.Turn{
		{Text: "looking", ToolCalls: []modeltest.ToolCall{{ID: "c1", Name: "list_dir", Args: `{"path":"."}`}}},
		{Text: "done.\n```ledger\n" + `[{"question":"Q?","status":"open"}]` + "\n```"},
	})
	be := &fakeBackend{}
	p := wizard.NewPlanner(newCompatAdapter(t, srv.URL()), "persona",
		wizard.WithMaxSessions(1), wizard.WithTurnTimeout(10*time.Second), withSandboxOpt(t, be))
	defer p.Shutdown(context.Background())

	a := p.New()
	sub, cancel := a.Hub().Subscribe()
	if !a.Send("explore") {
		t.Fatal("Send returned false")
	}
	awaitTurn(t, sub)
	cancel()
	if be.count() != 1 {
		t.Fatalf("provisioned %d sandboxes, want 1", be.count())
	}

	_ = p.New() // evicts A (cap is 1)

	// Teardown runs in a goroutine off the eviction path — poll briefly.
	deadline := time.After(5 * time.Second)
	for !be.first().wasTornDown() {
		select {
		case <-deadline:
			t.Fatal("evicted session's sandbox was not torn down")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// awaitTurn blocks until the terminal `turn` SSE event arrives (the turn completed) or fails on
// a deadline. Shared by the exploration tests.
func awaitTurn(t *testing.T, sub <-chan live.Event) {
	t.Helper()
	deadline := time.After(8 * time.Second)
	for {
		select {
		case ev := <-sub:
			if ev.Name == "turn" {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for the turn to complete")
		}
	}
}
