package query

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/model"
)

// mustTranscript marshals turns the way the runner's relay harvests them, so the test feeds
// the read side the exact wire format the write side produces (single-source: both decode
// model.TranscriptTurn).
func mustTranscript(t *testing.T, turns []model.TranscriptTurn) string {
	t.Helper()
	b, err := json.Marshal(turns)
	if err != nil {
		t.Fatalf("marshal transcript: %v", err)
	}
	return string(b)
}

// twoTurnTranscript models a real agent loop: turn 0 sees the brief and asks to call a
// tool; the loop appends the assistant turn + the tool result to the history; turn 1 sees
// that result and finishes. It exercises the inbound-delta (the suffix of new messages) and
// the assistant-echo skip.
func twoTurnTranscript(t *testing.T) string {
	t.Helper()
	user := model.Message{Role: model.RoleUser, Text: "implement the widget"}
	asst := model.Message{Role: model.RoleAssistant, Text: "I will write it", ToolCalls: []model.ToolCall{{ID: "c1", Name: "write_file", Args: json.RawMessage(`{"path":"w.go"}`)}}}
	toolRes := model.Message{Role: model.RoleTool, ToolResults: []model.ToolResult{{ToolCallID: "c1", Content: "wrote 12 bytes"}}}
	return mustTranscript(t, []model.TranscriptTurn{
		{
			Request: model.Request{System: "you are an implementor", Messages: []model.Message{user}},
			Response: model.Response{
				Text:      "I will write it",
				ToolCalls: []model.ToolCall{{ID: "c1", Name: "write_file", Args: json.RawMessage(`{"path":"w.go"}`)}},
				Stop:      model.StopToolUse,
				Usage:     model.Usage{InputTokens: 100, OutputTokens: 20},
			},
		},
		{
			// History is append-only: brief + the assistant echo + the tool result.
			Request: model.Request{System: "you are an implementor", Messages: []model.Message{user, asst, toolRes}},
			Response: model.Response{
				Text:  "done",
				Stop:  model.StopEndTurn,
				Usage: model.Usage{InputTokens: 130, OutputTokens: 5, CacheReadTokens: 90},
			},
		},
	})
}

// TestReplayReconstructsTrail is T4.11's core contract: a merged issue whose transcript is
// in the store yields the full per-turn decision trail, with the inbound delta computed and
// the assistant echo dropped (rendered once, as the response).
func TestReplayReconstructsTrail(t *testing.T) {
	const hash = "sha256:transcriptaaaa"
	issues := &fakeIssues{all: []core.Issue{{ID: "h-1", Title: "Widget", Status: "closed"}}}
	arts := &fakeArts{present: map[string]string{hash: twoTurnTranscript(t)}}
	prov := &fakeProv{byIssue: map[string]core.Provenance{"h-1": {Issue: "h-1", Transcript: hash}}}
	r := NewReader(issues, arts, prov)

	rep, err := r.Replay(context.Background(), "h-1")
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if !rep.Available {
		t.Fatal("Available = false, want a reconstructed trail")
	}
	if !rep.Merged || rep.Hash != hash {
		t.Errorf("Merged/Hash = %v/%q, want true/%q", rep.Merged, rep.Hash, hash)
	}
	if rep.System != "you are an implementor" {
		t.Errorf("System = %q", rep.System)
	}
	if len(rep.Turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(rep.Turns))
	}

	// Turn 0: the brief is the only inbound (no prior turn), and the tool call surfaces with
	// pretty-printed args.
	t0 := rep.Turns[0]
	if len(t0.Inbound) != 1 || t0.Inbound[0].Role != "user" || t0.Inbound[0].Text != "implement the widget" {
		t.Errorf("turn 0 inbound = %+v", t0.Inbound)
	}
	if t0.Text != "I will write it" || t0.Stop != "tool_use" {
		t.Errorf("turn 0 response = %q stop=%q", t0.Text, t0.Stop)
	}
	if len(t0.ToolCalls) != 1 || t0.ToolCalls[0].Name != "write_file" || !strings.Contains(t0.ToolCalls[0].Args, "\"path\": \"w.go\"") {
		t.Errorf("turn 0 tool calls = %+v (args should be indented JSON)", t0.ToolCalls)
	}

	// Turn 1: the only inbound is the tool result — the assistant echo in the history is
	// dropped because it was already rendered as turn 0's response.
	t1 := rep.Turns[1]
	if len(t1.Inbound) != 1 {
		t.Fatalf("turn 1 inbound = %+v, want only the tool result (assistant echo dropped)", t1.Inbound)
	}
	if t1.Inbound[0].Role != "tool" || len(t1.Inbound[0].ToolResults) != 1 || t1.Inbound[0].ToolResults[0].Content != "wrote 12 bytes" {
		t.Errorf("turn 1 inbound tool result = %+v", t1.Inbound[0])
	}
	if t1.Text != "done" || t1.Stop != "end_turn" || t1.CacheRead != 90 {
		t.Errorf("turn 1 response = %q stop=%q cache=%d", t1.Text, t1.Stop, t1.CacheRead)
	}

	// Totals sum every turn (real billed input, including the re-sent history).
	if rep.TotalInput != 230 || rep.TotalOutput != 25 {
		t.Errorf("totals = %d in / %d out, want 230/25", rep.TotalInput, rep.TotalOutput)
	}
}

