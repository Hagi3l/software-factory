//go:build docker_e2e

// Package main's Docker-backed spine e2e. It is behind the docker_e2e build tag so the
// always-on suite (make check / go test ./...) never compiles it — only `make
// test-e2e-docker` runs it. It drives the same spine as TestSpineE2ELocal but through
// the real Docker sandbox backend, so it additionally covers what the non-isolating
// local backend gives up: real isolation, the zero-network container, resource limits,
// and container teardown (see TE.1 in IMPLEMENTATION_PLAN.md, specs/components/sandbox.md).
package main

import (
	"os"
	"os/exec"
	"testing"

	"github.com/Loxstomper/software-factory/internal/sandbox"
)

// e2eDockerImage is the sandbox profile (image) the Docker variant boots. It must have
// git + sh and no entrypoint that swallows the `sleep` keep-alive command — the shipped
// go-toolchain image (deploy/go-toolchain.Dockerfile) satisfies this. Override via the
// HARNESS_E2E_IMAGE env var for a different prebuilt image.
const e2eDockerImage = "go-toolchain"

// TestSpineE2EDocker runs the full spine against the Docker backend. It skips (rather
// than fails) when Docker or the profile image is unavailable, because the image is an
// operator-provided prerequisite of `make test-e2e-docker`, not something the test
// builds — keeping the suite runnable wherever the image is present and a clean skip
// elsewhere.
func TestSpineE2EDocker(t *testing.T) {
	requireBd(t)
	image := dockerImage()
	requireDockerImage(t, image)
	runSpineE2E(t, sandbox.NewDockerBackend(), image)
}

func dockerImage() string {
	if v := os.Getenv("HARNESS_E2E_IMAGE"); v != "" {
		return v
	}
	return e2eDockerImage
}

func requireDockerImage(t *testing.T, image string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH; skipping Docker spine e2e")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not reachable; skipping Docker spine e2e")
	}
	if err := exec.Command("docker", "image", "inspect", image).Run(); err != nil {
		t.Skipf("docker image %q not present (build deploy/go-toolchain.Dockerfile or set HARNESS_E2E_IMAGE); skipping", image)
	}
}
