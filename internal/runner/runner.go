package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/Loxstomper/software-factory/internal/artifact"
	"github.com/Loxstomper/software-factory/internal/broker"
	"github.com/Loxstomper/software-factory/internal/config"
	"github.com/Loxstomper/software-factory/internal/core"
	"github.com/Loxstomper/software-factory/internal/messaging"
	"github.com/Loxstomper/software-factory/internal/model"
	"github.com/Loxstomper/software-factory/internal/sandbox"
	"github.com/Loxstomper/software-factory/internal/secret"
	"github.com/Loxstomper/software-factory/internal/telemetry"
)

// teardownTimeout bounds the reap of a sandbox. Teardown runs on a fresh context
// (not the invocation's) so a canceled or timed-out invocation still reaps its
// sandbox — the ephemerality guarantee must not depend on the happy path (see
// specs/components/runner.md, specs/components/sandbox.md).
const teardownTimeout = 30 * time.Second

// Invoker runs one agent against a provisioned sandbox and returns its Result. It is
// the seam the agent inner loop (plan T1.13, internal/agent.Loop) fills: the runner owns
// the sandbox lifecycle and the broker, and hands the live sandbox, the Brief, and the
// broker endpoint to the agent. The Brief is delivered to the in-process bootstrap loop
// as a value — that is what "inject the Brief" means while the agent is co-located;
// physical injection into a separately-running sandboxed agent process arrives with the
// Firecracker work. The broker endpoint is the loop's only route out: it dials it for
// every model call, git push, and event (see specs/models.md, specs/components/runner.md).
type Invoker interface {
	Invoke(ctx context.Context, sb sandbox.Sandbox, brief core.Brief, brokerEndpoint sandbox.Endpoint) (core.Result, error)
}

// AdapterResolver maps an invocation's soul.Model to the provider adapter the runner
// calls on the agent's behalf. It is the model registry (plan T1.10); abstracted here
// so the broker relay can be exercised with a fake. The runner holds the registry and
// the API key — the sandbox never does (see specs/models.md, specs/security.md).
type AdapterResolver interface {
	Adapter(modelName string) (model.Adapter, error)
}

// Publisher is the core-NATS publish seam the broker relay uses for the live agent
// event/token feed (agent events are core NATS, not JetStream — see specs/messaging.md).
// A *nats.Conn satisfies it; tests supply a recorder.
type Publisher interface {
	Publish(subject string, data []byte) error
}

// Options configures a Runner. They are the runner-instance knobs (which roles it
// serves, where it seeds worktrees from) separate from the injected collaborators.
type Options struct {
	// Roles are the DAG roles this runner serves. It binds one JetStream pull
	// consumer per role and competes with other runners to pull (horizontal scale by
	// adding runners — see specs/messaging.md).
	Roles []string
	// Repo is the source repository each sandbox worktree is seeded from. In bootstrap
	// this is the local harness repo path; the backend clones it at the Brief's base ref.
	Repo string
	// SocketDir is the directory the per-invocation broker unix sockets are created in.
	SocketDir string
	// Limits is the per-sandbox resource ceiling, passed straight through to every Spec
	// (single source of truth — config.Infra.Sandbox.Limits).
	Limits config.SandboxLimits
	// MaxConcurrency is how many invocations this runner serves PER ROLE at once
	// (config.Infra.Sandbox.MaxConcurrency). 0 or 1 is serial — a role's ready siblings run
	// back-to-back; >1 binds that many worker loops per role so same-role siblings run
	// concurrently. It is the lever that fans out a wide decomposition instead of serializing
	// it on the slowest stage. Peak RAM is bounded by (MaxConcurrency x Limits.Mem) per busy
	// role, so size it to the host. Each worker holds its own one-message iterator, so a
	// message's lease (AckWait) ticks only while a worker is actually processing it.
	MaxConcurrency int
	// ResolveImage maps a soul's logical sandbox profile to the concrete artifact the
	// backend boots (config.Infra.Sandbox.ResolveImage). Nil leaves Spec.Image empty, so
	// the backend falls back to the profile name — the test-only path; production always
	// wires this so the resolved (ideally digest-pinned) image lands in the spec and the
	// boot span's provenance.
	ResolveImage func(profile string) string
	// Allowlist is the broker egress allowlist (config.Infra.Broker.Allowlist). Empty
	// means deny every destination — the secure default.
	Allowlist []string
	// PackageProxy is the base URL the relay forwards brokered package fetches to
	// (config.Infra.Broker.PackageProxyURL — proxy.golang.org by default). It is consulted
	// only when "package-proxy" is in Allowlist; otherwise package fetch is denied at the
	// broker. Empty disables the egress even if allowlisted. See specs/security.md Control 2.
	PackageProxy string
	// GitRemote is the real git remote the candidate branch is pushed to (T5.7,
	// config.Infra.Git.Remote). Empty keeps the bootstrap local-repo apply into Repo; set
	// routes the push to the remote, authenticated by Minter when present. See
	// specs/security.md Control 3, specs/components/runner.md.
	GitRemote string
	// Minter mints the per-task, short-lived, branch-scoped git push credential and revokes
	// it after the push (T5.7). Consulted only when GitRemote is set; nil there means an
	// unauthenticated remote push (a file:// remote — the dev shape). The runner holds the
	// minter and the token; the sandbox never does. See specs/security.md Control 3.
	Minter secret.Minter
	// AckWait is the JetStream lease: a work message is acked only after harvest, so a
	// runner that dies mid-task lets JetStream redeliver after AckWait. It must exceed
	// the longest an invocation can take; it defaults to Limits.Wall when unset.
	AckWait time.Duration
	// Logger receives lifecycle logs. Defaults to a discard logger.
	Logger *slog.Logger
	// Telemetry receives the invocation/boot trace spans and the per-invocation throughput
	// + duration metrics (specs/observability.md). A nil Provider defaults to telemetry.Noop
	// so instrumentation runs unconditionally with zero overhead when export is off.
	Telemetry *telemetry.Provider
}

