package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/Loxstomper/harness/internal/broker"
	"github.com/Loxstomper/harness/internal/model"
	"github.com/Loxstomper/harness/internal/sandbox"
	"github.com/Loxstomper/harness/internal/telemetry"
)

// relay is the runner's concrete broker.Handler for one invocation: it performs the
// brokered calls the deny-by-default server lets through. It is the load-bearing
// security boundary made real — the sandbox has no network and no credentials, so the
// only way an agent reaches the model API, git, or NATS is through this type, which
// holds the provider adapter + key, the source repo, and the NATS connection (see
// specs/components/runner.md, specs/security.md).
//
// It is built per invocation because every brokered call is invocation-scoped: the
// adapter is resolved from this Brief's soul.Model, events go to this invocation's
// agent-event subject, and git push is constrained to this task's branch. Building one
// shared handler could not bind those.
type relay struct {
	adapter model.Adapter   // resolved for this invocation's soul.Model; the agent is provider-unaware
	pub     Publisher       // core-NATS connection for the live event/token feed
	sb      sandbox.Sandbox // the live sandbox, used to extract the candidate branch on push
	log     *slog.Logger

	tel       *telemetry.Provider // per-turn llm-turn / tool-call spans + the llm-turn metric
	model     string              // soul.Model, for AttrModel + RecordLLMTurn (model.Request omits it)
	parentCtx context.Context     // carries the invocation span so brokered spans parent off it

	eventSubject  string // harness.agent.<id>.events — where token/progress events fan out
	repo          string // source repository the candidate branch is pushed into
	allowedBranch string // the ONLY branch this invocation may push (task branch only)

	// pushBundle applies a git bundle (the candidate branch, extracted from the
	// sandbox) into the source repo on the runner host and returns the pushed head.
	// A seam so the relay's orchestration is unit-testable without real git; the
	// default is pushBundleToRepo.
	pushBundle func(ctx context.Context, repo, branch string, bundle []byte) (string, error)

	mu       sync.Mutex
	usage    model.Usage      // tallied across every completion this invocation (budget input, plan T1.16)
	firstReq *model.Request   // the prompt: the first request this invocation ran with (provenance, plan T1.20)
	turns    []transcriptTurn // every completion this invocation made, in order (the transcript)
}

// transcriptTurn is one model exchange the relay recorded: the canonical request the
// agent sent and the canonical response the provider returned. The ordered slice of
// these is the invocation transcript the runner harvests to the artifact store as
// provenance evidence (see specs/security.md, specs/observability.md). Because the
// relay is the trusted egress chokepoint, this transcript is recorded by the runner
// from the calls it actually relayed — never self-reported by the untrusted agent.
type transcriptTurn struct {
	Request  model.Request  `json:"request"`
	Response model.Response `json:"response"`
}

var _ broker.Handler = (*relay)(nil)

// relayConfig carries the per-invocation bindings newRelay needs beyond its
// collaborators, grouped so the constructor call stays legible.
type relayConfig struct {
	eventSubject  string
	repo          string
	allowedBranch string
	log           *slog.Logger
	tel           *telemetry.Provider
	model         string
	parentCtx     context.Context
}

func newRelay(adapter model.Adapter, pub Publisher, sb sandbox.Sandbox, cfg relayConfig) *relay {
	tel := cfg.tel
	if tel == nil {
		tel = telemetry.Noop()
	}
	return &relay{
		adapter:       adapter,
		pub:           pub,
		sb:            sb,
		log:           cfg.log,
		tel:           tel,
		model:         cfg.model,
		parentCtx:     cfg.parentCtx,
		eventSubject:  cfg.eventSubject,
		repo:          cfg.repo,
		allowedBranch: cfg.allowedBranch,
		pushBundle:    pushBundleToRepo,
	}
}

// spanParent returns ctx with its parent span replaced by the invocation span (captured at
// relay construction). The relay is served on a connection-scoped context that carries no
// span, so without re-parenting the brokered llm-turn / tool-call spans would be orphan
// roots. ctx's own deadline/cancellation is preserved — only the trace parentage changes.
func (r *relay) spanParent(ctx context.Context) context.Context {
	if r.parentCtx == nil {
		return ctx
	}
	return trace.ContextWithSpan(ctx, trace.SpanFromContext(r.parentCtx))
}

// tokenEvent is the live-feed envelope for one streamed assistant text delta. It is
// published best-effort to the agent's event subject so the control room can watch an
// agent think in real time (see specs/observability.md); losing one is harmless.
type tokenEvent struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
}

