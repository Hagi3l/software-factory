package model

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// Transient/FaultUsage classify through arbitrary wrapping (the adapters prefix their
// errors), a bare error is terminal with no usage, and Unwrap keeps errors.Is working
// on the underlying cause.
func TestFaultClassificationAndUnwrap(t *testing.T) {
	base := errors.New("boom")
	fault := &Fault{Err: fmt.Errorf("openai: stream: %w", base), Transient: true, Usage: Usage{InputTokens: 3}}
	wrapped := fmt.Errorf("relay: %w", fault)

	if !Transient(wrapped) {
		t.Error("Transient(wrapped transient fault) = false, want true")
	}
	if u, ok := FaultUsage(wrapped); !ok || u.InputTokens != 3 {
		t.Errorf("FaultUsage(wrapped) = %+v %v, want {InputTokens:3} true", u, ok)
	}
	if !errors.Is(wrapped, base) {
		t.Error("errors.Is through Fault.Unwrap lost the underlying cause")
	}

	if Transient(base) {
		t.Error("Transient(bare error) = true, want false (unclassified errors are terminal)")
	}
	if _, ok := FaultUsage(base); ok {
		t.Error("FaultUsage(bare error) reported usage, want none")
	}
	if Transient(&Fault{Err: base}) {
		t.Error("Transient(terminal fault) = true, want false")
	}
}

func TestTransientStatus(t *testing.T) {
	for code, want := range map[int]bool{
		408: true, 429: true, 500: true, 502: true, 503: true, 529: true,
		200: false, 400: false, 401: false, 403: false, 404: false, 422: false,
	} {
		if got := TransientStatus(code); got != want {
			t.Errorf("TransientStatus(%d) = %v, want %v", code, got, want)
		}
	}
}

// A dead caller ctx is terminal (retrying cannot help someone who left); any other
// status-less error is the wire failing — transient.
func TestTransientWire(t *testing.T) {
	if TransientWire(context.Canceled) || TransientWire(fmt.Errorf("call: %w", context.DeadlineExceeded)) {
		t.Error("a canceled/deadline ctx must classify terminal")
	}
	if !TransientWire(errors.New("connection reset by peer")) {
		t.Error("a status-less wire error must classify transient")
	}
}