// Runner is the per-host daemon: it pulls ready work for its roles, provisions a
// sandbox per work item, stands up the broker the sandbox talks to, runs the agent,
// publishes the Result, reaps the sandbox, and acks the work message only after that
// harvest (ack = the lease). See specs/components/runner.md.
type Runner struct {
	opts     Options
	backend  sandbox.Backend
	resolver AdapterResolver
	pub      Publisher
	invoker  Invoker
	store    artifact.Store
	js       jetstream.JetStream
	log      *slog.Logger
	tel      *telemetry.Provider
}

// New builds a Runner from its options and injected collaborators: the sandbox
// backend (T1.6), the model AdapterResolver and event Publisher the per-invocation
// broker relay is built from (T1.12), the agent Invoker (T1.13), the artifact Store the
// invocation's prompt + transcript are harvested into (T1.18/T1.20), and the JetStream
// handle used to pull work and publish results. It validates the options up front
// (fail loud, consistent with config validation being a startup gate).
func New(opts Options, backend sandbox.Backend, resolver AdapterResolver, pub Publisher, invoker Invoker, store artifact.Store, js jetstream.JetStream) (*Runner, error) {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if len(opts.Roles) == 0 {
		add("at least one role is required")
	}
	for _, r := range opts.Roles {
		if r == "" {
			add("role is empty")
		}
	}
	if opts.Repo == "" {
		add("repo is required (the source the worktree is seeded from)")
	}
	if opts.SocketDir == "" {
		add("socket dir is required")
	}
	if backend == nil {
		add("sandbox backend is required")
	}
	if resolver == nil {
		add("adapter resolver is required")
	}
	if pub == nil {
		add("event publisher is required")
	}
	if invoker == nil {
		add("invoker is required")
	}
	if store == nil {
		add("artifact store is required")
	}
	if js == nil {
		add("jetstream handle is required")
	}
	if opts.AckWait <= 0 {
		if opts.Limits.Wall.Duration() > 0 {
			opts.AckWait = opts.Limits.Wall.Duration()
		} else {
			add("ack wait must be positive (set Options.AckWait or Limits.Wall)")
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("runner: invalid options:\n  - %s", strings.Join(problems, "\n  - "))
	}

	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	tel := opts.Telemetry
	if tel == nil {
		tel = telemetry.Noop()
	}
	return &Runner{opts: opts, backend: backend, resolver: resolver, pub: pub, invoker: invoker, store: store, js: js, log: log, tel: tel}, nil
}

// Run binds a pull consumer per role and serves each until ctx is canceled. It
// returns the first error a role loop reports (other than a clean ctx-cancel
// shutdown). It blocks until every role loop has stopped.
func (r *Runner) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	errs := make(chan error, len(r.opts.Roles))
	for _, role := range r.opts.Roles {
		cons, err := messaging.EnsureWorkConsumer(ctx, r.js, role, r.opts.AckWait)
		if err != nil {
			return err
		}
		wg.Add(1)
		go func(role string, cons jetstream.Consumer) {
			defer wg.Done()
			errs <- r.serveRole(ctx, role, cons)
		}(role, cons)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// serveRole pulls and handles work for one role until ctx is canceled. Canceling ctx stops
// the iterator, which unblocks Next with a closed-iterator error treated as a clean shutdown.
//
// MaxConcurrency (>=1) sets how many same-role invocations run at once: at 1 it is the original
// serial loop; above 1 it dispatches each message to a bounded worker pool so a wide
// decomposition's sibling children fan out instead of serializing on the slowest stage. The
// pull is gated on a free worker slot, so the runner never holds more *in-flight* work than it
// can start — the one shared iterator and the semaphore bound concurrency, and a single queue
// feeding N workers keeps the distribution fair without any per-worker prefetch tuning.
func (r *Runner) serveRole(ctx context.Context, role string, cons jetstream.Consumer) error {
	workers := r.opts.MaxConcurrency
	if workers < 1 {
		workers = 1
	}
	iter, err := cons.Messages()
	if err != nil {
		return fmt.Errorf("runner: open messages iterator for role %q: %w", role, err)
	}
	go func() {
		<-ctx.Done()
		iter.Stop()
	}()

	if workers == 1 {
		for {
			msg, err := iter.Next()
			if err != nil {
				if ctx.Err() != nil || errors.Is(err, jetstream.ErrMsgIteratorClosed) {
					return nil //nolint:nilerr // ctx cancel stopped the iterator; this Next error is a clean shutdown, not a failure
				}
				return fmt.Errorf("runner: pull work for role %q: %w", role, err)
			}
			r.handle(ctx, role, msg)
		}
	}

	// Concurrent path: acquire a worker slot, then pull — so we only fetch work we can start
	// immediately — and run each handle in its own goroutine. On shutdown drain the in-flight
	// invocations before returning so their sandboxes reap cleanly.
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return nil
		}
		msg, err := iter.Next()
		if err != nil {
			<-sem
			wg.Wait()
			if ctx.Err() != nil || errors.Is(err, jetstream.ErrMsgIteratorClosed) {
				return nil //nolint:nilerr // ctx cancel stopped the iterator; this Next error is a clean shutdown, not a failure
			}
			return fmt.Errorf("runner: pull work for role %q: %w", role, err)
		}
		wg.Add(1)
		go func(m jetstream.Msg) {
			defer wg.Done()
			defer func() { <-sem }()
			r.handle(ctx, role, m)
		}(msg)
	}
}

