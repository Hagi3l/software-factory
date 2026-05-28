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

	"github.com/Loxstomper/harness/internal/broker"
	"github.com/Loxstomper/harness/internal/model"
	"github.com/Loxstomper/harness/internal/sandbox"
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

	eventSubject  string // harness.agent.<id>.events — where token/progress events fan out
	repo          string // source repository the candidate branch is pushed into
	allowedBranch string // the ONLY branch this invocation may push (task branch only)

	// pushBundle applies a git bundle (the candidate branch, extracted from the
	// sandbox) into the source repo on the runner host and returns the pushed head.
	// A seam so the relay's orchestration is unit-testable without real git; the
	// default is pushBundleToRepo.
	pushBundle func(ctx context.Context, repo, branch string, bundle []byte) (string, error)

	mu    sync.Mutex
	usage model.Usage // tallied across every completion this invocation (budget input, plan T1.16)
}

var _ broker.Handler = (*relay)(nil)

// relayConfig carries the per-invocation bindings newRelay needs beyond its
// collaborators, grouped so the constructor call stays legible.
type relayConfig struct {
	eventSubject  string
	repo          string
	allowedBranch string
	log           *slog.Logger
}

func newRelay(adapter model.Adapter, pub Publisher, sb sandbox.Sandbox, cfg relayConfig) *relay {
	return &relay{
		adapter:       adapter,
		pub:           pub,
		sb:            sb,
		log:           cfg.log,
		eventSubject:  cfg.eventSubject,
		repo:          cfg.repo,
		allowedBranch: cfg.allowedBranch,
		pushBundle:    pushBundleToRepo,
	}
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

	resp, err := r.adapter.Complete(ctx, req, onEvent)
	if err != nil {
		r.log.Error("broker: model completion failed", "err", err)
		return model.Response{}, err
	}

	r.addUsage(resp.Usage)
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
	if req.Branch != r.allowedBranch {
		r.log.Warn("broker: git push refused, not the task branch", "requested", req.Branch, "allowed", r.allowedBranch)
		return broker.GitPushResult{}, fmt.Errorf("git push denied: branch %q is not this task's branch %q", req.Branch, r.allowedBranch)
	}

	bundle, err := r.extractBundle(ctx, req.Branch)
	if err != nil {
		r.log.Error("broker: git push failed extracting branch", "branch", req.Branch, "err", err)
		return broker.GitPushResult{}, err
	}

	commit, err := r.pushBundle(ctx, r.repo, req.Branch, bundle)
	if err != nil {
		r.log.Error("broker: git push failed applying branch", "branch", req.Branch, "err", err)
		return broker.GitPushResult{}, err
	}

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
