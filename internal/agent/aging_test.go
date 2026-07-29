package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Loxstomper/software-factory/internal/model"
)

// agingHistory synthesizes a loop-shaped conversation: the Brief as the opening user
// turn, then `rounds` of assistant(read_file call) + one RoleTool answer whose content
// is `size` bytes.
func agingHistory(rounds, size int) []model.Message {
	msgs := []model.Message{{Role: model.RoleUser, Text: "the brief"}}
	content := strings.Repeat("x", size)
	for r := 1; r <= rounds; r++ {
		id := fmt.Sprintf("c%d", r)
		msgs = append(msgs,
			model.Message{Role: model.RoleAssistant, Text: fmt.Sprintf("reading %d", r), ToolCalls: []model.ToolCall{
				{ID: id, Name: "read_file", Args: json.RawMessage(`{"path":"foo.go"}`)},
			}},
			model.Message{Role: model.RoleTool, ToolResults: []model.ToolResult{
				{ToolCallID: id, Content: content},
			}})
	}
	return msgs
}

// toolMsg returns the RoleTool message of round r in an agingHistory-shaped slice.
func toolMsg(t *testing.T, msgs []model.Message, r int) model.Message {
	t.Helper()
	i := 2 * r // brief at 0; round r = assistant at 2r-1, tool at 2r
	if i >= len(msgs) || msgs[i].Role != model.RoleTool {
		t.Fatalf("round %d tool message not at index %d", r, i)
	}
	return msgs[i]
}

// Below the first batch boundary (fewer than keep+batch rounds) nothing ages and the
// input slice is returned as-is — no copy, no stats.
func TestAgedViewBelowThresholdUntouched(t *testing.T) {
	msgs := agingHistory(elideKeepRounds+elideBatchRounds-1, 4096) // 15 rounds
	out, stats := agedView(msgs)
	if stats.results != 0 || stats.bytes != 0 {
		t.Fatalf("stats = %+v, want zero below the threshold", stats)
	}
	if &out[0] != &msgs[0] || len(out) != len(msgs) {
		t.Error("below the threshold agedView must return the input slice unchanged (no copy)")
	}
}

// At keep+batch rounds the boundary quantizes to one batch: the oldest batch's results
// are stubbed, everything in the keep window (and beyond the boundary) stays verbatim.
// The boundary then holds until a full further batch accumulates.
func TestAgedViewBoundaryQuantized(t *testing.T) {
	// 16 rounds: boundary = ((16-8)/8)*8 = 8 → rounds 1-8 elided, 9-16 intact.
	msgs := agingHistory(16, 4096)
	out, stats := agedView(msgs)
	if got := toolMsg(t, out, 1).ToolResults[0].Content; !strings.Contains(got, "elided") {
		t.Errorf("round 1 content = %q, want the elision stub", got)
	}
	if got := toolMsg(t, out, 8).ToolResults[0].Content; !strings.Contains(got, "elided") {
		t.Errorf("round 8 content not elided; boundary should cover the whole first batch")
	}
	if got := toolMsg(t, out, 9).ToolResults[0].Content; strings.Contains(got, "elided") {
		t.Errorf("round 9 elided; the keep window must stay verbatim")
	}
	if stats.results != 8 {
		t.Errorf("stats.results = %d, want 8", stats.results)
	}
	if stats.bytes <= 0 {
		t.Errorf("stats.bytes = %d, want positive savings", stats.bytes)
	}

	// 23 rounds: boundary still 8 (batch cadence — no per-turn creep).
	out, _ = agedView(agingHistory(23, 4096))
	if got := toolMsg(t, out, 9).ToolResults[0].Content; strings.Contains(got, "elided") {
		t.Errorf("round 9 elided at 23 rounds; boundary must hold at 8 until a full batch accumulates")
	}

	// 24 rounds: boundary advances to 16 in one step.
	out, stats = agedView(agingHistory(24, 4096))
	if got := toolMsg(t, out, 16).ToolResults[0].Content; !strings.Contains(got, "elided") {
		t.Errorf("round 16 not elided at 24 rounds; boundary should advance to 16")
	}
	if stats.results != 16 {
		t.Errorf("stats.results = %d, want 16 after the second advance", stats.results)
	}
}

