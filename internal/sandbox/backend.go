package sandbox

import (
	"fmt"

	"github.com/Loxstomper/harness/internal/config"
)

// gvisorRuntime is the OCI runtime name gVisor registers with Docker/containerd
// (`docker run --runtime=runsc`). The gVisor backend is the Docker provisioning path
// pinned to this runtime — same image, same unix-socket broker, same worktree seeding
// and wall-clock watchdog — so a poisoned-toolchain build runs under gVisor's
// user-space kernel instead of the shared host kernel, with no second provisioning
// implementation to keep in step. See specs/components/sandbox.md "Pluggable backends".
const gvisorRuntime = "runsc"

// NewGVisorBackend builds the medium-trust gVisor backend: a DockerBackend that boots
// every container under the runsc runtime. gVisor interposes a user-space kernel
// between the container and the host kernel, so it isolates untrusted code more
// strongly than plain Docker (shared host kernel) while booting an ordinary container
// image — which is why it reuses the entire Docker backend rather than duplicating it.
// runsc must be registered as a Docker runtime on the host (install gVisor, then
// `docker run --runtime=runsc`); when it is not, Provision fails loudly at `docker run`
// rather than silently degrading to runc.
func NewGVisorBackend(opts ...DockerOption) *DockerBackend {
	return NewDockerBackend(append([]DockerOption{WithRuntime(gvisorRuntime)}, opts...)...)
}

// NewBackend builds the sandbox backend the infra config selects — the seam that makes
// "the backend is config, not code" (specs/components/sandbox.md) actually hold: the
// runner/gate/wizard depend only on the Backend interface, and which concrete one they
// get is decided here from sandbox.backend.
//
//   - "docker" (and "", the test/unconfigured default) -> the weak shared-kernel Docker
//     backend, fine for local dev and human-reviewed runs.
//   - "gvisor" -> the medium-trust gVisor backend (Docker under the runsc runtime).
//   - "firecracker" -> not yet available: the production microVM backend (T5.2) is
//     hardware-blocked (needs KVM) and unbuilt, so selecting it fails CLOSED here rather
//     than silently handing back a weaker backend than the operator asked for.
//
// An unknown value is rejected; config.Validate catches it earlier, but failing here too
// keeps the factory honest if it is ever called on an unvalidated config.
func NewBackend(cfg config.SandboxConfig, opts ...DockerOption) (Backend, error) {
	switch cfg.Backend {
	case "", config.BackendDocker:
		return NewDockerBackend(opts...), nil
	case config.BackendGVisor:
		return NewGVisorBackend(opts...), nil
	case config.BackendFirecracker:
		return nil, fmt.Errorf("sandbox: the %q backend is not yet available (hardware-blocked; needs KVM — see T5.2); configure %q or %q", config.BackendFirecracker, config.BackendDocker, config.BackendGVisor)
	default:
		return nil, fmt.Errorf("sandbox: unknown backend %q (want %q, %q, or %q)", cfg.Backend, config.BackendDocker, config.BackendGVisor, config.BackendFirecracker)
	}
}
