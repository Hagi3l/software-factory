package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Loxstomper/software-factory/internal/config"
	"github.com/Loxstomper/software-factory/internal/core"
	"github.com/Loxstomper/software-factory/internal/gate"
	"github.com/Loxstomper/software-factory/internal/messaging"
	"github.com/Loxstomper/software-factory/internal/telemetry"
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

// defaultSweepInterval paces the slow reconcile sweep — re-deriving already-merged work on a
// human spec edit (recompileMergedDelta) — separately from the fast dispatch tick. That sweep
// does a full-table beads ListAll every pass purely to detect minute-plus-granularity human
// edits, so pacing it at the dispatch interval needlessly multiplies the Dolt read pressure that
// is the very cause of the write-visibility lag the in-flight projection has to tolerate (T3.13).
// It runs an order of magnitude less often than dispatch; the in-flight lease and spec-drift
// sweeps stay on the fast tick because they now read the projection, not beads (see tickLoop).
const defaultSweepInterval = 30 * time.Second

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
	// RepointDependents moves every blocked-by edge from a superseded predecessor (oldID) onto its
	// successor (newID) when route/resolveConflict replace a closed issue, so its dependents wait on
	// the replacement instead of being freed by the predecessor's closure. It is the reverse-
	// direction complement to the successor inheriting the predecessor's own blockers via
	// Proposal.DependsOn (see specs/workflow.md "A retry preserves its predecessor's DAG position").
	RepointDependents(ctx context.Context, oldID, newID string) error
	PinSpecHash(ctx context.Context, id, hash string) error
	Reissue(ctx context.Context, id string) error
	// ListAll returns every issue regardless of status (including closed). It is the cold-start
	// hydration source for the work-graph projection (rebuildInflight reads the whole graph once at
	// startup, then the live loop reads the projection, not beads — the lease sweep, the spec-drift
	// sweep, and dispatch/result gating all consult it; T3.13, T8.1, specs/components/orchestrator.md
	// "Live state vs. durable state"). It also backs the cross-issue epic-budget aggregate read (sum
	// the per-issue closing spend over all issues sharing an epic id; see authorizeEpic).
	ListAll(ctx context.Context) ([]core.Issue, error)
	// StampClosingSpend records an issue's own invocation marginal (the usage breakdown and its
	// priced USD) so the epic budget can be summed across all issues of an epic (it stamps the
	// scalar ClosingTokens=usage.TotalTokens() the aggregate reads, plus the display-only
	// per-kind ClosingUsage; see core.Issue.ClosingTokens / ClosingUsage).
	StampClosingSpend(ctx context.Context, id string, usage core.Usage, usd float64) error
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
	// StampTransformLog records the artifact hash of an issue's transformation log (the semantic-
	// vs-text-fallback record of its writes), for every disposition, so the verification view can
	// weigh a candidate's text-fallback transformations (see core.Issue.TransformLog, T6.3).
	StampTransformLog(ctx context.Context, id, hash string) error
	// StampIntegrated marks a child whose verified candidate landed on its integration branch,
	// the durable distinction `closed` cannot make (integrated vs. superseded vs. closed root).
	// The orchestrator stamps it in the merge path the instant a candidate lands, so the board
	// hero's epic roll-up counts integration rather than any close (see core.Issue.Integrated,
	// the plan's T8.3).
	StampIntegrated(ctx context.Context, id string) error
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
// the default implementation (see specs/integration.md, specs/bootstrap.md). The progress
// callback announces the merge train's internal steps (rebasing, re-gating) as they happen
// (T4.24, see MergeProgress); a nil callback skips it.
// target is the fully-qualified ref the candidate integrates onto and the merge train advances:
// refs/heads/main in per-item mode, or the epic branch refs/heads/epic/<epic_id> in epic mode
// (specs/integration.md, T7.3). Everywhere the per-item queue read "main", it now reads target;
// in epic mode the candidate rebases onto, re-gates against, and lands on the epic branch, and
// the real main advances only later at the epic's terminal merge (T7.4). The merger creates the
// epic branch off main on first use, so the orchestrator need only name the target.
type Merger interface {
	Merge(ctx context.Context, repo, ref, target string, prov core.Provenance, regate ReGate, progress MergeProgress) (commit string, err error)
	// MergeEpic performs an epic's terminal merge: the single, atomic landing of a whole feature
	// on main (specs/integration.md "Atomic feature integration"). It writes a two-parent merge
	// commit on target — first parent the current main, second parent the epic branch tip — so
	// main's first-parent history reads as one commit per feature while every per-child provenance
	// commit stays reachable under the merge (two-tier provenance). It is idempotent: a merge
	// commit already citing the epic id on main means the terminal merge already landed, so it
	// writes nothing and reports merged=false. merged=true means a fresh terminal merge was
	// written (commit is the new main tip); merged=false with a nil error means nothing was done
	// (already landed, or the epic branch never advanced past main). epicRef and target are
	// fully-qualified refs (refs/heads/epic/<id>, refs/heads/main); the merger does not DWIM.
	MergeEpic(ctx context.Context, repo, epicRef, target string, prov core.Provenance) (commit string, merged bool, err error)
}

