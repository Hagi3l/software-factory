// Package sandbox isolates untrusted agents. It defines the microVM-shaped sandbox
// interface — explicit rootfs/worktree seeding, no casual bind mounts, local-socket
// I/O, resource limits, deterministic teardown — and the backends that implement it.
//
// Agents are assumed hostile, so this isolation (zero direct network, bounded
// resources, unconditional reaping) is the load-bearing security boundary; the
// interface is microVM-shaped from day one so Docker can later give way to
// Firecracker without changing callers.
package sandbox
