package agent

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/lsp"
	"github.com/Loxstomper/harness/internal/sandbox"
)

// TestSessionsRealGopls drives the session manager against a REAL gopls inside the
// go-toolchain image: it proves the whole T6.1 pipeline end to end — manifest resolution
// from the image, lazy launch of `gopls serve` over the streamed session, the LSP
// handshake, didOpen, a documentSymbol query, the async diagnostics path, AND the
// edit-coupling thesis: after an edit_file-style NotifyEdit (didChange), a fresh query
// reflects the in-memory change with no disk write, so the session never reads stale text.
//
// It is skipped unless docker + git + the go-toolchain image are all present, so it stays
// runnable in a provisioned dev box and inert everywhere else.
func TestSessionsRealGopls(t *testing.T) {
	requireGoToolchain(t)

	sockDir := t.TempDir()
	sock := filepath.Join(sockDir, "broker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen on broker socket: %v", err)
	}
	defer ln.Close()

	spec := sandbox.Spec{
		Profile:   "go-toolchain:latest",
		Workspace: sandbox.Workspace{Repo: seedGoModule(t), BaseRef: "main"},
		Limits:    config.SandboxLimits{CPU: 2, Mem: "2Gi", Wall: config.Duration(5 * time.Minute)},
		Broker:    sandbox.Endpoint{Network: "unix", Address: sock},
	}

	be := sandbox.NewDockerBackend()
	sb, err := be.Provision(context.Background(), spec)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer func() { _ = sb.Teardown(context.Background()) }()

	sessions := NewSessions(sb, nil)
	defer sessions.Close()

	// gopls cold start + module load is slow; give the whole flow a generous budget.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// 1) documentSymbol launches gopls, initializes, didOpens main.go, and returns the
	//    declared symbols — the core comprehension round-trip against a real server.
	syms, err := sessions.DocumentSymbol(ctx, "main.go")
	if err != nil {
		t.Fatalf("DocumentSymbol: %v", err)
	}
	if !hasSymbol(syms, "greet") || !hasSymbol(syms, "main") {
		t.Fatalf("DocumentSymbol = %v, want greet + main", symbolNames(syms))
	}

	// 2) Diagnostics exercises the async publishDiagnostics path; a valid file yields an
	//    empty (but delivered) batch, which must return without error.
	if _, err := sessions.Diagnostics(ctx, "main.go"); err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}

	// 3) The edit-coupling thesis: NotifyEdit (what edit_file calls) sends didChange with
	//    a new in-memory body adding func extra(); a fresh documentSymbol must see it,
	//    proving the session is kept in sync and never reads stale text.
	edited := "package main\n\nfunc greet() string { return \"hi\" }\n\nfunc extra() {}\n\nfunc main() { _ = greet() }\n"
	sessions.NotifyEdit(ctx, "main.go", edited)
	syms, err = sessions.DocumentSymbol(ctx, "main.go")
	if err != nil {
		t.Fatalf("DocumentSymbol after edit: %v", err)
	}
	if !hasSymbol(syms, "extra") {
		t.Fatalf("after didChange DocumentSymbol = %v, want it to include extra", symbolNames(syms))
	}

	// 4) Real semantic rename (T6.3): write the edited body to disk so the overlay and disk
	//    agree, then rename greet -> welcome via gopls and apply the WorkspaceEdit to the
	//    worktree. Proves applyWorkspaceEdit consumes REAL gopls output correctly — every
	//    greet reference (the declaration and the call in main) becomes welcome on disk.
	if out, err := writeFile(ctx, sb, "main.go", edited); err != nil || out != nil {
		t.Fatalf("seed edited main.go to disk: out=%v err=%v", out, err)
	}
	sessions.NotifyEdit(ctx, "main.go", edited)
	we, err := sessions.Rename(ctx, "main.go", 2, 5, "welcome") // 0-based: greet at line 3, col 6
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	files, edits, err := sessions.applyWorkspaceEdit(ctx, we)
	if err != nil {
		t.Fatalf("applyWorkspaceEdit: %v", err)
	}
	if files != 1 || edits < 2 {
		t.Fatalf("rename applied files=%d edits=%d, want 1 file and >=2 edits (decl + call)", files, edits)
	}
	res, err := sb.Exec(ctx, sandbox.Command{Path: "cat", Args: []string{"--", "main.go"}})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("read renamed main.go: err=%v exit=%d", err, res.ExitCode)
	}
	if got := string(res.Stdout); strings.Contains(got, "greet") || !strings.Contains(got, "welcome") {
		t.Fatalf("after real rename main.go =\n%s\nwant greet replaced by welcome everywhere", got)
	}
}

func requireGoToolchain(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH; skipping real-gopls session test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping real-gopls session test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skipf("docker daemon not reachable; skipping: %v", err)
	}
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", "go-toolchain:latest").Run(); err != nil {
		t.Skip("go-toolchain:latest image not present; build it (make image) to run the real-gopls test")
	}
}

// seedGoModule builds a tiny, stdlib-only Go module git repo (so gopls loads it offline)
// and returns its path for the sandbox to seed from.
func seedGoModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("go.mod", "module example.com/m\n\ngo 1.21\n")
	write("main.go", "package main\n\nfunc greet() string { return \"hi\" }\n\nfunc main() { _ = greet() }\n")

	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("add", ".")
	run("commit", "-q", "-m", "seed")
	return dir
}

func hasSymbol(syms []lsp.Symbol, name string) bool {
	for _, s := range syms {
		// gopls may qualify (e.g. "greet") or namespace symbols; match on the leaf name.
		if s.Name == name || strings.HasSuffix(s.Name, "."+name) {
			return true
		}
	}
	return false
}

func symbolNames(syms []lsp.Symbol) []string {
	out := make([]string, len(syms))
	for i, s := range syms {
		out[i] = s.Name
	}
	return out
}