// handle processes one work message. Ack/Nak/Term encode the lease semantics:
//   - a message whose body is not a decodable Brief is poison; Term it so JetStream
//     does not redeliver it forever (a redelivery cannot fix bad bytes).
//   - a provisioning/invocation/publish failure is transient; Nak it so the
//     assignment is redelivered to another runner — this is the lease at work.
//   - only after the Result is harvested (published) is the message Acked.
func (r *Runner) handle(ctx context.Context, role string, msg jetstream.Msg) {
	var brief core.Brief
	if err := json.Unmarshal(msg.Data(), &brief); err != nil {
		r.log.ErrorContext(ctx, "runner: undecodable work message, terminating", "role", role, "err", err)
		_ = msg.TermWithReason("undecodable brief")
		return
	}

	res, err := r.invoke(ctx, brief)
	if err != nil {
		r.log.ErrorContext(ctx, "runner: invocation failed, redelivering", "role", role, "issue", brief.Issue.ID, "err", err)
		_ = msg.Nak()
		return
	}

	// Stamp the issue correlation from the trusted dispatch, not from agent self-report:
	// the orchestrator must be able to map this Result back to its issue (especially a
	// failed/escalated one with no candidate branch) without trusting sandboxed code.
	res.IssueID = brief.Issue.ID

	if err := r.publishResult(ctx, role, res); err != nil {
		r.log.ErrorContext(ctx, "runner: publish result failed, redelivering", "role", role, "issue", brief.Issue.ID, "err", err)
		_ = msg.Nak()
		return
	}

	if err := msg.Ack(); err != nil {
		r.log.ErrorContext(ctx, "runner: ack failed", "role", role, "issue", brief.Issue.ID, "err", err)
		return
	}
	r.log.InfoContext(ctx, "runner: invocation harvested", "role", role, "issue", brief.Issue.ID, "status", res.Status)
}

