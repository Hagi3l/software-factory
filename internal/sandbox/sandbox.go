package sandbox

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Loxstomper/harness/internal/config"
)

// Backend creates sandboxes. Which backend is used is config, not code
// (sandbox.backend: docker | gvisor | firecracker — see specs/components/sandbox.md);
// the runner selects one at startup and the rest of the system depends only on this
// interface, so moving Docker -> Firecracker is a backend swap, not a rewrite.
type Backend interface {
	// Provision builds one isolated sandbox from spec: it seeds an ephemeral,
	// writable git worktree at spec.Workspace.BaseRef, applies the resource limits,
	// and wires the single local-socket channel to the runner — and nothing else is
	// reachable (zero direct network). The returned Sandbox is live; the caller MUST
	// Teardown it, even on a later error, since the backend holds host resources.
	Provision(ctx context.Context, spec Spec) (Sandbox, error)
}

// Sandbox is one ephemeral, isolated execution environment: one work item, one
// sandbox, destroyed after (see specs/components/sandbox.md). The interface is shaped
// around the strict microVM model from the start so a weak-isolation Docker backend
// and a Firecracker microVM satisfy the same contract:
//
//   - There is no method to reach the host filesystem or a host worktree path: a
//     microVM has none, and the candidate branch leaves the sandbox only via the
//     runner's brokered git push, never by the runner reaching in. Code runs inside
//     via Exec; that is the only way in.
//   - There is no general network/mount surface. The single channel to the outside
//     world is established at provision (Spec.Broker); everything an agent does to
//     the outside is an RPC over it, brokered and logged by the runner.
type Sandbox interface {
	// ID is the backend's identifier for this sandbox, for logs and teardown.
	ID() string

	// Exec runs a command inside the sandbox against the seeded worktree and returns
	// its outcome. It is how the agent process, and the build/test/lint workspace
	// tools, run — there are no casual bind mounts; the worktree was seeded at
	// provision. A non-zero ExitCode is reported in the ExecResult, NOT as the error:
	// the error is reserved for a failure to run the command at all (e.g. the sandbox
	// is gone), so callers can distinguish "tests failed" from "could not run tests".
	Exec(ctx context.Context, cmd Command) (ExecResult, error)

	// Teardown destroys the sandbox and all its state unconditionally. It must be
	// idempotent and safe to call on a partially-provisioned sandbox, because the
	// runner reaps in a deferred/cleanup path that may run after a mid-provision
	// failure. No state survives teardown — that is the ephemerality guarantee.
	Teardown(ctx context.Context) error
}

// Spec is a provisioning request: the explicit, minimal description of one sandbox.
// It is deliberately small — there is no free-form mounts or network field — because
// every additional way into a sandbox is an additional hole in the security boundary.
// The only inputs are the rootfs profile, the worktree to seed, the resource ceiling,
// and the single broker channel.
type Spec struct {
	// Profile is the logical toolchain profile (from soul.Sandbox, e.g. "go-toolchain").
	// It is the soul-facing identity, carried for telemetry/provenance; the concrete
	// artifact a backend boots is Image, resolved from this name via the infra
	// sandbox.profiles registry (see config.SandboxConfig.ResolveImage).
	Profile string

	// Image is the concrete, backend-specific bootable artifact: a (digest-pinned)
	// container image for docker/gvisor, a rootfs for firecracker. It is resolved from
	// Profile by the orchestrator/runner when they build the spec, so the backend only
	// ever boots a concrete artifact. When empty, a backend falls back to Profile — the
	// historical "name == image tag" behavior the test-only paths rely on.
	Image string

	// Workspace describes the git worktree seeded into the sandbox.
	Workspace Workspace

	// Limits is the resource ceiling the backend enforces from outside. It reuses the
	// config schema type directly (single source of truth — the runner passes the
	// loaded config limits straight through, no adapter).
	Limits config.SandboxLimits

	// Broker is the single local-socket channel the agent reaches the runner on. It
	// is the ONLY route out of the sandbox; the backend wires it in and nothing else.
	Broker Endpoint
}

// Workspace is the ephemeral, writable git worktree seeded at the brief's base ref.
type Workspace struct {
	Repo    string // source repository the worktree is seeded from
	BaseRef string // git ref to check out (from core.Brief.Base)
}

// Endpoint names the local socket the agent uses to reach the runner's broker. For
// Docker the backend wires a unix domain socket (Network "unix", Address a path);
// for Firecracker a vsock (Network "vsock", Address "<cid>:<port>"). Modeling the
// transport as a value keeps the Sandbox interface backend-agnostic — the runner
// stands up the broker listener and tells the backend where it is.
type Endpoint struct {
	Network string // "unix" | "vsock"
	Address string // socket path, or "<cid>:<port>" for vsock
}

// Command is a process to run inside the sandbox.
type Command struct {
	Path  string   // executable to run inside the sandbox
	Args  []string // arguments, not including Path
	Dir   string   // working directory relative to the worktree root; empty means the root
	Env   []string // extra KEY=VALUE pairs added to the sandbox's base environment
	Stdin []byte   // optional stdin
}

// ExecResult is the outcome of a Command. A non-zero ExitCode is a normal result,
// not an error (see Sandbox.Exec).
type ExecResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// InvalidSpecError reports every problem with a Spec at once. Provisioning is a
// boundary where a malformed request must fail loud before a backend allocates host
// resources, so all problems are surfaced together rather than one at a time.
type InvalidSpecError struct {
	Problems []string
}

func (e *InvalidSpecError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "sandbox: invalid spec, %d problem(s):", len(e.Problems))
	for _, p := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(p)
	}
	return b.String()
}

// Validate checks a Spec is well-formed before any backend acts on it. It enforces
// the invariants the contract depends on: a rootfs profile, a worktree base ref to
// seed, a broker channel (the lone route out), and a bounded resource ceiling. The
// wall-clock limit must be positive because an unbounded sandbox would defeat the
// termination guarantee (see specs/workflow.md); disk is optional because not every
// backend meters it. A backend may assume a Spec that passes Validate is sane.
func (s Spec) Validate() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if s.Profile == "" {
		add("profile is empty")
	}
	if s.Workspace.Repo == "" {
		add("workspace repo is empty")
	}
	if s.Workspace.BaseRef == "" {
		add("workspace base ref is empty")
	}
	if s.Broker.Network == "" {
		add("broker network is empty")
	}
	if s.Broker.Address == "" {
		add("broker address is empty")
	}
	if s.Limits.CPU <= 0 {
		add("limits cpu must be positive, got %d", s.Limits.CPU)
	}
	if s.Limits.Mem == "" {
		add("limits mem is empty")
	}
	if s.Limits.Wall.Duration() <= 0 {
		add("limits wall must be positive (an unbounded sandbox defeats termination)")
	}

	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return &InvalidSpecError{Problems: problems}
}
