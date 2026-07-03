package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/Loxstomper/harness/internal/broker"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/model"
	"github.com/Loxstomper/harness/internal/packageproxy"
	"github.com/Loxstomper/harness/internal/sandbox"
	"github.com/Loxstomper/harness/internal/secret"
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

	// exploreAdapter/exploreModel are the SECOND pinned model in this one sandbox: the explore
	// tool's helper soul (specs/models.md "Helper souls", T12.2). Resolved by the runner from
	// the trusted dispatch (brief.Explorer.Model), never from anything the agent names — an
	// explorer-tagged completion routes here, refusing the agent any path to a stronger tier.
	// Nil when explore is disabled for this invocation; an explorer-tagged call then fails
	// closed (the sub-loop degrades to a partial, the parent reads directly).
	exploreAdapter model.Adapter
	exploreModel   string
	// exploreBudget is the FIXED per-call cap the explorer's own model stream is metered against
	// (policy.explore_budget). Breaching a stream's token cap refuses further calls on it so the
	// sub-loop harvests a partial-budget answer, never failing the parent.
	exploreBudget core.ExploreBudget

	eventSubject  string // harness.agent.<id>.events — where token/progress events fan out
	issueID       string // the issue this invocation is working — stamped on every published event
	role          string // the issue's role/stage — stamped alongside issueID (specs/messaging.md)
	repo          string // source repository the candidate branch is pushed into (local-push fallback)
	allowedBranch string // the ONLY branch this invocation may push (task branch only)

	// remote + minter select the push destination (T5.7). When remote is set, the
	// candidate branch is pushed to that real remote authenticated by a per-task token
	// minter mints (nil minter ⇒ unauthenticated, valid for a file:// remote — the dev
	// shape); when remote is empty, the branch is applied to the local repo (the bootstrap
	// path, pushBundle below). See specs/security.md Control 3, specs/components/runner.md.
	remote string
	minter secret.Minter

	// fetcher performs the brokered package-proxy egress (config.BrokerConfig.PackageProxyURL
	// — proxy.golang.org by default). It is the same host-side fetcher the gate verifier uses
	// (internal/packageproxy), so the producer's fetch and the verifier's re-gating fetch can
	// never drift. Nil when no proxy is configured: a fetch then errors rather than dialing
	// nothing. The runner host has network; the sandbox does not, so this egress happens here,
	// the audited chokepoint.
	fetcher *packageproxy.Fetcher

	// pushBundle applies a git bundle (the candidate branch, extracted from the
	// sandbox) into the source repo on the runner host and returns the pushed head.
	// A seam so the relay's orchestration is unit-testable without real git; the
	// default is pushBundleToRepo. Used only on the local-push path (remote == "").
	pushBundle func(ctx context.Context, repo, branch string, bundle []byte) (string, error)

	// pushRemote pushes the extracted bundle's branch to the configured remote with the
	// minted credential and returns the pushed head. The remote-push counterpart of
	// pushBundle, a seam for the same reason; the default is pushBundleToRemote.
	pushRemote func(ctx context.Context, remote string, cred secret.GitCredential, branch string, bundle []byte) (string, error)

	// sleep waits out one retry backoff, or returns early with ctx's error if the
	// invocation is going away. A seam (default sleepCtx) so the transient-fault
	// retry loop is unit-testable without real waits, like pushBundle above.
	sleep func(ctx context.Context, d time.Duration) error

	mu       sync.Mutex
	usage    model.Usage            // parent-stream tokens tallied across this invocation (budget input, plan T1.16)
	firstReq *model.Request         // the prompt: the first request this invocation ran with (provenance, plan T1.20)
	turns    []model.TranscriptTurn // every completion this invocation made, in order (the transcript)

	// exploreUsage is the explorer streams' tokens combined across every explore call. Usage()
	// sums it into the invocation total so the explorer's spend draws the parent-task ceiling
	// too (specs/configuration.md: "the parent's budget is still the real ceiling — every
	// explorer stream draws against it"). streamTok is per-explore-call (keyed on the wire's
	// Stream id), the input for the per-call sub-budget cap so multiple explore calls each get
	// the full fixed budget.
	exploreUsage model.Usage
	streamTok    map[string]int
	// exploreTurns is every explorer completion in order — the explore transcript, captured
	// SEPARATELY from the parent's turns (never appended to r.turns, never overwriting r.firstReq)
	// so the cheap-tier comprehension is first-class evidence harvested on its own content hash,
	// not folded into the parent prompt/transcript (specs/components/agent.md rule 5, T12.4).
	exploreTurns []model.TranscriptTurn
}

