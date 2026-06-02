package gate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/Loxstomper/harness/internal/artifact"
	"github.com/Loxstomper/harness/internal/broker"
	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/model"
	"github.com/Loxstomper/harness/internal/sandbox"
	"github.com/Loxstomper/harness/internal/telemetry"
)

// teardownTimeout bounds the reap of the verification sandbox. Teardown runs on a
// context detached from the caller's so a canceled or timed-out gate still releases
// the host resources the backend holds.
const teardownTimeout = 30 * time.Second

// checkKind distinguishes how the gate runs a check. The zero value is a command
// check (run once against the candidate; exit 0 = pass), so any Check built without
// naming a kind keeps that meaning. A redGreenProof runs the acceptance tests against
// two refs — the base (must fail) and the candidate (must pass) — see Run. A redProof
// runs the acceptance tests once against the candidate, which must FAIL (the
// author-tests stage's tests are red before any implementation exists). A metricCheck
// runs a measurement command once against the candidate and grades a numeric score it
// prints against a threshold (e.g. mutation>=0.8) — see runMetric.
type checkKind int

const (
	cmdCheck checkKind = iota
	redGreenProof
	redProof
	metricCheck
)

// Check is one verification the gate runs in the clean sandbox. A command check passes
// iff its command exits zero; the red→green proof passes iff the command fails on the
// base and passes on the candidate. The command is operator-configured (not agent
// input), so it is run through the sandbox shell verbatim. Checks are not constructed
// directly at the gate; they are resolved from a stage's declared postconditions
// through a Registry, so the set the gate runs is data (config), not code.
type Check struct {
	Name string // the postcondition identifier this check realizes, e.g. "tests-pass"
	Cmd  string // shell command run via `sh -c` at the worktree root
	kind checkKind
	// op and threshold carry the comparison a metricCheck grades against (e.g. ">=" and
	// 0.8 for "mutation>=0.8"). They are the zero value for every other kind.
	op        string
	threshold float64
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
// The reserved red→green proof (core.PostconditionRedGreen) is not a command check: it
// has no entry of its own and instead reuses the acceptance-test command registered
// under core.CheckAcceptanceTests, run against two refs (see runRedGreen). Resolve binds
// it to that command here so a stage that asks for the proof without a registered
// acceptance-test command is a config fault caught before any sandbox is spent. The
// tests-red proof (core.PostconditionTestsRed) is the same shape run against one ref —
// the candidate, which must fail — and binds to the same command.
//
// A metric comparison ("mutation>=0.8") binds to the measurement command registered under
// its metric name (here "mutation"), the way a command check binds to its own name: the
// gate runs that command and grades the score it prints against the threshold (see
// runMetric). The tool invocation stays in config, so the gate is agnostic to which
// mutation tool produced the number. A comparison whose metric has no registered command
// is unresolvable, the same config fault as a missing command-check entry.
func (r Registry) Resolve(postconditions []string) ([]Check, error) {
	checks := make([]Check, 0, len(postconditions))
	var unresolved []string
	for _, pc := range postconditions {
		if pc == core.PostconditionRedGreen || pc == core.PostconditionTestsRed {
			cmd, ok := r[core.CheckAcceptanceTests]
			if !ok {
				unresolved = append(unresolved, fmt.Sprintf("%s (no %q command registered)", pc, core.CheckAcceptanceTests))
				continue
			}
			kind := redGreenProof
			if pc == core.PostconditionTestsRed {
				kind = redProof
			}
			checks = append(checks, Check{Name: pc, Cmd: cmd, kind: kind})
			continue
		}
		if metric, op, threshold, ok := core.ParseMetricComparison(pc); ok {
			cmd, found := r[metric]
			if !found {
				unresolved = append(unresolved, fmt.Sprintf("%s (no %q command registered)", pc, metric))
				continue
			}
			checks = append(checks, Check{Name: pc, Cmd: cmd, kind: metricCheck, op: op, threshold: threshold})
			continue
		}
		cmd, ok := r[pc]
		if !ok {
			unresolved = append(unresolved, pc)
			continue
		}
		checks = append(checks, Check{Name: pc, Cmd: cmd, kind: cmdCheck})
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
	// BaseRef is the pre-implementation base the candidate branched from. A red→green
	// proof checks it out in a second verification sandbox to confirm the acceptance
	// tests FAIL without the change (see specs/verification.md). Command checks never
	// touch it; only a red→green postcondition requires it to be set.
	BaseRef string
	// Postconditions are the stage's declared postconditions; the gate resolves them
	// to runnable checks through its Registry. This is what makes the gate run the
	// *stage's* declared checks rather than a single hardcoded set (see
	// specs/verification.md): a Done result is graded against the postconditions of
	// the very stage that produced it.
	Postconditions []string
	Profile        string               // logical sandbox profile the candidate was produced under (carried for provenance)
	Image          string               // concrete artifact the verification sandbox boots, resolved from Profile (empty -> backend falls back to Profile)
	Limits         config.SandboxLimits // resource ceiling (wall-clock bounds the gate's runtime)
}

// CheckResult records one check's outcome and its captured output. Stdout/Stderr are
// the raw bytes the check produced; Evidence is a content-addressed reference to the
// persisted evidence record (the same bytes plus a small header), written to the
// artifact store before the verdict is returned so it survives the verification
// sandbox's teardown. The orchestrator cites Evidence.Hash in the provenance trailer,
// making each verified check auditable by hash (see specs/components/artifact-store.md,
// specs/verification.md). Evidence is the zero value when no store is configured or a
// store write failed — a degraded record, never a dropped verdict.
type CheckResult struct {
	Name     string
	Cmd      string
	ExitCode int
	Passed   bool
	Stdout   []byte
	Stderr   []byte
	Evidence core.ArtifactRef
	// kind records how this check was graded, so the evidence record can label it (a
	// red proof passes on a nonzero exit, which would otherwise read as a contradiction).
	// It is unexported because only the gate writes evidence; callers read Passed.
	kind checkKind
	// Base, when non-nil, is the red half of a red→green proof: the acceptance tests
	// run against the pre-implementation base, which must FAIL. The inline
	// ExitCode/Stdout/Stderr above are then the green half — the same tests against the
	// candidate, which must PASS — and Passed is true only when Base failed and the
	// candidate passed. Base is nil for an ordinary command check.
	Base *RunResult
	// Metric, when non-nil, is the parsed result of a metric check (kind == metricCheck):
	// the score read from the command's stdout and the comparison it was graded against,
	// kept for the evidence record. It is nil for every other kind.
	Metric *metricResult
}

// metricResult is the graded outcome of a metric check: the numeric score the measurement
// command printed (Parsed is false when the command exited nonzero or its output did not
// parse to a number — an unverifiable score, which fails closed) and the comparison the
// gate applied to it.
type metricResult struct {
	Score     float64
	Parsed    bool
	Op        string
	Threshold float64
}

// RunResult captures one execution of a check's command against a single git ref. It
// holds the extra (base) run behind a red→green proof; the candidate run stays inline
// on CheckResult so command checks are unchanged.
type RunResult struct {
	ExitCode int
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
// build + test and the red→green / tests-red proofs; a metric postcondition
// (mutation>=0.8) grades a measured score against a threshold (T2.7), and spec-independent
// scanners are command checks layered on in Phase 2.
type Runner struct {
	backend   sandbox.Backend
	registry  Registry
	store     artifact.Store
	socketDir string
	log       *slog.Logger
	tel       *telemetry.Provider
}

// New builds a gate Runner over a sandbox backend and the check registry that resolves
// each candidate's declared postconditions into the commands to run. store is the
// content-addressed home for each check's persisted evidence; like the runner's
// harvest it is best-effort (a nil store, or a write that fails, degrades provenance
// but never the verdict). socketDir is where the per-gate broker socket is minted (the
// verification sandbox still needs a broker endpoint to provision, but it is a deny-all
// one — see Run). A nil logger discards; a nil telemetry Provider defaults to Noop, so the
// gate-run span and metric are emitted unconditionally with no overhead when export is off.
func New(backend sandbox.Backend, registry Registry, store artifact.Store, socketDir string, log *slog.Logger, tel *telemetry.Provider) *Runner {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if tel == nil {
		tel = telemetry.Noop()
	}
	return &Runner{backend: backend, registry: registry, store: store, socketDir: socketDir, log: log, tel: tel}
}

// Run provisions a fresh verification sandbox seeded with the candidate branch, runs
// the checks in order, and reports pass/fail. A red→green proof additionally provisions
// a second sandbox seeded at the base ref, so the acceptance tests can be shown to fail
// without the change. A returned error means the gate could not run to a verdict
// (provisioning failed, a sandbox died mid-check) — that is a transient/infrastructure
// failure the orchestrator retries, distinct from a clean Report whose Passed is false
// (a real gate failure routed via on_failure). Every sandbox is always torn down.
func (r *Runner) Run(ctx context.Context, c Candidate) (Report, error) {
	// The gate is the verification span. It runs in the trusted orchestrator's own process —
	// a fresh sandbox, a distinct trace from the untrusted producer (producer ≠ verifier) —
	// so it carries no inherited invocation context and correlates back to its issue by id,
	// recovered from the candidate ref (specs/observability.md). start times the whole run for
	// both the span and the gate-duration metric; both are emitted on every exit path below.
	start := time.Now()
	ctx, span := r.tel.Tracer().Start(ctx, telemetry.SpanGateRun, trace.WithAttributes(
		attribute.String(telemetry.AttrComponent, telemetry.ComponentGate),
		attribute.String(telemetry.AttrCandidateRef, c.Ref),
	))
	if id, ok := core.IssueIDFromCandidateBranch(c.Ref); ok {
		span.SetAttributes(attribute.String(telemetry.AttrIssueID, id))
	}
	defer span.End()

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
	// A red→green proof with no base ref is a wiring fault (the orchestrator failed to
	// thread it); catch it before spending any sandbox, alongside the other config faults.
	needBase := requiresBase(checks)
	if needBase && c.BaseRef == "" {
		return Report{}, fmt.Errorf("gate: a red→green proof requires a base ref, but the candidate carries none")
	}

	// The candidate verifier is always needed: command checks and the green half of any
	// red→green proof run against the candidate branch.
	cand, candDone, err := r.provisionVerifier(ctx, c, c.Ref)
	if err != nil {
		return Report{}, err
	}
	defer candDone()
	r.log.Info("gate: provisioned verification sandbox", "id", cand.ID(), "ref", c.Ref, "profile", c.Profile, "checks", len(checks))

	// A red→green proof additionally needs a second verifier seeded at the
	// pre-implementation base, to confirm the acceptance tests fail without the change.
	// Provision it once, up front, only when a check requires it — so a base provisioning
	// failure surfaces as an infra error before any check runs, and a gate with no
	// red→green proof spends exactly one sandbox.
	var base sandbox.Sandbox
	if needBase {
		b, baseDone, err := r.provisionVerifier(ctx, c, c.BaseRef)
		if err != nil {
			return Report{}, err
		}
		defer baseDone()
		base = b
		r.log.Info("gate: provisioned base verification sandbox", "id", b.ID(), "ref", c.BaseRef)
	}

	report := Report{Passed: true}
	for _, check := range checks {
		cr, err := runCheck(ctx, check, base, cand)
		if err != nil {
			// Could not run the check at all (sandbox gone) — not a gate failure, a gate
			// infrastructure error the orchestrator retries.
			return Report{}, err
		}
		report.Checks = append(report.Checks, cr)
		if !cr.Passed {
			report.Passed = false
			r.logFailure(c.Ref, check, cr)
			break // a failed check makes the remaining ones meaningless; stop at the first
		}
		r.log.Debug("gate: check passed", "ref", c.Ref, "check", check.Name)
	}

	// Persist each check's evidence to the artifact store and stamp the ref onto the
	// result. The captured bytes are already in memory (Exec copied them out of the
	// sandbox), so this survives the deferred teardown regardless of ordering. Both
	// passing and failing checks are persisted — a rejected gate's output is precisely
	// what a human triages from the dead-letter queue.
	r.persistEvidence(ctx, c.Ref, &report)

	// A verdict was reached: record it. The early returns above are infra errors (no verdict)
	// — deliberately not recorded, so the throughput counter and pass/fail split count only
	// real gate outcomes, never a sandbox that died mid-run (specs/observability.md).
	span.SetAttributes(
		attribute.Bool(telemetry.AttrGatePassed, report.Passed),
		attribute.Int(telemetry.AttrGateChecksRun, len(report.Checks)),
	)
	r.tel.RecordGateRun(ctx, report.Passed, time.Since(start))

	r.log.Info("gate: verdict", "ref", c.Ref, "passed", report.Passed, "checks_run", len(report.Checks))
	return report, nil
}

// requiresBase reports whether any check needs a base-ref verifier (a red→green proof).
func requiresBase(checks []Check) bool {
	for _, c := range checks {
		if c.kind == redGreenProof {
			return true
		}
	}
	return false
}

// runCheck dispatches one check to its execution shape: a command check runs once
// against the candidate; a red→green proof runs the acceptance tests against the base
// and the candidate. A non-nil error means a sandbox could not run the command at all
// (an infra failure), never a clean fail verdict.
func runCheck(ctx context.Context, check Check, base, cand sandbox.Sandbox) (CheckResult, error) {
	switch check.kind {
	case redGreenProof:
		return runRedGreen(ctx, check, base, cand)
	case metricCheck:
		return runMetric(ctx, check, cand)
	}
	res, err := execCheck(ctx, cand, check.Cmd)
	if err != nil {
		return CheckResult{}, fmt.Errorf("gate: run check %q: %w", check.Name, err)
	}
	// A red proof inverts the command-check verdict: the acceptance tests must FAIL
	// against the candidate (no implementation yet), so a nonzero exit is a pass and a
	// zero exit — the suite passing with no implementation — means the tests are
	// vacuous or do not exercise the change, which is the failure this proof catches.
	passed := res.ExitCode == 0
	if check.kind == redProof {
		passed = res.ExitCode != 0
	}
	return CheckResult{
		Name:     check.Name,
		Cmd:      check.Cmd,
		ExitCode: res.ExitCode,
		Passed:   passed,
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		kind:     check.kind,
	}, nil
}

// runRedGreen realizes the red→green proof: the acceptance tests must FAIL against the
// pre-implementation base (red) and PASS against the candidate (green). A base that
// passes means the tests don't exercise the new behavior (vacuously green) — the exact
// failure mode this proof exists to catch. The candidate run is recorded inline; the
// base run is kept on Base for the evidence record.
func runRedGreen(ctx context.Context, check Check, base, cand sandbox.Sandbox) (CheckResult, error) {
	redRes, err := execCheck(ctx, base, check.Cmd)
	if err != nil {
		return CheckResult{}, fmt.Errorf("gate: run red→green base check %q: %w", check.Name, err)
	}
	greenRes, err := execCheck(ctx, cand, check.Cmd)
	if err != nil {
		return CheckResult{}, fmt.Errorf("gate: run red→green candidate check %q: %w", check.Name, err)
	}
	return CheckResult{
		Name:     check.Name,
		Cmd:      check.Cmd,
		ExitCode: greenRes.ExitCode,
		Passed:   redRes.ExitCode != 0 && greenRes.ExitCode == 0,
		Stdout:   greenRes.Stdout,
		Stderr:   greenRes.Stderr,
		Base:     &RunResult{ExitCode: redRes.ExitCode, Stdout: redRes.Stdout, Stderr: redRes.Stderr},
		kind:     redGreenProof,
	}, nil
}

// runMetric realizes a metric check: it runs the measurement command once against the
// candidate and grades the numeric score the command prints against the check's
// comparison (e.g. mutation>=0.8). The score is read from stdout — the last
// whitespace-separated token, parsed as a decimal — so a command may print a label or a
// report before the number. The check passes only when the command ran cleanly (exit 0),
// emitted a parseable score, and that score satisfies the comparison: a nonzero exit (the
// tool could not measure) or unparseable output fails closed, because an unverifiable
// score is not a passing one. The tool-specific work (how to invoke the mutation tool,
// how to reduce its report to a number) lives in the registered command, keeping this
// path agnostic to which tool produced the score.
func runMetric(ctx context.Context, check Check, cand sandbox.Sandbox) (CheckResult, error) {
	res, err := execCheck(ctx, cand, check.Cmd)
	if err != nil {
		return CheckResult{}, fmt.Errorf("gate: run metric check %q: %w", check.Name, err)
	}
	score, parsed := parseScore(res.Stdout)
	passed := res.ExitCode == 0 && parsed && core.CompareMetric(score, check.op, check.threshold)
	return CheckResult{
		Name:     check.Name,
		Cmd:      check.Cmd,
		ExitCode: res.ExitCode,
		Passed:   passed,
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		kind:     metricCheck,
		Metric:   &metricResult{Score: score, Parsed: parsed, Op: check.op, Threshold: check.threshold},
	}, nil
}

// parseScore reads a metric score from a measurement command's stdout: the last
// whitespace-separated token, parsed as a float. ok is false when stdout is empty or its
// final token is not a number, so the caller fails the check closed rather than treating
// an unverifiable score as zero.
func parseScore(stdout []byte) (float64, bool) {
	fields := strings.Fields(string(stdout))
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// execCheck runs a check's command once in sb. A nil error means the command ran to an
// exit code (the caller decides pass/fail); a non-nil error means the sandbox could not
// run it at all (an infra failure).
func execCheck(ctx context.Context, sb sandbox.Sandbox, cmd string) (sandbox.ExecResult, error) {
	return sb.Exec(ctx, sandbox.Command{Path: "sh", Args: []string{"-c", cmd}})
}

// logFailure surfaces the tail of a failed check's output so a rejected gate is
// triageable from the log (full evidence goes to the artifact store). A red→green proof
// logs both halves, since which half failed is the first thing a human needs to know.
func (r *Runner) logFailure(ref string, check Check, cr CheckResult) {
	if cr.Base != nil {
		r.log.Info("gate: red→green proof failed", "ref", ref, "check", check.Name,
			"base_exit", cr.Base.ExitCode, "candidate_exit", cr.ExitCode,
			"base_stdout_tail", tailString(cr.Base.Stdout, 1000),
			"candidate_stdout_tail", tailString(cr.Stdout, 1000))
		return
	}
	if cr.Metric != nil {
		// A metric check can fail with exit 0 (a measured score below threshold), so log the
		// score and comparison rather than just the exit code, which would read as a pass.
		r.log.Info("gate: metric check failed", "ref", ref, "check", check.Name,
			"score", cr.Metric.Score, "parsed", cr.Metric.Parsed,
			"op", cr.Metric.Op, "threshold", cr.Metric.Threshold, "exit_code", cr.ExitCode,
			"stdout_tail", tailString(cr.Stdout, 1000), "stderr_tail", tailString(cr.Stderr, 1000))
		return
	}
	r.log.Info("gate: check failed", "ref", ref, "check", check.Name, "exit_code", cr.ExitCode,
		"stdout_tail", tailString(cr.Stdout, 2000), "stderr_tail", tailString(cr.Stderr, 2000))
}

// provisionVerifier provisions one fresh verification sandbox seeded at ref, behind its
// own deny-all broker (the verifier must reach nothing — no model calls, git push, or
// events; the socket exists only because Provision requires a broker endpoint, and
// serving deny-all is how "the verifier has zero I/O" is enforced by construction). It
// returns the live sandbox and a cleanup that stops the broker and tears the sandbox
// down; the caller MUST defer the cleanup, since the backend holds host resources.
func (r *Runner) provisionVerifier(ctx context.Context, c Candidate, ref string) (sandbox.Sandbox, func(), error) {
	id, err := gateID()
	if err != nil {
		return nil, nil, err
	}
	sockPath := filepath.Join(r.socketDir, "gate-"+id+".sock")
	ln, err := broker.Listen("unix", sockPath)
	if err != nil {
		return nil, nil, fmt.Errorf("gate: listen broker socket: %w", err)
	}
	spec := sandbox.Spec{
		Profile:   c.Profile,
		Image:     c.Image,
		Workspace: sandbox.Workspace{Repo: c.Repo, BaseRef: ref},
		Limits:    c.Limits,
		Broker:    sandbox.Endpoint{Network: "unix", Address: sockPath},
	}
	sb, err := r.backend.Provision(ctx, spec)
	if err != nil {
		_ = ln.Close() // unlinks the socket we just bound
		return nil, nil, fmt.Errorf("gate: provision verification sandbox: %w", err)
	}
	srv := broker.NewServer(denyHandler{}, broker.WithAllowlist(nil))
	brokerCtx, stopBroker := context.WithCancel(ctx)
	go func() {
		if err := srv.Serve(brokerCtx, ln); err != nil {
			r.log.Error("gate: broker serve", "err", err)
		}
	}()
	cleanup := func() {
		stopBroker() // closes ln, unblocking Serve and unlinking the socket
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), teardownTimeout)
		defer cancel()
		if err := sb.Teardown(tctx); err != nil {
			r.log.Error("gate: teardown verification sandbox", "id", sb.ID(), "err", err)
		}
	}
	return sb, cleanup, nil
}

// persistEvidence writes each check's evidence record to the artifact store and stamps
// the returned ref onto the CheckResult. It is best-effort, mirroring the runner's
// harvest (see specs/components/artifact-store.md): a missing store or a failed write
// is logged loudly and leaves the ref empty (a degraded provenance record), but never
// changes the verdict — a good candidate must not be rejected because a store write
// hiccuped, and a bad one must not be accepted because its evidence failed to save.
func (r *Runner) persistEvidence(ctx context.Context, ref string, report *Report) {
	if r.store == nil {
		return
	}
	for i := range report.Checks {
		cr := &report.Checks[i]
		a, err := r.store.Put(ctx, core.ArtifactKindGateEvidence, bytes.NewReader(formatEvidence(*cr)))
		if err != nil {
			r.log.Error("gate: persist check evidence", "ref", ref, "check", cr.Name, "err", err)
			continue
		}
		cr.Evidence = a
	}
}

// formatEvidence renders a check's result as a stable, human-readable evidence record:
// a header (the check name, the exact command, exit code, verdict) followed by the
// captured stdout and stderr. The format is deterministic so identical output
// content-addresses to the same hash, and it is what the control room renders for a
// gate run (see specs/control-room.md).
func formatEvidence(cr CheckResult) []byte {
	status := "pass"
	if !cr.Passed {
		status = "fail"
	}
	var b bytes.Buffer
	if cr.Base != nil {
		// Red→green proof: render both halves so the record shows the tests fail on the
		// base and pass on the candidate (or which expectation was violated).
		fmt.Fprintf(&b, "check: %s\nkind: red-green\ncommand: %s\nstatus: %s\n", cr.Name, cr.Cmd, status)
		fmt.Fprintf(&b, "base (must fail): exit %d\n", cr.Base.ExitCode)
		b.WriteString("--- base stdout ---\n")
		writeStream(&b, cr.Base.Stdout)
		b.WriteString("--- base stderr ---\n")
		writeStream(&b, cr.Base.Stderr)
		fmt.Fprintf(&b, "candidate (must pass): exit %d\n", cr.ExitCode)
		b.WriteString("--- candidate stdout ---\n")
		writeStream(&b, cr.Stdout)
		b.WriteString("--- candidate stderr ---\n")
		writeStream(&b, cr.Stderr)
		return b.Bytes()
	}
	if cr.kind == redProof {
		// Red proof: the acceptance tests must FAIL here (no implementation yet), so a
		// nonzero exit is a pass — spell that out so the record does not read as a
		// contradiction when triaged.
		fmt.Fprintf(&b, "check: %s\nkind: tests-red\ncommand: %s\nexit: %d (must be nonzero)\nstatus: %s\n", cr.Name, cr.Cmd, cr.ExitCode, status)
		b.WriteString("--- stdout ---\n")
		writeStream(&b, cr.Stdout)
		b.WriteString("--- stderr ---\n")
		writeStream(&b, cr.Stderr)
		return b.Bytes()
	}
	if cr.Metric != nil {
		// Metric check: record the measured score and the comparison it was graded against,
		// so a human can audit why the gate passed or failed. An unparseable/unmeasured score
		// is spelled out rather than rendered as 0, which would read as a real measurement.
		score := "unparseable"
		if cr.Metric.Parsed {
			score = strconv.FormatFloat(cr.Metric.Score, 'f', -1, 64)
		}
		fmt.Fprintf(&b, "check: %s\nkind: metric\ncommand: %s\nmetric: score %s (want %s %s)\nexit: %d\nstatus: %s\n",
			cr.Name, cr.Cmd, score, cr.Metric.Op, strconv.FormatFloat(cr.Metric.Threshold, 'f', -1, 64), cr.ExitCode, status)
		b.WriteString("--- stdout ---\n")
		writeStream(&b, cr.Stdout)
		b.WriteString("--- stderr ---\n")
		writeStream(&b, cr.Stderr)
		return b.Bytes()
	}
	fmt.Fprintf(&b, "check: %s\ncommand: %s\nexit: %d\nstatus: %s\n", cr.Name, cr.Cmd, cr.ExitCode, status)
	b.WriteString("--- stdout ---\n")
	writeStream(&b, cr.Stdout)
	b.WriteString("--- stderr ---\n")
	writeStream(&b, cr.Stderr)
	return b.Bytes()
}

// writeStream appends a captured stream to b, ensuring it ends with a newline so the
// next section header starts on its own line even when a check's output did not end
// with one.
func writeStream(b *bytes.Buffer, s []byte) {
	b.Write(s)
	if len(s) > 0 && s[len(s)-1] != '\n' {
		b.WriteByte('\n')
	}
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