// Complete relays a canonical model request to the resolved provider adapter, streams
// each text delta out to NATS for the live view, tallies the returned Usage against
// this invocation's running total (the budget input), and returns the canonical
// response. The runner attached the key and adapter; the agent never learns which
// provider answered. Every call is logged — the broker is the audited chokepoint.
func (r *relay) Complete(ctx context.Context, req model.Request) (model.Response, error) {
	onEvent := func(ev model.StreamEvent) {
		if ev.TextDelta == "" {
			return
		}
		r.publishEvent(tokenEvent{Type: "token", Delta: ev.TextDelta})
	}

	// One model turn = one llm-turn span, parented to the invocation and timed around the
	// adapter call (the latency the turn-duration metric records). The span carries the
	// per-turn token breakdown a replay reads; the aggregate counter is RecordLLMTurn below.
	_, span := r.tel.Tracer().Start(r.spanParent(ctx), telemetry.SpanLLMTurn, trace.WithAttributes(
		attribute.String(telemetry.AttrComponent, telemetry.ComponentBroker),
		attribute.String(telemetry.AttrModel, r.model),
	))
	defer span.End()

	start := time.Now()
	resp, err := r.adapter.Complete(ctx, req, onEvent)
	d := time.Since(start)
	if err != nil {
		span.RecordError(err)
		r.log.Error("broker: model completion failed", "err", err)
		return model.Response{}, err
	}

	span.SetAttributes(
		attribute.String(telemetry.AttrStopReason, string(resp.Stop)),
		attribute.Int(telemetry.AttrToolCalls, len(resp.ToolCalls)),
		attribute.Int(telemetry.AttrInputTokens, resp.Usage.InputTokens),
		attribute.Int(telemetry.AttrOutputTokens, resp.Usage.OutputTokens),
		attribute.Int(telemetry.AttrCacheReadTokens, resp.Usage.CacheReadTokens),
		attribute.Int(telemetry.AttrCacheWriteTokens, resp.Usage.CacheCreationTokens),
	)
	r.tel.RecordLLMTurn(ctx, r.model,
		resp.Usage.InputTokens, resp.Usage.OutputTokens,
		resp.Usage.CacheReadTokens, resp.Usage.CacheCreationTokens, d)

	r.addUsage(resp.Usage)
	r.record(req, resp)
	r.log.Info("broker: model completion",
		"stop", resp.Stop,
		"input_tokens", resp.Usage.InputTokens,
		"output_tokens", resp.Usage.OutputTokens,
		"cache_read_tokens", resp.Usage.CacheReadTokens,
		"cache_creation_tokens", resp.Usage.CacheCreationTokens,
		"tool_calls", len(resp.ToolCalls),
	)
	return resp, nil
}

// GitPush lands the candidate branch the agent built inside the sandbox into the source
// repo. The branch must be exactly this invocation's task branch — naming any other
// branch (in particular a protected one like main) is refused, which is how "push only
// the task branch" is enforced without yet minting a scoped token (that token is plan
// Phase 5; here the destination is the local source repo). The commits are extracted
// from the network-less sandbox as a git bundle over Exec stdout — never a bind mount
// or copy-out — preserving the microVM-shaped isolation (see specs/components/sandbox.md).
func (r *relay) GitPush(ctx context.Context, req broker.GitPushRequest) (broker.GitPushResult, error) {
	// The git-push is the one egress tool the broker mediates, so it gets a tool-call span
	// (workspace tools run unbrokered inside the sandbox and are invisible by design). The
	// span opens before the branch guard so a denied push is traced too, not silently dropped.
	_, span := r.tel.Tracer().Start(r.spanParent(ctx), telemetry.SpanToolCall, trace.WithAttributes(
		attribute.String(telemetry.AttrComponent, telemetry.ComponentBroker),
		attribute.String(telemetry.AttrToolName, telemetry.ToolGitPush),
		attribute.String(telemetry.AttrGitBranch, req.Branch),
	))
	defer span.End()

	if req.Branch != r.allowedBranch {
		r.log.Warn("broker: git push refused, not the task branch", "requested", req.Branch, "allowed", r.allowedBranch)
		err := fmt.Errorf("git push denied: branch %q is not this task's branch %q", req.Branch, r.allowedBranch)
		span.RecordError(err)
		return broker.GitPushResult{}, err
	}

	bundle, err := r.extractBundle(ctx, req.Branch)
	if err != nil {
		span.RecordError(err)
		r.log.Error("broker: git push failed extracting branch", "branch", req.Branch, "err", err)
		return broker.GitPushResult{}, err
	}

	commit, err := r.pushBundle(ctx, r.repo, req.Branch, bundle)
	if err != nil {
		span.RecordError(err)
		r.log.Error("broker: git push failed applying branch", "branch", req.Branch, "err", err)
		return broker.GitPushResult{}, err
	}

	span.SetAttributes(attribute.String(telemetry.AttrGitCommit, commit))
	r.log.Info("broker: git push", "branch", req.Branch, "commit", commit)
	return broker.GitPushResult{Commit: commit}, nil
}

