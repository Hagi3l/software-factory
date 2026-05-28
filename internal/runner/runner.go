package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/Loxstomper/harness/internal/broker"
	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/messaging"
	"github.com/Loxstomper/harness/internal/model"
	"github.com/Loxstomper/harness/internal/sandbox"
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
	// Allowlist is the broker egress allowlist (config.Infra.Broker.Allowlist). Empty
	// means deny every destination — the secure default.
	Allowlist []string
	// AckWait is the JetStream lease: a work message is acked only after harvest, so a
	// runner that dies mid-task lets JetStream redeliver after AckWait. It must exceed
	// the longest an invocation can take; it defaults to Limits.Wall when unset.
	AckWait time.Duration
	// Logger receives lifecycle logs. Defaults to a discard logger.
	Logger *slog.Logger
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
	js       jetstream.JetStream
	log      *slog.Logger
}

// New builds a Runner from its options and injected collaborators: the sandbox
// backend (T1.6), the model AdapterResolver and event Publisher the per-invocation
// broker relay is built from (T1.12), the agent Invoker (T1.13), and the JetStream
// handle used to pull work and publish results. It validates the options up front
// (fail loud, consistent with config validation being a startup gate).
func New(opts Options, backend sandbox.Backend, resolver AdapterResolver, pub Publisher, invoker Invoker, js jetstream.JetStream) (*Runner, error) {
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
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Runner{opts: opts, backend: backend, resolver: resolver, pub: pub, invoker: invoker, js: js, log: log}, nil
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

// serveRole pulls and handles work for one role until ctx is canceled. Canceling ctx
// stops the iterator, which unblocks Next with a closed-iterator error treated as a
// clean shutdown.
func (r *Runner) serveRole(ctx context.Context, role string, cons jetstream.Consumer) error {
	iter, err := cons.Messages()
	if err != nil {
		return fmt.Errorf("runner: open messages iterator for role %q: %w", role, err)
	}
	go func() {
		<-ctx.Done()
		iter.Stop()
	}()
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

// handle processes one work message. Ack/Nak/Term encode the lease semantics:
//   - a message whose body is not a decodable Brief is poison; Term it so JetStream
//     does not redeliver it forever (a redelivery cannot fix bad bytes).
//   - a provisioning/invocation/publish failure is transient; Nak it so the
//     assignment is redelivered to another runner — this is the lease at work.
//   - only after the Result is harvested (published) is the message Acked.
func (r *Runner) handle(ctx context.Context, role string, msg jetstream.Msg) {
	var brief core.Brief
	if err := json.Unmarshal(msg.Data(), &brief); err != nil {
		r.log.Error("runner: undecodable work message, terminating", "role", role, "err", err)
		_ = msg.TermWithReason("undecodable brief")
		return
	}

	res, err := r.invoke(ctx, brief)
	if err != nil {
		r.log.Error("runner: invocation failed, redelivering", "role", role, "issue", brief.Issue.ID, "err", err)
		_ = msg.Nak()
		return
	}

	if err := r.publishResult(ctx, role, res); err != nil {
		r.log.Error("runner: publish result failed, redelivering", "role", role, "issue", brief.Issue.ID, "err", err)
		_ = msg.Nak()
		return
	}

	if err := msg.Ack(); err != nil {
		r.log.Error("runner: ack failed", "role", role, "issue", brief.Issue.ID, "err", err)
		return
	}
	r.log.Info("runner: invocation harvested", "role", role, "issue", brief.Issue.ID, "status", res.Status)
}

// invoke runs one agent end to end: stand up the broker socket the sandbox will talk
// to, provision the sandbox (which seeds the worktree at the Brief's base ref), run
// the agent against it, and reap the sandbox unconditionally afterward.
func (r *Runner) invoke(ctx context.Context, brief core.Brief) (core.Result, error) {
	invID, err := r.invocationID()
	if err != nil {
		return core.Result{}, err
	}

	// Resolve the provider adapter for this invocation's soul up front: the runner
	// holds the key and the adapter, so the agent's brokered model calls are
	// provider-unaware. config.Validate already guarantees the model resolves; this is
	// defense-in-depth, and failing here Naks the work rather than crashing mid-invoke.
	adapter, err := r.resolver.Adapter(brief.Soul.Model)
	if err != nil {
		return core.Result{}, fmt.Errorf("runner: resolve model %q for soul %q: %w", brief.Soul.Model, brief.Soul.Name, err)
	}

	sockPath := filepath.Join(r.opts.SocketDir, "broker-"+invID+".sock")
	ln, err := broker.Listen("unix", sockPath)
	if err != nil {
		return core.Result{}, fmt.Errorf("runner: listen broker socket: %w", err)
	}

	spec := sandbox.Spec{
		Profile:   brief.Soul.Sandbox,
		Workspace: sandbox.Workspace{Repo: r.opts.Repo, BaseRef: brief.Base},
		Limits:    r.opts.Limits,
		Broker:    sandbox.Endpoint{Network: "unix", Address: sockPath},
	}
	sb, err := r.backend.Provision(ctx, spec)
	if err != nil {
		_ = ln.Close() // unlinks the unix socket we just bound
		return core.Result{}, fmt.Errorf("runner: provision sandbox: %w", err)
	}
	defer func() {
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), teardownTimeout)
		defer cancel()
		if err := sb.Teardown(tctx); err != nil {
			r.log.Error("runner: teardown sandbox", "id", sb.ID(), "err", err)
		}
	}()

	// Build the per-invocation broker relay now that the sandbox exists (it needs the
	// live handle to extract the candidate branch on push) and serve it on the socket.
	// The relay is the audited chokepoint: model calls go through the resolved adapter,
	// events to this invocation's subject, git push only onto this task's branch.
	rel := newRelay(adapter, r.pub, sb, relayConfig{
		eventSubject:  messaging.AgentEventsSubject(invID),
		repo:          r.opts.Repo,
		allowedBranch: core.CandidateBranch(brief.Issue.ID),
		log:           r.log.With("invocation", invID, "issue", brief.Issue.ID),
	})
	srv := broker.NewServer(rel, broker.WithAllowlist(r.opts.Allowlist))
	brokerCtx, stopBroker := context.WithCancel(ctx)
	defer stopBroker() // closes ln via Serve's ctx-cancel path
	go func() {
		if err := srv.Serve(brokerCtx, ln); err != nil {
			r.log.Error("runner: broker serve", "err", err)
		}
	}()

	r.log.Info("runner: provisioned sandbox", "id", sb.ID(), "issue", brief.Issue.ID, "profile", spec.Profile, "base", brief.Base)
	res, invErr := r.invoker.Invoke(ctx, sb, brief, spec.Broker)
	u := rel.Usage()
	r.log.Info("runner: invocation usage", "issue", brief.Issue.ID,
		"input_tokens", u.InputTokens, "output_tokens", u.OutputTokens,
		"cache_read_tokens", u.CacheReadTokens, "cache_creation_tokens", u.CacheCreationTokens)
	return res, invErr
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
// agent's NATS event subject (harness.agent.<id>.events), and tags the invocation's
// logs. Random hex keeps it safe as a NATS subject token (no '.').
func (r *Runner) invocationID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("runner: generate invocation id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
