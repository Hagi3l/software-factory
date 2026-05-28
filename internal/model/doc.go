// Package model holds the provider-agnostic canonical types — messages, tool
// definitions, tool calls/results, usage — and the thin per-provider adapters that
// translate them to and from each vendor's wire format.
//
// The agent loop is model-agnostic by design: the agent emits canonical requests
// and a runner-held adapter performs the provider translation. That split (no agent
// framework) is what lets a soul switch models with only a config change.
package model
