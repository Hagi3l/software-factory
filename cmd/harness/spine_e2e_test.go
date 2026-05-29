package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/Loxstomper/harness/internal/beads"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/model/modeltest"
	"github.com/Loxstomper/harness/internal/sandbox"
)

// TestSpineE2ELocal drives the whole kernel — spec → implement → gate → merge — in one
// process against a fixture repo, with NO Docker and NO network: a deterministic fake
// model (modeltest) scripts the agent turns, and a non-isolating host-exec sandbox
// backend runs the workspace tools and gate checks on the host. It is the fast, always-on
// regression guard for the spine's plumbing — routing, the tool contract, the broker
// git push, gating, and the provenance merge — independent of any capable runtime model
// (see specs/bootstrap.md "Testing the spine without a capable model", TE.1 in
// IMPLEMENTATION_PLAN.md). The Docker-backed sibling (TestSpineE2EDocker, build tag
// docker_e2e) covers the isolation properties this local backend deliberately gives up.
//
// It is the first test that exercises an agent turn end-to-end: the existing wiring test
// (run_test.go) dispatches no work.
func TestSpineE2ELocal(t *testing.T) {
	requireBd(t)
	// profile is irrelevant to the local backend (it never boots an image); any
	// non-empty string satisfies sandbox.Spec.Validate.
	runSpineE2E(t, newLocalBackend(), "local-noop")
}

// runSpineE2E is the variant-agnostic driver shared by the local and Docker e2e tests:
// it seeds a fixture (git repo + beads issue + config pointed at a scripted fake model),
// assembles the kernel with the given sandbox backend injected, runs it until the seed
// issue is accepted and merged, and asserts main advanced with a provenance trailer.
func runSpineE2E(t *testing.T, backend sandbox.Backend, profile string) {
	t.Helper()
	logs := &syncBuffer{}
	defer func() {
		if t.Failed() {
			t.Logf("---- run logs ----\n%s", logs.String())
		}
	}()
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	repo := t.TempDir()
	initialMain := e2eInitRepo(t, repo)
	issueID := e2eSeedIssue(t, repo)

	// The script the fake model replays: create the candidate branch and commit a
	// change (turn 1), then submit it for verification (turn 2). This is the minimal
	// real agent behavior the spine requires, and it pins the run/submit tool contract.
	branch := core.CandidateBranch(issueID)
	commitCmd := fmt.Sprintf(
		"git config user.email e2e@harness.test && git config user.name e2e-agent && "+
			"git checkout -q -b %s && printf 'e2e change\\n' > E2E.txt && "+
			"git add -A && git commit -q -m 'e2e: add change' && echo committed",
		branch)
	srv := modeltest.NewServer(t, []modeltest.Turn{
		{ToolCalls: []modeltest.ToolCall{{ID: "call_run", Name: "run", Args: mustJSON(map[string]string{"command": commitCmd})}}},
		{ToolCalls: []modeltest.ToolCall{{ID: "call_submit", Name: "submit", Args: mustJSON(map[string]string{"summary": "e2e candidate"})}}},
	})

	cfgDir := e2eWriteConfig(t, srv.URL(), profile)
	cfg, err := loadConfig(cfgDir, "dev")
	if err != nil {
		t.Fatalf("load fixture config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate fixture config: %v", err)
	}
	resolvePersonas(cfg)

	comp, err := buildRunComponents(cfg, repo, runOptions{bdBin: "bd", backend: backend}, log)
	if err != nil {
		t.Fatalf("buildRunComponents: %v", err)
	}
	defer comp.cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return comp.orch.Run(gctx) })
	g.Go(func() error { return comp.rnr.Run(gctx) })

	// Wait for the terminal state: the orchestrator closes the seed issue only after a
	// passing gate and a successful merge, so a closed issue means the whole spine ran.
	bd := beads.New(beads.WithBinary("bd"), beads.WithDir(repo))
	if !waitFor(30*time.Second, func() bool {
		is, gerr := bd.Get(context.Background(), issueID)
		return gerr == nil && is.Status == "closed"
	}) {
		cancel()
		_ = g.Wait()
		t.Fatalf("seed issue %s was not accepted+merged within timeout", issueID)
	}

	cancel()
	if err := g.Wait(); err != nil {
		t.Fatalf("run loops returned error on shutdown: %v", err)
	}

	// main must have advanced past the seed commit to a trusted provenance commit.
	finalMain := gitOut(t, repo, "rev-parse", "main")
	if finalMain == initialMain {
		t.Fatalf("main did not advance from %s", initialMain)
	}
	msg := gitOut(t, repo, "log", "-1", "--format=%B", "main")
	for _, want := range []string{"Soul: impl-fake", "Model: fake", "Verified: tests-pass", "Issue: " + issueID} {
		if !strings.Contains(msg, want) {
			t.Errorf("merge commit message missing %q; got:\n%s", want, msg)
		}
	}
	// The candidate's change must be present on main (the merge carried the tree).
	if files := gitOut(t, repo, "ls-tree", "--name-only", "main"); !strings.Contains(files, "E2E.txt") {
		t.Errorf("merged tree missing E2E.txt; got: %s", files)
	}
	if got := srv.Requests(); got != 2 {
		t.Errorf("fake model served %d requests, want 2 (run, submit)", got)
	}
}

