package model

import (
	"context"
	"errors"
	"net/http"
)

// Fault wraps a provider completion error with the canonical transport-level
// classification the trusted layer retries on (specs/models.md "Transient provider
// faults are absorbed at the relay"). Adapters construct it — they are the only
// layer that can read their SDK's error shape — and everything above classifies
// through the predicates below, staying provider-unaware.
type Fault struct {
	// Err is the underlying error, already wrapped with the adapter's provider prefix.
	Err error
	// Transient marks a fault of the wire, not of the request: a rate limit, a
	// provider 5xx, a dropped or reset stream. Safe to re-issue as a fresh request.
	// Terminal faults — auth, malformed request, context overflow — stay false.
	Transient bool
	// Usage is what the provider billed for the failed attempt: a mid-stream fault
	// lands after input tokens (and some output) were already counted. Usually zero.
	// The relay tallies it so retried attempts still draw the invocation's budget
	// and the termination guarantee holds across retries.
	Usage Usage
}

func (f *Fault) Error() string { return f.Err.Error() }
func (f *Fault) Unwrap() error { return f.Err }

// Transient reports whether err carries a Fault classified as transient. A bare,
// unclassified error is terminal — retry is opted into by the adapter that
// understands the provider, never guessed at by the caller.
func Transient(err error) bool {
	var f *Fault
	return errors.As(err, &f) && f.Transient
}

// FaultUsage returns the billed usage attached to a failed completion attempt, and
// false when err carries no Fault (an unclassified error has no usage to report).
func FaultUsage(err error) (Usage, bool) {
	var f *Fault
	if errors.As(err, &f) {
		return f.Usage, true
	}
	return Usage{}, false
}

// TransientStatus is the shared HTTP-status policy for provider faults: a request
// timeout, a rate limit, and any server-side 5xx are transient; every other status
// is terminal (the 4xx family is the request being wrong — auth, malformed body,
// context overflow — which no retry can fix).
func TransientStatus(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500
}

// TransientWire classifies an error with no readable provider status: a canceled or
// deadline-exceeded context is terminal (the caller is going away — retrying cannot
// help), anything else is the wire itself failing — a connection reset, an aborted
// SSE stream — and worth a fresh request.
func TransientWire(err error) bool {
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}