// invoke runs one agent end to end: stand up the broker socket the sandbox will talk
// to, provision the sandbox (which seeds the worktree at the Brief's base ref), run
// the agent against it, and reap the sandbox unconditionally afterward.
func (r *Runner) invoke(ctx context.Context, brief core.Brief) (core.Result, error) {
	invID, err := r.invocationID()
	if err != nil {
		return core.Result{}, err
	}

	// An invocation is one trace (specs/observability.md): this is its root span. The
	// brokered llm-turn / tool-call spans the relay opens parent off it in-process (the
	// agent loop is co-located with the runner), and the boot span below covers sandbox
	// provisioning beneath it. ctx now carries the span, so everything downstream inherits it.
	ctx, span := r.tel.Tracer().Start(ctx, telemetry.SpanInvocation, trace.WithAttributes(
		attribute.String(telemetry.AttrComponent, telemetry.ComponentRunner),
		attribute.String(telemetry.AttrInvocationID, invID),
		attribute.String(telemetry.AttrIssueID, brief.Issue.ID),
		attribute.String(telemetry.AttrEpicID, brief.Issue.EpicID),
		attribute.String(telemetry.AttrIssueRole, brief.Issue.Role),
		attribute.String(telemetry.AttrSoul, brief.Soul.Name),
		attribute.String(telemetry.AttrModel, brief.Soul.Model),
		attribute.Int(telemetry.AttrAttempt, brief.Issue.Attempt),
		attribute.String(telemetry.AttrBase, brief.Base),
	))
	defer span.End()

	// One per-invocation logger, enriched with the invocation's identity using the
	// telemetry schema's join-column keys (never inline strings). Every record it writes
	// lands in the OTLP logs backend (T5.13) carrying the same issue/epic/soul/role/model/
	// invocation/attempt columns as the span on its trace and the metrics it feeds, so all
	// three signals correlate in the backend (specs/observability.md "Correlation: one
	// schema across all three signals"). It is the trusted-side runner's voice for this
	// invocation — the sandboxed agent's own output is span/artifact evidence, not a log.
	ilog := r.log.With(
		slog.String(telemetry.AttrInvocationID, invID),
		slog.String(telemetry.AttrIssueID, brief.Issue.ID),
		slog.String(telemetry.AttrEpicID, brief.Issue.EpicID),
		slog.String(telemetry.AttrSoul, brief.Soul.Name),
		slog.String(telemetry.AttrIssueRole, brief.Issue.Role),
		slog.String(telemetry.AttrModel, brief.Soul.Model),
		slog.Int(telemetry.AttrAttempt, brief.Issue.Attempt),
	)

	// Resolve the provider adapter for this invocation's soul up front: the runner
	// holds the key and the adapter, so the agent's brokered model calls are
	// provider-unaware. config.Validate already guarantees the model resolves; this is
	// defense-in-depth, and failing here Naks the work rather than crashing mid-invoke.
	adapter, err := r.resolver.Adapter(brief.Soul.Model)
	if err != nil {
		return core.Result{}, fmt.Errorf("runner: resolve model %q for soul %q: %w", brief.Soul.Model, brief.Soul.Name, err)
	}

	// Resolve the SECOND pinned model when the trusted dispatch attached an explorer soul: the
	// explore tool's cheap helper (specs/models.md "Helper souls", T12.2). Failing to resolve it
	// is NOT fatal — explore is additive and never load-bearing, so a bad explorer model disables
	// the helper loop (an explorer-tagged call then fails closed) rather than sinking the whole
	// invocation. config.Validate already guarantees the model resolves; this is defense-in-depth.
	var exploreAdapter model.Adapter
	var exploreModel string
	if brief.Explorer != nil {
		if ea, eerr := r.resolver.Adapter(brief.Explorer.Model); eerr != nil {
			ilog.WarnContext(ctx, "runner: resolve explorer model; explore disabled for this invocation",
				"explorer_soul", brief.Explorer.Name, "model", brief.Explorer.Model, "err", eerr)
		} else {
			exploreAdapter = ea
			exploreModel = brief.Explorer.Model
		}
	}

	sockPath := filepath.Join(r.opts.SocketDir, "broker-"+invID+".sock")
	ln, err := broker.Listen("unix", sockPath)
	if err != nil {
		return core.Result{}, fmt.Errorf("runner: listen broker socket: %w", err)
	}

	image := ""
	if r.opts.ResolveImage != nil {
		image = r.opts.ResolveImage(brief.Soul.Sandbox)
	}
	spec := sandbox.Spec{
		Profile:   brief.Soul.Sandbox,
		Image:     image,
		Workspace: sandbox.Workspace{Repo: r.opts.Repo, BaseRef: brief.Base},
		Limits:    r.opts.Limits,
		Broker:    sandbox.Endpoint{Network: "unix", Address: sockPath},
	}
	bootCtx, bootSpan := r.tel.Tracer().Start(ctx, telemetry.SpanBoot, trace.WithAttributes(
		attribute.String(telemetry.AttrComponent, telemetry.ComponentRunner),
		attribute.String(telemetry.AttrSandboxProfile, spec.Profile),
		attribute.String(telemetry.AttrSandboxImage, image),
	))
	sb, err := r.backend.Provision(bootCtx, spec)
	if err != nil {
		bootSpan.End()
		_ = ln.Close() // unlinks the unix socket we just bound
		return core.Result{}, fmt.Errorf("runner: provision sandbox: %w", err)
	}
	bootSpan.SetAttributes(attribute.String(telemetry.AttrSandboxID, sb.ID()))
	bootSpan.End()
	defer func() {
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), teardownTimeout)
		defer cancel()
		if err := sb.Teardown(tctx); err != nil {
			ilog.ErrorContext(ctx, "runner: teardown sandbox", "id", sb.ID(), "err", err)
		}
	}()

	// Build the per-invocation broker relay now that the sandbox exists (it needs the
	// live handle to extract the candidate branch on push) and serve it on the socket.
	// The relay is the audited chokepoint: model calls go through the resolved adapter,
	// events to this invocation's subject, git push only onto this task's branch.
	rel := newRelay(adapter, r.pub, sb, relayConfig{
		eventSubject:   messaging.AgentEventsSubject(invID),
		issueID:        brief.Issue.ID,
		role:           brief.Issue.Role,
		repo:           r.opts.Repo,
		allowedBranch:  core.CandidateBranch(brief.Issue.ID),
		remote:         r.opts.GitRemote,
		minter:         r.opts.Minter,
		packageProxy:   r.opts.PackageProxy,
		log:            ilog,
		tel:            r.tel,
		model:          brief.Soul.Model,
		exploreAdapter: exploreAdapter,
		exploreModel:   exploreModel,
		exploreBudget:  brief.ExploreBudget,
		// parentCtx carries the invocation span so the relay's per-turn llm-turn / tool-call
		// spans parent off it. The broker serves the relay on a separate connection-scoped
		// context, so without this the brokered spans would be orphan roots, not children.
		parentCtx: ctx,
	})
	srv := broker.NewServer(rel, broker.WithAllowlist(r.opts.Allowlist))
	brokerCtx, stopBroker := context.WithCancel(ctx)
	defer stopBroker() // closes ln via Serve's ctx-cancel path
	go func() {
		if err := srv.Serve(brokerCtx, ln); err != nil {
			ilog.ErrorContext(ctx, "runner: broker serve", "err", err)
		}
	}()

	ilog.InfoContext(ctx, "runner: provisioned sandbox", "id", sb.ID(), "profile", spec.Profile, "base", brief.Base)
	invStart := time.Now()
	res, invErr := r.invoker.Invoke(ctx, sb, brief, spec.Broker)
	elapsed := time.Since(invStart)

	// Record the invocation's throughput + duration whatever the disposition — the counter
	// measures real agent work, including failed/retried attempts (specs/observability.md).
	// An invErr is an infra/agent-loop failure with no clean Result status, recorded under a
	// distinct "error" status so it is visible but never mistaken for a graded outcome.
	status := string(res.Status)
	if invErr != nil {
		status = "error"
	}
	r.tel.RecordInvocation(ctx, brief.Issue.Role, status, elapsed)
	span.SetAttributes(attribute.String(telemetry.AttrResultStatus, status))

	u := rel.Usage()
	ilog.InfoContext(ctx, "runner: invocation usage",
		"input_tokens", u.InputTokens, "output_tokens", u.OutputTokens,
		"cache_read_tokens", u.CacheReadTokens, "cache_creation_tokens", u.CacheCreationTokens)

	// Harvest the invocation's evidence into the artifact store while the relay still
	// holds it. Only on a clean invocation: a failed/redelivered one (invErr != nil) has
	// its Result discarded and is retried, so there is no envelope to attach evidence to.
	if invErr == nil {
		// Stamp the relay's token tally onto the envelope so the orchestrator can price it
		// (per-model cost table) and enforce the cumulative per-issue budget across the
		// on_failure loop — the budget half of the termination guarantee (see
		// specs/workflow.md). Taken from the relay, the trusted egress chokepoint, so it
		// reflects the calls actually relayed, never the untrusted agent's self-report.
		res.Usage = core.Usage{
			InputTokens:         u.InputTokens,
			OutputTokens:        u.OutputTokens,
			CacheCreationTokens: u.CacheCreationTokens,
			CacheReadTokens:     u.CacheReadTokens,
		}
		// Stamp the measured wall-clock too (trusted side, like Usage): the orchestrator
		// threads it onto the issue's cumulative SpentWall and enforces the cumulative wall
		// budget across the on_failure loop (see specs/workflow.md).
		res.Elapsed = elapsed
		// Stamp the pinned explorer model when the explore sub-loop actually ran, so the merge
		// provenance trailer records the tier the exploration ran under (specs/models.md "Helper
		// souls"). Taken from the relay — the trusted chokepoint that pinned it — not the agent.
		// Unset otherwise, keeping the trailer backward-compatible for the no-explore common case.
		if em, ok := rel.ExploreModel(); ok {
			res.ExploreModel = em
		}
		r.harvest(ctx, ilog, rel, &res)
	}
	return res, invErr
}

