package gate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/Loxstomper/software-factory/internal/artifact"
	"github.com/Loxstomper/software-factory/internal/broker"
	"github.com/Loxstomper/software-factory/internal/config"
	"github.com/Loxstomper/software-factory/internal/core"
	"github.com/Loxstomper/software-factory/internal/model"
	"github.com/Loxstomper/software-factory/internal/packageproxy"
	"github.com/Loxstomper/software-factory/internal/sandbox"
	"github.com/Loxstomper/software-factory/internal/telemetry"
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

// checkBuildName is the registry key whose command the gate runs as the build
// precondition — the one check every other depends on, since nothing meaningful can run on
// a tree that does not compile (see specs/verification.md "A check is tri-state; a broken
// build never reads as green"). It is the same key a stage may declare as a plain
// postcondition, so the precondition reuses that one configured command (single source of
// truth) rather than hardcoding a toolchain: the gate grades a command, not `go build`. A
// deployment with no `build` entry registered gets no precondition (the build still runs
// folded into another check, e.g. `make test-unit`), so this is opt-in by configuration and
// changes nothing for a config that does not register it.
const checkBuildName = "build"

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
// executes, mirroring (at run time) the same `checks` map software-factory validate gates the
// config against (see specs/configuration.md, specs/verification.md). It is built
// once from config.Harness.Checks and resolves each candidate's postconditions per
// gate run.
type Registry map[string]string

// Resolve turns a stage's declared postconditions into the ordered command checks the
// gate runs, preserving postcondition order. Every postcondition must have a command
// in the registry; a postcondition with no entry is unresolvable and returns an error
// (a configuration fault — software-factory validate accepts a command-check postcondition only
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
	// Findings is the structured parse of this check's tool output, produced by the
	// check's per-tool adapter (T9.2/T9.6) when one exists. It is the compact, signal-dense
	// form the verdict and a retry Brief carry instead of the raw Stdout/Stderr (which stay
	// the gate-evidence record, by hash). It is nil for a check with no adapter yet — that
	// check still grades on ExitCode with its raw output as evidence. See
	// specs/verification.md "Findings: structured evidence, not the grade".
	Findings core.Findings
	// Status is the tri-state outcome (passed / failed / not-run). It is empty for a check
	// that ran (verdictRecord derives passed/failed from Passed); the build precondition
	// (T9.4) sets it to core.CheckStatusNotRun for a check it short-circuited.
	Status string
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

// Report is the gate's verdict. Passed is true iff every check passed. The gate stops at
// the first failing *non-independent* check — a failed build makes a subsequent test run
// meaningless — so Checks holds the results up to and including that failure. Independent
// command checks (the scanners; see WithIndependentChecks, T2.12) do not stop the run, so
// Checks may carry several independent failures aggregated in one pass.
//
// Verdict is the artifact-store reference to the assembled gate-verdict record (a
// core.GateVerdict) the gate harvests after grading — the index over the per-check
// evidence, kept so the verification view can render one gate run forensically. It is the
// zero value when no store is configured or the harvest failed (a degraded record, never a
// dropped verdict); the orchestrator stamps Verdict.Hash onto the issue for every
// disposition so a rejected candidate's verdict stays reachable (see specs/verification.md).
type Report struct {
	Passed  bool
	Checks  []CheckResult
	Verdict core.ArtifactRef
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

	// packageProxy, when set, gives the verification sandbox a package-proxy egress so a
	// candidate that adds a brand-new dependency can be re-gated (T5.6a). Empty keeps the
	// verifier deny-all (the default — see provisionVerifier). It is the same base URL the
	// runner's relay forwards to (config.BrokerConfig.PackageProxyURL); cmd/software-factory sets it
	// only when the operator has allowlisted package-proxy, so enabling the producer's
	// dependency fetch enables the verifier's by the same opt-in.
	packageProxy string

	// independent is the set of command-check names the gate keeps running *past* a failure,
	// so one qa pass aggregates every independent-scanner finding instead of stopping at the
	// first (better dead-letter triage — a human/agent sees all the findings to fix at once,
	// not one round-trip per scanner; T2.12). It is keyed by the bare check name (a command
	// check's CheckResult.Name == its postcondition), so the reserved proofs and metric
	// comparisons — whose Names are never plain registry keys — are never in it and stay
	// fail-fast (a mutation score on red tests is meaningless; see specs/verification.md). It
	// is the validated config.Harness.IndependentChecks; empty restores pure fail-fast.
	independent map[string]bool
}

