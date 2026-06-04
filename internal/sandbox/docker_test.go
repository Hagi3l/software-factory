package sandbox

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Loxstomper/harness/internal/config"
)

// --- Unit tests: arg shapes and control flow via injected seams (no docker/git) ---

func TestParseQuantity(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"2Gi", 2 << 30, false},
		{"512Mi", 512 << 20, false},
		{"1Gi", 1 << 30, false},
		{"8Gi", 8 << 30, false},
		{"64Mi", 64 << 20, false},
		{"1Ki", 1024, false},
		{"1G", 1_000_000_000, false},
		{"2G", 2_000_000_000, false},
		{"1k", 1000, false},
		{"1K", 1000, false},
		{"1048576", 1048576, false},
		{"  2Gi  ", 2 << 30, false},
		{"", 0, true},
		{"abc", 0, true},
		{"-1Gi", 0, true},
		{"1.5Gi", 0, true}, // non-integer numeric part is rejected
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseQuantity(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseQuantity(%q) = %d, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseQuantity(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parseQuantity(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// recordingBackend wires a DockerBackend whose docker calls are recorded and whose
// worktree prep is a no-op stub, so Provision/Exec/Teardown can be exercised with no
// daemon. seed names the host dir the cp should copy from.
func recordingBackend(t *testing.T, seed string) (*DockerBackend, *[][]string) {
	t.Helper()
	var calls [][]string
	b := NewDockerBackend()
	b.run = func(_ context.Context, _ []byte, args ...string) ([]byte, []byte, int, error) {
		calls = append(calls, args)
		// `docker run -d` prints the new container id on stdout.
		if len(args) > 0 && args[0] == "run" {
			return []byte("container-abc\n"), nil, 0, nil
		}
		return nil, nil, 0, nil
	}
	b.prepareWorktree = func(_ context.Context, _ Workspace) (string, func(), error) {
		return seed, func() {}, nil
	}
	return b, &calls
}

// unitSpec is a spec whose broker address points at a real, existing socket file so
// Provision's socket-exists check passes without a running runner.
func unitSpec(t *testing.T) Spec {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "broker.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatalf("create fake socket: %v", err)
	}
	return Spec{
		Profile:   "busybox:latest",
		Workspace: Workspace{Repo: "/srv/repo.git", BaseRef: "main"},
		Limits:    config.SandboxLimits{CPU: 2, Mem: "2Gi", Wall: config.Duration(30 * time.Minute)},
		Broker:    Endpoint{Network: "unix", Address: sock},
	}
}

func TestProvisionArgShapes(t *testing.T) {
	b, calls := recordingBackend(t, "/tmp/seed")
	spec := unitSpec(t)

	sb, err := b.Provision(context.Background(), spec)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if sb.ID() != "container-abc" {
		t.Errorf("ID = %q, want container-abc", sb.ID())
	}
	if len(*calls) != 3 {
		t.Fatalf("got %d docker calls, want 3 (run, cp, chown): %v", len(*calls), *calls)
	}

	run := strings.Join((*calls)[0], " ")
	for _, frag := range []string{
		"run -d --init",
		"--network none",
		"--cpus 2",
		"--memory " + // 2Gi in bytes
			"2147483648",
		"-w /workspace",
		"-v " + spec.Broker.Address + ":/run/harness/broker.sock",
		"busybox:latest sleep " + keepAliveSeconds,
	} {
		if !strings.Contains(run, frag) {
			t.Errorf("run args missing %q\n got: %s", frag, run)
		}
	}

	cp := strings.Join((*calls)[1], " ")
	if want := "cp /tmp/seed/. container-abc:/workspace"; cp != want {
		t.Errorf("cp args = %q, want %q", cp, want)
	}

	// The seeded worktree is re-owned to the container's exec user (T5.4) so git's
	// dubious-ownership guard never fires — replacing the image's safe.directory crutch.
	chown := (*calls)[2]
	wantChown := []string{"exec", "container-abc", "sh", "-c", chownWorktreeCmd}
	if strings.Join(chown, "\x00") != strings.Join(wantChown, "\x00") {
		t.Errorf("chown args = %v, want %v", chown, wantChown)
	}
}

func TestProvisionRejectsNonUnixBroker(t *testing.T) {
	b, calls := recordingBackend(t, "/tmp/seed")
	spec := unitSpec(t)
	spec.Broker.Network = "vsock"
	spec.Broker.Address = "3:5000"
	if _, err := b.Provision(context.Background(), spec); err == nil {
		t.Fatal("Provision accepted a non-unix broker")
	}
	if len(*calls) != 0 {
		t.Errorf("docker was called for a rejected spec: %v", *calls)
	}
}

func TestProvisionRequiresExistingSocket(t *testing.T) {
	b, calls := recordingBackend(t, "/tmp/seed")
	spec := unitSpec(t)
	spec.Broker.Address = filepath.Join(t.TempDir(), "does-not-exist.sock")
	if _, err := b.Provision(context.Background(), spec); err == nil {
		t.Fatal("Provision accepted a missing broker socket")
	}
	if len(*calls) != 0 {
		t.Errorf("docker was called despite a missing socket: %v", *calls)
	}
}

// A directory at the broker address is rejected (the auto-created-dir footgun).
func TestProvisionRejectsDirectoryBroker(t *testing.T) {
	b, _ := recordingBackend(t, "/tmp/seed")
	spec := unitSpec(t)
	spec.Broker.Address = t.TempDir() // a directory
	if _, err := b.Provision(context.Background(), spec); err == nil {
		t.Fatal("Provision accepted a directory as the broker socket")
	}
}

func TestProvisionInvalidSpec(t *testing.T) {
	b, calls := recordingBackend(t, "/tmp/seed")
	if _, err := b.Provision(context.Background(), Spec{}); err == nil {
		t.Fatal("Provision accepted an invalid spec")
	}
	if len(*calls) != 0 {
		t.Errorf("docker was called for an invalid spec: %v", *calls)
	}
}

// A docker run failure surfaces as an error and yields no sandbox.
func TestProvisionRunFailure(t *testing.T) {
	b := NewDockerBackend()
	b.prepareWorktree = func(_ context.Context, _ Workspace) (string, func(), error) {
		return "/tmp/seed", func() {}, nil
	}
	b.run = func(_ context.Context, _ []byte, args ...string) ([]byte, []byte, int, error) {
		return nil, []byte("no such image"), 125, nil
	}
	if _, err := b.Provision(context.Background(), unitSpec(t)); err == nil {
		t.Fatal("Provision did not fail when docker run failed")
	}
}

// A failed worktree seed (docker cp) must tear the container back down.
func TestProvisionSeedFailureTearsDown(t *testing.T) {
	var calls [][]string
	b := NewDockerBackend()
	b.prepareWorktree = func(_ context.Context, _ Workspace) (string, func(), error) {
		return "/tmp/seed", func() {}, nil
	}
	b.run = func(_ context.Context, _ []byte, args ...string) ([]byte, []byte, int, error) {
		calls = append(calls, args)
		switch args[0] {
		case "run":
			return []byte("container-abc\n"), nil, 0, nil
		case "cp":
			return nil, []byte("cp failed"), 1, nil
		default:
			return nil, nil, 0, nil
		}
	}
	if _, err := b.Provision(context.Background(), unitSpec(t)); err == nil {
		t.Fatal("Provision did not fail when seeding failed")
	}
	// Expect run, cp (failed), then rm (cleanup).
	if len(calls) != 3 || calls[2][0] != "rm" {
		t.Fatalf("expected run, cp, rm; got %v", calls)
	}
	if calls[2][1] != "-f" || calls[2][2] != "container-abc" {
		t.Errorf("teardown args = %v, want [rm -f container-abc]", calls[2])
	}
}

// A failed worktree chown (the T5.4 ownership fix) must also tear the container back
// down — a worktree git refuses to operate on is no use, so the provision fails closed.
func TestProvisionChownFailureTearsDown(t *testing.T) {
	var calls [][]string
	b := NewDockerBackend()
	b.prepareWorktree = func(_ context.Context, _ Workspace) (string, func(), error) {
		return "/tmp/seed", func() {}, nil
	}
	b.run = func(_ context.Context, _ []byte, args ...string) ([]byte, []byte, int, error) {
		calls = append(calls, args)
		switch args[0] {
		case "run":
			return []byte("container-abc\n"), nil, 0, nil
		case "exec":
			return nil, []byte("chown: operation not permitted"), 1, nil
		default: // cp, rm
			return nil, nil, 0, nil
		}
	}
	if _, err := b.Provision(context.Background(), unitSpec(t)); err == nil {
		t.Fatal("Provision did not fail when the worktree chown failed")
	}
	// Expect run, cp, exec(chown, failed), then rm (cleanup).
	if len(calls) != 4 || calls[3][0] != "rm" {
		t.Fatalf("expected run, cp, exec, rm; got %v", calls)
	}
	if calls[3][1] != "-f" || calls[3][2] != "container-abc" {
		t.Errorf("teardown args = %v, want [rm -f container-abc]", calls[3])
	}
}

func TestExecArgShapesAndResult(t *testing.T) {
	var got []string
	s := &dockerSandbox{
		id:      "container-abc",
		workdir: "/workspace",
		done:    make(chan struct{}),
		run: func(_ context.Context, stdin []byte, args ...string) ([]byte, []byte, int, error) {
			got = args
			return []byte("hi"), []byte("warn"), 7, nil
		},
	}
	res, err := s.Exec(context.Background(), Command{
		Path: "go",
		Args: []string{"test", "./..."},
		Dir:  "sub/pkg",
		Env:  []string{"CGO_ENABLED=0"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	// Non-zero exit is a normal result, not an error.
	if res.ExitCode != 7 || string(res.Stdout) != "hi" || string(res.Stderr) != "warn" {
		t.Errorf("result = %+v", res)
	}
	want := []string{"exec", "-w", "/workspace/sub/pkg", "-e", "CGO_ENABLED=0", "container-abc", "go", "test", "./..."}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("exec args = %v, want %v", got, want)
	}
}

// Stdin presence adds -i to the exec invocation and is passed through.
func TestExecStdin(t *testing.T) {
	var gotArgs []string
	var gotStdin []byte
	s := &dockerSandbox{
		id: "c", workdir: "/workspace", done: make(chan struct{}),
		run: func(_ context.Context, stdin []byte, args ...string) ([]byte, []byte, int, error) {
			gotArgs, gotStdin = args, stdin
			return nil, nil, 0, nil
		},
	}
	if _, err := s.Exec(context.Background(), Command{Path: "cat", Stdin: []byte("data")}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !containsArg(gotArgs, "-i") {
		t.Errorf("expected -i in args, got %v", gotArgs)
	}
	if string(gotStdin) != "data" {
		t.Errorf("stdin = %q, want data", gotStdin)
	}
}

// Docker's 125 (CLI/daemon could not exec) becomes a Go error, distinct from the
// command running and exiting non-zero.
func TestExecContainerGone(t *testing.T) {
	s := &dockerSandbox{
		id: "c", workdir: "/workspace", done: make(chan struct{}),
		run: func(_ context.Context, _ []byte, _ ...string) ([]byte, []byte, int, error) {
			return nil, []byte("No such container"), dockerCLIError, nil
		},
	}
	if _, err := s.Exec(context.Background(), Command{Path: "true"}); err == nil {
		t.Fatal("Exec did not error when docker reported the container gone")
	}
}

// A launch failure (docker binary missing / ctx canceled) is a Go error.
func TestExecLaunchError(t *testing.T) {
	s := &dockerSandbox{
		id: "c", workdir: "/workspace", done: make(chan struct{}),
		run: func(_ context.Context, _ []byte, _ ...string) ([]byte, []byte, int, error) {
			return nil, nil, -1, errors.New("exec: docker not found")
		},
	}
	if _, err := s.Exec(context.Background(), Command{Path: "true"}); err == nil {
		t.Fatal("Exec did not propagate a launch error")
	}
}

// Once the wall-clock deadline has passed, Exec refuses to run without touching docker.
func TestExecWallExhausted(t *testing.T) {
	called := false
	s := &dockerSandbox{
		id: "c", workdir: "/workspace", done: make(chan struct{}),
		deadline: time.Now().Add(-time.Second),
		run: func(_ context.Context, _ []byte, _ ...string) ([]byte, []byte, int, error) {
			called = true
			return nil, nil, 0, nil
		},
	}
	if _, err := s.Exec(context.Background(), Command{Path: "true"}); err == nil {
		t.Fatal("Exec ran past the wall-clock deadline")
	}
	if called {
		t.Error("Exec invoked docker despite an exhausted wall budget")
	}
}

func TestTeardownIdempotent(t *testing.T) {
	var rmCount int
	s := &dockerSandbox{
		id: "container-abc", workdir: "/workspace", done: make(chan struct{}),
		run: func(_ context.Context, _ []byte, args ...string) ([]byte, []byte, int, error) {
			if len(args) > 0 && args[0] == "rm" {
				rmCount++
			}
			return nil, nil, 0, nil
		},
	}
	if err := s.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if err := s.Teardown(context.Background()); err != nil {
		t.Fatalf("second Teardown: %v", err)
	}
	if rmCount != 1 {
		t.Errorf("docker rm called %d times, want exactly 1 (idempotent)", rmCount)
	}
}

// The watchdog reaps the container once the wall budget elapses.
func TestWatchdogReaps(t *testing.T) {
	reaped := make(chan struct{}, 1)
	s := &dockerSandbox{
		id: "container-abc", workdir: "/workspace", done: make(chan struct{}),
		run: func(_ context.Context, _ []byte, args ...string) ([]byte, []byte, int, error) {
			if len(args) > 0 && args[0] == "rm" {
				select {
				case reaped <- struct{}{}:
				default:
				}
			}
			return nil, nil, 0, nil
		},
	}
	s.startWatchdog(10 * time.Millisecond)
	select {
	case <-reaped:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not reap the container after the wall budget elapsed")
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// --- Integration tests: real docker + git, skipped when unavailable ---

func requireDockerAndGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH; skipping docker sandbox integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; skipping docker sandbox integration test")
	}
	// A reachable daemon is required; `docker info` is cheap and fails fast otherwise.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skipf("docker daemon not reachable; skipping: %v", err)
	}
}

// seedRepo builds a tiny git repo on branch main with one committed file and returns
// its path. It is the source the sandbox worktree is seeded from.
func seedRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("seeded\n"), 0o600); err != nil {
		t.Fatalf("write hello.txt: %v", err)
	}
	run("add", "hello.txt")
	run("commit", "-q", "-m", "seed")
	return dir
}

func TestDockerSandboxIntegration(t *testing.T) {
	requireDockerAndGit(t)

	// A real listening unix socket stands in for the runner's broker; the sandbox
	// only needs the socket file to exist so the mount attaches it.
	sockDir := t.TempDir()
	sock := filepath.Join(sockDir, "broker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen on broker socket: %v", err)
	}
	defer ln.Close()

	spec := Spec{
		Profile:   "busybox:latest",
		Workspace: Workspace{Repo: seedRepo(t), BaseRef: "main"},
		Limits:    config.SandboxLimits{CPU: 1, Mem: "64Mi", Wall: config.Duration(2 * time.Minute)},
		Broker:    Endpoint{Network: "unix", Address: sock},
	}

	ctx := context.Background()
	be := NewDockerBackend()
	sb, err := be.Provision(ctx, spec)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	id := sb.ID()
	tornDown := false
	defer func() {
		if !tornDown {
			_ = sb.Teardown(context.Background())
		}
	}()

	// The worktree is seeded at the base ref, with a writable .git (full clone).
	mustExec(t, sb, "the seeded file is present", Command{Path: "cat", Args: []string{"hello.txt"}}, "seeded")
	mustExec(t, sb, "the worktree has a .git", Command{Path: "sh", Args: []string{"-c", "test -d /workspace/.git && echo ok"}}, "ok")

	// T5.4: the seeded worktree is re-owned to the container's exec user (uid 0, the
	// toolchain image's default), not the host uid `docker cp` would otherwise leave —
	// so git's dubious-ownership guard never fires without the safe.directory crutch.
	mustExec(t, sb, "the worktree is owned by the container user",
		Command{Path: "sh", Args: []string{"-c", `test "$(stat -c %u /workspace/.git)" = "$(id -u)" && echo ok`}}, "ok")

	// The worktree is writable.
	mustExec(t, sb, "the worktree is writable", Command{Path: "sh", Args: []string{"-c", "echo x > newfile && cat newfile"}}, "x")

	// The broker socket is the one wired channel.
	mustExec(t, sb, "the broker socket is mounted", Command{Path: "sh", Args: []string{"-c", "test -S /run/harness/broker.sock && echo ok"}}, "ok")

	// Zero direct network: `--network none` attaches no usable interface. The invariant
	// is "no non-loopback interface is UP", not "only lo exists": on some hosts the
	// kernel auto-creates down, address-less tunnel pseudo-devices (gre0, sit0, tunl0,
	// erspan0, …) in every netns, which carry no traffic. A real egress interface (a
	// bridged eth0) would be UP, so operstate is the property that proves isolation.
	res, err := sb.Exec(ctx, Command{Path: "sh", Args: []string{"-c",
		`for i in /sys/class/net/*; do echo "$(basename "$i") $(cat "$i/operstate")"; done`}})
	if err != nil {
		t.Fatalf("Exec(enumerate interfaces): %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(res.Stdout)), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] != "lo" && f[1] == "up" {
			t.Errorf("non-loopback interface %q is up, want zero-network isolation; interfaces:\n%s", f[0], res.Stdout)
		}
	}

	// A non-zero exit is a result, not an error.
	res, err = sb.Exec(ctx, Command{Path: "sh", Args: []string{"-c", "exit 3"}})
	if err != nil {
		t.Fatalf("Exec(exit 3) returned a Go error, want exit code in result: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}

	// Deterministic teardown: the container is gone afterward, and teardown is idempotent.
	if err := sb.Teardown(ctx); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	tornDown = true
	if err := sb.Teardown(ctx); err != nil {
		t.Fatalf("second Teardown: %v", err)
	}
	if exec.Command("docker", "inspect", id).Run() == nil {
		t.Errorf("container %s still exists after teardown", id)
	}
}

func mustExec(t *testing.T, sb Sandbox, what string, cmd Command, wantSubstr string) {
	t.Helper()
	res, err := sb.Exec(context.Background(), cmd)
	if err != nil {
		t.Fatalf("Exec for %q: %v", what, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("Exec for %q exited %d: %s", what, res.ExitCode, res.Stderr)
	}
	if !strings.Contains(string(res.Stdout), wantSubstr) {
		t.Errorf("Exec for %q: stdout %q does not contain %q", what, res.Stdout, wantSubstr)
	}
}