// --- fixture helpers ----------------------------------------------------------

// e2eInitRepo creates the integration repo: a non-bare git repo with an initial commit
// on `main` (the base the candidate branches from, and the ref the merger advances). It
// is used purely as a ref store by the runner (bundle fetch) and the merger (ref
// advance); the seeded worktrees are clones of it. Returns the initial main commit.
func e2eInitRepo(t *testing.T, repo string) string {
	t.Helper()
	gitRun(t, repo, "init", "-q", "-b", "main")
	gitRun(t, repo, "config", "user.email", "fixture@harness.test")
	gitRun(t, repo, "config", "user.name", "fixture")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# e2e fixture\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "initial")
	return gitOut(t, repo, "rev-parse", "main")
}

// e2eSeedIssue initializes a beads store in the repo (prefix "harness", per the bd
// foreign-prefix caveat in CLAUDE.md) and creates one ready issue at the entry role,
// exactly as `harness seed` would, going through the single-writer beads.Apply path.
// Returns the assigned issue id.
func e2eSeedIssue(t *testing.T, repo string) string {
	t.Helper()
	runBd(t, repo, "init", "--prefix", "harness")
	bd := beads.New(beads.WithBinary("bd"), beads.WithDir(repo))
	created, err := bd.Apply(context.Background(), []core.Proposal{{
		Issue: core.Issue{Title: "e2e seed", Body: "make the change", Role: "implementor"},
	}})
	if err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	if len(created) != 1 || created[0].ID == "" {
		t.Fatalf("seed returned %d issues, want 1 with an id", len(created))
	}
	return created[0].ID
}

// e2eWriteConfig writes a minimal-but-faithful config tree (harness.yaml, souls/*.yaml,
// infra.dev.yaml, persona) into a temp dir and returns it. The DAG is the kernel's
// implement → integrate; the single check `tests-pass` resolves to `true` so the gate
// passes deterministically without a real test suite; the lone soul runs on the `fake`
// model whose endpoint points at the scripted modeltest server.
func e2eWriteConfig(t *testing.T, modelURL, profile string) string {
	t.Helper()
	dir := t.TempDir()
	writeFileE2E(t, filepath.Join(dir, "harness.yaml"), `
dag:
  implement:
    role: implementor
    precondition: blockers-closed
    postcondition: [tests-pass]
    on_failure: implement
    produces: [integrate]
  integrate:
    kind: trusted-merge
checks:
  tests-pass: "true"
policy:
  max_retries: 3
  budget: { tokens: 2000000, usd: 20, wall: 2h }
  dead_letter: harness.dlq
`)
	writeFileE2E(t, filepath.Join(dir, "infra.dev.yaml"), fmt.Sprintf(`
sandbox:
  backend: docker
  egress: broker-only
  limits: { cpu: 2, mem: 2Gi, wall: 5m }
broker:
  allowlist: [llm-api, nats, git]
artifacts:
  backend: files
  path: ./.harness/artifacts
models:
  fake: { provider: openai-compat, endpoint: %q }
`, modelURL))
	writeFileE2E(t, filepath.Join(dir, "souls", "impl-fake.yaml"), fmt.Sprintf(`
name: impl-fake
role: implementor
model: fake
persona: souls/prompts/impl-fake.md
tools: [fs, shell, git]
sandbox: %s
selector: { lang: go }
`, profile))
	writeFileE2E(t, filepath.Join(dir, "souls", "prompts", "impl-fake.md"),
		"# Fake implementor\n\nYou are a deterministic test agent driven by a scripted model.\n")
	return dir
}

