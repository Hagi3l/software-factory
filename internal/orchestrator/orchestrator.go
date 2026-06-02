package orchestrator

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/gate"
	"github.com/Loxstomper/harness/internal/messaging"
	"github.com/Loxstomper/harness/internal/telemetry"
)

// defaultTick is how often the orchestrator runs a schedule + sweep pass when no
// interval is configured. Result handling is event-driven (a JetStream consumer), so
// this only paces dispatch of newly-ready work and recovery of stranded leases.
const defaultTick = 2 * time.Second

// defaultLeaseTTL is the default claim lease. It must comfortably exceed a single
// invocation; the runner's JetStream AckWait plays the same role on the messaging
// side, and the two together are the crash-recovery window (see
// specs/components/orchestrator.md).
const defaultLeaseTTL = 30 * time.Minute

// Beads is the orchestrator's single-writer view of the work store. It is the only
// component that mutates the graph; concentrating every read and write the
// reconcile loop needs behind one interface keeps that invariant enforceable and
// lets the loop be tested against a fake store. *beads.Client satisfies it.
type Beads interface {
	Ready(ctx context.Context) ([]core.Issue, error)
	Get(ctx context.Context, id string) (core.Issue, error)
	Claim(ctx context.Context, id string, ttl time.Duration) (time.Time, error)
	Release(ctx context.Context, id string) error
	Close(ctx context.Context, id string) error
	Block(ctx context.Context, id, reason string) error
	// AwaitApproval parks an integrate candidate awaiting human approval (T2.10): blocks the
	// issue and records the candidate ref + provenance to replay on approval.
	AwaitApproval(ctx context.Context, id, candidateRef, parkedProv string) error
	// RecordApproval stamps a human's approval (who, which candidate sha) on a parked issue.
	RecordApproval(ctx context.Context, id, approvedRef, approver string) error
	Apply(ctx context.Context, proposals []core.Proposal) ([]core.Issue, error)
	ListStranded(ctx context.Context, now time.Time) ([]string, error)
	InProgress(ctx context.Context) ([]core.Issue, error)
	PinSpecHash(ctx context.Context, id, hash string) error
	Reissue(ctx context.Context, id string) error
	// ListAll returns every issue regardless of status (including closed) — the input to the
	// cross-issue epic-budget aggregate read (sum the per-issue closing spend over all issues
	// sharing an epic id; see authorizeEpic, specs/workflow.md).
	ListAll(ctx context.Context) ([]core.Issue, error)
	// StampClosingSpend records an issue's own invocation marginal (tokens, USD) so the epic
	// budget can be summed across all issues of an epic (see core.Issue.ClosingTokens).
	StampClosingSpend(ctx context.Context, id string, tokens int, usd float64) error
	// StampTranscript records the artifact hash of an issue's most recent invocation transcript
	// so the decision trail is reachable from the issue for in-flight/dead-lettered work, not
	// only from a merge trailer (see core.Issue.Transcript, the plan's T4.15).
	StampTranscript(ctx context.Context, id, hash string) error
	// StampSouls records an issue's producing soul(s) — TestsSoul (author-tests) / ImplementSoul
	// (implement) — keyed off its stage's reserved proof, so producer ≠ verifier is demonstrable
	// after the fact and threaded forward (see core.Issue.TestsSoul, the plan's T4.22).
	StampSouls(ctx context.Context, id, testsSoul, implementSoul string) error
	// StampGateVerdict records the artifact hash of the assembled gate-verdict record for an
	// issue's gate run, for every disposition, so a rejected candidate's verdict is reachable
	// for the verification view (see core.Issue.GateVerdict, the plan's T4.22).
	StampGateVerdict(ctx context.Context, id, hash string) error
}

// Gate verifies a candidate in a fresh, orchestrator-controlled sandbox and returns a
// pass/fail report, or an error if it could not reach a verdict (a transient
// infrastructure failure, distinct from a clean failing report). *gate.Runner
// satisfies it (see specs/verification.md).
type Gate interface {
	Run(ctx context.Context, c gate.Candidate) (gate.Report, error)
}

// Merger lands a verified candidate branch onto main in the integration repo as a
// serialized merge queue: each candidate is rebased onto the current main tip (so
// independently-based green branches integrate one at a time), the rebased result is
// re-gated against what will actually land, and a trusted provenance commit is written on
// top. A rebase conflict is reported, not retried. The regate callback re-verifies the
// rebased result (see ReGate); a fast-forward, or a nil callback, skips it. gitMerger is
// the default implementation (see specs/integration.md, specs/bootstrap.md).
type Merger interface {
	Merge(ctx context.Context, repo, ref string, prov core.Provenance, regate ReGate) (commit string, err error)
}

// Options configures an Orchestrator. They are the instance knobs (which config it
// schedules from, where the integration repo lives, the lease/tick cadence) separate
// from the injected collaborators.
type Options struct {
	// Config supplies the workflow DAG, the souls that fulfill roles, and the
	// termination policy. It is the validated configuration (harness validate has run).
	Config *config.Config
	// Repo is the integration repository: candidates are pushed here by runners and
	// merged to main here on acceptance. The orchestrator and its runners share it.
	Repo string
	// Base is the git ref new work branches from (the candidate's base). Defaults to
	// "main".
	Base string
	// Limits is the resource ceiling for the verification (gate) sandbox.
	Limits config.SandboxLimits
	// LeaseTTL is how long a claim holds an issue in_progress before the reconcile
	// sweep considers it stranded. Defaults to defaultLeaseTTL.
	LeaseTTL time.Duration
	// Tick paces the schedule + sweep loop. Defaults to defaultTick.
	Tick time.Duration
	// Logger receives lifecycle logs. Defaults to a discard logger.
	Logger *slog.Logger
	// Telemetry receives the per-invocation cost metric (the orchestrator is the only
	// component holding the per-model price table). A nil Provider defaults to telemetry.Noop.
	Telemetry *telemetry.Provider
}

