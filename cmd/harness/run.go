package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"golang.org/x/sync/errgroup"

	"github.com/Loxstomper/harness/internal/agent"
	"github.com/Loxstomper/harness/internal/artifact"
	"github.com/Loxstomper/harness/internal/beads"
	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/controlroom"
	"github.com/Loxstomper/harness/internal/controlroom/live"
	"github.com/Loxstomper/harness/internal/controlroom/query"
	"github.com/Loxstomper/harness/internal/controlroom/wizard"
	"github.com/Loxstomper/harness/internal/gate"
	"github.com/Loxstomper/harness/internal/messaging"
	"github.com/Loxstomper/harness/internal/model/registry"
	"github.com/Loxstomper/harness/internal/orchestrator"
	"github.com/Loxstomper/harness/internal/runner"
	"github.com/Loxstomper/harness/internal/sandbox"
	"github.com/Loxstomper/harness/internal/telemetry"
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
	bdBin := fs.String("bd", "bd", "path to the beads CLI")
	serveAddr := fs.String("serve-addr", "", "if set, also serve the control room on this address (live SSE shares this run's in-process NATS)")
	natsAddr := fs.String("nats-addr", "", "if set, expose this run's NATS on this address so `harness approve`/`reject` can reach it (default: in-process only)")
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
		bdBin:     *bdBin,
		serveAddr: *serveAddr,
		natsAddr:  *natsAddr,
		env:       *env,
	}, log)
	if err != nil {
		return err
	}
	defer comp.cleanup()

	// Cancel on Ctrl-C / SIGTERM. Both loops treat a ctx cancel as a clean shutdown,
	// so the errgroup returns nil on signal and a real error otherwise.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("harness run: starting", "repo", absRepo, "roles", agentRoles(cfg), "serve", *serveAddr)
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return comp.orch.Run(ctx) })
	g.Go(func() error { return comp.rnr.Run(ctx) })
	// The control room, when enabled, is co-located in this process so it can tail the
	// in-process NATS directly (a separate `harness serve` cannot reach a DontListen
	// embedded server — that awaits distributed NATS, T5.8). A ctx cancel drains it.
	if comp.server != nil {
		addr := *serveAddr
		g.Go(func() error { return comp.server.ListenAndServe(ctx, addr) })
	}
	err = g.Wait()
	log.Info("harness run: stopped", "err", err)
	return err
}

// runOptions carries the run-only knobs that are not in the config file (the beads
// binary path), so buildRunComponents has a stable signature the wiring test can call
// directly. The gate check commands live in harness.yaml's `checks` registry, not
// here — config is their single source of truth (see specs/configuration.md).
type runOptions struct {
	bdBin string
	// serveAddr, when non-empty, co-locates the control room in the run process and
	// serves it here, sharing this run's in-process NATS so the SSE feed has a live
	// source. Empty (the default) builds no server. Kept here, not in config, because
	// it is a deployment knob of this command like bdBin.
	serveAddr string
	// env is the active infra-overlay environment name (infra.<env>.yaml). It is threaded
	// into the control room's Config view (T4.26) for the identity strip — "which overlay is
	// in force". A deployment knob of this command like serveAddr, not config.
	env string
	// natsAddr, when non-empty (host:port), exposes this run's embedded NATS on a TCP
	// listener so a separate `harness approve`/`reject` process can publish approvals to it
	// (the trusted-dev gate, T2.10). Empty (the default) keeps NATS in-process only. Like
	// serveAddr it is a deployment knob of this command, not config.
	natsAddr string
	// backend lets a test inject a non-production sandbox backend (the non-isolating
	// host-exec local backend) so the spine can be driven end-to-end without Docker
	// (see TE.1 in IMPLEMENTATION_PLAN.md, specs/bootstrap.md). It is INJECTION-ONLY and
	// deliberately not config-selectable: `sandbox.backend` config still maps only to
	// real isolating backends, so a non-isolating backend can never reach a deployment.
	// nil — the production path — uses the Docker backend, unchanged.
	backend sandbox.Backend
}

