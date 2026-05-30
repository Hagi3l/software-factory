package model

// TranscriptTurn is one model exchange in an invocation transcript: the canonical Request
// the agent sent and the canonical Response the provider returned. The ordered slice of
// these is the invocation's full decision trail — exactly what the LLM saw and did, turn
// by turn (the llm-turn spans of specs/observability.md).
//
// The runner's broker — the trusted egress chokepoint — records these from the calls it
// actually relayed (never self-reported by the untrusted agent) and harvests the slice to
// the artifact store as JSON, referenced by hash from the merge provenance trailer (see
// specs/security.md, specs/observability.md). It lives in the model package, not the
// runner, so the write side (the relay) and the read side (the control room's replay view,
// T4.11) share one wire format with no second definition to drift — the same single-source
// posture core.Provenance takes for the trailer itself. The json tags pin that wire format.
type TranscriptTurn struct {
	Request  Request  `json:"request"`
	Response Response `json:"response"`
}