// Option configures a gate Runner. Functional options keep New's positional collaborators
// stable while letting later capabilities (the verifier's package egress, T5.6a) be added
// without churning every call site.
type Option func(*Runner)

// WithPackageProxy grants the verification sandbox a package-proxy egress forwarding to
// base (the runner's package proxy, proxy.golang.org by default), so a candidate adding a
// new dependency can be re-gated. An empty base is a no-op (verifier stays deny-all), so
// callers may pass it unconditionally. This widens the verifier's *reach* but not what it
// *trusts*: go.sum pins the exact bytes the producer fetched (see specs/verification.md,
// specs/security.md).
func WithPackageProxy(base string) Option {
	return func(r *Runner) { r.packageProxy = base }
}

// WithIndependentChecks marks the named command checks as independent: the gate records a
// failure of one and keeps running the remaining checks rather than stopping, so a single
// qa pass surfaces every independent-scanner finding at once (T2.12). Names that are not
// plain command checks (reserved proofs, metric comparisons) never match a graded check's
// Name, so they stay fail-fast even if mistakenly listed; software-factory validate rejects such
// entries up front. An empty list is a no-op (pure fail-fast), so callers may pass it
// unconditionally.
func WithIndependentChecks(names []string) Option {
	return func(r *Runner) {
		if len(names) == 0 {
			return
		}
		r.independent = make(map[string]bool, len(names))
		for _, n := range names {
			r.independent[n] = true
		}
	}
}