// --- local (non-isolating, host-exec) sandbox backend -------------------------

// localBackend is a non-isolating sandbox.Backend that runs commands directly on the
// host in a throwaway git worktree. It provides ZERO isolation — no namespace, no
// resource enforcement, no network cut — and exists ONLY to drive the spine e2e test
// without Docker. It is injected via runOptions.backend and is never reachable from
// config (`sandbox.backend` maps only to real isolating backends), so it cannot reach a
// deployment. The Docker backend is what the production path and the docker_e2e test use.
type localBackend struct{}

func newLocalBackend() *localBackend { return &localBackend{} }

func (b *localBackend) Provision(ctx context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	// Enforce the same Spec contract a real backend does, so the local path exercises
	// the same provisioning invariants (a missing base ref, profile, or broker endpoint
	// is a fault here too).
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "harness-local-sbx-")
	if err != nil {
		return nil, err
	}
	// Seed the worktree exactly like the Docker backend: an independent clone at the
	// brief's base ref. --no-hardlinks keeps it a real copy safe to delete on teardown.
	if out, cerr := runGitCombined(ctx, "", "clone", "--no-hardlinks", "--quiet", spec.Workspace.Repo, dir); cerr != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("localBackend: clone worktree: %w: %s", cerr, out)
	}
	if out, cerr := runGitCombined(ctx, dir, "checkout", "--quiet", spec.Workspace.BaseRef); cerr != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("localBackend: checkout %s: %w: %s", spec.Workspace.BaseRef, cerr, out)
	}
	id, err := randomID("local")
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return &localSandbox{id: id, dir: dir}, nil
}

type localSandbox struct {
	id   string
	dir  string
	once sync.Once
}

func (s *localSandbox) ID() string { return s.id }

// Exec runs the command on the host in the worktree (or a subdir). It mirrors the
// Sandbox contract: a non-zero exit is returned in ExecResult, and only a failure to
// run the command at all (e.g. the binary is missing) is the error.
func (s *localSandbox) Exec(ctx context.Context, cmd sandbox.Command) (sandbox.ExecResult, error) {
	c := exec.CommandContext(ctx, cmd.Path, cmd.Args...)
	c.Dir = s.dir
	if cmd.Dir != "" {
		c.Dir = filepath.Join(s.dir, cmd.Dir)
	}
	c.Env = append(os.Environ(), cmd.Env...)
	if len(cmd.Stdin) > 0 {
		c.Stdin = bytes.NewReader(cmd.Stdin)
	}
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return sandbox.ExecResult{ExitCode: ee.ExitCode(), Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, nil
		}
		return sandbox.ExecResult{}, fmt.Errorf("localSandbox %s: run %s: %w", s.id, cmd.Path, err)
	}
	return sandbox.ExecResult{ExitCode: 0, Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, nil
}

// Teardown removes the worktree. Idempotent via sync.Once, matching the contract.
func (s *localSandbox) Teardown(context.Context) error {
	var err error
	s.once.Do(func() {
		if s.dir != "" {
			err = os.RemoveAll(s.dir)
		}
	})
	return err
}

// --- small shared utilities ---------------------------------------------------

func requireBd(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not on PATH; skipping spine e2e test")
	}
}

// gitRun / gitOut run git in a repo, failing the test on error (fixture arrangement).
func gitRun(t *testing.T, repo string, args ...string) {
	t.Helper()
	if out, err := runGitCombined(context.Background(), repo, args...); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitOut(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := runGitCombined(context.Background(), repo, args...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(out)
}

func runGitCombined(ctx context.Context, dir string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		c.Dir = dir
	}
	out, err := c.CombinedOutput()
	return string(out), err
}

func runBd(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("bd", args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("bd %v: %v\n%s", args, err, out)
	}
}

func writeFileE2E(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func randomID(prefix string) (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(b[:]), nil
}

// waitFor polls cond every 150ms until it returns true or the timeout elapses.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return cond()
}

// mustJSON encodes a scripted tool's arguments object. Values are shell command
// strings; the stdlib encoder escapes them correctly for the wire.
func mustJSON(v map[string]string) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// syncBuffer is a goroutine-safe sink for the run loops' logs, dumped only if the test
// fails so a green run stays quiet.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
