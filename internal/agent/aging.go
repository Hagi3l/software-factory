package agent

import (
	"fmt"
	"unicode/utf8"

	"github.com/Loxstomper/software-factory/internal/model"
)

// Tool-result aging (specs/components/agent.md "Tool-result aging"): the loop re-sends
// the whole conversation every turn, and old tool results dominate a deep run's input.
// Prompt caching makes that history cache-cheap but not smaller — and a read_file from
// before the soul edited that file is actively misleading. agedView derives the model's
// view of the history with old bulk tool-result content replaced by a deterministic
// stub. The worktree is durable state, so an elided result is always recoverable with
// one re-run of the tool.
const (
	// elideKeepRounds is how many of the most recent tool rounds always keep their full
	// content — the model's working set.
	elideKeepRounds = 8
	// elideBatchRounds quantizes the elision boundary: it advances only in steps of this
	// many rounds, so the aged view is byte-stable between advances and the cached prefix
	// is re-written once per batch, not on every turn (a sliding horizon would invalidate
	// the cache every turn — see specs/models.md "Prompt caching").
	elideBatchRounds = 8
	// elideMinBytes exempts small results: below this the stub costs as much as the
	// content, and tiny results ("diagnostics: clean") are load-bearing signal.
	elideMinBytes = 1024
)

// elisionStats totals what agedView removed from the model's view, for the
// context-discipline counters (specs/observability.md).
type elisionStats struct {
	results int
	bytes   int
}

// agedView returns the message slice a model request should carry: a copy of msgs in
// which tool-result content older than the elision boundary is replaced by a stub. It is
// a pure function of msgs — the caller's slice is never mutated (the pristine history
// stays the source of truth for future views), and the same input always yields the same
// view, so the bytes below the boundary are identical across turns until the boundary
// advances. When nothing is old enough to age it returns msgs unchanged (no copy).
//
// The boundary keeps the last elideKeepRounds tool rounds intact and is quantized to
// elideBatchRounds. Only RoleTool content ages: the Brief (messages[0]) and every
// assistant message (the soul's plan/reasoning trail) pass through untouched, and an
// aged result keeps its ToolCallID and IsError — only bulk Content is stubbed.
func agedView(msgs []model.Message) ([]model.Message, elisionStats) {
	rounds := 0
	for _, m := range msgs {
		if m.Role == model.RoleTool {
			rounds++
		}
	}
	boundary := (rounds - elideKeepRounds) / elideBatchRounds * elideBatchRounds
	if boundary <= 0 {
		return msgs, elisionStats{}
	}

	out := make([]model.Message, len(msgs))
	copy(out, msgs)
	var stats elisionStats
	// ToolResult carries only ToolCallID; the tool's name+args live on the assistant
	// message that issued the calls, always the message preceding its RoleTool answer.
	calls := map[string]model.ToolCall{}
	round := 0
	for i, m := range msgs {
		switch m.Role {
		case model.RoleAssistant:
			calls = make(map[string]model.ToolCall, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				calls[tc.ID] = tc
			}
		case model.RoleTool:
			round++
			if round > boundary {
				return out, stats
			}
			aged := make([]model.ToolResult, len(m.ToolResults))
			changed := false
			for j, tr := range m.ToolResults {
				if len(tr.Content) < elideMinBytes {
					aged[j] = tr
					continue
				}
				stub := elideStub(calls[tr.ToolCallID], round)
				stats.results++
				stats.bytes += len(tr.Content) - len(stub)
				aged[j] = model.ToolResult{ToolCallID: tr.ToolCallID, Content: stub, IsError: tr.IsError}
				changed = true
			}
			if changed {
				out[i] = model.Message{Role: model.RoleTool, ToolResults: aged}
			}
		}
	}
	return out, stats
}

// elideStub renders the deterministic replacement for an aged tool result. It names the
// call and how to recover, and nothing else — determinism keeps the aged prefix
// byte-identical across turns (any variance would defeat the batch-cadence cache
// stability agedView exists to preserve).
func elideStub(tc model.ToolCall, round int) string {
	name := tc.Name
	if name == "" {
		name = "tool"
	}
	hint := string(tc.Args)
	if len(hint) > 64 {
		cut := 64
		for cut > 0 && !utf8.RuneStart(hint[cut]) {
			cut--
		}
		hint = hint[:cut] + "…"
	}
	if hint != "" {
		hint = " " + hint
	}
	return fmt.Sprintf("[%s%s — result elided (round %d); the worktree is current, re-run the tool if you need it]", name, hint, round)
}
