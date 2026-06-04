package sandbox

import (
	"context"
	"io"
)

// SessionStream is a live, long-lived process inside a sandbox with its standard
// input and output attached as streams. Unlike Exec — which runs a command to
// completion and buffers the result — a SessionStream stays open across many writes
// and reads, so a stdio protocol (an LSP language server speaking JSON-RPC) can run
// for the lifetime of an invocation rather than one request at a time.
//
// It is the in-sandbox transport for the agent's LSP session manager (Phase 6, T6.1):
// gopls is launched once, kept warm, and notified of edits, so semantic queries never
// pay a cold start and never read stale text. Stderr is drained by the backend (an
// unread pipe would deadlock the server) and is not part of this contract.
type SessionStream interface {
	// Stdin is the process's standard input. The caller writes framed requests to it.
	// Closing it sends EOF to the process (most stdio servers exit on EOF).
	Stdin() io.WriteCloser
	// Stdout is the process's standard output. The caller reads framed responses from
	// it; it returns io.EOF when the process exits or the sandbox is torn down.
	Stdout() io.Reader
	// Close terminates the process and releases the backend resources behind the
	// streams. It is idempotent and safe to call after the process has already exited
	// (e.g. because the sandbox was torn down out from under it).
	Close() error
}

// SessionOpener is an OPTIONAL sandbox capability: running a long-lived process with
// attached stdin/stdout streams. It is separate from the core Sandbox interface on
// purpose — not every backend (and no test fake) needs to support a streamed process,
// and a backend that does not simply does not implement it. The agent's semantic-tool
// layer type-asserts for it and, when a sandbox does not provide it, degrades to the
// text floor (grep/sed) exactly as the spec's "reads degrade silently" requires
// (specs/components/agent.md). This keeps the security-critical Sandbox contract
// minimal: a SessionStream is just a streamed Exec — code still only runs inside via
// the backend, never by the host reaching in.
type SessionOpener interface {
	// OpenSession launches cmd inside the sandbox and returns its live streams. The ctx
	// bounds the launch, not the session's lifetime — the process lives until Close (or
	// until the sandbox's own wall-clock watchdog reaps it). The working directory and
	// environment follow the same rules as Exec's Command.
	OpenSession(ctx context.Context, cmd Command) (SessionStream, error)

	// Workdir is the absolute path of the seeded worktree inside the sandbox (the
	// default working directory for Exec). The LSP session manager needs it to build
	// the `file://` URIs and the initialize rootUri the language server expects, since
	// tool paths are worktree-relative but LSP speaks absolute in-sandbox paths.
	Workdir() string
}
