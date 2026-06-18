package sandbox

import (
	"context"
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/config"
)

// The gVisor backend boots the container under the runsc runtime: the `docker run` argv
// must carry `--runtime runsc` (before the image), and nothing else about provisioning
// changes — it reuses the Docker path, so this is verifiable here with no daemon, via the
// recorded run seam.
func TestGVisorProvisionPinsRunscRuntime(t *testing.T) {
	b := NewGVisorBackend()
	var calls [][]string
	b.run = func(_ context.Context, _ []byte, args ...string) ([]byte, []byte, int, error) {
		calls = append(calls, args)
		if len(args) > 0 && args[0] == "run" {
			return []byte("container-abc\n"), nil, 0, nil
		}
		return nil, nil, 0, nil
	}
	b.prepareWorktree = func(_ context.Context, _ Workspace) (string, func(), error) {
		return "/tmp/seed", func() {}, nil
	}

	if _, err := b.Provision(context.Background(), unitSpec(t)); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	run := strings.Join(calls[0], " ")
	if !strings.Contains(run, "--runtime "+gvisorRuntime) {
		t.Errorf("gVisor run args missing %q\n got: %s", "--runtime "+gvisorRuntime, run)
	}
	// The runtime flag must precede the image (it is a `docker run` flag, not a container arg).
	if ri, ii := strings.Index(run, "--runtime"), strings.Index(run, "busybox:latest"); ri < 0 || ii < 0 || ri > ii {
		t.Errorf("--runtime must precede the image: %s", run)
	}
}

// The plain Docker backend does NOT pin a runtime — it keeps Docker's default (runc), so
// no --runtime flag leaks into the argv. Guards against the gVisor change altering the
// dev backend.
func TestDockerProvisionHasNoRuntimeFlag(t *testing.T) {
	b, calls := recordingBackend(t, "/tmp/seed")
	if _, err := b.Provision(context.Background(), unitSpec(t)); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if run := strings.Join((*calls)[0], " "); strings.Contains(run, "--runtime") {
		t.Errorf("docker backend should not pin a runtime, got: %s", run)
	}
}

// NewBackend honors sandbox.backend so the config selection is real, not decorative:
// docker/"" -> Docker (no runtime), gvisor -> gVisor (runsc), firecracker -> a loud
// fail-closed error (hardware-blocked, must not silently degrade), unknown -> error.
func TestNewBackendSelectsByConfig(t *testing.T) {
	t.Run("docker", func(t *testing.T) {
		be, err := NewBackend(config.SandboxConfig{Backend: config.BackendDocker})
		if err != nil {
			t.Fatalf("NewBackend(docker): %v", err)
		}
		if db, ok := be.(*DockerBackend); !ok || db.runtime != "" {
			t.Errorf("docker backend = %#v, want *DockerBackend with no runtime", be)
		}
	})

	t.Run("empty defaults to docker", func(t *testing.T) {
		be, err := NewBackend(config.SandboxConfig{})
		if err != nil {
			t.Fatalf("NewBackend(\"\"): %v", err)
		}
		if db, ok := be.(*DockerBackend); !ok || db.runtime != "" {
			t.Errorf("empty backend = %#v, want *DockerBackend with no runtime", be)
		}
	})

	t.Run("gvisor", func(t *testing.T) {
		be, err := NewBackend(config.SandboxConfig{Backend: config.BackendGVisor})
		if err != nil {
			t.Fatalf("NewBackend(gvisor): %v", err)
		}
		if db, ok := be.(*DockerBackend); !ok || db.runtime != gvisorRuntime {
			t.Errorf("gvisor backend = %#v, want *DockerBackend with runtime=%q", be, gvisorRuntime)
		}
	})

	t.Run("firecracker fails closed", func(t *testing.T) {
		be, err := NewBackend(config.SandboxConfig{Backend: config.BackendFirecracker})
		if err == nil {
			t.Fatalf("NewBackend(firecracker) = %#v, want error (hardware-blocked, must not degrade)", be)
		}
		if be != nil {
			t.Errorf("firecracker backend = %#v, want nil", be)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		if _, err := NewBackend(config.SandboxConfig{Backend: "qemu"}); err == nil {
			t.Fatal("NewBackend(qemu) accepted an unknown backend")
		}
	})
}

// DockerOptions thread through the factory (e.g. a pinned binary), so a gVisor backend
// can still be told which docker CLI to invoke.
func TestNewBackendThreadsOptions(t *testing.T) {
	be, err := NewBackend(config.SandboxConfig{Backend: config.BackendGVisor}, WithDockerBinary("/usr/local/bin/docker"))
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	db, ok := be.(*DockerBackend)
	if !ok {
		t.Fatalf("backend = %#v, want *DockerBackend", be)
	}
	if db.bin != "/usr/local/bin/docker" {
		t.Errorf("bin = %q, want /usr/local/bin/docker", db.bin)
	}
	if db.runtime != gvisorRuntime {
		t.Errorf("runtime = %q, want %q", db.runtime, gvisorRuntime)
	}
}
