package query

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/Loxstomper/software-factory/internal/core"
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

// signatureStatusFromGitCode maps git %G? codes; the view distinguishes verified / unsigned
// / untrusted (T5.10).
func TestSignatureStatusFromGitCode(t *testing.T) {
	cases := map[string]SignatureStatus{
		"G": SignatureVerified,
		"N": SignatureUnsigned,
		"":  SignatureUnsigned,
		"U": SignatureUntrusted, // good signature, key not in allowed-signers
		"B": SignatureUntrusted, // bad signature
		"E": SignatureUntrusted, // cannot check
		"X": SignatureUntrusted, // expired
	}
	for code, want := range cases {
		if got := signatureStatusFromGitCode(code); got != want {
			t.Errorf("signatureStatusFromGitCode(%q) = %q, want %q", code, got, want)
		}
	}
}

// With no allowed-signers file configured, Recent runs the original 2-field query and every
// commit reads as SignatureUnchecked — an unsigned deployment's provenance view is unchanged.
func TestRecentLeavesSignatureUncheckedWithoutAllowedSigners(t *testing.T) {
	p := core.Provenance{Issue: "h-1", Soul: "s", Model: "m"}
	logOut := "c1" + fieldSep + p.CommitMessage() + "\n" + recordSep
	var loggedArgs []string
	g := newStubReader(func(_ context.Context, args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "rev-parse" {
			return []byte("deadbeef\n"), nil
		}
		if len(args) > 0 && args[0] == "log" {
			loggedArgs = args
			return []byte(logOut), nil
		}
		return nil, errors.New("unexpected")
	})
	got, err := g.Recent(context.Background(), 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 1 || got[0].Signature != SignatureUnchecked {
		t.Fatalf("got %+v, want one commit with SignatureUnchecked", got)
	}
	if strings.Contains(strings.Join(loggedArgs, " "), "%G?") || strings.Contains(strings.Join(loggedArgs, " "), "gpg.format") {
		t.Errorf("unsigned read should not request signature verification; args = %v", loggedArgs)
	}
}

// With an allowed-signers file configured, Recent folds %G? into the SAME log call (with the
// gpg.format=ssh + allowedSignersFile -c overrides) and maps each commit's code to a status.
func TestRecentVerifiesSignaturesWithAllowedSigners(t *testing.T) {
	p1 := core.Provenance{Issue: "h-1", Soul: "s", Model: "m"}
	p2 := core.Provenance{Issue: "h-2", Soul: "s", Model: "m"}
	p3 := core.Provenance{Issue: "h-3", Soul: "s", Model: "m"}
	// git --format=%H<US>%G?<US>%B<RS> output for three commits: verified, unsigned, untrusted.
	logOut := "c1" + fieldSep + "G" + fieldSep + p1.CommitMessage() + "\n" + recordSep +
		"\nc2" + fieldSep + "N" + fieldSep + p2.CommitMessage() + "\n" + recordSep +
		"\nc3" + fieldSep + "U" + fieldSep + p3.CommitMessage() + "\n" + recordSep
	var loggedArgs []string
	g := newStubReader(func(_ context.Context, args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "rev-parse" {
			return []byte("deadbeef\n"), nil
		}
		// the -c overrides precede the log subcommand
		for _, a := range args {
			if a == "log" {
				loggedArgs = args
				return []byte(logOut), nil
			}
		}
		return nil, errors.New("unexpected")
	})
	g.allowedSigners = "/etc/harness/allowed_signers"

	got, err := g.Recent(context.Background(), 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d commits, want 3: %+v", len(got), got)
	}
	want := []SignatureStatus{SignatureVerified, SignatureUnsigned, SignatureUntrusted}
	for i, w := range want {
		if got[i].Signature != w {
			t.Errorf("commit[%d] (%s) signature = %q, want %q", i, got[i].Commit, got[i].Signature, w)
		}
	}
	if got[0].Commit != "c1" || got[0].Provenance.Issue != "h-1" {
		t.Errorf("commit[0] = %+v, want c1/h-1", got[0])
	}
	joined := strings.Join(loggedArgs, " ")
	if !strings.Contains(joined, "%G?") {
		t.Errorf("verified read did not request %%G? column; args = %v", loggedArgs)
	}
	if !strings.Contains(joined, "gpg.ssh.allowedSignersFile=/etc/harness/allowed_signers") {
		t.Errorf("verified read did not pass the allowed-signers file; args = %v", loggedArgs)
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
	// commitForIssue greps with --format=%H<US>%B, so the grep output frames the hash and
	// the message with the field separator.
	g := newStubReader(stubRun(true, "", "c7"+fieldSep+p.CommitMessage()+"\n"+recordSep))
	got, found, err := g.ByIssue(context.Background(), "h-7")
	if err != nil {
		t.Fatalf("ByIssue: %v", err)
	}
	if !found || got.Issue != "h-7" || got.PromptSHA != "sha256:p" {
		t.Errorf("got %+v found=%v", got, found)
	}
}

// TestDiffByIssueShowsPatch proves DiffByIssue resolves the issue's integration commit and
// returns the patch git show emits, with the leading blank line `--format=` produces
// trimmed off so the diff renders flush.
func TestDiffByIssueShowsPatch(t *testing.T) {
	p := core.Provenance{Issue: "h-7", Soul: "s", Model: "m"}
	patch := "diff --git a/x b/x\nindex 000..111 100644\n+++ b/x\n+hello"
	run := func(_ context.Context, args []string) ([]byte, error) {
		switch args[0] {
		case "rev-parse":
			return []byte("ok\n"), nil
		case "log":
			return []byte("c7" + fieldSep + p.CommitMessage() + "\n" + recordSep), nil
		case "show":
			// git show --format= prints a leading newline before the patch; assert that the
			// commit hash commitForIssue resolved is the one diffed.
			if args[len(args)-1] != "c7" {
				t.Errorf("git show on %q, want the resolved commit c7", args[len(args)-1])
			}
			return []byte("\n" + patch), nil
		}
		return nil, errors.New("unexpected git call")
	}
	g := newStubReader(run)
	diff, found, err := g.DiffByIssue(context.Background(), "h-7")
	if err != nil {
		t.Fatalf("DiffByIssue: %v", err)
	}
	if !found || diff != patch {
		t.Errorf("diff = %q found=%v, want the trimmed patch", diff, found)
	}
}

// TestDiffByIssueNotFound proves an unmerged issue yields found=false with no git show call.
func TestDiffByIssueNotFound(t *testing.T) {
	g := newStubReader(stubRun(true, "", "")) // grep returns nothing
	diff, found, err := g.DiffByIssue(context.Background(), "h-404")
	if err != nil {
		t.Fatalf("DiffByIssue: %v", err)
	}
	if found || diff != "" {
		t.Errorf("diff = %q found=%v, want empty/not-found for an unmerged issue", diff, found)
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

// TestDiffByIssueIntegration proves DiffByIssue speaks real git: it commits a file under a
// genuine provenance message and reads back the unified diff that commit landed — the
// candidate diff the issue-detail view renders.
func TestDiffByIssueIntegration(t *testing.T) {
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
	if err := os.WriteFile(repo+"/widget.go", []byte("package widget\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	git("add", "widget.go")
	prov := core.Provenance{Soul: "implementor-go", Model: "claude", Issue: "harness-77", Transcript: "sha256:tx"}
	git("commit", "-m", prov.CommitMessage())

	g := NewGitProvenance(repo)
	diff, found, err := g.DiffByIssue(context.Background(), "harness-77")
	if err != nil || !found {
		t.Fatalf("DiffByIssue: found=%v err=%v", found, err)
	}
	if !strings.Contains(diff, "widget.go") || !strings.Contains(diff, "+package widget") {
		t.Errorf("diff missing the landed change:\n%s", diff)
	}
	// The diff must not carry the commit-message header `--format=` suppresses.
	if strings.Contains(diff, "harness-77") {
		t.Errorf("diff leaked the commit message header:\n%s", diff)
	}

	// An unmerged issue is cleanly not-found, with no diff.
	if d, found, _ := g.DiffByIssue(context.Background(), "harness-999"); found || d != "" {
		t.Errorf("DiffByIssue found a never-merged issue: %q", d)
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
