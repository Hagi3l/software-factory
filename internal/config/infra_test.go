package config

import "testing"

// ResolveImage maps a logical profile to the concrete artifact the active backend boots:
// the image for docker/gvisor, the rootfs for firecracker, and the bare profile name as a
// total fallback when the profile is unregistered or missing the backend's field.
func TestResolveImage(t *testing.T) {
	profiles := map[string]SandboxProfile{
		"go-toolchain": {Image: "harness/go-toolchain@sha256:abc", Rootfs: "/var/lib/harness/go.ext4"},
		"image-only":   {Image: "harness/image-only:dev"},
		"rootfs-only":  {Rootfs: "/var/lib/harness/rootfs-only.ext4"},
	}
	cases := []struct {
		name    string
		backend string
		profile string
		want    string
	}{
		{"docker picks image", BackendDocker, "go-toolchain", "harness/go-toolchain@sha256:abc"},
		{"gvisor picks image", BackendGVisor, "go-toolchain", "harness/go-toolchain@sha256:abc"},
		{"firecracker picks rootfs", BackendFirecracker, "go-toolchain", "/var/lib/harness/go.ext4"},
		{"unregistered falls back to name", BackendDocker, "rust-toolchain", "rust-toolchain"},
		{"docker missing image falls back to name", BackendDocker, "rootfs-only", "rootfs-only"},
		{"firecracker missing rootfs falls back to name", BackendFirecracker, "image-only", "image-only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := SandboxConfig{Backend: tc.backend, Profiles: profiles}
			if got := sc.ResolveImage(tc.profile); got != tc.want {
				t.Errorf("ResolveImage(%q) on %s = %q, want %q", tc.profile, tc.backend, got, tc.want)
			}
		})
	}
}

// With no profiles registry at all, every name resolves to itself — the historical
// "name == image tag" behavior the unconfigured/test paths depend on.
func TestResolveImageNoRegistry(t *testing.T) {
	sc := SandboxConfig{Backend: BackendDocker}
	if got := sc.ResolveImage("go-toolchain"); got != "go-toolchain" {
		t.Errorf("ResolveImage with no registry = %q, want %q", got, "go-toolchain")
	}
}
