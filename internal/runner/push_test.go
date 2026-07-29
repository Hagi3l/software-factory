package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Loxstomper/software-factory/internal/broker"
	"github.com/Loxstomper/software-factory/internal/sandbox"
	"github.com/Loxstomper/software-factory/internal/secret"
)

// execOK builds a successful bundle-extraction ExecResult carrying the given stdout bytes.
func execOK(stdout string) sandbox.ExecResult {
	return sandbox.ExecResult{ExitCode: 0, Stdout: []byte(stdout)}
}

// gitRun runs a git command in dir, failing the test on error. Test-only helper for
// building source/remote repos.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// makeBundle builds a source repo with one commit on branch and returns the bundle bytes
// (what extractBundle produces inside the sandbox) plus the commit sha.
func makeBundle(t *testing.T, branch string) ([]byte, string) {
	t.Helper()
	src := t.TempDir()
	gitRun(t, src, "init", "--quiet", "-b", branch)
	writeFile(t, filepath.Join(src, "f.txt"), "hello")
	gitRun(t, src, "add", "f.txt")
	gitRun(t, src, "commit", "--quiet", "-m", "c1")
	commit := gitRun(t, src, "rev-parse", "refs/heads/"+branch)
	bundlePath := filepath.Join(t.TempDir(), "b.bundle")
	gitRun(t, src, "bundle", "create", bundlePath, branch)
	data, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	return data, commit
}

// TestPushBundleToRemote drives the T5.7 remote-push mechanics against a real local bare
// repo as the "remote" (no auth — the Empty-credential path), proving the unbundle → push →
// rev-parse chain lands the branch at the right commit. Offline; exercises real git.
func TestPushBundleToRemote(t *testing.T) {
	const branch = "candidate/iss-1"
	bundle, commit := makeBundle(t, branch)

	remote := t.TempDir()
	gitRun(t, remote, "init", "--quiet", "--bare")

	got, err := pushBundleToRemote(context.Background(), remote, secret.GitCredential{}, branch, bundle)
	if err != nil {
		t.Fatalf("pushBundleToRemote: %v", err)
	}
	if got != commit {
		t.Errorf("returned head = %q, want %q", got, commit)
	}
	// The bare remote must now carry the branch at that commit.
	landed := gitRun(t, remote, "rev-parse", "refs/heads/"+branch)
	if landed != commit {
		t.Errorf("remote branch head = %q, want %q", landed, commit)
	}
}

// TestCredentialEnvDoesNotLeakTokenInArgs proves the scoped token rides in the environment
// (the credential helper reads it from there), never in the git argv — the no-leak property
// that keeps it out of `ps` output and reflogs.
func TestCredentialEnvDoesNotLeakTokenInArgs(t *testing.T) {
	cred := secret.GitCredential{Username: "x-access-token", Token: "supersecret"}
	env := credentialEnv(cred)
	var sawToken bool
	for _, e := range env {
		if strings.Contains(e, "supersecret") {
			sawToken = true
		}
	}
	if !sawToken {
		t.Fatal("token must be present in the credential env")
	}
	// The inline helper references the env var, not the literal token.
	if strings.Contains(inlineCredHelper, "supersecret") {
		t.Error("inline credential helper must not embed a literal token")
	}
	if !strings.Contains(inlineCredHelper, credHelperTokenEnv) {
		t.Errorf("inline credential helper must read %s from the env", credHelperTokenEnv)
	}
}

// fakeMinter records Mint/Revoke calls and returns a scripted credential/errors.
type fakeMinter struct {
	cred      secret.GitCredential
	mintErr   error
	revokeErr error
	minted    []secret.MintRequest
	revoked   []secret.GitCredential
}

func (m *fakeMinter) Mint(_ context.Context, req secret.MintRequest) (secret.GitCredential, error) {
	m.minted = append(m.minted, req)
	return m.cred, m.mintErr
}

func (m *fakeMinter) Revoke(_ context.Context, cred secret.GitCredential) error {
	m.revoked = append(m.revoked, cred)
	return m.revokeErr
}

