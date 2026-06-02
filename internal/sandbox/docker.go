package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Fixed locations and sentinels inside a Docker sandbox. They are constants, not
// config, because they are the backend's internal contract with the container, not
// something an operator tunes — the agent and the workspace tools always find the
// worktree and the broker socket at the same place.
const (
	// containerWorkdir is where the seeded git worktree lives inside the container
	// and the default working directory for every Exec.
	containerWorkdir = "/workspace"

	// containerBrokerSock is where the runner's broker socket is mounted inside the
	// container. This single mount is the deliberate, only route out of the sandbox —
	// the Docker analog of the Firecracker vsock — not a casual bind mount.
	containerBrokerSock = "/run/harness/broker.sock"

	// keepAliveSeconds keeps the container alive between Exec calls. The container's
	// entrypoint is `sleep <this>`; a fresh `docker exec` runs each Command against
	// the same long-lived container and seeded worktree. ~68 years (max int32) — both
	// GNU and busybox sleep accept it, so it works regardless of the profile image.
	keepAliveSeconds = "2147483647"

	// dockerCLIError is Docker's exit code when the CLI/daemon itself fails to run an
	// `exec` (e.g. the container is gone) — distinct from the inner command's own
	// exit code. Exec surfaces this as a Go error so callers can tell "could not run
	// the command" from "command ran and failed" (see Sandbox.Exec).
	dockerCLIError = 125
)

// dockerRunFunc runs one `docker` subcommand and returns its stdout, stderr, the
// process exit code, and a launch error. The launch error is non-nil ONLY when the
// command could not be run at all (docker binary missing, context canceled); a
// non-zero exit of a command that *did* run is reported in exitCode with a nil
// error. This split is what lets Exec distinguish a failed-but-ran command from a
// dead sandbox. It is a seam so the arg-shaping and control flow can be unit-tested
// against canned output without a Docker daemon.
type dockerRunFunc func(ctx context.Context, stdin []byte, args ...string) (stdout, stderr []byte, exitCode int, err error)

// DockerBackend provisions sandboxes as Docker containers by shelling out to the
// `docker` CLI. Docker shares the host kernel, so it is local-dev-only (see
// specs/components/sandbox.md); it satisfies the same microVM-shaped Backend
// contract as the Firecracker production target, so swapping backends is config,
// not a rewrite. It holds no per-sandbox state — every sandbox is a self-contained
// handle.
type DockerBackend struct {
	bin string
	run dockerRunFunc
	// prepareWorktree materializes a writable git worktree at the brief's base ref on
	// the host and returns its directory plus a cleanup func. The contents are copied
	// into the container with `docker cp`; the host copy is removed afterward. It is a
	// seam so Provision's arg-shaping can be unit-tested without real git.
	prepareWorktree func(ctx context.Context, ws Workspace) (hostDir string, cleanup func(), err error)
}

// DockerOption configures a DockerBackend.
type DockerOption func(*DockerBackend)

// WithDockerBinary sets the docker executable to invoke (default "docker", resolved
// on PATH).
func WithDockerBinary(path string) DockerOption {
	return func(b *DockerBackend) { b.bin = path }
}

// NewDockerBackend builds a DockerBackend. With no options it invokes "docker" from
// PATH and seeds worktrees with a real `git clone`.
func NewDockerBackend(opts ...DockerOption) *DockerBackend {
	b := &DockerBackend{bin: "docker"}
	for _, o := range opts {
		o(b)
	}
	if b.run == nil {
		b.run = execDocker(b.bin)
	}
	if b.prepareWorktree == nil {
		b.prepareWorktree = defaultPrepareWorktree
	}
	return b
}

var _ Backend = (*DockerBackend)(nil)

