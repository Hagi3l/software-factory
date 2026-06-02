package live_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Loxstomper/harness/internal/controlroom/live"
)

func token(delta string) []byte {
	return []byte(fmt.Sprintf(`{"type":"token","delta":%q}`, delta))
}

func reasoning(delta string) []byte {
	return []byte(fmt.Sprintf(`{"type":"reasoning","delta":%q}`, delta))
}

func toolEvent(label string) []byte {
	return []byte(fmt.Sprintf(`{"type":"tool","delta":%q}`, label))
}

func TestActivity_CarriesIssueBinding(t *testing.T) {
	a := live.NewActivity(16)
	// The pump tags each event with the invocation id (from the subject) and the issue id +
	// role (from the wire envelope, plan T4.20). Both a discrete event and a coalesced token
	// run must carry the binding so a view can scope a feed to one invocation (plan T4.21).
	a.Record("inv-1", "harness-7", "implementor", token("par"))
	a.Record("inv-1", "harness-7", "implementor", token("tial"))
	a.Record("inv-1", "harness-7", "implementor", toolEvent("run_tests"))

	got := a.Recent()
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2 (coalesced token run + tool row)", len(got))
	}
	for _, e := range got {
		if e.IssueID != "harness-7" || e.Role != "implementor" {
			t.Fatalf("entry %+v missing issue binding, want harness-7 / implementor", e)
		}
	}
}

func TestActivity_CoalescesTokensFromSameAgent(t *testing.T) {
	a := live.NewActivity(16)
	a.Record("inv-1", "", "", token("Hel"))
	a.Record("inv-1", "", "", token("lo "))
	a.Record("inv-1", "", "", token("world"))

	got := a.Recent()
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1 (coalesced)", len(got))
	}
	if got[0].Detail != "Hello world" {
		t.Fatalf("detail = %q, want %q", got[0].Detail, "Hello world")
	}
	if got[0].Kind != "token" || got[0].AgentID != "inv-1" {
		t.Fatalf("entry = %+v", got[0])
	}
	if got[0].At.IsZero() {
		t.Fatalf("entry timestamp not set")
	}
}

func TestActivity_CoalescesReasoningFromSameAgent(t *testing.T) {
	a := live.NewActivity(16)
	a.Record("inv-1", "", "", reasoning("Let "))
	a.Record("inv-1", "", "", reasoning("me "))
	a.Record("inv-1", "", "", reasoning("think"))

	got := a.Recent()
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1 (reasoning coalesced)", len(got))
	}
	if got[0].Kind != "reasoning" || got[0].Detail != "Let me think" {
		t.Fatalf("entry = %+v, want reasoning 'Let me think'", got[0])
	}
	if got[0].Source != live.SourceAgent {
		t.Fatalf("source = %q, want %q", got[0].Source, live.SourceAgent)
	}
}

func TestActivity_TokenAndReasoningDoNotMerge(t *testing.T) {
	a := live.NewActivity(16)
	a.Record("inv-1", "", "", reasoning("planning"))
	a.Record("inv-1", "", "", token("answer"))

	got := a.Recent()
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2 (token and reasoning are separate channels)", len(got))
	}
	if got[0].Kind != "token" || got[1].Kind != "reasoning" {
		t.Fatalf("kinds = [%q,%q], want [token,reasoning]", got[0].Kind, got[1].Kind)
	}
}

func TestActivity_ToolEventRendersLabel(t *testing.T) {
	a := live.NewActivity(16)
	a.Record("inv-1", "", "", token("ok"))
	a.Record("inv-1", "", "", toolEvent("write_file index.html"))

	got := a.Recent()
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2 (tool is discrete, breaks the token run)", len(got))
	}
	if got[0].Kind != "tool" || got[0].Detail != "write_file index.html" {
		t.Fatalf("newest = %+v, want tool 'write_file index.html'", got[0])
	}
	if got[0].Source != live.SourceAgent {
		t.Fatalf("source = %q, want %q", got[0].Source, live.SourceAgent)
	}
}

func TestActivity_RecordSystem(t *testing.T) {
	a := live.NewActivity(16)
	a.Record("inv-1", "", "", token("partial"))
	a.RecordSystem("info", "orchestrator", "dispatched issue=harness-1")
	// A system row between agent tokens must break the token run: the prior entry is a
	// system row, so the next token cannot reopen the agent's coalesced line.
	a.Record("inv-1", "", "", token("more"))

	got := a.Recent()
	if len(got) != 3 {
		t.Fatalf("entries = %d, want 3", len(got))
	}
	sys := got[1]
	if sys.Source != live.SourceSystem || sys.AgentID != "orchestrator" || sys.Kind != "info" {
		t.Fatalf("system entry = %+v, want system/orchestrator/info", sys)
	}
	if sys.Detail != "dispatched issue=harness-1" {
		t.Fatalf("system detail = %q", sys.Detail)
	}
	if got[0].Kind != "token" || got[0].Detail != "more" {
		t.Fatalf("trailing token = %+v, want fresh token 'more'", got[0])
	}
	// Seq stays monotonic across the mixed agent/system stream (newest first).
	if got[0].Seq <= got[1].Seq || got[1].Seq <= got[2].Seq {
		t.Fatalf("seq not monotonic across sources: %d %d %d", got[2].Seq, got[1].Seq, got[0].Seq)
	}
}

