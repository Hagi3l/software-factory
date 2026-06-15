// Package secret holds the runner's per-task git push credential custody: minting a
// narrowly-scoped, short-lived token for one invocation and revoking it the instant the
// push is done. It is the concrete machinery behind security.md Control 3 ("scoped,
// short-lived secrets") — the secret lives only on the trusted runner host, is never
// injected into the network-less sandbox, and the agent never holds it (the agent reaches
// git only through the broker's git.push, which the runner authenticates host-side).
//
// The seam is the Minter interface: a general contract with a thin per-authority
// implementation, mirroring the model layer's provider-adapter split. The shipped
// implementation is the GitHub App installation-token minter (the realistic production
// authority); a single-host/dev deployment uses no minter and pushes to a local or
// unauthenticated remote. See specs/components/runner.md, specs/security.md.
package secret

import (
	"context"
	"time"
)

// GitCredential is the short-lived git push credential the runner injects into one
// host-side push. Username/Token are the git basic-auth pair the credential helper feeds
// to git — for a GitHub App installation token the username is the literal
// "x-access-token". Expiry is the hard backstop the authority stamped on the token: even
// if revocation fails, the token self-expires by then (GitHub installation tokens last at
// most an hour). The agent never sees any of this; it lives only on the trusted runner.
type GitCredential struct {
	Username string
	Token    string
	Expiry   time.Time
}

// Empty reports whether the credential carries no token — the no-mint case (a local or
// unauthenticated remote push). The push path skips all credential injection when true,
// so a file:// or local-path "remote" works with no authority configured (the dev shape).
func (c GitCredential) Empty() bool { return c.Token == "" }

// MintRequest scopes one minted credential to one task. Branch is the single ref the
// credential is meant to push; whether the authority can enforce branch-level scope
// depends on the minter (a GitHub App installation token scopes to a repository +
// permission, not a branch, so "only the task branch" is enforced by the runner's broker
// branch guard there — see specs/security.md). IssueID is carried for the audit log.
type MintRequest struct {
	IssueID string
	Branch  string
}

// Minter mints and revokes per-task, short-lived, narrowly-scoped git push credentials.
// It is the runner's secret-custody seam: the production minter talks to a credential
// authority (a GitHub App), a single-host/dev deployment uses none. Revoke is best-effort
// at the call site — the credential's own short TTL bounds exposure if it fails — but a
// minter should make a genuine attempt so a token does not outlive its one push.
type Minter interface {
	// Mint issues a fresh credential scoped to req. It reads any runtime secret it needs
	// (e.g. an App private key) at call time, not at construction, so the secret can be
	// mounted only at run time (the API-key posture).
	Mint(ctx context.Context, req MintRequest) (GitCredential, error)
	// Revoke invalidates a previously-minted credential. A no-op for authorities that
	// cannot revoke (the TTL is then the only bound); GitHub installation tokens can.
	Revoke(ctx context.Context, cred GitCredential) error
}