var _ broker.Handler = (*relay)(nil)

// relayConfig carries the per-invocation bindings newRelay needs beyond its
// collaborators, grouped so the constructor call stays legible.
type relayConfig struct {
	eventSubject  string
	issueID       string
	role          string
	repo          string
	allowedBranch string
	remote        string
	minter        secret.Minter
	packageProxy  string
	httpClient    *http.Client
	log           *slog.Logger
	tel           *telemetry.Provider
	model         string
	parentCtx     context.Context
	// exploreAdapter/exploreModel/exploreBudget pin the explore tool's helper model for this
	// invocation (T12.2). Zero/nil when explore is disabled — the relay then refuses any
	// explorer-tagged completion.
	exploreAdapter model.Adapter
	exploreModel   string
	exploreBudget  core.ExploreBudget
}

func newRelay(adapter model.Adapter, pub Publisher, sb sandbox.Sandbox, cfg relayConfig) *relay {
	tel := cfg.tel
	if tel == nil {
		tel = telemetry.Noop()
	}
	// A configured proxy gets a Fetcher (shared with the gate verifier); empty disables the
	// egress, so a fetch errors loudly rather than dialing nothing. cfg.httpClient is a seam
	// so FetchPackage is unit-testable against an httptest server.
	var fetcher *packageproxy.Fetcher
	if cfg.packageProxy != "" {
		fetcher = packageproxy.NewFetcher(cfg.packageProxy, cfg.httpClient)
	}
	return &relay{
		adapter:        adapter,
		sleep:          sleepCtx,
		exploreAdapter: cfg.exploreAdapter,
		exploreModel:   cfg.exploreModel,
		exploreBudget:  cfg.exploreBudget,
		pub:            pub,
		sb:             sb,
		log:            cfg.log,
		tel:            tel,
		model:          cfg.model,
		parentCtx:      cfg.parentCtx,
		eventSubject:  cfg.eventSubject,
		issueID:       cfg.issueID,
		role:          cfg.role,
		repo:          cfg.repo,
		allowedBranch: cfg.allowedBranch,
		remote:        cfg.remote,
		minter:        cfg.minter,
		fetcher:       fetcher,
		pushBundle:    pushBundleToRepo,
		pushRemote:    pushBundleToRemote,
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

// tokenEvent is the live-feed envelope for one agent activity datum, published
// best-effort to the agent's event subject so the control room can watch an agent work
// in real time (see specs/observability.md); losing one is harmless. Type discriminates
// the channel — "token" (assistant text delta), "reasoning" (chain-of-thought delta), or
// "tool" (a tool call the turn requested) — and Delta carries the text the feed shows.
//
// SubContext labels which of the invocation's pinned models produced the datum: empty for
// the parent soul (the default), "explorer" for the explore tool's nested sub-loop. It lets
// the control room NEST the explorer's reasoning/tokens under the parent invocation rather
// than mistaking them for parent turns (specs/messaging.md, specs/observability.md). Emitted
// only when non-empty (omitempty), so a parent event stays byte-identical to before T12.4.
type tokenEvent struct {
	Type       string `json:"type"`
	Delta      string `json:"delta"`
	SubContext string `json:"subContext,omitempty"`
}

// toolCallSummary renders a tool call as a compact one-line feed label: the tool name
// and its most salient argument (a path/file/command/branch when present, else the
// compacted args). It is display-only — best-effort, never parsed back — so an odd or
// missing argument degrades to just the name rather than erroring.
func toolCallSummary(tc model.ToolCall) string {
	name := tc.Name
	if name == "" {
		name = "tool"
	}
	var args map[string]any
	if len(tc.Args) == 0 || json.Unmarshal(tc.Args, &args) != nil || len(args) == 0 {
		return name
	}
	for _, k := range []string{"path", "file", "filename", "command", "cmd", "branch", "query"} {
		if v, ok := args[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return name + " " + s
			}
		}
	}
	if compact, err := json.Marshal(args); err == nil {
		return name + " " + string(compact)
	}
	return name
}

// Complete relays a model completion to the pinned adapter for its sub-context. The parent
// stream runs on the invocation's soul model; the explorer stream (the explore tool's nested
// read-only sub-loop) runs on the runner-pinned explorer model and is metered against the fixed
// explore sub-budget. The agent names only the sub-context tag — never a model — so it cannot
// escape its tier even with two models live in one sandbox (specs/models.md "Helper souls",
// specs/messaging.md, T12.2). Routing here, in the trusted runner, is what enforces that.
// Transient-fault retry at the relay (specs/models.md "Transient provider faults are
// absorbed at the relay"): a rate limit, a provider 5xx, or a mid-stream reset would
// otherwise surface as a fatal invocation error — the runner Naks, the whole invocation
// re-runs, and the sandbox plus every token already spent is discarded over a fault a
// one-second retry would have absorbed. The relay retries a *transient* completion
// fault (classified by the adapter into model.Fault — the relay itself stays
// provider-unaware) a bounded number of times with exponential backoff, re-issuing a
// fresh request each time, never resuming a stream. The attempt bound is the halting
// guarantee here — the invocation ctx carries no deadline — and the sandbox wall clock
// keeps running throughout, so a real provider outage still exhausts the budget and
// dead-letters rather than looping. The provider SDKs additionally retry the *initial*
// connect internally (their default policy); that layering is deliberate — stripping
// it would regress the trusted wizard path, which holds an adapter directly and gets
// no relay retry.
const (
	// completionMaxAttempts bounds one logical completion: the first attempt plus up
	// to N-1 retries of a transient fault.
	completionMaxAttempts = 4
	// completionBackoffBase is the first retry's delay, doubling per attempt
	// (1s, 2s, 4s) — long enough to ride out a rate-limit window, short enough not
	// to forfeit the provider's ~5-min prompt-cache TTL (specs/models.md).
	completionBackoffBase = time.Second
)

// sleepCtx waits d or until ctx is done, whichever comes first. The default for the
// relay's sleep seam.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// completeWithRetry runs one logical completion against the given adapter, absorbing
// transient provider faults with bounded backoff (see the constants above). tallyFailed
// books a FAILED attempt's billed usage (a mid-stream fault lands after tokens were
// counted) into the caller's meter — the parent tally or the explore stream meter — so
// every attempt draws the budget and termination holds across retries. A retried
// attempt re-invokes onEvent from the top, so the live feed may show a partial turn's
// deltas twice; the transcript is unaffected (only the successful response is recorded).
func (r *relay) completeWithRetry(ctx context.Context, adapter model.Adapter, req model.Request, onEvent model.StreamHandler, tallyFailed func(model.Usage)) (model.Response, error) {
	for attempt := 1; ; attempt++ {
		resp, err := adapter.Complete(ctx, req, onEvent)
		if err == nil {
			return resp, nil
		}
		if u, ok := model.FaultUsage(err); ok && u != (model.Usage{}) {
			tallyFailed(u)
		}
		if !model.Transient(err) || attempt >= completionMaxAttempts {
			return model.Response{}, err
		}
		delay := completionBackoffBase << (attempt - 1)
		r.log.WarnContext(ctx, "broker: transient model fault, retrying",
			"attempt", attempt, "max_attempts", completionMaxAttempts, "delay", delay.String(), "err", err)
		if serr := r.sleep(ctx, delay); serr != nil {
			// The invocation is going away (shutdown); surface the provider fault —
			// it is what the caller can act on — not the interrupted sleep.
			return model.Response{}, err
		}
	}
}

func (r *relay) Complete(ctx context.Context, p broker.CompletionParams) (model.Response, error) {
	if p.SubContext == broker.SubContextExplorer {
		return r.completeExplore(ctx, p.Stream, p.Request)
	}
	return r.completeParent(ctx, p.Request)
}

// completeExplore relays one explorer-tagged completion to the pinned explorer adapter, metered
// against the fixed per-call sub-budget scoped by stream. A breach refuses the call with a typed
// CodeSubBudgetExhausted error the sub-loop maps to a partial-budget answer — it never fails the
// parent (explore is additive, specs/components/agent.md rule 3). Explorer tokens also feed
// Usage() so they draw the parent-task ceiling. This is the trusted, authoritative meter: the
// in-sandbox sub-loop self-caps too, but the agent is untrusted, so this bound is the real one.
func (r *relay) completeExplore(ctx context.Context, stream string, req model.Request) (model.Response, error) {
	if r.exploreAdapter == nil {
		// Explore was not pinned for this invocation (no explorer soul / adapter resolved). The
		// tool should not have been offered, but fail closed rather than silently answering on
		// the parent's frontier model — that would let the agent reach a stronger tier.
		return model.Response{}, &broker.Error{Code: broker.CodeHandlerError, Message: "explore is not enabled for this invocation"}
	}
	if limit := r.exploreBudget.Tokens; limit > 0 {
		if used := r.streamTokens(stream); used >= limit {
			r.log.WarnContext(ctx, "broker: explore sub-budget exhausted; refusing further explorer calls on stream",
				"stream", stream, "used", used, "cap", limit)
			return model.Response{}, &broker.Error{
				Code:    broker.CodeSubBudgetExhausted,
				Message: fmt.Sprintf("explore sub-budget exhausted: %d tokens on stream %q (cap %d)", used, stream, limit),
			}
		}
	}

	// Every live datum from this stream is tagged with the explorer sub-context so the control
	// room nests it under the parent invocation instead of showing it as a parent turn — the
	// wire counterpart of the AttrSubContext span attribute below (specs/observability.md).
	const sub = string(broker.SubContextExplorer)
	onEvent := func(ev model.StreamEvent) {
		if ev.TextDelta != "" {
			r.publishEvent(tokenEvent{Type: "token", Delta: ev.TextDelta, SubContext: sub})
		}
		if ev.ReasoningDelta != "" {
			r.publishEvent(tokenEvent{Type: "reasoning", Delta: ev.ReasoningDelta, SubContext: sub})
		}
	}

	_, span := r.tel.Tracer().Start(r.spanParent(ctx), telemetry.SpanLLMTurn, trace.WithAttributes(
		attribute.String(telemetry.AttrComponent, telemetry.ComponentBroker),
		attribute.String(telemetry.AttrModel, r.exploreModel),
		attribute.String(telemetry.AttrSubContext, sub),
	))
	defer span.End()

	start := time.Now()
	resp, err := r.completeWithRetry(ctx, r.exploreAdapter, req, onEvent, func(u model.Usage) { r.addExploreUsage(stream, u) })
	d := time.Since(start)
	if err != nil {
		span.RecordError(err)
		r.log.ErrorContext(ctx, "broker: explore model completion failed", "err", err)
		return model.Response{}, err
	}

	span.SetAttributes(
		attribute.String(telemetry.AttrStopReason, string(resp.Stop)),
		attribute.Int(telemetry.AttrToolCalls, len(resp.ToolCalls)),
		attribute.Int(telemetry.AttrInputTokens, resp.Usage.InputTokens),
		attribute.Int(telemetry.AttrOutputTokens, resp.Usage.OutputTokens),
	)
	r.tel.RecordLLMTurn(ctx, r.exploreModel,
		resp.Usage.InputTokens, resp.Usage.OutputTokens,
		resp.Usage.CacheReadTokens, resp.Usage.CacheCreationTokens, d)

	r.addExploreUsage(stream, resp.Usage)
	// Capture the exchange into the SEPARATE explore transcript (never the parent's) so the
	// exploration is harvestable, auditable evidence rather than a hidden side-channel (T12.4).
	r.recordExplore(req, resp)
	for _, tc := range resp.ToolCalls {
		r.publishEvent(tokenEvent{Type: "tool", Delta: toolCallSummary(tc), SubContext: sub})
	}
	r.log.InfoContext(ctx, "broker: explore model completion",
		"stream", stream, "model", r.exploreModel, "stop", resp.Stop,
		"input_tokens", resp.Usage.InputTokens, "output_tokens", resp.Usage.OutputTokens,
		"tool_calls", len(resp.ToolCalls))
	return resp, nil
}

// completeParent relays the invocation's own soul-model stream — the original broker completion
// path. It streams each delta out to NATS for the live view, tallies Usage against this
// invocation's running total (the budget input), records the transcript, and returns the
// canonical response. The runner attached the key and adapter; the agent never learns which
// provider answered. Every call is logged — the broker is the audited chokepoint.
func (r *relay) completeParent(ctx context.Context, req model.Request) (model.Response, error) {
	onEvent := func(ev model.StreamEvent) {
		// The visible answer and the model's chain of thought stream on separate channels
		// and are labeled distinctly for the feed; a turn may carry both, either, or — when
		// it is all tool calls — neither (the tool rows are published from resp, below).
		if ev.TextDelta != "" {
			r.publishEvent(tokenEvent{Type: "token", Delta: ev.TextDelta})
		}
		if ev.ReasoningDelta != "" {
			r.publishEvent(tokenEvent{Type: "reasoning", Delta: ev.ReasoningDelta})
		}
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
	resp, err := r.completeWithRetry(ctx, r.adapter, req, onEvent, r.addUsage)
	d := time.Since(start)
	if err != nil {
		span.RecordError(err)
		r.log.ErrorContext(ctx, "broker: model completion failed", "err", err)
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
	// One feed row per tool call this turn requested. Tool calls don't stream as text, so
	// without this an agent that works purely through tools (the common case) produces no
	// live activity at all — the feed would look idle while the agent is busy. Published
	// best-effort like the token stream; the durable record is the transcript.
	for _, tc := range resp.ToolCalls {
		r.publishEvent(tokenEvent{Type: "tool", Delta: toolCallSummary(tc)})
	}
	r.log.InfoContext(ctx, "broker: model completion",
		"stop", resp.Stop,
		"input_tokens", resp.Usage.InputTokens,
		"output_tokens", resp.Usage.OutputTokens,
		"cache_read_tokens", resp.Usage.CacheReadTokens,
		"cache_creation_tokens", resp.Usage.CacheCreationTokens,
		"tool_calls", len(resp.ToolCalls),
	)
	return resp, nil
}

// GitPush lands the candidate branch the agent built inside the sandbox. The branch must
// be exactly this invocation's task branch — naming any other branch (in particular a
// protected one like main) is refused, which is how "push only the task branch" is
// enforced: the runner-held token scopes to the repository, the broker guard scopes to the
// one branch (see specs/security.md). The commits are extracted from the network-less
// sandbox as a git bundle over Exec stdout — never a bind mount or copy-out — preserving
// the microVM-shaped isolation (see specs/components/sandbox.md).
//
// Destination (T5.7): when a remote is configured the bundle is pushed to that real remote
// authenticated by a per-task token the relay's minter mints just-in-time and revokes the
// instant the push completes (the token dies with its one use, tighter than the invocation
// lifetime; its TTL is the backstop). The agent never holds the token or the remote URL.
// With no remote configured the bundle is applied to the local source repo (the bootstrap
// path). Either way the token-minting and remote URL live only on the trusted runner.
func (r *relay) GitPush(ctx context.Context, req broker.GitPushRequest) (broker.GitPushResult, error) {
	// The git-push is the one egress tool the broker mediates, so the broker spans it here.
	// The unbrokered workspace/lifecycle tools get their own tool-call spans from the agent
	// loop (agent.Loop.invokeTool); this span is just the broker's view of the egress it
	// guards. It opens before the branch guard so a denied push is traced too, not dropped.
	_, span := r.tel.Tracer().Start(r.spanParent(ctx), telemetry.SpanToolCall, trace.WithAttributes(
		attribute.String(telemetry.AttrComponent, telemetry.ComponentBroker),
		attribute.String(telemetry.AttrToolName, telemetry.ToolGitPush),
		attribute.String(telemetry.AttrGitBranch, req.Branch),
	))
	defer span.End()

	if req.Branch != r.allowedBranch {
		r.log.WarnContext(ctx, "broker: git push refused, not the task branch", "requested", req.Branch, "allowed", r.allowedBranch)
		err := fmt.Errorf("git push denied: branch %q is not this task's branch %q", req.Branch, r.allowedBranch)
		span.RecordError(err)
		return broker.GitPushResult{}, err
	}

	bundle, err := r.extractBundle(ctx, req.Branch)
	if err != nil {
		span.RecordError(err)
		r.log.ErrorContext(ctx, "broker: git push failed extracting branch", "branch", req.Branch, "err", err)
		return broker.GitPushResult{}, err
	}

	commit, err := r.landBundle(ctx, req.Branch, bundle)
	if err != nil {
		span.RecordError(err)
		r.log.ErrorContext(ctx, "broker: git push failed applying branch", "branch", req.Branch, "err", err)
		return broker.GitPushResult{}, err
	}

	span.SetAttributes(attribute.String(telemetry.AttrGitCommit, commit))
	r.log.InfoContext(ctx, "broker: git push", "branch", req.Branch, "commit", commit)
	return broker.GitPushResult{Commit: commit}, nil
}

// landBundle routes the extracted candidate bundle to its destination: a configured remote
// (with a just-in-time minted, immediately-revoked scoped token) or, in the bootstrap
// single-host shape, the local source repo. The token is minted right before the push and
// revoked right after — its only use — so it never outlives the push. Revoke is best-effort:
// the token's TTL bounds exposure if it fails, so a revoke error is logged, not propagated.
func (r *relay) landBundle(ctx context.Context, branch string, bundle []byte) (string, error) {
	if r.remote == "" {
		return r.pushBundle(ctx, r.repo, branch, bundle)
	}

	var cred secret.GitCredential
	if r.minter != nil {
		c, err := r.minter.Mint(ctx, secret.MintRequest{IssueID: r.issueID, Branch: branch})
		if err != nil {
			return "", fmt.Errorf("mint scoped push token: %w", err)
		}
		cred = c
		defer func() {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), revokeTimeout)
			defer cancel()
			if err := r.minter.Revoke(rctx, cred); err != nil {
				r.log.WarnContext(rctx, "broker: revoke scoped push token failed (token self-expires by its TTL)", "err", err)
			}
		}()
	}
	return r.pushRemote(ctx, r.remote, cred, branch, bundle)
}

// FetchPackage proxies one Go module-proxy GET to the configured package proxy (default
// proxy.golang.org) on behalf of the zero-network sandbox, and returns the upstream
// status/body. This is the supply-chain chokepoint (specs/security.md Control 2): the
// runner host holds the network, so the fetch happens here and is logged, while the agent
// only ever sees the bytes. The fetch itself lives in internal/packageproxy (shared with
// the gate verifier so the two can never drift); this method wraps it in the tool-call span
// + audit log that make the egress observable in the invocation trace.
func (r *relay) FetchPackage(ctx context.Context, req broker.FetchPackageRequest) (broker.FetchPackageResult, error) {
	// A package fetch is a brokered egress tool, so it gets a tool-call span like git push.
	_, span := r.tel.Tracer().Start(r.spanParent(ctx), telemetry.SpanToolCall, trace.WithAttributes(
		attribute.String(telemetry.AttrComponent, telemetry.ComponentBroker),
		attribute.String(telemetry.AttrToolName, telemetry.ToolPackageFetch),
	))
	defer span.End()

	res, err := r.fetcher.Fetch(ctx, req)
	if err != nil {
		span.RecordError(err)
		r.log.ErrorContext(ctx, "broker: package fetch failed", "path", req.Path, "err", err)
		return broker.FetchPackageResult{}, err
	}
	span.SetAttributes(attribute.Int(telemetry.AttrHTTPStatus, res.Status))
	r.log.InfoContext(ctx, "broker: package fetch", "path", req.Path, "status", res.Status, "bytes", len(res.Body))
	return res, nil
}

// PublishEvent forwards a best-effort agent progress/log event to NATS on this
// invocation's event subject. The agent holds no NATS credentials and does not know
// its own subject — the broker supplies it. Delivery is fire-and-forget.
func (r *relay) PublishEvent(ctx context.Context, ev broker.PublishRequest) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("broker: marshal event: %w", err)
	}
	r.log.InfoContext(ctx, "broker: publish event", "type", ev.Type)
	if err := r.publishEnveloped(data); err != nil {
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

// Usage returns the total token usage tallied across every completion this invocation made —
// the parent stream PLUS every explorer stream, so the explorer's spend draws the parent-task
// ceiling the orchestrator enforces (specs/configuration.md). The runner logs it and the budget
// enforcer reads it. Per-model USD precision for the explorer stream is a T12.4 refinement.
func (r *relay) Usage() model.Usage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return model.Usage{
		InputTokens:         r.usage.InputTokens + r.exploreUsage.InputTokens,
		OutputTokens:        r.usage.OutputTokens + r.exploreUsage.OutputTokens,
		CacheCreationTokens: r.usage.CacheCreationTokens + r.exploreUsage.CacheCreationTokens,
		CacheReadTokens:     r.usage.CacheReadTokens + r.exploreUsage.CacheReadTokens,
	}
}