func TestActivity_DifferentAgentsDoNotCoalesce(t *testing.T) {
	a := live.NewActivity(16)
	a.Record("inv-1", "", "", token("a"))
	a.Record("inv-2", "", "", token("b"))
	a.Record("inv-1", "", "", token("c"))

	got := a.Recent()
	if len(got) != 3 {
		t.Fatalf("entries = %d, want 3 (no cross-agent coalesce)", len(got))
	}
	// Newest first: the second inv-1 token is its own entry, not folded into the first.
	if got[0].AgentID != "inv-1" || got[0].Detail != "c" {
		t.Fatalf("newest = %+v, want inv-1 'c'", got[0])
	}
}

func TestActivity_DiscreteEventBreaksTokenRun(t *testing.T) {
	a := live.NewActivity(16)
	a.Record("inv-1", "", "", token("partial"))
	a.Record("inv-1", "", "", []byte(`{"type":"progress","payload":{"msg":"gate passed"}}`))
	a.Record("inv-1", "", "", token("more"))

	got := a.Recent()
	if len(got) != 3 {
		t.Fatalf("entries = %d, want 3", len(got))
	}
	if got[1].Kind != "progress" || got[1].Detail != "gate passed" {
		t.Fatalf("middle entry = %+v, want progress 'gate passed'", got[1])
	}
	// The trailing token starts a fresh entry rather than reopening the first.
	if got[0].Kind != "token" || got[0].Detail != "more" {
		t.Fatalf("newest = %+v, want token 'more'", got[0])
	}
}

func TestActivity_SummarizesOpaquePayload(t *testing.T) {
	a := live.NewActivity(16)
	a.Record("inv-1", "", "", []byte(`{"type":"progress","payload":{"step":2,"of":5}}`))
	got := a.Recent()
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1", len(got))
	}
	// No msg/message/text field, so it falls back to compact JSON of the payload.
	if !strings.Contains(got[0].Detail, `"step":2`) || !strings.Contains(got[0].Detail, `"of":5`) {
		t.Fatalf("detail = %q, want compact payload JSON", got[0].Detail)
	}
}

func TestActivity_RecentIsNewestFirst(t *testing.T) {
	a := live.NewActivity(16)
	a.Record("inv-1", "", "", []byte(`{"type":"log","payload":{"msg":"first"}}`))
	a.Record("inv-2", "", "", []byte(`{"type":"log","payload":{"msg":"second"}}`))

	got := a.Recent()
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2", len(got))
	}
	if got[0].Detail != "second" || got[1].Detail != "first" {
		t.Fatalf("order = [%q,%q], want [second,first]", got[0].Detail, got[1].Detail)
	}
	if got[0].Seq <= got[1].Seq {
		t.Fatalf("seq not monotonic: %d then %d", got[1].Seq, got[0].Seq)
	}
}

func TestActivity_BoundedToMax(t *testing.T) {
	a := live.NewActivity(3)
	for i := 0; i < 10; i++ {
		// Distinct agents so each event is its own entry (no coalescing).
		a.Record(fmt.Sprintf("inv-%d", i), "", "", token("x"))
	}
	got := a.Recent()
	if len(got) != 3 {
		t.Fatalf("entries = %d, want 3 (bounded)", len(got))
	}
	if got[0].AgentID != "inv-9" {
		t.Fatalf("newest = %q, want inv-9", got[0].AgentID)
	}
}

func TestActivity_DropsMalformedPayload(t *testing.T) {
	a := live.NewActivity(16)
	a.Record("inv-1", "", "", []byte(`not json`))
	a.Record("inv-1", "", "", []byte(``))
	if got := a.Recent(); len(got) != 0 {
		t.Fatalf("entries = %d, want 0 (malformed dropped)", len(got))
	}
}

func TestActivity_RollingTokenTextIsBounded(t *testing.T) {
	a := live.NewActivity(16)
	for i := 0; i < 1000; i++ {
		a.Record("inv-1", "", "", token("0123456789"))
	}
	got := a.Recent()
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1", len(got))
	}
	// Detail is capped (plus a leading ellipsis when truncated); far below 10k chars.
	if n := len([]rune(got[0].Detail)); n > 300 {
		t.Fatalf("rolling detail len = %d, want bounded (<=~281)", n)
	}
}

func TestActivity_ConcurrentRecord(t *testing.T) {
	a := live.NewActivity(64)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a.Record(fmt.Sprintf("inv-%d", i), "", "", token("x"))
			_ = a.Recent()
		}(i)
	}
	wg.Wait()
	if got := a.Recent(); len(got) == 0 {
		t.Fatalf("expected some entries after concurrent record")
	}
}
