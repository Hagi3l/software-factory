package wizard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/Loxstomper/harness/internal/agent"
	"github.com/Loxstomper/harness/internal/broker"
	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/model"
	"github.com/Loxstomper/harness/internal/sandbox"
)

// Read-only codebase exploration (T4.28, specs/control-room.md). The requirements planner is
// trusted and converses host-side, but to ground its specs + seed issues in an *existing*
// codebase it needs to read that code. Letting a model drive reads on the host is exactly the
// surface the architecture refuses, so the reads run where every other model-driven read runs:
// inside a fresh, read-only, ZERO-NETWORK sandbox seeded from the integration repo, behind a
// deny-all broker — the same construction the gate's verification sandbox uses. The model
// emits canonical tool calls; this layer executes them against that sandbox (the agent's own
// read tools, unchanged) and feeds the results back. Nothing here can write: the worktree is a
// throwaway clone discarded on teardown, the broker denies all egress, and the write/run tools
// are filtered out. The planner's only durable outputs remain the consent-gated spec + issues.

const (
	// teardownTimeout bounds tearing one exploration sandbox down so a wedged backend cannot
	// block session eviction or planner shutdown forever (mirrors the gate's own ceiling).
	teardownTimeout = 30 * time.Second
)

// sandboxConfig is the template the composition root hands the Planner (via WithSandbox) to
// enable exploration. It is copied onto each Create session at mint; a nil sandboxConfig means
// exploration is disabled and the session behaves exactly as a pre-T4.28 pure-conversation
// planner. Everything here is resolved host-side once, so a session can provision lazily
// without touching config or the filesystem.
type sandboxConfig struct {
	backend sandbox.Backend
	repo    string // absolute integration-repo path the worktree is seeded from
	profile string // logical infra sandbox profile (carried for provenance)
	image   string // concrete image, pre-resolved from profile via ResolveImage
	baseRef string // git ref the read-only worktree is checked out at
	limits  config.SandboxLimits
	sockDir string // directory the per-session deny-all broker socket is bound in
	log     *slog.Logger
}

// explorer owns one session's live read-only exploration stack: the provisioned sandbox, the
// dispatch index of read tools over it, the static tool definitions advertised to the model,
// and a cleanup that tears the whole thing down. It is built lazily on the first tool call and
// reused across the session's turns, then torn down once on session end / eviction.
type explorer struct {
	sb      sandbox.Sandbox
	byName  map[string]agent.Tool
	defs    []model.ToolDef
	cleanup func()
}

// readOnlyToolDefs returns the tool definitions advertised to the model every turn, WITHOUT a
// live sandbox. The defs are pure data (name/description/schema); only Invoke needs a sandbox.
// Advertising them cheaply is what lets a session boot Docker lazily — only when the model
// actually calls a tool — so a conversation that never explores stays as fast as before. The
// set is exactly the read tools buildExplorer binds, so what is advertised is always what can
// be invoked.
func readOnlyToolDefs() []model.ToolDef {
	// A nil sandbox is never executed against — we only read Def(), which is static data.
	tools := readOnlyToolsOver(nil, agent.NewSessions(nil, nil))
	defs := make([]model.ToolDef, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, t.Def())
	}
	return defs
}

// readOnlyToolsOver builds the read-only tool set over sb and its LSP sessions: the agent's text
// read tools (read_file/list_dir/search) with write_file/edit_file/run filtered out, plus the
// LSP comprehension tools (find_symbol/references/…). sessions is passed in (not created here)
// so the caller can Close() it on teardown. It doubles as the workspace tools' edit notifier —
// no edit ever fires here, but the wiring matches the agent's so the constructors are reused
// verbatim. Passing a nil sandbox is valid ONLY for reading Def() (see readOnlyToolDefs).
func readOnlyToolsOver(sb sandbox.Sandbox, sessions *agent.Sessions) []agent.Tool {
	read := filterTools(agent.WorkspaceTools(sb, sessions), "read_file", "list_dir", "search")
	tools := make([]agent.Tool, 0, len(read)+6)
	tools = append(tools, read...)
	tools = append(tools, agent.SemanticReadTools(sessions)...)
	return tools
}