// harvest writes the invocation's prompt and transcript to the content-addressed
// artifact store and stamps the references onto the Result's Evidence: the prompt's
// hash becomes Prompt-SHA (the provenance trailer cites it — see specs/security.md), and
// the transcript is recorded as an artifact. The runner harvests from the relay (the
// trusted egress chokepoint), so the evidence reflects the calls actually relayed, never
// the untrusted agent's self-report.
//
// A harvest failure degrades provenance but does not fail the invocation: a good
// candidate must not be thrown away (and re-run at cost) because a store write hiccuped.
// It is logged loudly, and the orchestrator merges with whatever evidence is present.
func (r *Runner) harvest(ctx context.Context, log *slog.Logger, rel *relay, res *core.Result) {
	if prompt, ok := rel.Prompt(); ok {
		ref, err := r.store.Put(ctx, core.ArtifactKindPrompt, bytes.NewReader(prompt))
		if err != nil {
			log.ErrorContext(ctx, "runner: harvest prompt", "err", err)
		} else {
			res.Evidence.PromptSHA = ref.Hash
			res.Evidence.Artifacts = append(res.Evidence.Artifacts, ref)
		}
	}
	if transcript, ok := rel.Transcript(); ok {
		ref, err := r.store.Put(ctx, core.ArtifactKindTranscript, bytes.NewReader(transcript))
		if err != nil {
			log.ErrorContext(ctx, "runner: harvest transcript", "err", err)
		} else {
			res.Evidence.Artifacts = append(res.Evidence.Artifacts, ref)
		}
	}
	// The explore sub-loop's conversation is harvested as its OWN content-addressed artifact,
	// alongside — never merged into — the main transcript, so the exploration is auditable
	// evidence in its own right (specs/components/agent.md rule 5). Absent on the common
	// no-explore invocation; a store failure degrades provenance (no Explore-Transcript hash)
	// but never fails the candidate, exactly like the main transcript above.
	if exploreTranscript, ok := rel.ExploreTranscript(); ok {
		ref, err := r.store.Put(ctx, core.ArtifactKindExploreTranscript, bytes.NewReader(exploreTranscript))
		if err != nil {
			log.ErrorContext(ctx, "runner: harvest explore transcript", "err", err)
		} else {
			res.Evidence.Artifacts = append(res.Evidence.Artifacts, ref)
		}
	}
	// The test↔spec traceability map (author-tests only) is the one piece of evidence the
	// agent itself produces rather than the relay capturing it: it arrives structured on
	// the Result. Persist it like the rest, then clear the structured form so the bulky
	// map travels by hash, not inline, on the result envelope (large evidence is always
	// referenced, never carried — see specs/components/artifact-store.md). On a store
	// failure the structured map is kept so the orchestrator at least logs it; the
	// provenance trailer simply carries no Traceability hash (degraded, self-describing).
	if len(res.Trace) > 0 {
		ref, err := r.store.Put(ctx, core.ArtifactKindTraceabilityMap, bytes.NewReader(formatTraceabilityMap(res.Trace)))
		if err != nil {
			log.ErrorContext(ctx, "runner: harvest traceability map", "err", err)
		} else {
			res.Evidence.Artifacts = append(res.Evidence.Artifacts, ref)
			res.Trace = nil
		}
	}
	// The transformation log (T6.3) records the MECHANISM of every semantic write — semantic
	// vs the degraded text floor — so a text-fallback rename can be weighed more suspiciously
	// than a server-precise one. Persisted as JSON (the canonical []core.TransformRecord), the
	// same content-addressed discipline the gate-verdict record uses, so the verification view
	// can read it back structurally and render each transformation with its mechanism. Harvested
	// by hash, the structured form cleared on success; a marshal or store failure keeps it inline
	// so the orchestrator still sees it.
	if len(res.Transforms) > 0 {
		if data, err := json.Marshal(res.Transforms); err != nil {
			log.ErrorContext(ctx, "runner: marshal transform log", "err", err)
		} else if ref, perr := r.store.Put(ctx, core.ArtifactKindTransformLog, bytes.NewReader(data)); perr != nil {
			log.ErrorContext(ctx, "runner: harvest transform log", "err", perr)
		} else {
			res.Evidence.Artifacts = append(res.Evidence.Artifacts, ref)
			res.Transforms = nil
		}
	}
}