// PublishEvent forwards a best-effort agent progress/log event to NATS on this
// invocation's event subject. The agent holds no NATS credentials and does not know
// its own subject — the broker supplies it. Delivery is fire-and-forget.
func (r *relay) PublishEvent(_ context.Context, ev broker.PublishRequest) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("broker: marshal event: %w", err)
	}
	r.log.Info("broker: publish event", "type", ev.Type)
	if err := r.pub.Publish(r.eventSubject, data); err != nil {
		return fmt.Errorf("broker: publish event: %w", err)
	}
	return nil
}

// extractBundle produces a git bundle of the candidate branch from inside the sandbox,
// captured as raw bytes on Exec stdout (binary-safe: ExecResult.Stdout is []byte). This
// is the only route the candidate code takes out of the zero-network sandbox.
func (r *relay) extractBundle(ctx context.Context, branch string) ([]byte, error) {
	res, err := r.sb.Exec(ctx, sandbox.Command{
		Path: "git",
		Args: []string{"bundle", "create", "-", branch},
	})
	if err != nil {
		return nil, fmt.Errorf("git bundle: exec: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("git bundle: exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	if len(res.Stdout) == 0 {
		return nil, fmt.Errorf("git bundle: empty bundle for branch %q", branch)
	}
	return res.Stdout, nil
}

// Usage returns the total token usage tallied across every completion this invocation
// made. The runner logs it and (plan T1.16) the budget enforcer reads it.
func (r *relay) Usage() model.Usage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.usage
}

func (r *relay) addUsage(u model.Usage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.usage.InputTokens += u.InputTokens
	r.usage.OutputTokens += u.OutputTokens
	r.usage.CacheCreationTokens += u.CacheCreationTokens
	r.usage.CacheReadTokens += u.CacheReadTokens
}

// record appends one model exchange to the transcript and, on the first call, captures
// the request as the invocation's prompt. The prompt is the exact input the invocation
// ran with — system persona + the Brief-built opening turn — and its artifact-store hash
// becomes the Prompt-SHA in the merge provenance trailer (see specs/security.md). The
// request is copied so a later mutation of the agent's message slice cannot alter the
// recorded prompt.
func (r *relay) record(req model.Request, resp model.Response) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.firstReq == nil {
		captured := req
		r.firstReq = &captured
	}
	r.turns = append(r.turns, transcriptTurn{Request: req, Response: resp})
}

// Prompt returns the JSON-encoded prompt (the first request) the invocation ran with,
// and false if no model call was made (e.g. an invocation that failed before completing
// a turn) — in which case there is no prompt to harvest.
func (r *relay) Prompt() ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.firstReq == nil {
		return nil, false
	}
	data, err := json.Marshal(r.firstReq)
	if err != nil {
		return nil, false
	}
	return data, true
}

// Transcript returns the JSON-encoded ordered transcript of every model exchange this
// invocation made, and false if none was made.
func (r *relay) Transcript() ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.turns) == 0 {
		return nil, false
	}
	data, err := json.Marshal(r.turns)
	if err != nil {
		return nil, false
	}
	return data, true
}

// publishEvent emits one live-feed event best-effort; a failure is logged at debug and
// swallowed, since the stream is observability, not control flow, and must never stall
// or fail a model call (the StreamHandler runs on the token-draining goroutine).
func (r *relay) publishEvent(ev tokenEvent) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	if err := r.pub.Publish(r.eventSubject, data); err != nil {
		r.log.Debug("broker: drop live event", "err", err)
	}
}

// pushBundleToRepo applies a git bundle (the candidate branch extracted from the
// sandbox) into the source repo on the runner host and returns the pushed branch head.
// It writes the bundle to a temp file, fetches the branch ref out of it into the repo,
// then resolves the head — all host-side git, since the sandbox is network-less and the
// source repo is not reachable from inside it.
func pushBundleToRepo(ctx context.Context, repo, branch string, bundle []byte) (string, error) {
	f, err := os.CreateTemp("", "harness-bundle-*.bundle")
	if err != nil {
		return "", fmt.Errorf("git push: temp bundle: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(bundle); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("git push: write bundle: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("git push: close bundle: %w", err)
	}

	ref := "refs/heads/" + branch
	refspec := "+" + ref + ":" + ref
	if out, err := runGit(ctx, repo, "fetch", f.Name(), refspec); err != nil {
		return "", fmt.Errorf("git push: fetch bundle: %w: %s", err, out)
	}
	out, err := runGit(ctx, repo, "rev-parse", ref)
	if err != nil {
		return "", fmt.Errorf("git push: rev-parse %s: %w: %s", ref, err, out)
	}
	return strings.TrimSpace(out), nil
}

// runGit runs a git subcommand in repo and returns its combined output. Combined output
// (not just stdout) is returned so a failure carries git's stderr diagnostics.
func runGit(ctx context.Context, repo string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
