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

func TestActivity_CoalescesTokensFromSameAgent(t *testing.T) {
	a := live.NewActivity(16)
	a.Record("inv-1", token("Hel"))
	a.Record("inv-1", token("lo "))
	a.Record("inv-1", token("world"))

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

func TestActivity_DifferentAgentsDoNotCoalesce(t *testing.T) {
	a := live.NewActivity(16)
	a.Record("inv-1", token("a"))
	a.Record("inv-2", token("b"))
	a.Record("inv-1", token("c"))

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
	a.Record("inv-1", token("partial"))
	a.Record("inv-1", []byte(`{"type":"progress","payload":{"msg":"gate passed"}}`))
	a.Record("inv-1", token("more"))

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
	a.Record("inv-1", []byte(`{"type":"progress","payload":{"step":2,"of":5}}`))
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
	a.Record("inv-1", []byte(`{"type":"log","payload":{"msg":"first"}}`))
	a.Record("inv-2", []byte(`{"type":"log","payload":{"msg":"second"}}`))

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
		a.Record(fmt.Sprintf("inv-%d", i), token("x"))
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
	a.Record("inv-1", []byte(`not json`))
	a.Record("inv-1", []byte(``))
	if got := a.Recent(); len(got) != 0 {
		t.Fatalf("entries = %d, want 0 (malformed dropped)", len(got))
	}
}

func TestActivity_RollingTokenTextIsBounded(t *testing.T) {
	a := live.NewActivity(16)
	for i := 0; i < 1000; i++ {
		a.Record("inv-1", token("0123456789"))
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
			a.Record(fmt.Sprintf("inv-%d", i), token("x"))
			_ = a.Recent()
		}(i)
	}
	wg.Wait()
	if got := a.Recent(); len(got) == 0 {
		t.Fatalf("expected some entries after concurrent record")
	}
}