// Options configures an Orchestrator. They are the instance knobs (which config it
// schedules from, where the integration repo lives, the lease/tick cadence) separate
// from the injected collaborators.
type Options struct {
	// Config supplies the workflow DAG, the souls that fulfill roles, and the
	// termination policy. It is the validated configuration (software-factory validate has run).
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
	// Tick paces the fast dispatch loop: scheduleReady plus the in-memory in-flight sweeps
	// (lease recovery, spec-drift). Defaults to defaultTick.
	Tick time.Duration
	// SweepInterval paces the slow reconcile sweep (recompileMergedDelta, the full-table
	// ListAll that tracks human spec edits to already-merged work) separately from the fast
	// dispatch tick, so the heavy/rare beads read does not pace dispatch or add to the read
	// pressure that causes write-visibility lag (T3.13). Defaults to defaultSweepInterval.
	SweepInterval time.Duration
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
	// events on (factory.issue.<id>.state). It is the core conn under js — distinct because
	// issue-state events are core NATS with no stream, not JetStream (see announceState,
	// specs/messaging.md "Issue-state events").
	nc  *nats.Conn
	log *slog.Logger
	tel *telemetry.Provider

	leaseTTL time.Duration
	tick     time.Duration
	sweep    time.Duration
	base     string
	dlq      string

	// inflight is the in-memory work-graph projection: the single writer's read-your-writes
	// consistent record of the live state of every issue it knows — in_progress AND settled —
	// maintained at the transition choke point and consulted by dispatch + result gating instead
	// of a lagging beads read. It is derived from beads and rebuilt from the full graph at startup
	// (rebuildInflight), so it is a consistency cache, never a source of truth
	// (specs/components/orchestrator.md "Live state vs. durable state").
	inflight *inflightProjection

	// createMu serializes the creation choke point (applyTracked's bd.Apply+track) against the
	// dispatch oracle read (scheduleReady's bd.Ready). bd.Apply is non-atomic — it creates a child,
	// then adds its blocking edges as separate writes — so without this lock the dispatch loop's
	// bd.ready() can observe a half-built decomposition child (created, no edges yet) and claim it,
	// bypassing both the parent-plan gate and the inter-sibling order, then wedging it permanently
	// (the 2026-06-23 vault-demo stall). Holding it makes creation atomic w.r.t. the oracle: bd.ready()
	// sees a decomposition either not-yet-started or fully built. It is deliberately NOT a coarse lock
	// around handleResult's slow gate/merge work — only the fast beads writes — so dispatch is never
	// stalled for a merge (T10.2, specs/components/orchestrator.md "The creation window").
	createMu sync.Mutex

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
		add("config has no factory (DAG/policy) loaded")
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
		log = slog.New(slog.DiscardHandler)
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
		sweep:    opts.SweepInterval,
		base:     opts.Base,
		dlq:      messaging.SubjectDLQ,
		inflight: newInflightProjection(),
	}
	if o.leaseTTL <= 0 {
		o.leaseTTL = defaultLeaseTTL
	}
	if o.tick <= 0 {
		o.tick = defaultTick
	}
	if o.sweep <= 0 {
		o.sweep = defaultSweepInterval
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

// streamOptions derives the JetStream stream knobs (replication factor, result
// retention) from the infra overlay so the orchestrator's idempotent SetupStreams call
// reconciles the streams to the SAME definition the composition root created them with —
// not back to the zero-value defaults (see messaging.SetupStreams). Nil-safe: a test
// orchestrator built without an Infra overlay falls back to the bootstrap defaults.
func (o *Orchestrator) streamOptions() messaging.StreamOptions {
	if c := o.opts.Config; c != nil && c.Infra != nil {
		return messaging.StreamOptions{
			Replicas:     c.Infra.NATS.JetStream.Replicas,
			ResultMaxAge: time.Duration(c.Infra.NATS.JetStream.MaxAge),
		}
	}
	return messaging.StreamOptions{}
}

// Run starts the reconciliation loop and blocks until ctx is canceled. It ensures the
// streams and the consumers exist (idempotent — safe on every startup, matching the
// crash-and-resume model), then runs concurrent loops: event-driven Result and approval
// consumers, and a single tick loop that dispatches ready work and runs the reconcile
// sweeps (the in-memory in-flight sweeps on a fast tick, the heavy merged-delta sweep on
// a slower one — see tickLoop). It returns the first non-shutdown error any loop reports.
func (o *Orchestrator) Run(ctx context.Context) error {
	if err := messaging.SetupStreams(ctx, o.js, o.streamOptions()); err != nil {
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

	// Rebuild the in-flight projection from beads' durable in_progress set BEFORE the first
	// dispatch or result is handled, so a restart resumes with an accurate live view. This keeps
	// crash-safety unchanged: the projection is derived from beads, never a second source of truth
	// (see rebuildInflight, specs/components/orchestrator.md "Crash safety"). Best-effort, like the
	// loop's other beads reads (scheduleReady/sweeps log-and-continue): a failed seed self-heals —
	// results for pre-restart in-flight work are briefly ignored until the lease sweep restrands
	// and redispatches them — so a transient beads hiccup at boot must not crash the orchestrator.
	if err := o.rebuildInflight(ctx); err != nil {
		o.log.ErrorContext(ctx, "orchestrator: rebuild in-flight projection at startup; continuing with empty projection", "err", err)
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

// rebuildInflight hydrates the work-graph projection from beads' FULL graph (ListAll, every status)
// — generalizing the original in_progress-only seed. It runs once at startup, before the first
// dispatch or result is handled, so a restarted orchestrator resumes with an accurate live view of
// every issue it knows: which are in flight (with their durable leases, so the lease sweep recovers
// stranded work on its original deadline) AND which are already settled (so the scheduler will not
// re-dispatch a just-closed/just-blocked issue a lagging bd.ready() still lists, and the control
// room reads a consistent surface). This is the one heavy beads read, paid once at cold start rather
// than on the hot path — beads stays the durable log + hydration source, the projection the read
// model (specs/components/orchestrator.md "Live state vs. durable state"). Crash-safety is unchanged
// because the projection is derived from beads, never a second source of truth: a crash loses only
// the cache, which this rebuilds. The caller (Run) treats a read failure as best-effort — it logs
// and continues with an empty projection rather than crashing — because the loop's other beads reads
// are best-effort too and a missed seed self-heals: bd.ready() never returns in_progress work, so an
// empty projection cannot re-dispatch it; the only effect is that a result for pre-restart in-flight
// work is briefly ignored until the lease sweep restrands and redispatches it.
func (o *Orchestrator) rebuildInflight(ctx context.Context) error {
	all, err := o.bd.ListAll(ctx)
	if err != nil {
		return fmt.Errorf("orchestrator: rebuild work-graph projection: %w", err)
	}
	o.inflight.reset(all)
	o.log.InfoContext(ctx, "orchestrator: rebuilt work-graph projection from beads", "issues", len(all), "in_flight", o.inflight.size())
	return nil
}

// Snapshot returns the work-graph projection — every issue the orchestrator knows, with its live
// status and lease stamped on — as the control room's projection-backed live read model (T8.4,
// specs/observability.md "The live read model"). When co-located (software-factory run) the control room
// reads this in-memory, read-your-writes-consistent surface for its board / DAG / dead-letter /
// status views instead of polling beads, so they never lag the single writer (no card showing
// `open` while its agent runs) and add no `bd list` load — the read overload the demo run hit. ctx
// is unused (a pure in-memory read) but kept so this satisfies the same ctx-taking shape the
// beads-backed reader has. Safe for concurrent calls from control-room HTTP handlers (the
// projection is mutex-guarded).
func (o *Orchestrator) Snapshot(_ context.Context) ([]core.Issue, error) {
	return o.inflight.snapshot(), nil
}

// Track records externally-written issues into the work-graph projection, keeping it consistent
// with the factory's two discrete human-approved write paths that intentionally bypass the
// reconcile loop (like `software-factory seed`): the Create-Task wizard's seed (new open issues) and the
// Resolve wizard's reopen of a dead-letter (Reissue, blocked→open). Both write beads directly, so
// without this the projection — which is now both the scheduler's dispatch oracle (T8.2) and the
// control room's board (T8.4) — would not see a seed until it was claimed, and would keep reading a
// reopened dead-letter as blocked (the scheduler would then skip it forever, and the board would
// mis-show it). The projection is a derived cache, so this only keeps it in step with an authorized
// durable write; it is mutex-guarded for the cross-goroutine call from the control room.
func (o *Orchestrator) Track(issues ...core.Issue) {
	now := time.Now().UTC()
	for _, is := range issues {
		o.inflight.track(is, now)
	}
}

// tickLoop is the single-goroutine reconcile loop, paced by two tickers so the heavy, rare sweep
// does not pace dispatch (T3.13). Both fire an immediate pass at startup so neither is delayed by
// one interval.
//
//   - The fast tick (o.tick) runs dispatchPass: schedule ready work, recover stranded leases, and
//     re-derive spec drift on in-flight work. All three are now in-memory against the in-flight
//     projection (the single writer's read-your-writes record) — scheduleReady's bd.ready() is the
//     only beads read on this path and is just the candidate oracle.
//   - The slow tick (o.sweep) runs recompileMergedDelta (a full-table beads ListAll that re-derives
//     ALREADY-MERGED work after a human spec edit) and, under integration.mode: epic,
//     sweepEpicCompletion (the epic_id aggregate read that lands a drained feature's terminal merge,
//     T7.4). These are the remaining beads reads off the hot path; pacing them slower cuts the Dolt
//     read pressure that causes the write-visibility lag the projection has to tolerate, and both
//     human spec edits and epic completion are minute-plus-granularity events anyway.
//
// Both reconcilers run in the SAME goroutine (a select over the two tickers), so beads writes stay
// serialized within the loop exactly as before — the split changes cadence, not concurrency. Every
// pass is idempotent: "already dispatched" is "in the projection"; "stale in-flight" is "pinned
// spec hash no longer matches the re-resolved slice"; "stranded" is "lease expired". The in-flight
// reconcilers return affected work to the ready pool; recompileMergedDelta spawns a re-derivation
// plan. The next scheduleReady redispatches whatever they produced.
func (o *Orchestrator) tickLoop(ctx context.Context) error {
	dispatch := time.NewTicker(o.tick)
	defer dispatch.Stop()
	sweep := time.NewTicker(o.sweep)
	defer sweep.Stop()
	o.dispatchPass(ctx)
	o.recompileMergedDelta(ctx)
	o.sweepEpicCompletion(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-dispatch.C:
			o.dispatchPass(ctx)
		case <-sweep.C:
			o.recompileMergedDelta(ctx)
			o.sweepEpicCompletion(ctx)
		}
	}
}

// dispatchPass is the fast tick's body: dispatch newly-ready work, then run the two in-memory
// in-flight sweeps (lease recovery, spec drift) so a stranded or spec-drifted issue is returned to
// the ready pool and the same pass's — or the next tick's — scheduleReady picks it up. Order is
// deliberate: dispatch first (the latency-sensitive path), then the cheap projection scans.
func (o *Orchestrator) dispatchPass(ctx context.Context) {
	o.scheduleReady(ctx)
	o.sweepLeases(ctx)
	o.recompileSpecDelta(ctx)
}
