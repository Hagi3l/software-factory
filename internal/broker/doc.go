// Package broker defines the local-socket RPC between a sandboxed agent and its
// runner: the small, deny-by-default set of brokered calls — model completion,
// git push, event publish — and their request/response framing.
//
// Everything an untrusted agent does to the outside world crosses this surface,
// so the protocol is intentionally tiny and explicit: anything not on the
// allowlist is rejected rather than forwarded.
package broker