// New builds a gate Runner over a sandbox backend and the check registry that resolves
// each candidate's declared postconditions into the commands to run. store is the
// content-addressed home for each check's persisted evidence; like the runner's
// harvest it is best-effort (a nil store, or a write that fails, degrades provenance
// but never the verdict). socketDir is where the per-gate broker socket is minted (the
// verification sandbox still needs a broker endpoint to provision, but it is a deny-all
// one — see Run). A nil logger discards; a nil telemetry Provider defaults to Noop, so the
// gate-run span and metric are emitted unconditionally with no overhead when export is off.
func New(backend sandbox.Backend, registry Registry, store artifact.Store, socketDir string, log *slog.Logger, tel *telemetry.Provider, opts ...Option) *Runner {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if tel == nil {
		tel = telemetry.Noop()
	}
	r := &Runner{backend: backend, registry: registry, store: store, socketDir: socketDir, log: log, tel: tel}
	for _, o := range opts {
		o(r)
	}
	return r
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
	r.log.InfoContext(ctx, "gate: provisioned verification sandbox", "id", cand.ID(), "ref", c.Ref, "profile", c.Profile, "checks", len(checks))

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
		r.log.InfoContext(ctx, "gate: provisioned base verification sandbox", "id", b.ID(), "ref", c.BaseRef)
	}

	report := Report{Passed: true}

	// Build precondition (T9.4): the build is the one dependency every other check shares —
	// a tree that does not compile makes a test run, a scanner, or a mutation score
	// meaningless. So when a build command is registered, the gate runs it FIRST and, if it
	// fails, short-circuits the dependent checks and records each as not-run, instead of
	// letting every downstream tool independently rediscover the broken build in its own
	// error format (one honest compiler error beats N tool-specific renderings of it). The
	// build error itself is captured as a finding — that compiler output IS the signal the
	// retry Brief needs. not-run never counts as a pass: report.Passed is already forced
	// false below, so the gate still fails closed; the tri-state changes only what the
	// verdict *records* (we-never-got-to-check vs. we-checked-and-it's-clean), never the
	// verdict. This is the existing fail-fast rule (a non-independent failure stops the run)
	// made honest about the checks it skipped — and it does not touch independent-scanner
	// aggregation, which still applies among the remaining checks in the normal path below.
	remaining := checks
	if buildCmd, ok := r.registry[checkBuildName]; ok {
		buildCheck := Check{Name: checkBuildName, Cmd: buildCmd, kind: cmdCheck}
		cr, err := runCheck(ctx, buildCheck, base, cand)
		if err != nil {
			return Report{}, err
		}
		report.Checks = append(report.Checks, cr)
		if !cr.Passed {
			report.Passed = false
			cr := &report.Checks[len(report.Checks)-1]
			cr.Findings = buildFailureFindings(*cr)
			r.logFailure(c.Ref, buildCheck, *cr)
			// Short-circuit: every dependent check is not-run (recorded, never re-run), so the
			// verdict says "we never got to check this" rather than a misleading green or a
			// failure that never executed. A declared `build` postcondition is the precondition
			// itself and is excluded so it is not recorded twice.
			for _, check := range dropByName(remaining, checkBuildName) {
				report.Checks = append(report.Checks, notRunResult(check))
			}
			r.persistEvidence(ctx, c.Ref, &report)
			r.persistVerdict(ctx, c.Ref, &report)
			span.SetAttributes(
				attribute.Bool(telemetry.AttrGatePassed, report.Passed),
				attribute.Int(telemetry.AttrGateChecksRun, len(report.Checks)),
			)
			r.tel.RecordGateRun(ctx, report.Passed, time.Since(start))
			r.log.InfoContext(ctx, "gate: verdict", "ref", c.Ref, "passed", report.Passed, "checks_run", len(report.Checks))
			return report, nil
		}
		r.log.DebugContext(ctx, "gate: check passed", "ref", c.Ref, "check", buildCheck.Name)
		// The precondition already graded a declared `build` postcondition; drop it from the
		// loop so the same command is not run twice.
		remaining = dropByName(remaining, checkBuildName)
	}

	for _, check := range remaining {
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
			if !r.independent[check.Name] {
				// A non-independent failure (a red proof, a broken build, a failed metric)
				// makes the remaining checks meaningless — stop at the first.
				break
			}
			// An independent scanner failed: record it and keep running so this one qa pass
			// aggregates every independent finding (T2.12), not just the first.
			continue
		}
		r.log.DebugContext(ctx, "gate: check passed", "ref", c.Ref, "check", check.Name)
	}

	// Persist each check's evidence to the artifact store and stamp the ref onto the
	// result. The captured bytes are already in memory (Exec copied them out of the
	// sandbox), so this survives the deferred teardown regardless of ordering. Both
	// passing and failing checks are persisted — a rejected gate's output is precisely
	// what a human triages from the dead-letter queue.
	r.persistEvidence(ctx, c.Ref, &report)

	// Harvest the assembled verdict record AFTER per-check evidence is persisted, so each
	// check's outcome in the record cites its own evidence hash (the record is the index over
	// them). Best-effort like persistEvidence — a missing store or failed write degrades the
	// record but never the verdict — and recorded for pass and fail alike (a rejected gate's
	// verdict is exactly what a human triages from the dead-letter queue).
	r.persistVerdict(ctx, c.Ref, &report)

	// A verdict was reached: record it. The early returns above are infra errors (no verdict)
	// — deliberately not recorded, so the throughput counter and pass/fail split count only
	// real gate outcomes, never a sandbox that died mid-run (specs/observability.md).
	span.SetAttributes(
		attribute.Bool(telemetry.AttrGatePassed, report.Passed),
		attribute.Int(telemetry.AttrGateChecksRun, len(report.Checks)),
	)
	r.tel.RecordGateRun(ctx, report.Passed, time.Since(start))

	r.log.InfoContext(ctx, "gate: verdict", "ref", c.Ref, "passed", report.Passed, "checks_run", len(report.Checks))
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

// dropByName returns checks with any whose Name == name removed, preserving order. The build
// precondition uses it so a declared `build` postcondition (which the precondition already
// ran) is neither re-run in the normal loop nor recorded twice in the not-run cascade.
func dropByName(checks []Check, name string) []Check {
	out := make([]Check, 0, len(checks))
	for _, c := range checks {
		if c.Name == name {
			continue
		}
		out = append(out, c)
	}
	return out
}

