package query

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/core"
)

// --- Unit tests against the run seam (no git binary) ---

// stubRun dispatches canned responses by git subcommand, so the parsing logic is tested
// without a real repo. refOK toggles whether the ref is considered to exist.
func stubRun(refOK bool, logOut, grepOut string) func(context.Context, []string) ([]byte, error) {
	return func(_ context.Context, args []string) ([]byte, error) {
		switch {
		case len(args) > 0 && args[0] == "rev-parse":
			if refOK {
				return []byte("deadbeef\n"), nil
			}
			return nil, errors.New("exit status 1")
		case len(args) > 0 && args[0] == "log":
			for _, a := range args {
				if strings.HasPrefix(a, "--grep=") {
					return []byte(grepOut), nil
				}
			}
			return []byte(logOut), nil
		}
		return nil, errors.New("unexpected git call")
	}
}

func newStubReader(run func(context.Context, []string) ([]byte, error)) *GitProvenance {
	g := NewGitProvenance("/repo")
	g.run = run
	return g
}

func TestRecentParsesProvenanceCommits(t *testing.T) {
	p1 := core.Provenance{Soul: "implementor-go", Model: "claude", Issue: "h-1", PromptSHA: "sha256:a", Verified: []string{"build@sha256:b"}}
	p2 := core.Provenance{Soul: "qa-soul", Model: "haiku", Issue: "h-2", Verified: []string{"gosec"}}
	// Mirror git's --format=%H<US>%B<RS> output for two commits, including the leading
	// newline git puts before each subsequent record.
	logOut := "c1" + fieldSep + p1.CommitMessage() + "\n" + recordSep +
		"\nc2" + fieldSep + p2.CommitMessage() + "\n" + recordSep
	g := newStubReader(stubRun(true, logOut, ""))

	got, err := g.Recent(context.Background(), 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d commits, want 2: %+v", len(got), got)
	}
	if got[0].Commit != "c1" || got[0].Provenance.Issue != "h-1" || got[0].Provenance.Verified[0] != "build@sha256:b" {
		t.Errorf("commit[0] = %+v", got[0])
	}
	if got[1].Commit != "c2" || got[1].Provenance.Issue != "h-2" {
		t.Errorf("commit[1] = %+v", got[1])
	}
}

func TestRecentSkipsNonProvenanceCommits(t *testing.T) {
	good := core.Provenance{Issue: "h-1", Soul: "s", Model: "m"}
	logOut := "c0" + fieldSep + "chore: tidy up\n\njust a normal commit\n" + recordSep +
		"\nc1" + fieldSep + good.CommitMessage() + "\n" + recordSep
	g := newStubReader(stubRun(true, logOut, ""))
	got, err := g.Recent(context.Background(), 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 1 || got[0].Commit != "c1" {
		t.Errorf("got %+v, want only the provenance commit c1", got)
	}
}

func TestRecentEmptyWhenRefMissing(t *testing.T) {
	g := newStubReader(stubRun(false, "should-not-be-read", ""))
	got, err := g.Recent(context.Background(), 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil for a missing ref", got)
	}
}

func TestByIssueParsesMatch(t *testing.T) {
	p := core.Provenance{Issue: "h-7", Soul: "s", Model: "m", PromptSHA: "sha256:p"}
	g := newStubReader(stubRun(true, "", p.CommitMessage()+"\n"+recordSep))
	got, found, err := g.ByIssue(context.Background(), "h-7")
	if err != nil {
		t.Fatalf("ByIssue: %v", err)
	}
	if !found || got.Issue != "h-7" || got.PromptSHA != "sha256:p" {
		t.Errorf("got %+v found=%v", got, found)
	}
}

func TestByIssueNotFound(t *testing.T) {
	g := newStubReader(stubRun(true, "", "")) // grep returns nothing
	_, found, err := g.ByIssue(context.Background(), "h-404")
	if err != nil {
		t.Fatalf("ByIssue: %v", err)
	}
	if found {
		t.Error("found = true for an unmerged issue, want false")
	}
}

func TestByIssueEmptyID(t *testing.T) {
	g := newStubReader(stubRun(true, "", ""))
	if _, _, err := g.ByIssue(context.Background(), ""); err == nil {
		t.Fatal("ByIssue accepted an empty id")
	}
}

// --- Integration test against a real git repo ---

func gitAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

// TestGitProvenanceIntegration proves the reader speaks real git: it writes an integration
// commit whose message is a genuine core.Provenance.CommitMessage and reads the same record
// back through both Recent and ByIssue — the round trip across the orchestrator's writer
// and the control room's reader.
func TestGitProvenanceIntegration(t *testing.T) {
	gitAvailable(t)
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main")
	git("config", "user.name", "harness")
	git("config", "user.email", "harness@localhost")

	prov := core.Provenance{
		Soul: "implementor-go", Model: "claude-opus", Issue: "harness-42",
		PromptSHA: "sha256:abc", Verified: []string{"build@sha256:ev", "test"}, Traceability: "sha256:tm",
	}
	git("commit", "--allow-empty", "-m", prov.CommitMessage())

	g := NewGitProvenance(repo) // default ref refs/heads/main

	recent, err := g.Recent(context.Background(), 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(recent) != 1 || recent[0].Provenance.Issue != "harness-42" || recent[0].Provenance.Soul != "implementor-go" {
		t.Fatalf("Recent = %+v, want one harness-42 commit", recent)
	}
	if recent[0].Commit == "" {
		t.Error("Recent commit hash is empty")
	}

	got, found, err := g.ByIssue(context.Background(), "harness-42")
	if err != nil || !found {
		t.Fatalf("ByIssue: found=%v err=%v", found, err)
	}
	if got.Traceability != "sha256:tm" || len(got.Verified) != 2 || got.Verified[1] != "test" {
		t.Errorf("ByIssue provenance = %+v", got)
	}

	// An unmerged issue is cleanly not-found.
	if _, found, _ := g.ByIssue(context.Background(), "harness-999"); found {
		t.Error("ByIssue found a never-merged issue")
	}
}

// TestGitProvenanceEmptyRepo confirms a fresh repo with no main reads as empty, not an
// error — the provenance view renders blank before the first merge.
func TestGitProvenanceEmptyRepo(t *testing.T) {
	gitAvailable(t)
	repo := t.TempDir()
	cmd := exec.Command("git", "-C", repo, "init", "-b", "main")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	g := NewGitProvenance(repo)
	recent, err := g.Recent(context.Background(), 10)
	if err != nil {
		t.Fatalf("Recent on empty repo: %v", err)
	}
	if recent != nil {
		t.Errorf("Recent = %+v, want nil on empty repo", recent)
	}
	if _, found, err := g.ByIssue(context.Background(), "x"); err != nil || found {
		t.Errorf("ByIssue on empty repo: found=%v err=%v", found, err)
	}
}