func (r *relay) addUsage(u model.Usage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.usage.InputTokens += u.InputTokens
	r.usage.OutputTokens += u.OutputTokens
	r.usage.CacheCreationTokens += u.CacheCreationTokens
	r.usage.CacheReadTokens += u.CacheReadTokens
}

// streamTokens is the input+output tokens metered on one explore call's stream so far — the
// input to the per-call sub-budget check. A never-seen stream reads 0 (a fresh call gets the
// full fixed budget).
func (r *relay) streamTokens(stream string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.streamTok[stream]
}

// addExploreUsage tallies one explorer completion: into exploreUsage (combined, so Usage() draws
// the parent ceiling) and into the per-stream counter (so the fixed cap resets per explore call).
func (r *relay) addExploreUsage(stream string, u model.Usage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exploreUsage.InputTokens += u.InputTokens
	r.exploreUsage.OutputTokens += u.OutputTokens
	r.exploreUsage.CacheCreationTokens += u.CacheCreationTokens
	r.exploreUsage.CacheReadTokens += u.CacheReadTokens
	if r.streamTok == nil {
		r.streamTok = make(map[string]int)
	}
	r.streamTok[stream] += u.InputTokens + u.OutputTokens
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
	r.turns = append(r.turns, model.TranscriptTurn{Request: req, Response: resp})
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

// recordExplore appends one explorer exchange to the SEPARATE explore transcript. It
// deliberately does NOT touch firstReq or turns: the parent prompt/transcript must stay the
// parent's (the explorer's question is not the invocation's prompt), so the exploration is
// harvested as its own artifact rather than contaminating the Prompt-SHA/Transcript evidence.
func (r *relay) recordExplore(req model.Request, resp model.Response) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exploreTurns = append(r.exploreTurns, model.TranscriptTurn{Request: req, Response: resp})
}