// notRunResult builds the CheckResult for a check the build precondition short-circuited: it
// never executed, so it carries no exit code or output and is explicitly Status==not-run with
// Passed==false. not-run is NOT a pass — the verdict still fails closed — but it is recorded
// as "we never got to check this" rather than a misleading green or a failure that never ran,
// which is the distinction a human triaging the dead-letter queue (and the retry Brief) needs.
func notRunResult(check Check) CheckResult {
	return CheckResult{
		Name:   check.Name,
		Cmd:    check.Cmd,
		Passed: false,
		kind:   check.kind,
		Status: core.CheckStatusNotRun,
	}
}

// buildFailureFindings turns a failed build precondition into a single structured finding: the
// compiler output IS the signal, so it is preserved verbatim as the finding's Detail (the raw
// dump still travels as gate evidence by hash). A build failure has no single file/line — the
// stderr already carries the locations — so the finding is locationless with severity "error"
// and rule "build". This deliberately does NOT parse the output into per-error findings: the
// structured per-tool build parser is T9.5's to wire in; T9.4 owns only the precondition and
// the not-run cascade. The build's stderr is preferred (compilers write errors there); its
// stdout is the fallback so the finding is never empty when a tool prints errors to stdout.
func buildFailureFindings(cr CheckResult) core.Findings {
	detail := strings.TrimRight(string(cr.Stderr), "\n")
	if detail == "" {
		detail = strings.TrimRight(string(cr.Stdout), "\n")
	}
	return core.Findings{{
		Severity: "error",
		Rule:     "build",
		Message:  fmt.Sprintf("build precondition failed (exit %d): no dependent check could run", cr.ExitCode),
		Detail:   detail,
	}}
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
		// Parse the tool's machine-readable output into structured findings (the compact form
		// the verdict, the verification view, and a retry Brief carry instead of the raw dump).
		// A check with no adapter, or whose output is not machine-readable, yields nil here and
		// still grades on its exit code with its raw output as evidence (T9.5).
		Findings: findingsFor(check, res.Stdout, res.Stderr),
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
		// Findings come from the candidate (green) run — a failed proof's candidate output
		// names which tests failed, the signal a retry Brief needs (T9.5). The base (red) run
		// is expected to fail and is recorded by exit code on Base, not parsed for findings.
		Findings: findingsFor(check, greenRes.Stdout, greenRes.Stderr),
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
		r.log.InfoContext(context.Background(), "gate: red→green proof failed", "ref", ref, "check", check.Name,
			"base_exit", cr.Base.ExitCode, "candidate_exit", cr.ExitCode,
			"base_stdout_tail", tailString(cr.Base.Stdout, 1000),
			"candidate_stdout_tail", tailString(cr.Stdout, 1000))
		return
	}
	if cr.Metric != nil {
		// A metric check can fail with exit 0 (a measured score below threshold), so log the
		// score and comparison rather than just the exit code, which would read as a pass.
		r.log.InfoContext(context.Background(), "gate: metric check failed", "ref", ref, "check", check.Name,
			"score", cr.Metric.Score, "parsed", cr.Metric.Parsed,
			"op", cr.Metric.Op, "threshold", cr.Metric.Threshold, "exit_code", cr.ExitCode,
			"stdout_tail", tailString(cr.Stdout, 1000), "stderr_tail", tailString(cr.Stderr, 1000))
		return
	}
	r.log.InfoContext(context.Background(), "gate: check failed", "ref", ref, "check", check.Name, "exit_code", cr.ExitCode,
		"stdout_tail", tailString(cr.Stdout, 2000), "stderr_tail", tailString(cr.Stderr, 2000))
}