// filterTools keeps only the tools whose advertised name is in keep, preserving order. The
// concrete read-tool constructors are unexported in internal/agent, so selecting by name off
// the full WorkspaceTools set is the clean way to expose the read subset without duplicating
// them (and it fails closed: a renamed tool simply drops out rather than leaking a writer).
func filterTools(tools []agent.Tool, keep ...string) []agent.Tool {
	want := make(map[string]bool, len(keep))
	for _, k := range keep {
		want[k] = true
	}
	out := make([]agent.Tool, 0, len(keep))
	for _, t := range tools {
		if want[t.Def().Name] {
			out = append(out, t)
		}
	}
	return out
}

// buildExplorer provisions one read-only, zero-network sandbox seeded at cfg.baseRef and builds
// the read tool set over it. It mirrors the gate's provisionVerifier: a deny-all broker exists
// only because Provision requires a broker endpoint — serving deny-all is how "the planner's
// reads reach nothing" is enforced by construction. The returned explorer's cleanup stops the
// broker, closes the LSP sessions, and tears the sandbox down; the caller owns calling it.
func buildExplorer(ctx context.Context, cfg sandboxConfig) (*explorer, error) {
	id, err := exploreID()
	if err != nil {
		return nil, err
	}
	sockPath := filepath.Join(cfg.sockDir, "rp-"+id+".sock")
	ln, err := broker.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("wizard: listen broker socket: %w", err)
	}
	spec := sandbox.Spec{
		Profile:   cfg.profile,
		Image:     cfg.image,
		Workspace: sandbox.Workspace{Repo: cfg.repo, BaseRef: cfg.baseRef},
		Limits:    cfg.limits,
		Broker:    sandbox.Endpoint{Network: "unix", Address: sockPath},
	}
	sb, err := cfg.backend.Provision(ctx, spec)
	if err != nil {
		_ = ln.Close() // unlinks the socket we just bound
		return nil, fmt.Errorf("wizard: provision exploration sandbox: %w", err)
	}

	srv := broker.NewServer(denyHandler{}, broker.WithAllowlist(nil))
	brokerCtx, stopBroker := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		if err := srv.Serve(brokerCtx, ln); err != nil {
			cfg.log.ErrorContext(brokerCtx, "wizard: exploration broker serve", "err", err)
		}
	}()

	sessions := agent.NewSessions(sb, cfg.log)
	tools := readOnlyToolsOver(sb, sessions)

	byName := make(map[string]agent.Tool, len(tools))
	defs := make([]model.ToolDef, 0, len(tools))
	for _, t := range tools {
		byName[t.Def().Name] = t
		defs = append(defs, t.Def())
	}

	cleanup := func() {
		stopBroker() // closes ln, unblocking Serve and unlinking the socket
		sessions.Close()
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), teardownTimeout)
		defer cancel()
		if err := sb.Teardown(tctx); err != nil {
			cfg.log.ErrorContext(tctx, "wizard: teardown exploration sandbox", "id", sb.ID(), "err", err)
		}
	}
	return &explorer{sb: sb, byName: byName, defs: defs, cleanup: cleanup}, nil
}

// exploreID returns an unguessable id for the per-session broker socket name (crypto-random so
// two concurrent sessions never collide on a socket path).
func exploreID() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("wizard: explore id: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// denyHandler satisfies broker.Handler but performs nothing: it is served behind a deny-all
// allowlist so dispatch rejects every call before reaching it. It exists only because
// NewServer needs a non-nil Handler; every method erroring is defense in depth should the
// allowlist ever be widened by mistake (the exploration sandbox must reach nothing).
type denyHandler struct{}

func (denyHandler) Complete(context.Context, model.Request) (model.Response, error) {
	return model.Response{}, fmt.Errorf("wizard: exploration sandbox has no broker egress")
}

func (denyHandler) GitPush(context.Context, broker.GitPushRequest) (broker.GitPushResult, error) {
	return broker.GitPushResult{}, fmt.Errorf("wizard: exploration sandbox has no broker egress")
}

func (denyHandler) PublishEvent(context.Context, broker.PublishRequest) error {
	return fmt.Errorf("wizard: exploration sandbox has no broker egress")
}

func (denyHandler) FetchPackage(context.Context, broker.FetchPackageRequest) (broker.FetchPackageResult, error) {
	return broker.FetchPackageResult{}, fmt.Errorf("wizard: exploration sandbox has no broker egress")
}