// agedView is a pure function: the input history is never mutated, and the aged bytes of
// an elided round are identical across later views (until the boundary moves nothing
// changes; after it moves the already-elided rounds keep the exact same stubs) — the
// property the prompt-cache stability depends on.
func TestAgedViewPurityAndStability(t *testing.T) {
	msgs := agingHistory(16, 4096)
	before := toolMsg(t, msgs, 1).ToolResults[0].Content
	view1, _ := agedView(msgs)
	if got := toolMsg(t, msgs, 1).ToolResults[0].Content; got != before {
		t.Fatal("agedView mutated the caller's history")
	}
	// Same round viewed from a longer history (boundary unchanged, then advanced) must
	// render byte-identically.
	view2, _ := agedView(agingHistory(20, 4096))
	view3, _ := agedView(agingHistory(24, 4096))
	stub1 := toolMsg(t, view1, 1).ToolResults[0].Content
	if s := toolMsg(t, view2, 1).ToolResults[0].Content; s != stub1 {
		t.Errorf("round-1 stub changed between views at the same boundary:\n%q\n%q", stub1, s)
	}
	if s := toolMsg(t, view3, 1).ToolResults[0].Content; s != stub1 {
		t.Errorf("round-1 stub changed after a boundary advance:\n%q\n%q", stub1, s)
	}
	// Assistant messages and the Brief pass through untouched even in a copied view.
	if view1[0].Text != "the brief" || view1[1].Text != "reading 1" {
		t.Error("Brief or assistant message altered by aging")
	}
}

// Results under the size floor keep their content forever, whatever their round; an
// elided result keeps its ToolCallID and IsError — only bulk Content is stubbed.
func TestAgedViewSmallResultsExemptAndIdentityKept(t *testing.T) {
	msgs := agingHistory(16, 4096)
	// Make round 2's result tiny and round 3's an error.
	msgs[4].ToolResults[0].Content = "diagnostics: clean"
	msgs[6].ToolResults[0].IsError = true
	out, stats := agedView(msgs)

	if got := toolMsg(t, out, 2).ToolResults[0].Content; got != "diagnostics: clean" {
		t.Errorf("small result rewritten to %q; under %d bytes must be exempt", got, elideMinBytes)
	}
	r3 := toolMsg(t, out, 3).ToolResults[0]
	if !strings.Contains(r3.Content, "elided") {
		t.Errorf("round 3 (big, error) not elided: %q", r3.Content)
	}
	if !r3.IsError || r3.ToolCallID != "c3" {
		t.Errorf("elision must keep IsError and ToolCallID: %+v", r3)
	}
	if stats.results != 7 { // 8 old rounds minus the exempt small one
		t.Errorf("stats.results = %d, want 7", stats.results)
	}
}

// The stub names the call (tool + args hint + round) and truncates a long args hint
// without splitting a UTF-8 rune; a call the view cannot correlate degrades to "tool".
func TestElideStubShape(t *testing.T) {
	s := elideStub(model.ToolCall{ID: "c1", Name: "read_file", Args: json.RawMessage(`{"path":"foo.go"}`)}, 3)
	for _, want := range []string{"read_file", `{"path":"foo.go"}`, "round 3", "re-run"} {
		if !strings.Contains(s, want) {
			t.Errorf("stub %q missing %q", s, want)
		}
	}
	long := `{"path":"` + strings.Repeat("é", 60) + `"}`
	s = elideStub(model.ToolCall{Name: "search", Args: json.RawMessage(long)}, 1)
	if !utf8.ValidString(s) {
		t.Errorf("truncated stub is not valid UTF-8: %q", s)
	}
	if s2 := elideStub(model.ToolCall{}, 1); !strings.Contains(s2, "[tool") {
		t.Errorf("uncorrelated call should degrade to a generic name: %q", s2)
	}
}
