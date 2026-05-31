package live

import (
	"bytes"
	"encoding/json"
	"sync"
	"time"
)

// Entry is one row in the control room's activity feed: a single event, or a coalesced
// run of streamed token/reasoning deltas from one agent. Detail is a short human-readable
// summary — the rolling generated text for a token/reasoning run, the tool label for a
// tool call, or a compact summary of the event payload otherwise. Ordering is by Seq
// (monotonic), not At, so a coarse wall clock can't reorder the feed.
//
// Source separates the two streams the feed carries: "agent" rows are brokered from
// inside a sandbox (AgentID is the invocation id; Kind is token/reasoning/tool/…), while
// "system" rows are the factory's own log teed in by the bridge (AgentID is the emitting
// component, e.g. "orchestrator"; Kind is the log level). The view filters on Source.
type Entry struct {
	Seq     uint64
	Source  string
	AgentID string
	Kind    string
	Detail  string
	At      time.Time
}

const (
	// SourceAgent marks a row brokered from an agent's sandbox; SourceSystem marks a row
	// teed from the trusted side's own factory log.
	SourceAgent  = "agent"
	SourceSystem = "system"
)

const (
	// activityRollingMax bounds the rolling token text retained on a coalesced entry
	// so a long model turn can't grow one entry without limit.
	activityRollingMax = 280
	// activityDetailMax bounds a discrete event's summary length.
	activityDetailMax = 280
)

// Activity is a bounded, concurrent-safe record of recent agent events for the
// activity feed ("what agents are doing right now"). Consecutive streamed token
// deltas from the same agent coalesce into a single rolling entry — collapsing the
// per-token firehose into one line per model turn — while every other event becomes
// its own entry. Recent returns the retained entries newest first.
//
// It is intentionally in-memory and best-effort: the durable record of an agent's
// behavior is the artifact-store transcript, so losing live events on restart is
// harmless. The pump (the live substrate's only NATS-touching piece) feeds it.
type Activity struct {
	mu      sync.Mutex
	max     int
	entries []Entry // oldest .. newest
	seq     uint64
}

// NewActivity returns an Activity retaining up to max recent entries (min 1).
func NewActivity(max int) *Activity {
	if max <= 0 {
		max = 1
	}
	return &Activity{max: max}
}

// wireEvent is the shape the runner and agents publish on the agent-events subject:
// a type discriminator plus either a streamed token delta or an opaque payload.
type wireEvent struct {
	Type    string          `json:"type"`
	Delta   string          `json:"delta"`
	Payload json.RawMessage `json:"payload"`
}

// Record ingests one raw agent-event payload for agentID. A payload that does not
// parse as a wire event is dropped — matching the best-effort nature of the feed.
func (a *Activity) Record(agentID string, payload []byte) {
	var ev wireEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return
	}
	kind := ev.Type
	if kind == "" {
		kind = "event"
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()

	// Coalesce a run of token/reasoning deltas from the same agent into the newest entry,
	// so a single model turn reads as one continuously-updating line rather than thousands.
	// Token and reasoning never merge into each other (the kinds must match), so "thinking"
	// and "saying" stay as separate rows.
	if (kind == "token" || kind == "reasoning") && len(a.entries) > 0 {
		last := &a.entries[len(a.entries)-1]
		if last.Kind == kind && last.AgentID == agentID && last.Source == SourceAgent {
			last.Detail = tailRunes(last.Detail+ev.Delta, activityRollingMax)
			last.At = now
			a.seq++
			last.Seq = a.seq
			return
		}
	}

	e := Entry{Source: SourceAgent, AgentID: agentID, Kind: kind, At: now}
	switch kind {
	case "token", "reasoning":
		e.Detail = tailRunes(ev.Delta, activityRollingMax)
	default:
		// A tool call (and any future Delta-bearing kind) carries its label in Delta; a
		// progress/log event from the agent carries an opaque Payload to summarize.
		if ev.Delta != "" {
			e.Detail = headRunes(ev.Delta)
		} else {
			e.Detail = summarize(ev.Payload)
		}
	}
	a.appendEntry(e)
}

// RecordSystem ingests one factory log line as a system event: a non-agent row showing
// what the orchestrator/runner/gate is doing (the log bridge feeds it). component labels
// the emitter (the feed's id column), level becomes the Kind badge, and detail is the
// already-rendered message. Caller holds no lock; this takes it.
func (a *Activity) RecordSystem(level, component, detail string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.appendEntry(Entry{
		Source:  SourceSystem,
		AgentID: component,
		Kind:    level,
		Detail:  headRunes(detail),
		At:      time.Now(),
	})
}

// appendEntry stamps a fresh monotonic Seq onto e, appends it, and trims the buffer to
// max. Caller must hold a.mu.
func (a *Activity) appendEntry(e Entry) {
	a.seq++
	e.Seq = a.seq
	a.entries = append(a.entries, e)
	if len(a.entries) > a.max {
		// Copy the retained window into a fresh backing array so the trimmed prefix is
		// released rather than pinned by a re-slice.
		a.entries = append([]Entry(nil), a.entries[len(a.entries)-a.max:]...)
	}
}

// Recent returns a copy of the retained entries, newest first.
func (a *Activity) Recent() []Entry {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := len(a.entries)
	out := make([]Entry, n)
	for i := 0; i < n; i++ {
		out[i] = a.entries[n-1-i]
	}
	return out
}

// summarize renders an opaque event payload as a short human line: a top-level
// "msg"/"message"/"text" string if present, else compact JSON, truncated.
func summarize(payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err == nil {
		for _, k := range []string{"msg", "message", "text"} {
			raw, ok := obj[k]
			if !ok {
				continue
			}
			var s string
			if json.Unmarshal(raw, &s) == nil && s != "" {
				return headRunes(s)
			}
		}
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, payload); err != nil {
		return headRunes(string(payload))
	}
	return headRunes(buf.String())
}

// tailRunes keeps the last max runes of s (the latest output), prefixing an ellipsis
// when truncated. Rune-aware so it never splits a multi-byte character.
func tailRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return "…" + string(r[len(r)-max:])
}

// headRunes keeps the first activityDetailMax runes of s, appending an ellipsis when
// truncated. Rune-aware so it never splits a multi-byte character. (tailRunes is the
// rolling-token analog, keeping the tail; this keeps the head of a discrete summary.)
func headRunes(s string) string {
	const max = activityDetailMax
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
