package messaging

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// JetStream stream names. By convention they are uppercase and contain no dots.
const (
	StreamWork   = "HARNESS_WORK"
	StreamResult = "HARNESS_RESULT"
	StreamDLQ    = "HARNESS_DLQ"
)

// resultMaxAge bounds how long Result envelopes are retained for the orchestrator
// to consume and for observability/replay. Concrete retention is an OPEN question
// in specs/messaging.md; this is a sensible bootstrap default, not a contract.
const resultMaxAge = 7 * 24 * time.Hour

// JetStream opens the JetStream API over a connection.
func JetStream(nc *nats.Conn) (jetstream.JetStream, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("messaging: open jetstream: %w", err)
	}
	return js, nil
}

// streamConfigs returns the three streams the harness depends on. The retention
// choices encode the spec's semantics:
//   - work uses WorkQueue retention so each assignment is consumed exactly once and
//     the consumer ack doubles as the lease (AckWait → redelivery on a dead runner).
//   - result and dlq use Limits retention so messages persist — results for the
//     orchestrator to consume and for replay, dlq for human triage. DLQ has no
//     max-age: it must survive until a human handles it.
func streamConfigs() []jetstream.StreamConfig {
	return []jetstream.StreamConfig{
		{
			Name:        StreamWork,
			Description: "Work assignments dispatched to roles; pull consumers compete across runners.",
			Subjects:    []string{WorkStreamSubjects},
			Retention:   jetstream.WorkQueuePolicy,
			Storage:     jetstream.FileStorage,
		},
		{
			Name:        StreamResult,
			Description: "Agent Result envelopes consumed and validated by the orchestrator.",
			Subjects:    []string{ResultStreamSubjects},
			Retention:   jetstream.LimitsPolicy,
			Storage:     jetstream.FileStorage,
			MaxAge:      resultMaxAge,
		},
		{
			Name:        StreamDLQ,
			Description: "Dead-lettered work awaiting human triage.",
			Subjects:    []string{SubjectDLQ},
			Retention:   jetstream.LimitsPolicy,
			Storage:     jetstream.FileStorage,
		},
	}
}

// SetupStreams creates or updates the work, result, and dead-letter streams. It is
// idempotent — safe to call on every startup, matching the orchestrator's
// crash-and-resume model where startup steps are re-run (see
// specs/components/orchestrator.md).
func SetupStreams(ctx context.Context, js jetstream.JetStream) error {
	for _, cfg := range streamConfigs() {
		if _, err := js.CreateOrUpdateStream(ctx, cfg); err != nil {
			return fmt.Errorf("messaging: ensure stream %s: %w", cfg.Name, err)
		}
	}
	return nil
}

// EnsureWorkConsumer creates or updates the durable pull consumer that a role's
// runners share. Multiple runner processes bind to the same durable and compete to
// pull, so adding runners scales throughput with no central coordinator. ackWait is
// the lease: a runner acks only after harvesting the agent's result, so if it dies
// first JetStream redelivers the assignment to another runner (see
// specs/messaging.md, specs/components/runner.md).
func EnsureWorkConsumer(ctx context.Context, js jetstream.JetStream, role string, ackWait time.Duration) (jetstream.Consumer, error) {
	cons, err := js.CreateOrUpdateConsumer(ctx, StreamWork, jetstream.ConsumerConfig{
		Durable:       "work-" + role,
		FilterSubject: WorkSubject(role),
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       ackWait,
	})
	if err != nil {
		return nil, fmt.Errorf("messaging: ensure work consumer for role %q: %w", role, err)
	}
	return cons, nil
}

// EnsureResultConsumer creates or updates the orchestrator's durable consumer over
// all result subjects. The orchestrator is the single reader that validates Result
// envelopes; delivery is at-least-once, so its processing must be idempotent — which
// its single-writer beads model already guarantees.
func EnsureResultConsumer(ctx context.Context, js jetstream.JetStream) (jetstream.Consumer, error) {
	cons, err := js.CreateOrUpdateConsumer(ctx, StreamResult, jetstream.ConsumerConfig{
		Durable:       "orchestrator-results",
		FilterSubject: ResultStreamSubjects,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		return nil, fmt.Errorf("messaging: ensure result consumer: %w", err)
	}
	return cons, nil
}