// Orchestrator is the single scheduler, gatekeeper, and sole beads writer. It runs a
// reconciliation control loop: it holds no critical in-memory state, so its entire
// authoritative view lives in beads + JetStream and it can crash and resume by
// re-reading them (see specs/components/orchestrator.md).
type Orchestrator struct {
	opts   Options
	bd     Beads
	gate   Gate
	merger Merger
	js     jetstream.JetStream
	// nc is the core-NATS connection the orchestrator publishes fire-and-forget issue-state
	// events on (harness.issue.<id>.state). It is the core conn under js — distinct because
	// issue-state events are core NATS with no stream, not JetStream (see announceState,
	// specs/messaging.md "Issue-state events").
	nc  *nats.Conn
	log *slog.Logger
	tel *telemetry.Provider

	leaseTTL time.Duration
	tick     time.Duration
	base     string
	dlq      string

	// diffFiles returns the repo-relative paths a candidate ref changed relative to base.
	// It is the input to the TCB-touching approval decision (T2.10) and a seam so the
	// decision is unit-testable without a real repo; New defaults it to a git-backed impl.
	diffFiles func(ctx context.Context, repo, base, ref string) ([]string, error)
}

// New builds an Orchestrator from its options and injected collaborators: the beads
// store (T1.3/T1.4), the gate runner (T1.17), the git merger, and the JetStream
// handle used to dispatch work and consume Results. It validates options up front —
// fail loud, consistent with config validation being a startup gate.
func New(opts Options, bd Beads, g Gate, merger Merger, nc *nats.Conn, js jetstream.JetStream) (*Orchestrator, error) {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if opts.Config == nil {
		add("config is required")
	} else if opts.Config.Harness == nil {
		add("config has no harness (DAG/policy) loaded")
	}
	if opts.Repo == "" {
		add("repo is required (the integration repo candidates are merged in)")
	}
	if bd == nil {
		add("beads store is required")
	}
	if g == nil {
		add("gate is required")
	}
	if merger == nil {
		add("merger is required")
	}
	if nc == nil {
		add("nats connection is required (issue-state events)")
	}
	if js == nil {
		add("jetstream handle is required")
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("orchestrator: invalid options:\n  - %s", strings.Join(problems, "\n  - "))
	}

	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	tel := opts.Telemetry
	if tel == nil {
		tel = telemetry.Noop()
	}
	o := &Orchestrator{
		opts:     opts,
		bd:       bd,
		gate:     g,
		merger:   merger,
		js:       js,
		nc:       nc,
		log:      log,
		tel:      tel,
		leaseTTL: opts.LeaseTTL,
		tick:     opts.Tick,
		base:     opts.Base,
		dlq:      messaging.SubjectDLQ,
	}
	if o.leaseTTL <= 0 {
		o.leaseTTL = defaultLeaseTTL
	}
	if o.tick <= 0 {
		o.tick = defaultTick
	}
	if o.base == "" {
		o.base = "main"
	}
	if dl := opts.Config.Harness.Policy.DeadLetter; dl != "" {
		o.dlq = dl
	}
	o.diffFiles = gitChangedFiles
	return o, nil
}

// Run starts the reconciliation loop and blocks until ctx is canceled. It ensures the
// streams and the Result consumer exist (idempotent — safe on every startup, matching
// the crash-and-resume model), then runs two concurrent loops: an event-driven Result
// consumer, and a ticker that schedules ready work and sweeps stranded leases. It
// returns the first non-shutdown error either loop reports.
func (o *Orchestrator) Run(ctx context.Context) error {
	if err := messaging.SetupStreams(ctx, o.js); err != nil {
		return err
	}
	cons, err := messaging.EnsureResultConsumer(ctx, o.js)
	if err != nil {
		return err
	}
	approvals, err := messaging.EnsureApprovalConsumer(ctx, o.js)
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	errs := make(chan error, 3)

	wg.Add(1)
	go func() {
		defer wg.Done()
		errs <- o.consumeResults(ctx, cons)
	}()

	// The approvals consumer is the third reader of the single-writer orchestrator: human
	// approve/reject decisions for parked integrate candidates (T2.10). Like the result
	// consumer its handling is idempotent (status-gated), so at-least-once redelivery is safe.
	wg.Add(1)
	go func() {
		defer wg.Done()
		errs <- o.consumeApprovals(ctx, approvals)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		errs <- o.tickLoop(ctx)
	}()

	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// tickLoop schedules ready work, recompiles the spec delta (in-flight and already-merged), and
// sweeps stranded leases on each tick, plus an immediate pass at startup so dispatch is not
// delayed by one interval. Every pass is idempotent and reads its state from beads, never from
// orchestrator memory: "already dispatched" is "not in the ready set"; "stale in-flight" is
// "pinned spec hash no longer matches the re-resolved slice"; "stranded" is "lease expired". The
// in-flight reconcilers (recompileSpecDelta, sweepLeases) return affected in_progress issues to
// the ready pool; recompileMergedDelta instead spawns a re-derivation plan for already-merged work
// the spec edit reached. The next scheduleReady redispatches whatever they produced.
func (o *Orchestrator) tickLoop(ctx context.Context) error {
	t := time.NewTicker(o.tick)
	defer t.Stop()
	for {
		o.scheduleReady(ctx)
		o.recompileSpecDelta(ctx)
		o.recompileMergedDelta(ctx)
		o.sweepLeases(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
