package orchestrator

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/gate"
	"github.com/Loxstomper/harness/internal/messaging"
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
	Block(ctx context.Context, id string) error
	Apply(ctx context.Context, proposals []core.Proposal) ([]core.Issue, error)
	ListStranded(ctx context.Context, now time.Time) ([]string, error)
	InProgress(ctx context.Context) ([]core.Issue, error)
	PinSpecHash(ctx context.Context, id, hash string) error
	Reissue(ctx context.Context, id string) error
}

// Gate verifies a candidate in a fresh, orchestrator-controlled sandbox and returns a
// pass/fail report, or an error if it could not reach a verdict (a transient
// infrastructure failure, distinct from a clean failing report). *gate.Runner
// satisfies it (see specs/verification.md).
type Gate interface {
	Run(ctx context.Context, c gate.Candidate) (gate.Report, error)
}

// Merger fast-forwards a verified candidate branch onto main in the integration repo.
// In the bootstrap the merge queue is trivial — a single serialized stream, no rebase
// or re-gate — so a fast-forward is the whole of integration (see
// specs/integration.md, specs/bootstrap.md). gitMerger is the default implementation.
type Merger interface {
	Merge(ctx context.Context, repo, ref string, prov Provenance) (commit string, err error)
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
	log    *slog.Logger

	leaseTTL time.Duration
	tick     time.Duration
	base     string
	dlq      string
}

// New builds an Orchestrator from its options and injected collaborators: the beads
// store (T1.3/T1.4), the gate runner (T1.17), the git merger, and the JetStream
// handle used to dispatch work and consume Results. It validates options up front —
// fail loud, consistent with config validation being a startup gate.
func New(opts Options, bd Beads, g Gate, merger Merger, js jetstream.JetStream) (*Orchestrator, error) {
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
	o := &Orchestrator{
		opts:     opts,
		bd:       bd,
		gate:     g,
		merger:   merger,
		js:       js,
		log:      log,
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

	var wg sync.WaitGroup
	errs := make(chan error, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		errs <- o.consumeResults(ctx, cons)
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

// tickLoop schedules ready work, recompiles the spec delta, and sweeps stranded leases on
// each tick, plus an immediate pass at startup so dispatch is not delayed by one interval.
// Every pass is idempotent and reads its state from beads, never from orchestrator memory:
// "already dispatched" is "not in the ready set"; "stale in-flight" is "pinned spec hash no
// longer matches the re-resolved slice"; "stranded" is "lease expired". The two reconcilers
// (recompileSpecDelta, sweepLeases) return affected in_progress issues to the ready pool;
// the next scheduleReady redispatches them.
func (o *Orchestrator) tickLoop(ctx context.Context) error {
	t := time.NewTicker(o.tick)
	defer t.Stop()
	for {
		o.scheduleReady(ctx)
		o.recompileSpecDelta(ctx)
		o.sweepLeases(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