// Provision creates one container: validate the spec, boot a network-less container
// with the resource ceiling and the single broker-socket mount, then seed the
// writable worktree at the base ref. On any failure after the container exists it is
// torn down, so a failed Provision leaks nothing. The returned Sandbox is live and
// MUST be Teardown'd by the caller.
func (b *DockerBackend) Provision(ctx context.Context, spec Spec) (Sandbox, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	// Docker's local channel is a unix domain socket (vsock is Firecracker). Reject
	// anything else loudly rather than silently ignoring the transport.
	if spec.Broker.Network != "unix" {
		return nil, fmt.Errorf("sandbox: docker backend requires a unix broker transport, got %q", spec.Broker.Network)
	}
	// The broker socket must already exist: a bind mount whose source is missing makes
	// Docker create an empty *directory* in its place, which would silently break the
	// one route out. Failing here keeps the channel honest.
	if fi, err := os.Stat(spec.Broker.Address); err != nil {
		return nil, fmt.Errorf("sandbox: broker socket %s: %w", spec.Broker.Address, err)
	} else if fi.IsDir() {
		return nil, fmt.Errorf("sandbox: broker address %s is a directory, want a socket", spec.Broker.Address)
	}

	memBytes, err := parseQuantity(spec.Limits.Mem)
	if err != nil {
		return nil, fmt.Errorf("sandbox: parse mem limit %q: %w", spec.Limits.Mem, err)
	}

	name, err := containerName()
	if err != nil {
		return nil, err
	}

	// The bootable image is the concrete artifact resolved from the soul's logical
	// profile via the infra sandbox.profiles registry (see config.ResolveImage). When
	// unset (test-only specs constructed directly), fall back to the profile name — the
	// historical "name == image tag" behavior.
	image := spec.Image
	if image == "" {
		image = spec.Profile
	}

	// --network none is the zero-network invariant enforced by construction: the
	// container gets only loopback, so the bind-mounted broker socket is the sole way
	// out. Disk is not enforced here — Docker disk quotas need a specific storage
	// driver (storage-opt); it is optional in the contract and left to the production
	// (Firecracker) backend.
	runArgs := []string{
		"run", "-d", "--init",
		"--network", "none",
		"--name", name,
		"--cpus", strconv.Itoa(spec.Limits.CPU),
		"--memory", strconv.FormatInt(memBytes, 10),
		"-w", containerWorkdir,
		"-v", spec.Broker.Address + ":" + containerBrokerSock,
		image,
		"sleep", keepAliveSeconds,
	}
	out, err := b.dockerOK(ctx, nil, runArgs...)
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return nil, errors.New("sandbox: docker run returned no container id")
	}

	sb := &dockerSandbox{id: id, run: b.run, workdir: containerWorkdir, done: make(chan struct{})}

	hostDir, cleanup, err := b.prepareWorktree(ctx, spec.Workspace)
	if err != nil {
		_ = sb.Teardown(context.Background())
		return nil, fmt.Errorf("sandbox: prepare worktree: %w", err)
	}
	defer cleanup()

	// Seed by copying the worktree contents into the container — explicit seeding, not
	// a live mount of the host repo. The candidate branch later leaves only via the
	// runner's brokered git push; nothing reaches back into the container.
	if _, err := b.dockerOK(ctx, nil, "cp", hostDir+"/.", id+":"+containerWorkdir); err != nil {
		_ = sb.Teardown(context.Background())
		return nil, fmt.Errorf("sandbox: seed worktree: %w", err)
	}

	// Wall-clock is the termination guarantee (see specs/workflow.md). Docker has no
	// native wall-clock kill, so the backend reaps the container itself once the
	// budget elapses.
	sb.startWatchdog(spec.Limits.Wall.Duration())
	return sb, nil
}

// dockerOK runs a docker subcommand that must succeed (run/cp/rm), folding a
// non-zero exit or launch failure into a single error with stderr for legibility.
func (b *DockerBackend) dockerOK(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	out, stderr, code, err := b.run(ctx, stdin, args...)
	if err != nil {
		return out, fmt.Errorf("sandbox: docker %s: %w", args[0], err)
	}
	if code != 0 {
		return out, fmt.Errorf("sandbox: docker %s exited %d: %s", args[0], code, strings.TrimSpace(string(stderr)))
	}
	return out, nil
}

// dockerSandbox is one live container handle. It carries no business state beyond
// what teardown and wall-enforcement need.
type dockerSandbox struct {
	id      string
	run     dockerRunFunc
	workdir string

	deadline time.Time // wall-clock deadline; zero means unset (never, in practice)
	once     sync.Once
	done     chan struct{} // closed exactly once by Teardown to stop the watchdog
}

var _ Sandbox = (*dockerSandbox)(nil)

func (s *dockerSandbox) ID() string { return s.id }

// startWatchdog arms a timer that reaps the container when the wall-clock budget
// elapses, unless Teardown happens first. This is the outside-the-sandbox half of
// the termination guarantee; the per-Exec deadline in Exec is the other half.
func (s *dockerSandbox) startWatchdog(wall time.Duration) {
	s.deadline = time.Now().Add(wall)
	go func() {
		t := time.NewTimer(wall)
		defer t.Stop()
		select {
		case <-t.C:
			_ = s.Teardown(context.Background())
		case <-s.done:
		}
	}()
}

// Exec runs one Command inside the container via `docker exec` against the seeded
// worktree. A non-zero exit of the command is returned in ExecResult.ExitCode, not
// as an error; a Go error means the command could not be run (the sandbox is gone,
// or the wall-clock budget is exhausted).
func (s *dockerSandbox) Exec(ctx context.Context, cmd Command) (ExecResult, error) {
	// Cap the command at the remaining wall budget so a single runaway invocation
	// cannot outlive the sandbox even before the watchdog reaps it.
	if !s.deadline.IsZero() {
		if time.Now().After(s.deadline) {
			return ExecResult{}, fmt.Errorf("sandbox %s: wall-clock budget exhausted", s.id)
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, s.deadline)
		defer cancel()
	}

	workdir := s.workdir
	if cmd.Dir != "" {
		workdir = path.Join(s.workdir, cmd.Dir)
	}
	args := []string{"exec", "-w", workdir}
	for _, e := range cmd.Env {
		args = append(args, "-e", e)
	}
	if len(cmd.Stdin) > 0 {
		args = append(args, "-i")
	}
	args = append(args, s.id, cmd.Path)
	args = append(args, cmd.Args...)

	stdout, stderr, code, err := s.run(ctx, cmd.Stdin, args...)
	if err != nil {
		return ExecResult{}, fmt.Errorf("sandbox %s: exec %s: %w", s.id, cmd.Path, err)
	}
	if code == dockerCLIError {
		return ExecResult{}, fmt.Errorf("sandbox %s: docker could not exec (container gone?): %s", s.id, strings.TrimSpace(string(stderr)))
	}
	return ExecResult{ExitCode: code, Stdout: stdout, Stderr: stderr}, nil
}

