package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"golang.org/x/sync/errgroup"

	"github.com/Loxstomper/harness/internal/agent"
	"github.com/Loxstomper/harness/internal/artifact"
	"github.com/Loxstomper/harness/internal/beads"
	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/gate"
	"github.com/Loxstomper/harness/internal/messaging"
	"github.com/Loxstomper/harness/internal/model/registry"
	"github.com/Loxstomper/harness/internal/orchestrator"
	"github.com/Loxstomper/harness/internal/runner"
	"github.com/Loxstomper/harness/internal/sandbox"
)

// cmdRun boots the kernel: validate config, assemble every component over an
// in-process NATS, then run an orchestrator and one runner concurrently until the
// process is interrupted. This is the spec -> merged-commit loop from
// specs/bootstrap.md — the orchestrator dispatches ready beads work, runners build
// candidates in sandboxes, the gate verifies in a fresh sandbox, and accepted
// candidates fast-forward onto main with a provenance trailer.
func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	dir := fs.String("config", "config", "config directory (harness.yaml, souls/, infra.<env>.yaml)")
	env := fs.String("env", "dev", "infra environment overlay to load")
	repo := fs.String("repo", ".", "integration repository: candidates are pushed and merged here, and worktrees seeded from it")
	gateBuild := fs.String("gate-build", "make build", "build check command run in the verification sandbox")
	gateTest := fs.String("gate-test", "make test-unit", "test check command run in the verification sandbox")
	bdBin := fs.String("bd", "bd", "path to the beads CLI")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(*dir, *env)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	resolvePersonas(cfg)

	absRepo, err := filepath.Abs(*repo)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	comp, err := buildRunComponents(cfg, absRepo, runOptions{
		gateBuild: *gateBuild,
		gateTest:  *gateTest,
		bdBin:     *bdBin,
	}, log)
	if err != nil {
		return err
	}
	defer comp.cleanup()

	// Cancel on Ctrl-C / SIGTERM. Both loops treat a ctx cancel as a clean shutdown,
	// so the errgroup returns nil on signal and a real error otherwise.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("harness run: starting", "repo", absRepo, "roles", agentRoles(cfg))
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return comp.orch.Run(ctx) })
	g.Go(func() error { return comp.rnr.Run(ctx) })
	err = g.Wait()
	log.Info("harness run: stopped", "err", err)
	return err
}

// runOptions carries the run-only knobs that are not in the config file (the gate
// check commands and the beads binary), so buildRunComponents has a stable signature
// the wiring test can call directly.
type runOptions struct {
	gateBuild string
	gateTest  string
	bdBin     string
}

// runComponents holds the assembled, ready-to-run kernel and a cleanup that releases
// everything the assembly acquired (NATS server, the broker socket dir).
type runComponents struct {
	orch    *orchestrator.Orchestrator
	rnr     *runner.Runner
	cleanup func()
}

// buildRunComponents is the composition root proper: it stands up the embedded NATS
// server + JetStream, the artifact store, the model registry, the Docker sandbox
// backend, the agent loop (as the runner's Invoker), the gate, and finally the
// runner and orchestrator wired to all of the above. It is separated from cmdRun so
// the wiring can be exercised in a test without process signals — every constructor
// here is network-free (NewEmbeddedServer is in-process, the SDK clients and the
// Docker backend touch nothing until used), so the assembly is fully testable.
//
// repo must be absolute. On any failure it tears down whatever it already built so a
// failed assembly leaks no server or temp dir.
func buildRunComponents(cfg *config.Config, repo string, opts runOptions, log *slog.Logger) (_ *runComponents, err error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}

	// A deferred-on-error teardown stack: each acquired resource registers its
	// release, and we run them in reverse iff we return an error.
	var releases []func()
	cleanupAll := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	defer func() {
		if err != nil {
			cleanupAll()
		}
	}()

	// Embedded NATS + JetStream. The store dir lives under the repo so JetStream
	// state survives a restart (the crash-and-resume model), rather than a temp dir.
	storeDir := filepath.Join(repo, ".harness", "jetstream")
	if mkErr := os.MkdirAll(storeDir, 0o755); mkErr != nil {
		return nil, mkErr
	}
	srv, err := messaging.NewEmbeddedServer(messaging.ServerConfig{Name: "harness", StoreDir: storeDir})
	if err != nil {
		return nil, err
	}
	releases = append(releases, srv.Shutdown)
	nc, err := srv.Connect()
	if err != nil {
		return nil, err
	}
	releases = append(releases, nc.Close)
	js, err := messaging.JetStream(nc)
	if err != nil {
		return nil, err
	}
	// Create the streams up front so neither loop races the other on startup (the
	// runner's consumer needs the work stream to exist; the orchestrator also calls
	// this, idempotently).
	if ssErr := messaging.SetupStreams(context.Background(), js); ssErr != nil {
		return nil, ssErr
	}

	// Artifact store. Resolve a relative path against the repo so it does not depend
	// on the process working dir.
	art := cfg.Infra.Artifacts
	if art.Path != "" && !filepath.IsAbs(art.Path) {
		art.Path = filepath.Join(repo, art.Path)
	}
	store, err := artifact.Open(art)
	if err != nil {
		return nil, err
	}

	// Model registry: soul.model -> provider adapter. Keys come from the environment
	// inside the registry, never from config.
	reg, err := registry.New(cfg.Infra.Models)
	if err != nil {
		return nil, err
	}

	backend := sandbox.NewDockerBackend()

	sockDir, err := os.MkdirTemp("", "harness-broker-")
	if err != nil {
		return nil, err
	}
	releases = append(releases, func() { _ = os.RemoveAll(sockDir) })

	// The agent inner loop is the runner's Invoker. Its ToolSource composes the
	// in-sandbox workspace tools with the lifecycle tools (submit/escalate/
	// request_subtask) per invocation. Budget is derived from the per-issue policy.
	toolSource := func(inv agent.Invocation) ([]agent.Tool, error) {
		tools := agent.WorkspaceTools(inv.Sandbox)
		tools = append(tools, agent.LifecycleTools(inv.Brief, inv.Broker)...)
		return tools, nil
	}
	loop := agent.New(toolSource, agent.BudgetFromPolicy(cfg.Harness.Policy), log)

	rnr, err := runner.New(runner.Options{
		Roles:     agentRoles(cfg),
		Repo:      repo,
		SocketDir: sockDir,
		Limits:    cfg.Infra.Sandbox.Limits,
		Allowlist: cfg.Infra.Broker.Allowlist,
		Logger:    log,
	}, backend, reg, nc, loop, store, js)
	if err != nil {
		return nil, err
	}

	// The gate verifies in a fresh sandbox (producer != verifier): clean checkout of
	// the candidate branch, then build + test. The checks are data; the kernel runs
	// the two bootstrap commands.
	gateRunner := gate.New(backend, []gate.Check{
		{Name: "build", Cmd: opts.gateBuild},
		{Name: "test", Cmd: opts.gateTest},
	}, sockDir, log)

	orch, err := orchestrator.New(orchestrator.Options{
		Config: cfg,
		Repo:   repo,
		Base:   "main",
		Limits: cfg.Infra.Sandbox.Limits,
		Logger: log,
	}, beads.New(beads.WithBinary(opts.bdBin), beads.WithDir(repo)), gateRunner, orchestrator.NewGitMerger(""), js)
	if err != nil {
		return nil, err
	}

	return &runComponents{orch: orch, rnr: rnr, cleanup: cleanupAll}, nil
}
