package gate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/Loxstomper/harness/internal/broker"
	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/model"
	"github.com/Loxstomper/harness/internal/sandbox"
)

// teardownTimeout bounds the reap of the verification sandbox. Teardown runs on a
// context detached from the caller's so a canceled or timed-out gate still releases
// the host resources the backend holds.
const teardownTimeout = 30 * time.Second

// Check is one verification command the gate runs in the clean sandbox. A check
// passes iff its command exits zero. The command is operator-configured (not agent
// input), so it is run through the sandbox shell verbatim. Checks are not constructed
// directly at the gate; they are resolved from a stage's declared postconditions
// through a Registry, so the set the gate runs is data (config), not code.
type Check struct {
	Name string // the postcondition identifier this check realizes, e.g. "tests-pass"
	Cmd  string // shell command run via `sh -c` at the worktree root
}

// Registry is the check registry: postcondition identifier -> shell command. It is
// the bridge from a stage's declared postconditions to the runnable Checks the gate
// executes, mirroring (at run time) the same `checks` map harness validate gates the
// config against (see specs/configuration.md, specs/verification.md). It is built
// once from config.Harness.Checks and resolves each candidate's postconditions per
// gate run.
type Registry map[string]string

// Resolve turns a stage's declared postconditions into the ordered command checks the
// gate runs, preserving postcondition order. Every postcondition must have a command
// in the registry; a postcondition with no entry is unresolvable and returns an error
// (a configuration fault — harness validate accepts a command-check postcondition only
// when it has a `checks` entry, so a live gap means config and the gate disagree).
//
// Postconditions backed by a built-in check kind rather than a command — metric
// comparisons ("mutation>=0.8") and reserved proofs ("tests-red-then-green") — are not
// command checks; the gate does not yet run them, so they are reported as unresolved
// here until their check kind lands (red→green is T2.3, mutation is T2.7).
func (r Registry) Resolve(postconditions []string) ([]Check, error) {
	checks := make([]Check, 0, len(postconditions))
	var unresolved []string
	for _, pc := range postconditions {
		cmd, ok := r[pc]
		if !ok {
			unresolved = append(unresolved, pc)
			continue
		}
		checks = append(checks, Check{Name: pc, Cmd: cmd})
	}
	if len(unresolved) > 0 {
		return nil, fmt.Errorf("gate: no check command registered for postcondition(s) %v", unresolved)
	}
	return checks, nil
}

// Candidate is what the gate verifies: a clean checkout of a candidate branch in a
// fresh verification sandbox, distinct from the one that produced it. The branch was
// landed in Repo by the producer's brokered git push (see specs/components/runner.md);
// the gate seeds a new worktree from it, so the verifier never shares state with the
// producer — producer ≠ verifier (see specs/verification.md).
type Candidate struct {
	Repo string // repo the candidate branch was pushed to
	Ref  string // candidate branch ref, e.g. core.CandidateBranch(issueID)
	// Postconditions are the stage's declared postconditions; the gate resolves them
	// to runnable checks through its Registry. This is what makes the gate run the
	// *stage's* declared checks rather than a single hardcoded set (see
	// specs/verification.md): a Done result is graded against the postconditions of
	// the very stage that produced it.
	Postconditions []string
	Profile        string               // sandbox profile to provision the verification sandbox with
	Limits         config.SandboxLimits // resource ceiling (wall-clock bounds the gate's runtime)
}

// CheckResult records one check's outcome and its captured output. The output is the
// gate evidence the orchestrator attaches to the provenance trail (large items go to
// the artifact store by hash — plan T1.18).
type CheckResult struct {
	Name     string
	Cmd      string
	ExitCode int
	Passed   bool
	Stdout   []byte
	Stderr   []byte
}

// Report is the gate's verdict. Passed is true iff every check passed. The gate stops
// at the first failing check — a failed build makes a subsequent test run meaningless
// — so Checks holds the results up to and including any failure, not the full set.
type Report struct {
	Passed bool
	Checks []CheckResult
}

// Runner runs the ordered checks against a candidate in a fresh, orchestrator-controlled
// verification sandbox. It is the gatekeeper's executor: it grades an artifact in a
// sandbox distinct from the one that produced it, because an untrusted process must
// never report its own grade (see specs/verification.md). In the kernel the gate runs
// build + test; mutation testing and scanners are additional postconditions in Phase 2.
type Runner struct {
	backend   sandbox.Backend
	registry  Registry
	socketDir string
	log       *slog.Logger
}

// New builds a gate Runner over a sandbox backend and the check registry that resolves
// each candidate's declared postconditions into the commands to run. socketDir is
// where the per-gate broker socket is minted (the verification sandbox still needs a
// broker endpoint to provision, but it is a deny-all one — see Run). A nil logger
// discards.
func New(backend sandbox.Backend, registry Registry, socketDir string, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Runner{backend: backend, registry: registry, socketDir: socketDir, log: log}
}

