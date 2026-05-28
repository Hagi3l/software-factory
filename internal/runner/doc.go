// Package runner is the per-host daemon that pulls work, provisions a sandbox for
// one ephemeral agent, brokers all of that agent's I/O, harvests the Result, and
// reaps the sandbox.
//
// The runner is the only long-lived NATS citizen on its host and the only holder
// of credentials, making it the single auditable chokepoint through which an
// untrusted agent can reach the model API, git, or the event bus.
package runner