// runComponents holds the assembled, ready-to-run kernel and a cleanup that releases
// everything the assembly acquired (NATS server, the broker socket dir).
type runComponents struct {
	orch *orchestrator.Orchestrator
	rnr  *runner.Runner
	// server is the co-located control room, non-nil only when runOptions.serveAddr is
	// set. cmdRun runs it in the same errgroup as the two loops.
	server  *controlroom.Server
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

	// NATS + JetStream. Two deployment shapes, selected by the infra overlay's nats.url:
	//   - empty → an embedded in-process server (the bootstrap/dev default): this process
	//     hosts NATS, optionally exposed on a TCP listener via --nats-addr so a separate
	//     `harness approve` can reach it.
	//   - set   → connect to that EXTERNAL cluster (distributed, T5.8); no embedded server
	//     is started, so --nats-addr has nothing to expose and is ignored.
	// Either way the orchestrator and runner take the same *nats.Conn (location
	// transparency); only the composition root differs.
	var nc *nats.Conn
	if url := cfg.Infra.NATS.URL; url == "" {
		// The store dir lives under the repo so JetStream state survives a restart (the
		// crash-and-resume model), rather than a temp dir.
		storeDir := filepath.Join(repo, ".harness", "jetstream")
		if mkErr := os.MkdirAll(storeDir, 0o750); mkErr != nil {
			return nil, mkErr
		}
		srv, serr := messaging.NewEmbeddedServer(messaging.ServerConfig{Name: "harness", StoreDir: storeDir, ClientAddr: opts.natsAddr})
		if serr != nil {
			return nil, serr
		}
		releases = append(releases, srv.Shutdown)
		nc, err = srv.Connect()
		if err != nil {
			return nil, err
		}
	} else {
		if opts.natsAddr != "" {
			log.Warn("harness run: --nats-addr ignored because nats.url points at an external cluster", "nats_url", url)
		}
		nc, err = messaging.Connect(url, nats.Name("harness"))
		if err != nil {
			return nil, err
		}
	}
	releases = append(releases, nc.Close)
	js, err := messaging.JetStream(nc)
	if err != nil {
		return nil, err
	}
	// Create the streams up front so neither loop races the other on startup (the
	// runner's consumer needs the work stream to exist; the orchestrator also calls this,
	// idempotently, with the SAME options). Replicas/max-age come from the infra overlay.
	streamOpts := messaging.StreamOptions{
		Replicas:     cfg.Infra.NATS.JetStream.Replicas,
		ResultMaxAge: time.Duration(cfg.Infra.NATS.JetStream.MaxAge),
	}
	if ssErr := messaging.SetupStreams(context.Background(), js, streamOpts); ssErr != nil {
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

	// Telemetry: one Provider shared by the orchestrator, runner, and gate. Endpoint comes
	// from the validated infra overlay ("" off / "stdout" / OTLP host:port). Setup is
	// network-free (the OTLP exporter dials lazily), so it is safe in this network-free
	// composition root; a missing collector degrades to dropped exports, never a boot
	// failure. Shutdown joins the teardown stack so a clean exit flushes the final batch.
	tel, err := telemetry.Setup(context.Background(), telemetry.Config{
		Endpoint:    cfg.Infra.OTel.Endpoint,
		ServiceName: "harness",
	})
	if err != nil {
		return nil, err
	}
	releases = append(releases, func() { _ = tel.Shutdown(context.Background()) })

	// Model registry: soul.model -> provider adapter. Keys come from the environment
	// inside the registry, never from config. Built before the control room so the
	// requirements-planner wizard (T4.12) can resolve its configured model to an adapter.
	reg, err := registry.New(cfg.Infra.Models)
	if err != nil {
		return nil, err
	}

	// Production uses the Docker backend; a test may inject the local host-exec backend
	// (never config-selectable — see runOptions.backend). The same backend serves the runner
	// (building candidates), the gate (verifying them), and — when configured — the
	// requirements-planner wizard's read-only codebase exploration (T4.28). Built before the
	// control room so the planner wiring below can hand it to WithSandbox.
	backend := opts.backend
	if backend == nil {
		backend = sandbox.NewDockerBackend()
	}

	// One temp dir holds every per-sandbox broker socket (runner, gate, and the planner's
	// read-only exploration sandbox), cleaned up on teardown.
	sockDir, err := os.MkdirTemp("", "harness-broker-")
	if err != nil {
		return nil, err
	}
	releases = append(releases, func() { _ = os.RemoveAll(sockDir) })

	// Control room, co-located when serving is enabled. It shares this run's in-process
	// NATS (the pump tails the agent-event subjects into the SSE hub) and reads the same
	// three stores the loops write — beads (read-only; the orchestrator is the single
	// writer), the artifact store, and git provenance — so the human's window shows live
	// work. The server binds no socket until cmdRun calls ListenAndServe, so assembling
	// it here keeps buildRunComponents network-free and testable. The pump's unsubscribe
	// joins the teardown stack.
	var server *controlroom.Server
	if opts.serveAddr != "" {
		hub := live.NewHub()
		activity := live.NewActivity(200)
		mergeQueue := live.NewMergeQueue(100)
		// Tee the factory's own structured log into the feed's "system" stream, so the
		// activity view shows what the orchestrator/runner/gate are doing — not only
		// per-agent token output. This is sound only here, in the co-located run, where the
		// control room shares the process emitting these logs. Reassigned before the loops
		// are built below so every one of them (and the control room itself) logs through it.
		log = slog.New(live.NewLogBridge(log.Handler(), hub, activity))
		pumpStop, perr := live.StartAgentEventPump(nc, hub, activity)
		if perr != nil {
			return nil, perr
		}
		releases = append(releases, pumpStop)
		// The issue-state pump (T4.17) shares the same hub: it tails the single-writer
		// orchestrator's transition events so the board/DAG/DLQ views refresh crisply on the
		// actual state change rather than around agent activity (T4.18 swaps their triggers).
		statePumpStop, sperr := live.StartIssueStatePump(nc, hub)
		if sperr != nil {
			return nil, sperr
		}
		releases = append(releases, statePumpStop)
		// The DLQ pump (T4.19) shares the same hub: it tails the durable harness.dlq subject so a
		// dead-letter arrival reaches the operator as a browser notification (the queue is the
		// human's only action surface) and bumps the status bar's escalation count.
		dlqPumpStop, dlqErr := live.StartDLQPump(nc, hub)
		if dlqErr != nil {
			return nil, dlqErr
		}
		releases = append(releases, dlqPumpStop)
		// The merge-state pump (T4.25) shares the same hub: it tails harness.merge.*.state so the
		// merge-queue view sees each integrate candidate's step (queued → rebasing → re-gating →
		// landed, or terminal conflicted / regate-failed) — the rebase-and-re-gate interval beads
		// does not record — and records the latest step per candidate into the mergeQueue buffer.
		mergePumpStop, mergeErr := live.StartMergeStatePump(nc, hub, mergeQueue)
		if mergeErr != nil {
			return nil, mergeErr
		}
		releases = append(releases, mergePumpStop)
		reader := query.NewReader(
			beads.New(beads.WithBinary(opts.bdBin), beads.WithDir(repo)),
			store,
			query.NewGitProvenance(repo, query.WithAllowedSigners(cfg.Infra.Signing.AllowedSigners)),
		)
		// The requirements-planner wizard (T4.12), when configured. It is trusted and
		// non-sandboxed — a direct model.Adapter conversation, no broker/sandbox/NATS — so it
		// needs only the resolved adapter and the persona text (read here so the wizard package
		// depends on neither config nor the filesystem). Absent the requirements_planner block
		// the wizard stays disabled (/create renders a notice).
		var planner *wizard.Planner
		var seeder wizard.Seeder
		var resolver wizard.Resolver
		if rp := cfg.Harness.RequirementsPlanner; rp != nil {
			adapter, aerr := reg.Adapter(rp.Model)
			if aerr != nil {
				return nil, aerr
			}
			personaBytes, rerr := os.ReadFile(cfg.RequirementsPlannerPersonaPath())
			if rerr != nil {
				return nil, fmt.Errorf("read requirements planner persona: %w", rerr)
			}
			plannerOpts := []wizard.Option{wizard.WithMaxTokens(rp.MaxTokens), wizard.WithLogger(log)}
			// Read-only codebase exploration (T4.28): when a sandbox profile is configured, give the
			// planner the agent's read tools over a fresh read-only sandbox seeded from the repo, so
			// it grounds specs + seed issues in the real code. baseRef defaults to the repo's current
			// branch (the harness repo is master, a target repo may differ — never hardcode main).
			if rp.SandboxProfile != "" {
				baseRef := rp.BaseRef
				if baseRef == "" {
					baseRef = defaultBranch(repo)
				}
				image := cfg.Infra.Sandbox.ResolveImage(rp.SandboxProfile)
				plannerOpts = append(plannerOpts, wizard.WithSandbox(backend, repo, rp.SandboxProfile, image, baseRef, cfg.Infra.Sandbox.Limits, sockDir))
			}
			planner = wizard.NewPlanner(adapter, string(personaBytes), plannerOpts...)
			releases = append(releases, func() {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				planner.Shutdown(ctx)
			})
			// The consent-gated write seam: on Create-APPROVE it commits the drafted spec, writes the
			// decisions sidecar, stores the transcript, and creates the seed issues; on Resolve-APPROVE
			// (T4.15) it commits the refined spec and returns the dead-lettered issue to the ready pool.
			// One wizardSeeder implements both wizard.Seeder and wizard.Resolver — two consent-gated
			// write paths sharing the spec-write machinery. It uses a fresh read-only-by-convention
			// beads client (the orchestrator stays the sole long-lived writer; the wizard write is a
			// discrete human-approved seed/resolve, like `harness seed`).
			ws := newWizardSeeder(cfg, repo, beads.New(beads.WithBinary(opts.bdBin), beads.WithDir(repo)), store, log)
			seeder = ws
			resolver = ws
		}

		server = controlroom.New(controlroom.Options{
			Version:    version,
			Logger:     log,
			Events:     hub,
			Activity:   activity,
			MergeQueue: mergeQueue,
			Reader:     reader,
			StageOrder: pipelineRoles(cfg),
			BudgetCaps: budgetCaps(cfg),
			Planner:    planner,
			Seeder:     seeder,
			Resolver:   resolver,
			Repo:       repo,
			SpecDepth:  cfg.Harness.SpecDepth,
			Config:     cfg,
			Env:        opts.env,
		})
	}

	// The agent inner loop is the runner's Invoker. Its ToolSource composes the
	// in-sandbox workspace tools with the lifecycle tools (submit/escalate/
	// request_subtask) per invocation. Budget is derived from the per-issue policy.
	//
	// A per-invocation LSP session manager (Phase 6, T6.1) backs the edit tools so a
	// warm language server stays in sync with the worktree; the semantic comprehension/
	// transformation tools (T6.2/T6.3) query it. It is closed via the returned cleanup
	// when the invocation ends. A sandbox without streamed-session support leaves it
	// inert (edits no-op, queries degrade to the text floor).
	toolSource := func(inv agent.Invocation) ([]agent.Tool, func(), error) {
		sessions := agent.NewSessions(inv.Sandbox, log)
		ledger := agent.NewTransformLedger()
		tools := agent.WorkspaceTools(inv.Sandbox, sessions)
		tools = append(tools, agent.SemanticReadTools(sessions)...)
		tools = append(tools, agent.SemanticWriteTools(sessions, ledger)...)
		tools = append(tools, agent.LifecycleTools(inv.Brief, inv.Broker, ledger)...)
		return tools, sessions.Close, nil
	}
	loop := agent.New(toolSource, agent.BudgetFromPolicy(cfg.Harness.Policy), log)

	rnr, err := runner.New(runner.Options{
		Roles:        agentRoles(cfg),
		Repo:         repo,
		SocketDir:    sockDir,
		Limits:       cfg.Infra.Sandbox.Limits,
		ResolveImage: cfg.Infra.Sandbox.ResolveImage,
		Allowlist:    cfg.Infra.Broker.Allowlist,
		Logger:       log,
		Telemetry:    tel,
	}, backend, reg, nc, loop, store, js)
	if err != nil {
		return nil, err
	}

	// The gate verifies in a fresh sandbox (producer != verifier): a clean checkout of
	// the candidate branch graded against the producing stage's declared postconditions.
	// The registry (config's `checks` map) is how those postconditions resolve to the
	// commands run — the checks are data, not code. It shares the same artifact store as
	// the runner: each check's stdout/stderr is harvested there before teardown so the
	// provenance trailer can cite the evidence by hash.
	gateRunner := gate.New(backend, gate.Registry(cfg.Harness.Checks), store, sockDir, log, tel)

	orch, err := orchestrator.New(orchestrator.Options{
		Config:    cfg,
		Repo:      repo,
		Base:      "main",
		Limits:    cfg.Infra.Sandbox.Limits,
		Logger:    log,
		Telemetry: tel,
	}, beads.New(beads.WithBinary(opts.bdBin), beads.WithDir(repo)), gateRunner, orchestrator.NewGitMerger("", orchestrator.WithSigningKey(signingKey(cfg))), nc, js)
	if err != nil {
		return nil, err
	}

	return &runComponents{orch: orch, rnr: rnr, server: server, cleanup: cleanupAll}, nil
}

// defaultBranch returns the repo's current branch name, used as the base ref the planner's
// read-only exploration sandbox is seeded at when requirements_planner.base_ref is unset
// (T4.28). It must not hardcode "main": the harness repo is "master" and a target repo may use
// either. On any failure it falls back to "main" (the harness convention) — exploration will
// then degrade loudly at provision time if that ref does not exist, which is the right signal.
func defaultBranch(repo string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "rev-parse", "--abbrev-ref", "HEAD") // #nosec G204 -- fixed git binary, repo-scoped, no external input.
	out, err := cmd.Output()
	if err != nil {
		return "main"
	}
	if ref := strings.TrimSpace(string(out)); ref != "" && ref != "HEAD" {
		return ref
	}
	return "main"
}
