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

	mu       sync.Mutex
	usage    model.Usage            // tallied across every completion this invocation (budget input, plan T1.16)
	firstReq *model.Request         // the prompt: the first request this invocation ran with (provenance, plan T1.20)
	turns    []model.TranscriptTurn // every completion this invocation made, in order (the transcript)
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
		adapter:       adapter,
		pub:           pub,
		sb:            sb,
		log:           cfg.log,
		tel:           tel,
		model:         cfg.model,
		parentCtx:     cfg.parentCtx,
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
type tokenEvent struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
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

// Complete relays a canonical model request to the resolved provider adapter, streams
// each text delta out to NATS for the live view, tallies the returned Usage against
// this invocation's running total (the budget input), and returns the canonical
// response. The runner attached the key and adapter; the agent never learns which
// provider answered. Every call is logged — the broker is the audited chokepoint.
func (r *relay) Complete(ctx context.Context, req model.Request) (model.Response, error) {
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
	resp, err := r.adapter.Complete(ctx, req, onEvent)
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