// Teardown force-removes the container and stops the watchdog. It is idempotent
// (guarded by sync.Once) and safe on a partially-provisioned sandbox: a sandbox with
// no id (container never created) just stops the watchdog and returns. `docker rm
// -f` removes a running container unconditionally — no state survives.
func (s *dockerSandbox) Teardown(ctx context.Context) error {
	var rerr error
	s.once.Do(func() {
		close(s.done)
		if s.id == "" {
			return
		}
		_, stderr, code, err := s.run(ctx, nil, "rm", "-f", s.id)
		if err != nil {
			rerr = fmt.Errorf("sandbox %s: docker rm: %w", s.id, err)
			return
		}
		if code != 0 {
			rerr = fmt.Errorf("sandbox %s: docker rm exited %d: %s", s.id, code, strings.TrimSpace(string(stderr)))
		}
	})
	return rerr
}

// execDocker is the real dockerRunFunc: it runs the docker binary and reports a
// launch error only when the process could not run at all (so a non-zero command
// exit comes back as exitCode with a nil error).
func execDocker(bin string) dockerRunFunc {
	return func(ctx context.Context, stdin []byte, args ...string) ([]byte, []byte, int, error) {
		cmd := exec.CommandContext(ctx, bin, args...)
		if len(stdin) > 0 {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if ctx.Err() != nil {
			return stdout.Bytes(), stderr.Bytes(), -1, ctx.Err()
		}
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return stdout.Bytes(), stderr.Bytes(), ee.ExitCode(), nil
			}
			return stdout.Bytes(), stderr.Bytes(), -1, err
		}
		return stdout.Bytes(), stderr.Bytes(), 0, nil
	}
}

// defaultPrepareWorktree clones the repo and checks out the base ref into a temp
// directory on the host. A full clone (not an archive) is used so the seeded
// worktree has a writable .git: the agent commits to a candidate branch that the
// runner later pushes via the broker. --no-hardlinks keeps the temp copy independent
// of a local source repo so removing it never touches the original's objects.
func defaultPrepareWorktree(ctx context.Context, ws Workspace) (string, func(), error) {
	dir, err := os.MkdirTemp("", "harness-seed-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := runGit(ctx, "", "clone", "--quiet", "--no-hardlinks", ws.Repo, dir); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := runGit(ctx, dir, "checkout", "--quiet", ws.BaseRef); err != nil {
		cleanup()
		return "", nil, err
	}
	return dir, cleanup, nil
}

func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// containerName returns a unique container name so concurrent sandboxes never
// collide. The handle still addresses the container by the id Docker returns; the
// name is for human-legible `docker ps` output.
func containerName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("sandbox: generate container name: %w", err)
	}
	return "harness-sbx-" + hex.EncodeToString(b[:]), nil
}

// parseQuantity converts a k8s-style memory quantity (the form config uses, e.g.
// "2Gi", "512Mi", "1G", "1048576") to a byte count Docker's --memory understands as
// a bare integer. Binary suffixes (Ki/Mi/Gi/Ti/Pi) are powers of 1024; decimal
// suffixes (k/K/M/G/T/P) are powers of 1000; a bare number is bytes.
func parseQuantity(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty quantity")
	}
	type suffix struct {
		s   string
		mul int64
	}
	// Binary suffixes end in "i" and must be checked before the 1-char decimal ones so
	// "2Gi" matches "Gi", not "G".
	binary := []suffix{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"Pi", 1 << 50},
	}
	decimal := []suffix{
		{"k", 1000}, {"K", 1000}, {"M", 1_000_000}, {"G", 1_000_000_000},
		{"T", 1_000_000_000_000}, {"P", 1_000_000_000_000_000},
	}
	for _, u := range binary {
		if strings.HasSuffix(s, u.s) {
			return parseMul(strings.TrimSuffix(s, u.s), u.mul)
		}
	}
	for _, u := range decimal {
		if strings.HasSuffix(s, u.s) {
			return parseMul(strings.TrimSuffix(s, u.s), u.mul)
		}
	}
	return parseMul(s, 1)
}

func parseMul(num string, mul int64) (int64, error) {
	num = strings.TrimSpace(num)
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid quantity %q", num)
	}
	if n < 0 {
		return 0, fmt.Errorf("negative quantity %q", num)
	}
	return n * mul, nil
}
