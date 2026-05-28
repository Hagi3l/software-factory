// Package agent is the ephemeral, sandboxed worker that lives for exactly one work
// item: it boots a Soul, builds context from a Brief, runs the ReAct loop over
// brokered model calls and workspace tools, and proposes a Result before dying.
//
// Souls are stateless identity (config) carrying no cross-task memory — all durable
// state lives in beads, git, and the specs. The agent is untrusted: it never writes
// beads and never merges; it only proposes a candidate branch plus evidence.
package agent