// TestRelayGitPushRemoteMintsAndRevokes proves the relay's remote path mints a token scoped
// to the task branch, hands it to the push, and revokes it after — the token's whole life is
// the one push. The push itself is stubbed (pushRemote seam) so this stays git-free.
func TestRelayGitPushRemoteMintsAndRevokes(t *testing.T) {
	cred := secret.GitCredential{Username: "x-access-token", Token: "tok"}
	mint := &fakeMinter{cred: cred}
	r := testRelay(&recordingAdapter{}, &recordingPublisher{}, &bundleSandbox{result: execOK("BUNDLE")})
	r.remote = "https://example.test/acme/widgets.git"
	r.minter = mint

	var gotRemote, gotBranch string
	var gotCred secret.GitCredential
	r.pushRemote = func(_ context.Context, remote string, c secret.GitCredential, branch string, _ []byte) (string, error) {
		gotRemote, gotCred, gotBranch = remote, c, branch
		return "deadbeef", nil
	}

	res, err := r.GitPush(context.Background(), broker.GitPushRequest{Branch: "candidate/iss-1"})
	if err != nil {
		t.Fatalf("GitPush: %v", err)
	}
	if res.Commit != "deadbeef" {
		t.Errorf("commit = %q, want deadbeef", res.Commit)
	}
	if gotRemote != r.remote || gotBranch != "candidate/iss-1" || gotCred.Token != "tok" {
		t.Errorf("pushRemote got remote=%q branch=%q token=%q", gotRemote, gotBranch, gotCred.Token)
	}
	if len(mint.minted) != 1 || mint.minted[0].Branch != "candidate/iss-1" || mint.minted[0].IssueID != "iss-1" {
		t.Errorf("mint calls = %+v, want one for issue iss-1 branch candidate/iss-1", mint.minted)
	}
	if len(mint.revoked) != 1 || mint.revoked[0].Token != "tok" {
		t.Errorf("revoke calls = %+v, want one for the minted token", mint.revoked)
	}
}

// TestRelayGitPushRemoteMintFailureDoesNotPush proves a mint failure aborts the push (no
// branch leaves the sandbox without a credential to authenticate it).
func TestRelayGitPushRemoteMintFailureDoesNotPush(t *testing.T) {
	mint := &fakeMinter{mintErr: context.DeadlineExceeded}
	r := testRelay(&recordingAdapter{}, &recordingPublisher{}, &bundleSandbox{result: execOK("BUNDLE")})
	r.remote = "https://example.test/acme/widgets.git"
	r.minter = mint
	r.pushRemote = func(context.Context, string, secret.GitCredential, string, []byte) (string, error) {
		t.Fatal("pushRemote must not run when minting fails")
		return "", nil
	}
	if _, err := r.GitPush(context.Background(), broker.GitPushRequest{Branch: "candidate/iss-1"}); err == nil {
		t.Fatal("GitPush with mint failure: want error, got nil")
	}
	if len(mint.revoked) != 0 {
		t.Error("a never-minted token must not be revoked")
	}
}

// TestRelayGitPushRemoteRevokeFailureIsNonFatal proves a best-effort revoke failure does not
// fail the push — the token's TTL is the backstop, so a successful push is not thrown away.
func TestRelayGitPushRemoteRevokeFailureIsNonFatal(t *testing.T) {
	mint := &fakeMinter{cred: secret.GitCredential{Token: "tok"}, revokeErr: context.DeadlineExceeded}
	r := testRelay(&recordingAdapter{}, &recordingPublisher{}, &bundleSandbox{result: execOK("BUNDLE")})
	r.remote = "https://example.test/acme/widgets.git"
	r.minter = mint
	r.pushRemote = func(context.Context, string, secret.GitCredential, string, []byte) (string, error) {
		return "abc123", nil
	}
	res, err := r.GitPush(context.Background(), broker.GitPushRequest{Branch: "candidate/iss-1"})
	if err != nil {
		t.Fatalf("GitPush: revoke failure should not fail the push, got %v", err)
	}
	if res.Commit != "abc123" {
		t.Errorf("commit = %q, want abc123", res.Commit)
	}
}

// TestRelayGitPushRemoteUnauthenticated proves a configured remote with no minter pushes
// with an Empty credential (the file:// dev shape) — the local-repo fallback is not used.
func TestRelayGitPushRemoteUnauthenticated(t *testing.T) {
	r := testRelay(&recordingAdapter{}, &recordingPublisher{}, &bundleSandbox{result: execOK("BUNDLE")})
	r.remote = "file:///tmp/remote.git"
	r.minter = nil
	var used string
	r.pushRemote = func(_ context.Context, _ string, c secret.GitCredential, _ string, _ []byte) (string, error) {
		if !c.Empty() {
			t.Error("no minter should yield an empty credential")
		}
		used = "remote"
		return "f00", nil
	}
	r.pushBundle = func(context.Context, string, string, []byte) (string, error) {
		t.Fatal("a configured remote must not fall back to the local repo")
		return "", nil
	}
	if _, err := r.GitPush(context.Background(), broker.GitPushRequest{Branch: "candidate/iss-1"}); err != nil {
		t.Fatalf("GitPush: %v", err)
	}
	if used != "remote" {
		t.Error("expected the remote push path")
	}
}