// TestReplayNoTranscriptCited covers a merged issue whose trailer carries no transcript hash
// (unharvested): the page renders with Available=false and no hash, so the view shows the
// "none captured" notice rather than erroring.
func TestReplayNoTranscriptCited(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{{ID: "h-1", Status: "closed"}}}
	prov := &fakeProv{byIssue: map[string]core.Provenance{"h-1": {Issue: "h-1"}}} // no Transcript
	r := NewReader(issues, &fakeArts{}, prov)

	rep, err := r.Replay(context.Background(), "h-1")
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if rep.Available || rep.Hash != "" || len(rep.Turns) != 0 {
		t.Errorf("want no trail, got available=%v hash=%q turns=%d", rep.Available, rep.Hash, len(rep.Turns))
	}
	if !rep.Merged {
		t.Errorf("Merged should reflect the provenance presence")
	}
}

// TestReplayNotMerged covers in-flight work: no provenance, so no transcript is reachable
// (the hash is only retained on the merge trailer).
func TestReplayNotMerged(t *testing.T) {
	issues := &fakeIssues{all: []core.Issue{{ID: "h-2", Status: "in_progress"}}}
	r := NewReader(issues, &fakeArts{}, &fakeProv{}) // ByIssue returns ok=false

	rep, err := r.Replay(context.Background(), "h-2")
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if rep.Available || rep.Merged || rep.Hash != "" {
		t.Errorf("in-flight work should have no reachable trail: %+v", rep)
	}
}

// TestReplayUnresolvableTranscript covers a cited hash the store cannot serve: the page keeps
// the hash (so the view can offer the raw-bytes link) but Available stays false — a
// best-effort degrade, not an error.
func TestReplayUnresolvableTranscript(t *testing.T) {
	const hash = "sha256:gone"
	issues := &fakeIssues{all: []core.Issue{{ID: "h-1", Status: "closed"}}}
	prov := &fakeProv{byIssue: map[string]core.Provenance{"h-1": {Issue: "h-1", Transcript: hash}}}
	r := NewReader(issues, &fakeArts{present: map[string]string{}}, prov) // hash absent

	rep, err := r.Replay(context.Background(), "h-1")
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if rep.Available || rep.Hash != hash {
		t.Errorf("want degraded-with-hash, got available=%v hash=%q", rep.Available, rep.Hash)
	}
}

// TestReplayMalformedTranscript covers a corrupt transcript artifact: it decodes to nothing,
// so the page degrades (Available=false, hash retained) rather than 500ing.
func TestReplayMalformedTranscript(t *testing.T) {
	const hash = "sha256:corrupt"
	issues := &fakeIssues{all: []core.Issue{{ID: "h-1", Status: "closed"}}}
	arts := &fakeArts{present: map[string]string{hash: "{not valid json"}}
	prov := &fakeProv{byIssue: map[string]core.Provenance{"h-1": {Issue: "h-1", Transcript: hash}}}
	r := NewReader(issues, arts, prov)

	rep, err := r.Replay(context.Background(), "h-1")
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if rep.Available {
		t.Errorf("a corrupt transcript must not report Available")
	}
	if rep.Hash != hash {
		t.Errorf("Hash should be retained so the view can offer the raw link, got %q", rep.Hash)
	}
}

// TestReplayIssueError proves an unreadable issue is fatal (the page has nothing to render),
// unlike the best-effort transcript path.
func TestReplayIssueError(t *testing.T) {
	r := NewReader(&fakeIssues{getErr: errors.New("bd down")}, &fakeArts{}, &fakeProv{})
	if _, err := r.Replay(context.Background(), "h-1"); err == nil {
		t.Fatal("Replay swallowed an issue read error")
	}
}
