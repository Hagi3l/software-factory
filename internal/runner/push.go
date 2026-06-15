package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Loxstomper/harness/internal/secret"
)

// revokeTimeout bounds the best-effort revoke of a push token after the push completes. It
// runs on a fresh context (the invocation's may already be canceled), like sandbox teardown.
const revokeTimeout = 15 * time.Second

// credential-helper env var names + the inline git credential helper that reads them. The
// scoped token is supplied to git through this helper (read from the environment), NEVER
// placed in argv or the remote URL, so it never appears in `ps` output, the on-disk repo
// config, or git's reflog. The empty `-c credential.helper=` that precedes it resets any
// system/global helper so only this one runs.
const (
	credHelperUserEnv  = "HARNESS_GIT_USER"
	credHelperTokenEnv = "HARNESS_GIT_TOKEN"
	// inlineCredHelper answers git's "get" credential query with the user/token from the
	// environment and ignores "store"/"erase". The leading ! makes git run it as a shell
	// command (see gitcredentials(7)).
	inlineCredHelper = `!f() { test "$1" = get && printf 'username=%s\npassword=%s\n' "$` + credHelperUserEnv + `" "$` + credHelperTokenEnv + `"; }; f`
)

// pushBundleToRemote lands the candidate branch — extracted from the network-less sandbox
// as a git bundle — onto a real git remote, authenticated with the runner-minted scoped
// credential (T5.7). It unbundles the branch into a throwaway repo, pushes that ref to the
// remote, and returns the pushed head sha. This is the production replacement for the
// bootstrap local-repo apply (pushBundleToRepo): the destination is a remote the runner
// reaches with a per-task token, not a local path.
//
// When cred is Empty (no minter configured) the push is unauthenticated — valid for a
// file:// or local-path remote, the dev shape and what the offline tests exercise. With a
// token, the credential is injected via an inline credential helper reading it from the
// environment (never argv), so the secret never leaks into process listings or reflogs.
func pushBundleToRemote(ctx context.Context, remote string, cred secret.GitCredential, branch string, bundle []byte) (string, error) {
	dir, err := os.MkdirTemp("", "harness-push-*")
	if err != nil {
		return "", fmt.Errorf("git push: temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	bundlePath := filepath.Join(dir, "candidate.bundle")
	if err := os.WriteFile(bundlePath, bundle, 0o600); err != nil {
		return "", fmt.Errorf("git push: write bundle: %w", err)
	}

	ref := "refs/heads/" + branch
	refspec := "+" + ref + ":" + ref
	if out, err := runGit(ctx, dir, "init", "--quiet"); err != nil {
		return "", fmt.Errorf("git push: init scratch repo: %w: %s", err, out)
	}
	if out, err := runGit(ctx, dir, "fetch", "--quiet", bundlePath, refspec); err != nil {
		return "", fmt.Errorf("git push: fetch bundle: %w: %s", err, out)
	}

	// Push the unbundled ref to the remote. The token (when present) rides in the
	// environment via the credential helper, not the args, so it cannot leak.
	args := []string{"push", remote, refspec}
	var env []string
	if !cred.Empty() {
		args = append([]string{"-c", "credential.helper=", "-c", "credential.helper=" + inlineCredHelper}, args...)
		env = credentialEnv(cred)
	}
	if out, err := runGitEnv(ctx, dir, env, args...); err != nil {
		return "", fmt.Errorf("git push: push to remote: %w: %s", err, out)
	}

	out, err := runGit(ctx, dir, "rev-parse", ref)
	if err != nil {
		return "", fmt.Errorf("git push: rev-parse %s: %w: %s", ref, err, out)
	}
	return strings.TrimSpace(out), nil
}

// credentialEnv builds the environment the inline credential helper reads the scoped token
// from. GIT_TERMINAL_PROMPT=0 ensures git never blocks on an interactive prompt if the
// helper is bypassed (e.g. a redirect), failing the push loudly instead of hanging.
func credentialEnv(cred secret.GitCredential) []string {
	return []string{
		credHelperUserEnv + "=" + cred.Username,
		credHelperTokenEnv + "=" + cred.Token,
		"GIT_TERMINAL_PROMPT=0",
	}
}
