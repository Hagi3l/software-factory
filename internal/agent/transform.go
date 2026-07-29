package agent

import (
	"sync"

	"github.com/Loxstomper/software-factory/internal/core"
)

// TransformLedger accumulates the transformation records the semantic write tools emit
// across one invocation (Phase 6, T6.3), so the terminal lifecycle tool can fold them into
// the Result's evidence. It is the write-side analog of the lifecycle's proposal/trace
// accumulators: the write tools (rename, code_action) live in a different constructor than
// the lifecycle tools, so a single shared ledger — built once per invocation and handed to
// both — is how a transformation done by a write tool reaches the Result that submit
// produces. Recording the mechanism (semantic vs text floor) is the point: "writes degrade
// loudly", and a text-fallback rename warrants more suspicion than a semantic one (see
// specs/components/agent.md "Mechanism is recorded").
//
// All methods are nil-safe so a write tool can hold a nil ledger (no recording) without a
// guard at every call site. Safe for concurrent use.
type TransformLedger struct {
	mu      sync.Mutex
	records []core.TransformRecord
}

// NewTransformLedger builds an empty ledger for one invocation.
func NewTransformLedger() *TransformLedger { return &TransformLedger{} }

// Record appends one transformation record. A nil ledger drops it (the tool ran without a
// recording sink), so the write tools never need to nil-check before recording.
func (l *TransformLedger) Record(r core.TransformRecord) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, r)
}

// take returns a copy of the accumulated records (nil if none) for the terminal lifecycle
// tool to fold into the Result. Nil-safe.
func (l *TransformLedger) take() []core.TransformRecord {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.records) == 0 {
		return nil
	}
	return append([]core.TransformRecord(nil), l.records...)
}