// provisionVerifier provisions one fresh verification sandbox seeded at ref, behind its
// own broker. By default the broker is deny-all (the verifier must reach nothing — no
// model calls, git push, or events; the socket exists only because Provision requires a
// broker endpoint, and serving deny-all is how "the verifier has zero I/O" is enforced by
// construction). When a package proxy is configured (WithPackageProxy, T5.6a) the broker
// instead serves a fetch-only handler that permits exactly the package-proxy egress — so a
// candidate that adds a brand-new dependency can be re-gated against the same proxy the
// producer fetched from — and still denies everything else. It returns the live sandbox and
// a cleanup that stops the broker and tears the sandbox down; the caller MUST defer the
// cleanup, since the backend holds host resources.
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
	srv := r.verifierBroker()
	brokerCtx, stopBroker := context.WithCancel(ctx)
	go func() {
		if err := srv.Serve(brokerCtx, ln); err != nil {
			r.log.ErrorContext(brokerCtx, "gate: broker serve", "err", err)
		}
	}()
	cleanup := func() {
		stopBroker() // closes ln, unblocking Serve and unlinking the socket
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), teardownTimeout)
		defer cancel()
		if err := sb.Teardown(tctx); err != nil {
			r.log.ErrorContext(tctx, "gate: teardown verification sandbox", "id", sb.ID(), "err", err)
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
			r.log.ErrorContext(ctx, "gate: persist check evidence", "ref", ref, "check", cr.Name, "err", err)
			continue
		}
		cr.Evidence = a
	}
}

// persistVerdict assembles the gate run's verdict into a single content-addressed
// gate-verdict record and writes it to the artifact store, stamping the returned ref onto
// the Report. It runs after persistEvidence so each per-check outcome can cite its own
// evidence hash — the record is the *index* over the bulky per-check output, not a copy of
// it (see specs/components/artifact-store.md). Best-effort, mirroring persistEvidence: a
// nil store skips it, and a marshal or write failure is logged and leaves Verdict the zero
// value (a degraded record, never a changed verdict).
func (r *Runner) persistVerdict(ctx context.Context, ref string, report *Report) {
	if r.store == nil {
		return
	}
	data, err := json.Marshal(verdictRecord(*report))
	if err != nil {
		r.log.ErrorContext(ctx, "gate: marshal verdict record", "ref", ref, "err", err)
		return
	}
	a, err := r.store.Put(ctx, core.ArtifactKindGateVerdict, bytes.NewReader(data))
	if err != nil {
		r.log.ErrorContext(ctx, "gate: persist verdict record", "ref", ref, "err", err)
		return
	}
	report.Verdict = a
}

// verdictRecord maps the gate's internal Report onto the serializable core.GateVerdict the
// verification view reads back. It translates each check's unexported kind to the stable
// core.GateCheck* spelling and carries the kind-specific detail (a red→green proof's base
// exit, a metric check's score/comparison) plus the per-check evidence hash, so the record
// holds everything the view needs without re-reading each evidence blob.
func verdictRecord(report Report) core.GateVerdict {
	v := core.GateVerdict{Passed: report.Passed, Checks: make([]core.GateCheckOutcome, 0, len(report.Checks))}
	for _, cr := range report.Checks {
		status := cr.Status
		if status == "" {
			// A check that ran carries no explicit Status; derive its tri-state from the
			// pass grade. Only the build precondition records core.CheckStatusNotRun.
			status = core.CheckStatusOf(cr.Passed)
		}
		out := core.GateCheckOutcome{
			Name:     cr.Name,
			Kind:     verdictKind(cr.kind),
			Passed:   cr.Passed,
			Status:   status,
			ExitCode: cr.ExitCode,
			Evidence: cr.Evidence.Hash,
			Findings: cr.Findings,
		}
		if cr.Base != nil {
			out.Base = &core.GateRunOutcome{ExitCode: cr.Base.ExitCode}
		}
		if cr.Metric != nil {
			out.Metric = &core.GateMetricOutcome{
				Score:     cr.Metric.Score,
				Parsed:    cr.Metric.Parsed,
				Op:        cr.Metric.Op,
				Threshold: cr.Metric.Threshold,
			}
		}
		v.Checks = append(v.Checks, out)
	}
	return v
}

