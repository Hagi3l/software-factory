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
	StreamWork      = "SOFTWARE_FACTORY_WORK"
	StreamResult    = "SOFTWARE_FACTORY_RESULT"
	StreamDLQ       = "SOFTWARE_FACTORY_DLQ"
	StreamApprovals = "SOFTWARE_FACTORY_APPROVALS"
)

// defaultResultMaxAge bounds how long Result envelopes are retained for the
// orchestrator to consume and for observability/replay when the infra overlay does
// not override it. A week comfortably covers the consume-and-replay window; a
// distributed deployment tunes it via nats.jetstream.max_age (see StreamOptions).
const defaultResultMaxAge = 7 * 24 * time.Hour

// StreamOptions carries the environment-varying JetStream knobs the messaging package
// applies to every stream definition. They come from the infra overlay
// (config.JetStreamConfig, surfaced as nats.jetstream) and resolve the messaging.md
// "concrete stream definitions" OPEN: the subjects and retention *policy* are fixed by
// the harness's semantics (below), while the replication factor and the result
// stream's retention window genuinely vary by deployment. The zero value keeps the
// built-in bootstrap defaults, so a single-process dev run that omits these knobs
// behaves exactly as before.
type StreamOptions struct {
	// Replicas is the JetStream replication factor applied to every stream. 0 or 1 is a
	// single replica (the only option on the in-process embedded server); >1 needs an
	// external cluster of at least that size (validated in config — replicas>1 requires
	// nats.url). More replicas trade write latency for surviving a node loss.
	Replicas int
	// ResultMaxAge overrides how long the result stream retains envelopes. 0 keeps
	// defaultResultMaxAge. The work, dlq, and approvals streams are deliberately NOT
	// age-bounded (work is consume-once; dlq and approvals must survive until a human
	// acts), so this knob applies only to the replay/observability result stream.
	ResultMaxAge time.Duration
}

// JetStream opens the JetStream API over a connection.
func JetStream(nc *nats.Conn) (jetstream.JetStream, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("messaging: open jetstream: %w", err)
	}
	return js, nil
}

// streamConfigs returns the four streams the harness depends on, with opts applied.
// The retention choices encode the spec's semantics:
//   - work uses WorkQueue retention so each assignment is consumed exactly once and
//     the consumer ack doubles as the lease (AckWait → redelivery on a dead runner).
//   - result, dlq, and approvals use Limits retention so messages persist — results
//     for the orchestrator to consume and for replay, dlq for human triage, approvals
//     until consumed. DLQ and approvals have no max-age: they must survive until a
//     human handles them. Only the result stream is age-bounded.
//
// opts.Replicas applies uniformly (every stream survives the same node loss); a value
// <1 normalizes to 1, the single-replica embedded/dev case.
func streamConfigs(opts StreamOptions) []jetstream.StreamConfig {
	replicas := opts.Replicas
	if replicas < 1 {
		replicas = 1
	}
	resultMaxAge := opts.ResultMaxAge
	if resultMaxAge <= 0 {
		resultMaxAge = defaultResultMaxAge
	}
	return []jetstream.StreamConfig{
		{
			Name:        StreamWork,
			Description: "Work assignments dispatched to roles; pull consumers compete across runners.",
			Subjects:    []string{WorkStreamSubjects},
			Retention:   jetstream.WorkQueuePolicy,
			Storage:     jetstream.FileStorage,
			Replicas:    replicas,
		},
		{
			Name:        StreamResult,
			Description: "Agent Result envelopes consumed and validated by the orchestrator.",
			Subjects:    []string{ResultStreamSubjects},
			Retention:   jetstream.LimitsPolicy,
			Storage:     jetstream.FileStorage,
			MaxAge:      resultMaxAge,
			Replicas:    replicas,
		},
		{
			Name:        StreamDLQ,
			Description: "Dead-lettered work awaiting human triage.",
			Subjects:    []string{SubjectDLQ},
			Retention:   jetstream.LimitsPolicy,
			Storage:     jetstream.FileStorage,
			Replicas:    replicas,
		},
		{
			Name:        StreamApprovals,
			Description: "Human approve/reject decisions for parked integrate candidates.",
			Subjects:    []string{SubjectApprovals},
			Retention:   jetstream.LimitsPolicy,
			Storage:     jetstream.FileStorage,
			Replicas:    replicas,
		},
	}
}

// SetupStreams creates or updates the work, result, dead-letter, and approvals streams
// with the deployment's StreamOptions applied. It is idempotent — safe to call on every
// startup, matching the orchestrator's crash-and-resume model where startup steps are
// re-run (see specs/components/orchestrator.md). Every caller in one deployment must
// pass the SAME options: CreateOrUpdateStream reconciles an existing stream to the
// config it is handed, so a second caller passing zero-value options would silently
// reset replicas/max-age back to the defaults (hence the orchestrator threads the same
// infra-derived options the composition root does).
func SetupStreams(ctx context.Context, js jetstream.JetStream, opts StreamOptions) error {
	for _, cfg := range streamConfigs(opts) {
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

// EnsureApprovalConsumer creates or updates the orchestrator's durable consumer over the
// approvals subject. Like the result consumer it is the single reader of an at-least-once
// stream, so the orchestrator's handling must be idempotent — its status-gated approval
// handling (act only on a parked, awaiting-approval issue) provides exactly that (T2.10).
func EnsureApprovalConsumer(ctx context.Context, js jetstream.JetStream) (jetstream.Consumer, error) {
	cons, err := js.CreateOrUpdateConsumer(ctx, StreamApprovals, jetstream.ConsumerConfig{
		Durable:       "orchestrator-approvals",
		FilterSubject: SubjectApprovals,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		return nil, fmt.Errorf("messaging: ensure approval consumer: %w", err)
	}
	return cons, nil
}