// ExploreTranscript returns the JSON-encoded ordered transcript of every explorer exchange
// this invocation made, and false when explore never ran — in which case there is no explore
// transcript to harvest (most invocations). It mirrors Transcript for the explorer stream.
func (r *relay) ExploreTranscript() ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.exploreTurns) == 0 {
		return nil, false
	}
	data, err := json.Marshal(r.exploreTurns)
	if err != nil {
		return nil, false
	}
	return data, true
}

// ExploreModel returns the pinned explorer model and true WHEN the explore sub-loop actually
// ran at least one completion, else ("", false). This is the authoritative "explore happened"
// signal the runner stamps onto the Result so the provenance trailer records the tier the
// exploration ran under — independent of whether the explore transcript itself persisted, so a
// store hiccup degrades the transcript hash but never erases the recorded model (T12.4).
func (r *relay) ExploreModel() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.exploreTurns) == 0 {
		return "", false
	}
	return r.exploreModel, true
}

// publishEvent emits one live-feed event best-effort; a failure is logged at debug and
// swallowed, since the stream is observability, not control flow, and must never stall
// or fail a model call (the StreamHandler runs on the token-draining goroutine).
func (r *relay) publishEvent(ev tokenEvent) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	if err := r.publishEnveloped(data); err != nil {
		r.log.DebugContext(context.Background(), "broker: drop live event", "err", err)
	}
}