// verdictKind translates the gate's internal checkKind to the stable, serialized
// core.GateCheck* spelling the verification view reads.
func verdictKind(k checkKind) string {
	switch k {
	case redGreenProof:
		return core.GateCheckRedGreen
	case redProof:
		return core.GateCheckTestsRed
	case metricCheck:
		return core.GateCheckMetric
	default:
		return core.GateCheckCommand
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

func (denyHandler) Complete(context.Context, broker.CompletionParams) (model.Response, error) {
	return model.Response{}, fmt.Errorf("gate: verification sandbox has no broker egress")
}

func (denyHandler) GitPush(context.Context, broker.GitPushRequest) (broker.GitPushResult, error) {
	return broker.GitPushResult{}, fmt.Errorf("gate: verification sandbox has no broker egress")
}

func (denyHandler) PublishEvent(context.Context, broker.PublishRequest) error {
	return fmt.Errorf("gate: verification sandbox has no broker egress")
}

// FetchPackage is denied with the rest: the deny-all verifier builds the candidate against
// the baked module cache only. A deployment that allowlists package-proxy gets the
// fetch-only verifier instead (verifierBroker / fetchOnlyHandler, T5.6a), so a brand-new
// dependency can be re-gated.
func (denyHandler) FetchPackage(context.Context, broker.FetchPackageRequest) (broker.FetchPackageResult, error) {
	return broker.FetchPackageResult{}, fmt.Errorf("gate: verification sandbox has no broker egress")
}

// verifierBroker builds the broker the verification sandbox serves. Deny-all by default;
// when a package proxy is configured it serves a fetch-only handler with package-proxy the
// only allowlisted destination, so the verifier can pull a candidate's new dependency to
// re-gate it (T5.6a) and nothing else. The allowlist gate in the broker server rejects the
// other three methods before they reach the handler — the handler's denials are defense in
// depth, matching denyHandler.
func (r *Runner) verifierBroker() *broker.Server {
	if r.packageProxy == "" {
		return broker.NewServer(denyHandler{}, broker.WithAllowlist(nil))
	}
	h := fetchOnlyHandler{
		fetcher: packageproxy.NewFetcher(r.packageProxy, nil),
		tel:     r.tel,
		log:     r.log,
	}
	return broker.NewServer(h, broker.WithAllowlist([]string{config.DestPackageProxy}))
}

// fetchOnlyHandler is the verification sandbox's broker handler when a package proxy is
// configured (T5.6a): it permits exactly the package-proxy egress and denies model calls,
// git push, and event publish — which a verifier must never make (producer != verifier; the
// gate reaches nothing else by construction). Re-gating a new-dependency candidate widens the
// verifier's *reach*, not what it *trusts*: go.sum pins the exact bytes the producer fetched.
// It shares internal/packageproxy with the runner relay so the producer's fetch and the
// verifier's can never drift; the fetch is wrapped in a tool-call span so the egress shows up
// under the gate-run trace.
type fetchOnlyHandler struct {
	fetcher *packageproxy.Fetcher
	tel     *telemetry.Provider
	log     *slog.Logger
}

var _ broker.Handler = fetchOnlyHandler{}

func (fetchOnlyHandler) Complete(context.Context, broker.CompletionParams) (model.Response, error) {
	return model.Response{}, fmt.Errorf("gate: verification sandbox may only fetch packages")
}

func (fetchOnlyHandler) GitPush(context.Context, broker.GitPushRequest) (broker.GitPushResult, error) {
	return broker.GitPushResult{}, fmt.Errorf("gate: verification sandbox may only fetch packages")
}

func (fetchOnlyHandler) PublishEvent(context.Context, broker.PublishRequest) error {
	return fmt.Errorf("gate: verification sandbox may only fetch packages")
}

func (h fetchOnlyHandler) FetchPackage(ctx context.Context, req broker.FetchPackageRequest) (broker.FetchPackageResult, error) {
	// The broker serves on a context descending from the gate-run span (Run → provisionVerifier
	// → Serve), so the tool-call span nests under that trace without re-parenting.
	_, span := h.tel.Tracer().Start(ctx, telemetry.SpanToolCall, trace.WithAttributes(
		attribute.String(telemetry.AttrComponent, telemetry.ComponentBroker),
		attribute.String(telemetry.AttrToolName, telemetry.ToolPackageFetch),
	))
	defer span.End()

	res, err := h.fetcher.Fetch(ctx, req)
	if err != nil {
		span.RecordError(err)
		h.log.ErrorContext(ctx, "gate: package fetch failed", "path", req.Path, "err", err)
		return broker.FetchPackageResult{}, err
	}
	span.SetAttributes(attribute.Int(telemetry.AttrHTTPStatus, res.Status))
	h.log.InfoContext(ctx, "gate: package fetch", "path", req.Path, "status", res.Status, "bytes", len(res.Body))
	return res, nil
}
