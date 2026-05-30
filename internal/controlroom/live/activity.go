package live

import (
	"bytes"
	"encoding/json"
	"sync"
	"time"
)

// Entry is one row in the control room's activity feed: a single agent event, or a
// coalesced run of streamed token deltas from one agent. Detail is a short
// human-readable summary — the rolling generated text for a token run, or a compact
// summary of the event payload otherwise. Ordering is by Seq (monotonic), not At, so
// a coarse wall clock can't reorder the feed.
type Entry struct {
	Seq     uint64
	AgentID string
	Kind    string
	Detail  string
	At      time.Time
}

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

	// Coalesce a run of token deltas from the same agent into the newest entry, so a
	// single model turn reads as one continuously-updating line rather than thousands.
	if kind == "token" && len(a.entries) > 0 {
		last := &a.entries[len(a.entries)-1]
		if last.Kind == "token" && last.AgentID == agentID {
			last.Detail = tailRunes(last.Detail+ev.Delta, activityRollingMax)
			last.At = now
			a.seq++
			last.Seq = a.seq
			return
		}
	}

	a.seq++
	e := Entry{Seq: a.seq, AgentID: agentID, Kind: kind, At: now}
	if kind == "token" {
		e.Detail = tailRunes(ev.Delta, activityRollingMax)
	} else {
		e.Detail = summarize(ev.Payload)
	}
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
				return headRunes(s, activityDetailMax)
			}
		}
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, payload); err != nil {
		return headRunes(string(payload), activityDetailMax)
	}
	return headRunes(buf.String(), activityDetailMax)
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

// headRunes keeps the first max runes of s, appending an ellipsis when truncated.
func headRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