// publishEnveloped wraps one inner event payload in the issue/role-stamped envelope and
// publishes it to this invocation's event subject. Stamping issue id + role here — the runner
// holds the binding via the Brief, every event path funnels through this one helper — lets the
// control room scope a live feed to one invocation without a second beads read (specs/messaging.md,
// plan T4.20). The agent (invocation) id is not stamped: it is the subject's final token, recovered
// by the consumer, so the payload only adds what the subject does not already carry.
func (r *relay) publishEnveloped(payload []byte) error {
	data, err := json.Marshal(core.AgentEventEnvelope{
		IssueID: r.issueID,
		Role:    r.role,
		Payload: payload,
	})
	if err != nil {
		return fmt.Errorf("broker: marshal event envelope: %w", err)
	}
	return r.pub.Publish(r.eventSubject, data)
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
	return runGitEnv(ctx, repo, nil, args...)
}

// runGitEnv runs a git subcommand in repo with extra environment appended to the process
// environment, returning combined output. The extra env carries the scoped push token to
// git's credential helper (push.go) without ever placing it in argv. A nil extraEnv leaves
// the process environment untouched (the common read-only case).
func runGitEnv(ctx context.Context, repo string, extraEnv []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...) // #nosec G204 -- fixed git binary, repo-scoped; args are runner-controlled, not untrusted agent input.
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}