// formatTraceabilityMap renders the test↔spec traceability map as a stable, human-readable
// document, one block per test in the order the author emitted them. Determinism matters:
// identical entries content-address to the same hash, so the same map is stored once and
// the provenance citation is reproducible. This is the document the control room's issue
// detail view renders and the only window a human has into how the test author read the
// pure-prose spec (see specs/verification.md).
func formatTraceabilityMap(entries []core.TraceEntry) []byte {
	var b strings.Builder
	b.WriteString("# Test ↔ spec traceability map\n")
	for _, e := range entries {
		b.WriteString("\ntest: ")
		b.WriteString(e.Test)
		b.WriteByte('\n')
		if e.Spec != "" {
			b.WriteString("spec: ")
			b.WriteString(e.Spec)
			b.WriteByte('\n')
		}
		b.WriteString("heading: ")
		b.WriteString(e.Heading)
		b.WriteByte('\n')
		b.WriteString("sentence: ")
		b.WriteString(e.Sentence)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// publishResult sends the harvested Result back on the role's result subject for the
// orchestrator to consume and validate. Large evidence is referenced by hash into
// the artifact store (plan T1.18); only the envelope travels here.
func (r *Runner) publishResult(ctx context.Context, role string, res core.Result) error {
	data, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("runner: marshal result: %w", err)
	}
	if _, err := r.js.Publish(ctx, messaging.ResultSubject(role), data); err != nil {
		return fmt.Errorf("runner: publish result: %w", err)
	}
	return nil
}

// invocationID returns a fresh, unique per-invocation id. It names the broker socket
// (avoiding collisions between concurrent invocations sharing the socket dir) and the
// agent's NATS event subject (factory.agent.<id>.events), and tags the invocation's
// logs. Random hex keeps it safe as a NATS subject token (no '.').
func (r *Runner) invocationID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("runner: generate invocation id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