// Run provisions a fresh verification sandbox seeded with the candidate branch, runs
// the checks in order, and reports pass/fail. A returned error means the gate could
// not run to a verdict (provisioning failed, the sandbox died mid-check) — that is a
// transient/infrastructure failure the orchestrator retries, distinct from a clean
// Report whose Passed is false (a real gate failure routed via on_failure). The
// sandbox is always torn down.
func (r *Runner) Run(ctx context.Context, c Candidate) (Report, error) {
	// Resolve the stage's declared postconditions to runnable checks before spending
	// a sandbox: an unresolvable postcondition is a config fault, and a stage that
	// declared none would pass every candidate — both defeat verification and must
	// fail before any work is provisioned, not after.
	checks, err := r.registry.Resolve(c.Postconditions)
	if err != nil {
		return Report{}, err
	}
	if len(checks) == 0 {
		return Report{}, fmt.Errorf("gate: no checks configured (stage declared no postconditions)")
	}

	id, err := gateID()
	if err != nil {
		return Report{}, err
	}
	sockPath := filepath.Join(r.socketDir, "gate-"+id+".sock")
	ln, err := broker.Listen("unix", sockPath)
	if err != nil {
		return Report{}, fmt.Errorf("gate: listen broker socket: %w", err)
	}

	spec := sandbox.Spec{
		Profile:   c.Profile,
		Workspace: sandbox.Workspace{Repo: c.Repo, BaseRef: c.Ref},
		Limits:    c.Limits,
		Broker:    sandbox.Endpoint{Network: "unix", Address: sockPath},
	}
	sb, err := r.backend.Provision(ctx, spec)
	if err != nil {
		_ = ln.Close() // unlinks the socket we just bound
		return Report{}, fmt.Errorf("gate: provision verification sandbox: %w", err)
	}
	defer func() {
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), teardownTimeout)
		defer cancel()
		if err := sb.Teardown(tctx); err != nil {
			r.log.Error("gate: teardown verification sandbox", "id", sb.ID(), "err", err)
		}
	}()

	// The verification sandbox gets a deny-all broker: it must reach nothing — no model
	// calls, no git push, no events. It only runs build/test on a clean checkout and the
	// orchestrator reads the verdict. The socket exists only because Provision requires a
	// broker endpoint; serving deny-all is how "the verifier has zero I/O" is enforced by
	// construction (NewServer with no allowlist denies every destination).
	srv := broker.NewServer(denyHandler{}, broker.WithAllowlist(nil))
	brokerCtx, stopBroker := context.WithCancel(ctx)
	defer stopBroker()
	go func() {
		if err := srv.Serve(brokerCtx, ln); err != nil {
			r.log.Error("gate: broker serve", "err", err)
		}
	}()

	r.log.Info("gate: provisioned verification sandbox", "id", sb.ID(), "ref", c.Ref, "profile", c.Profile, "checks", len(checks))

	report := Report{Passed: true}
	for _, check := range checks {
		res, err := sb.Exec(ctx, sandbox.Command{Path: "sh", Args: []string{"-c", check.Cmd}})
		if err != nil {
			// Could not run the check at all (sandbox gone) — not a gate failure, a gate
			// infrastructure error the orchestrator retries.
			return Report{}, fmt.Errorf("gate: run check %q: %w", check.Name, err)
		}
		cr := CheckResult{
			Name:     check.Name,
			Cmd:      check.Cmd,
			ExitCode: res.ExitCode,
			Passed:   res.ExitCode == 0,
			Stdout:   res.Stdout,
			Stderr:   res.Stderr,
		}
		report.Checks = append(report.Checks, cr)
		if !cr.Passed {
			report.Passed = false
			// Surface the tail of the captured output so a failed gate is triageable from
			// the log; full evidence persistence into the artifact store is Phase 2.
			r.log.Info("gate: check failed", "ref", c.Ref, "check", check.Name, "exit_code", res.ExitCode,
				"stdout_tail", tailString(res.Stdout, 2000), "stderr_tail", tailString(res.Stderr, 2000))
			break // a failed build makes a later test run meaningless; stop at the first failure
		}
		r.log.Debug("gate: check passed", "ref", c.Ref, "check", check.Name)
	}

	r.log.Info("gate: verdict", "ref", c.Ref, "passed", report.Passed, "checks_run", len(report.Checks))
	return report, nil
}

// tailString returns the last max bytes of b as a string, prefixed with an elision
// marker when truncated, so a failed check's output is logged without flooding.
func tailString(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return "…(truncated)…" + string(b[len(b)-max:])
}

// gateID returns a fresh, unique id naming the per-gate broker socket so concurrent
// gates sharing a socket dir never collide.
func gateID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("gate: generate id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// denyHandler satisfies broker.Handler but performs nothing: the gate serves it behind
// a deny-all allowlist, so dispatch rejects every call before reaching it. It exists
// only because NewServer needs a non-nil Handler; every method erroring is defense in
// depth should the allowlist ever be widened by mistake.
type denyHandler struct{}

func (denyHandler) Complete(context.Context, model.Request) (model.Response, error) {
	return model.Response{}, fmt.Errorf("gate: verification sandbox has no broker egress")
}

func (denyHandler) GitPush(context.Context, broker.GitPushRequest) (broker.GitPushResult, error) {
	return broker.GitPushResult{}, fmt.Errorf("gate: verification sandbox has no broker egress")
}

func (denyHandler) PublishEvent(context.Context, broker.PublishRequest) error {
	return fmt.Errorf("gate: verification sandbox has no broker egress")
}
