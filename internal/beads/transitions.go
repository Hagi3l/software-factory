package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Loxstomper/harness/internal/core"
)

// MetadataKeyLease is the beads metadata key holding an in_progress issue's lease
// expiry (RFC3339, UTC). The lease is the crash-safety mechanism: the orchestrator
// holds no critical in-memory state, so a restart re-derives which in_progress
// issues are stranded by reading this expiry rather than trusting memory (see
// specs/components/orchestrator.md). beads has its own --claim verb, but it is
// owner-based with no TTL; the harness needs an expiry it can sweep, so the lease
// is modeled explicitly in metadata.
const MetadataKeyLease = "lease_until"

// MetadataKeyRetries holds an issue's on_failure retry generation (an integer). A
// freshly seeded or produced issue has none (treated as 0); each on_failure route
// creates a new fix issue carrying its predecessor's count plus one. It is the
// persistent counter the orchestrator enforces the retry cap against — the primary
// termination guarantee, since acyclicity alone does not guarantee termination (see
// specs/workflow.md).
const MetadataKeyRetries = "retries"

// MetadataKeyBase holds the git ref a produced issue's candidate must branch from
// (core.Issue.Base). A freshly seeded issue has none (the orchestrator seeds from the
// pipeline base, main); a produced issue carries its predecessor's verified candidate
// branch so the next stage builds on it — e.g. implement branches from the
// author-tests candidate that holds the failing acceptance tests (see
// specs/workflow.md, specs/verification.md).
const MetadataKeyBase = "base"

// MetadataKeySpec holds the repository-relative path of the spec file an issue is
// governed by (core.Issue.Spec). A seeded issue carries the spec the operator pointed at;
// the planner stamps each child's governing spec; thereafter it is threaded forward like
// MetadataKeyBase so every stage of an epic resolves the same bounded spec slice for its
// Brief (see internal/spec, specs/specs-process.md). It is a path, not content — the slice
// is resolved on demand from it; T3.6 adds a separate key for the slice's content hash.
const MetadataKeySpec = "spec"

// MetadataKeySpecHash holds the content address of the spec slice an issue was last
// briefed against (core.Issue.SpecHash). Unlike MetadataKeySpec (the path, written at
// creation), the orchestrator pins this at dispatch via PinSpecHash, because it records the
// spec *version* the agent worked against; a later spec edit changes the re-resolved hash,
// which is how T3.7 detects stale in-flight work (see internal/spec, specs/specs-process.md).
const MetadataKeySpecHash = "spec_hash"

// MetadataKeyTraceMap holds the artifact-store hash of the test↔spec traceability map
// (core.Issue.TraceMap). A freshly seeded issue has none; the author-tests stage's map is
// threaded forward onto the issues it produces, like MetadataKeyBase, so it survives to
// the integrate stage where the orchestrator cites it in the merge provenance trailer
// (see specs/verification.md, specs/security.md).
const MetadataKeyTraceMap = "trace_map"

// MetadataKeySpentTokens and MetadataKeySpentUSD hold the cumulative spend of an issue's
// on_failure retry chain so far (core.Issue.SpentTokens / SpentUSD): the total tokens and
// dollars burned by earlier attempts at the same logical work item. A freshly seeded or
// next-stage issue carries neither (treated as 0); each on_failure route stamps the
// predecessor's running total plus the just-finished invocation's spend, so the
// orchestrator can enforce the cumulative per-issue budget — the budget half of the
// termination guarantee — against the sum rather than any single attempt. Like
// MetadataKeyRetries they are threaded forward across the loop (see specs/workflow.md).
const (
	MetadataKeySpentTokens = "spent_tokens"
	MetadataKeySpentUSD    = "spent_usd"
)

// MetadataKeySpentWall holds the cumulative wall-clock of an issue's on_failure retry chain
// so far (core.Issue.SpentWall), stored as a Go-duration string (e.g. "5m0s"). Like
// MetadataKeySpentTokens/USD it is threaded forward across the loop — each route stamps the
// predecessor's running total plus the just-finished invocation's elapsed time — so the
// orchestrator enforces the cumulative wall budget (config Policy.Budget.Wall) against the sum.
// A freshly seeded or next-stage issue carries none (treated as 0). It is the wall-clock third
// of the budget half of the termination guarantee (see specs/workflow.md).
const MetadataKeySpentWall = "spent_wall"

// MetadataKeyClosingTokens and MetadataKeyClosingUSD hold an issue's OWN invocation spend —
// the marginal tokens/dollars the single invocation answering this issue consumed
// (core.Issue.ClosingTokens / ClosingUSD), as opposed to the threaded chain total in
// MetadataKeySpentTokens/USD (its predecessors' spend). Unlike every key above they are NOT
// set at creation and NOT threaded: the orchestrator stamps them post-hoc via StampClosingSpend
// when it processes the issue's Result, so an epic's total spend is an aggregate read — the sum
// of MetadataKeyClosingUSD over all issues sharing an epic id. An epic fans out, so its total
// cannot be a threaded counter (that double-counts the shared prefix); summing per-issue
// marginals is what makes the cross-issue epic budget enforceable (see MetadataKeyEpicID,
// specs/workflow.md "epic_budget"). Absent (treated as 0) until the issue's invocation is
// processed, and only stamped when an epic budget is configured.
const (
	MetadataKeyClosingTokens = "closing_tokens"
	MetadataKeyClosingUSD    = "closing_usd"
)

// MetadataKeyEpicID holds the id of the root seed issue of an issue's epic (core.Issue.EpicID):
// the work item a human seeded, from which the whole fan-out descends. It is threaded forward
// onto every produced child and on_failure fix like MetadataKeyBase — a root seed carries none
// (it is its own epic; the orchestrator's epicOf supplies its own id as the fallback, exactly as
// Base falls back to the pipeline base), and each descendant carries the root's id so all issues
// of one epic share it. It is the key the cross-issue epic budget aggregates over (sum the
// per-issue closing spend of all issues with the same epic id) and that later spec-re-derivation
// work groups by (see specs/workflow.md, the plan's T3.7b).
const MetadataKeyEpicID = "epic_id"

// MetadataKeyTranscript holds the artifact-store hash of the broker-captured conversation
// from an issue's most recent invocation (core.Issue.Transcript). Unlike the merge trailer's
// transcript (retained only for merged work), the orchestrator stamps this onto the issue via
// StampTranscript when it processes the Result — whatever the disposition — so the decision
// trail is reachable for in-flight and dead-lettered work, not only merged commits. It is NOT
// set at creation and NOT threaded forward (each issue records its own latest run); absent
// until the first invocation is processed (see specs/observability.md, the plan's T4.15).
const MetadataKeyTranscript = "transcript"

// MetadataKeyTestsSoul and MetadataKeyImplementSoul hold the producing souls of the
// author-tests and implement stages (core.Issue.TestsSoul / ImplementSoul). Like
// MetadataKeyTraceMap they are threaded forward onto every produced child and on_failure
// fix so the producer≠verifier identities survive to the integrate stage (where TestsSoul
// rides into the merge trailer) and stay readable on in-flight / dead-lettered work for the
// verification view. The orchestrator also stamps the issue's OWN producing soul post-hoc
// (via StampSouls) when it processes the issue's Result, keyed off the stage's reserved
// proof — so each is set as its stage runs, then threaded. Empty until the relevant stage
// has run in the lineage (see specs/verification.md, the plan's T4.22).
const (
	MetadataKeyTestsSoul     = "tests_soul"
	MetadataKeyImplementSoul = "implement_soul"
)

// MetadataKeyGateVerdict holds the artifact-store hash of the assembled gate-verdict record
// for an issue's gate run (core.Issue.GateVerdict). Like MetadataKeyTranscript it is stamped
// post-hoc (via StampGateVerdict) for every disposition and NOT threaded forward — each issue
// records the verdict of its own gate run — so a rejected candidate's verdict is reachable for
// the verification view, not only a merged one (see specs/verification.md, the plan's T4.22).
const MetadataKeyGateVerdict = "gate_verdict"

// MetadataKeyTransformLog holds the artifact-store hash of an issue's transformation log
// (core.Issue.TransformLog). Like MetadataKeyGateVerdict it is stamped post-hoc (via
// StampTransformLog) for every disposition and NOT threaded forward — each issue records its
// own run's semantic-write mechanisms — so the verification view can weigh a candidate's
// text-fallback transformations, for a rejected candidate as much as a merged one (T6.3).
const MetadataKeyTransformLog = "transform_log"

// MetadataKeyIntegrated marks a child whose verified candidate landed on its integration branch
// (core.Issue.Integrated) — the durable distinction `closed` cannot make, since a bead closes
// for several reasons (integrated, superseded by an on_failure retry, or — the epic root —
// closed at decomposition). The orchestrator stamps it (StampIntegrated) in the merge path the
// instant a candidate lands, for both per-item and epic mode, so the board hero's epic roll-up
// counts integration rather than any close (integrated vs. total). It is stamped post-hoc and
// NOT threaded forward — each bead records its own landing — and is the durable signal the
// cold-start projection rebuild re-derives from (no git read). Absent (false) on every issue
// whose candidate has not integrated (see specs/integration.md "Integrated vs. closed", the
// plan's T8.3).
const MetadataKeyIntegrated = "integrated"

// MetadataKeyDLQReason holds the orchestrator's one-line classification of why an issue
// dead-lettered (core.Issue.DeadLetterReason) — the same reason published on the DLQ alert.
// It is written in the same transition that blocks the issue (Block) so the dead-letter queue
// and the Resolve wizard can show *why* the work is stuck. Empty on any issue that is not
// blocked (see specs/workflow.md, specs/control-room.md).
const MetadataKeyDLQReason = "dlq_reason"

// Approval-gate metadata (T2.10), written when an integrate is held for human approval and
// read back to resume it. Unlike the keys above they are NOT set at issue creation — they
// are written by status transitions (AwaitApproval / RecordApproval) on an already-existing
// issue — so a fresh or produced issue never carries them.
//
//   - MetadataKeyCandidateRef pins the exact candidate sha the parked issue is awaiting
//     approval on (core.Issue.CandidateRef): the binding a `harness approve` must name and
//     the orchestrator re-checks, so a stale approval (the candidate changed) is invalidated.
//   - MetadataKeyParkedProv holds the JSON-encoded core.Provenance captured at park time
//     (core.Issue.ParkedProvenance), replayed onto the merge commit on approval so the
//     already-verified candidate's provenance is preserved rather than re-graded.
//   - MetadataKeyApprover / MetadataKeyApprovedRef record who approved and which candidate
//     sha they approved — an audit trail stamped before the merge proceeds.
const (
	MetadataKeyCandidateRef = "candidate_ref"
	MetadataKeyParkedProv   = "parked_prov"
	MetadataKeyApprover     = "approver"
	MetadataKeyApprovedRef  = "approved_ref"
)

// MetadataKeyStateEntered holds the time an issue last entered its current beads status
// (core.Issue.StateEnteredAt), as an RFC3339 UTC timestamp. Every status-changing write
// stamps it in the SAME bd update that changes the status — setStatus (Close/Block/
// AwaitApproval/Release/Reissue) and Claim — so it is atomic with the transition and stamped
// exactly once per real transition, mirroring how Claim stamps lease_until. A metadata-only
// write (PinSpecHash/StampClosingSpend/StampTranscript/RecordApproval) does NOT touch it: it
// records the *entry* into a status, not any later annotation. It is the durable anchor the
// control-room board ticks its time-in-state counter from and the close companion of the
// fire-and-forget issue-state event the orchestrator publishes on the same transition (see
// core.IssueStateEvent, specs/components/orchestrator.md §9). Absent (zero) on an issue that
// has not transitioned since the field was introduced.
const MetadataKeyStateEntered = "state_entered_at"

// stateEnteredNow returns the --set-metadata argument pair stamping state_entered_at at the
// current instant (UTC, RFC3339). It is appended to every status-changing write so the stamp
// is atomic with the status change (a single bd update), never a second write that could fail
// independently and leave the anchor stale.
func stateEnteredNow() []string {
	return []string{"--set-metadata", MetadataKeyStateEntered + "=" + time.Now().UTC().Format(time.RFC3339)}
}

// The transitions below are the orchestrator's single-writer interface to beads:
// only the orchestrator mutates the work graph, so funneling every status change
// and proposal application through these methods is what enforces the single-writer
// invariant (see specs/architecture.md). Agents never call them — they propose via
// a Result, and the orchestrator applies the validated proposals here.

// Claim transitions a ready issue to in_progress and stamps a lease expiring after
// ttl. It returns the lease expiry (UTC). Dispatch and claim are one step in the
// reconcile loop; the lease plus JetStream AckWait is what lets a dead runner's work
// be reclaimed (see specs/components/orchestrator.md). The lease is merged into
// metadata, preserving the role written at creation.
func (c *Client) Claim(ctx context.Context, id string, ttl time.Duration) (time.Time, error) {
	if id == "" {
		return time.Time{}, fmt.Errorf("beads: empty issue id")
	}
	if ttl <= 0 {
		return time.Time{}, fmt.Errorf("beads: claim ttl must be positive, got %s", ttl)
	}
	until := time.Now().UTC().Add(ttl)
	args := []string{"update", id, "--status", "in_progress",
		"--set-metadata", MetadataKeyLease + "=" + until.Format(time.RFC3339)}
	args = append(args, stateEnteredNow()...)
	if _, err := c.run(ctx, args); err != nil {
		return time.Time{}, fmt.Errorf("beads: claim issue %s: %w", id, err)
	}
	return until, nil
}

// PinSpecHash records the content address of the spec slice an issue was briefed against,
// merged into its metadata without touching status or other keys. The orchestrator calls it
// at dispatch, once the slice is materialized, so the issue durably carries the spec version
// its work was derived from — the pin T3.7 diffs against to find work made stale by a spec
// edit (see core.Issue.SpecHash, internal/spec, specs/specs-process.md). An empty hash is a
// no-op: an issue naming no spec has nothing to pin.
func (c *Client) PinSpecHash(ctx context.Context, id, hash string) error {
	if id == "" {
		return fmt.Errorf("beads: empty issue id")
	}
	if hash == "" {
		return nil
	}
	args := []string{"update", id, "--set-metadata", MetadataKeySpecHash + "=" + hash}
	if _, err := c.run(ctx, args); err != nil {
		return fmt.Errorf("beads: pin spec hash on %s: %w", id, err)
	}
	return nil
}

// StampClosingSpend records an issue's own invocation spend — the marginal tokens and dollars
// the single invocation answering this issue consumed — merged into its metadata without
// touching status or other keys (like PinSpecHash). The orchestrator calls it when it processes
// the issue's Result, so the cross-issue epic budget can be read as an aggregate: the sum of
// closing spend over every issue sharing an epic id (a fan-out's total cannot be a threaded
// counter without double-counting the shared prefix — see core.Issue.ClosingTokens,
// specs/workflow.md "epic_budget"). It is a set, not an increment, so re-stamping the same
// Result on an at-least-once redelivery is idempotent. bd stores a numeric --set-metadata value
// as a JSON number, so metaInt/metaFloat read these back directly.
func (c *Client) StampClosingSpend(ctx context.Context, id string, tokens int, usd float64) error {
	if id == "" {
		return fmt.Errorf("beads: empty issue id")
	}
	args := []string{"update", id,
		"--set-metadata", fmt.Sprintf("%s=%d", MetadataKeyClosingTokens, tokens),
		"--set-metadata", fmt.Sprintf("%s=%s", MetadataKeyClosingUSD, strconv.FormatFloat(usd, 'f', -1, 64))}
	if _, err := c.run(ctx, args); err != nil {
		return fmt.Errorf("beads: stamp closing spend on %s: %w", id, err)
	}
	return nil
}

// StampTranscript records the artifact-store hash of an issue's most recent invocation
// transcript, merged into its metadata without touching status or other keys (like
// PinSpecHash / StampClosingSpend). The orchestrator calls it when it processes the issue's
// Result — whatever the disposition — so the decision trail is reachable from the issue for
// in-flight and dead-lettered work, not only from a merge trailer (which exists only for
// merged work). It is a set, not an append, so re-stamping the same hash on an at-least-once
// redelivery is idempotent. An empty hash is a no-op: the runner could not persist a
// transcript, so there is nothing to cite.
func (c *Client) StampTranscript(ctx context.Context, id, hash string) error {
	if id == "" {
		return fmt.Errorf("beads: empty issue id")
	}
	if hash == "" {
		return nil
	}
	args := []string{"update", id, "--set-metadata", MetadataKeyTranscript + "=" + hash}
	if _, err := c.run(ctx, args); err != nil {
		return fmt.Errorf("beads: stamp transcript on %s: %w", id, err)
	}
	return nil
}

// StampSouls records the producing soul(s) of an issue's stage, merged into its metadata
// without touching status or other keys (like StampTranscript). The orchestrator calls it
// when it processes the issue's Result, stamping whichever of TestsSoul/ImplementSoul the
// stage's reserved proof identifies — so the producer≠verifier identities are recorded as
// each stage runs, then threaded forward like the traceability map. Only a non-empty value
// is written, so a caller stamping just the tests soul (the common case) leaves the other
// key untouched, and a call with both empty is a no-op. It is a set, not an append, so
// re-stamping under at-least-once redelivery is idempotent (see core.Issue.TestsSoul,
// specs/verification.md).
func (c *Client) StampSouls(ctx context.Context, id, testsSoul, implementSoul string) error {
	if id == "" {
		return fmt.Errorf("beads: empty issue id")
	}
	args := []string{"update", id}
	if testsSoul != "" {
		args = append(args, "--set-metadata", MetadataKeyTestsSoul+"="+testsSoul)
	}
	if implementSoul != "" {
		args = append(args, "--set-metadata", MetadataKeyImplementSoul+"="+implementSoul)
	}
	if len(args) == 2 {
		return nil // nothing to stamp
	}
	if _, err := c.run(ctx, args); err != nil {
		return fmt.Errorf("beads: stamp souls on %s: %w", id, err)
	}
	return nil
}

// StampGateVerdict records the artifact-store hash of an issue's gate-verdict record, merged
// into its metadata without touching status or other keys (like StampTranscript). The
// orchestrator calls it when it gates the issue's candidate — for every disposition — so the
// assembled verdict is reachable from the issue for the verification view, including for
// dead-lettered work. It is a set, not an append, so re-stamping the same hash under
// at-least-once redelivery is idempotent. An empty hash is a no-op: the gate could not
// persist a verdict, so there is nothing to cite (see core.Issue.GateVerdict).
func (c *Client) StampGateVerdict(ctx context.Context, id, hash string) error {
	if id == "" {
		return fmt.Errorf("beads: empty issue id")
	}
	if hash == "" {
		return nil
	}
	args := []string{"update", id, "--set-metadata", MetadataKeyGateVerdict + "=" + hash}
	if _, err := c.run(ctx, args); err != nil {
		return fmt.Errorf("beads: stamp gate verdict on %s: %w", id, err)
	}
	return nil
}

// StampTransformLog records the artifact-store hash of an issue's transformation log, merged
// into its metadata without touching status or other keys (like StampGateVerdict). The
// orchestrator calls it when it processes the issue's Result — for every disposition — so the
// semantic-vs-text-fallback record of the issue's writes is reachable from the issue for the
// verification view, including for dead-lettered work. It is a set, not an append, so
// re-stamping the same hash under at-least-once redelivery is idempotent. An empty hash is a
// no-op: the invocation ran no semantic write tools, so there is nothing to cite (see
// core.Issue.TransformLog).
func (c *Client) StampTransformLog(ctx context.Context, id, hash string) error {
	if id == "" {
		return fmt.Errorf("beads: empty issue id")
	}
	if hash == "" {
		return nil
	}
	args := []string{"update", id, "--set-metadata", MetadataKeyTransformLog + "=" + hash}
	if _, err := c.run(ctx, args); err != nil {
		return fmt.Errorf("beads: stamp transform log on %s: %w", id, err)
	}
	return nil
}

// StampIntegrated marks a child's bead as integrated — its verified candidate landed on the
// integration branch — merged into its metadata without touching status or other keys (like
// StampGateVerdict). The orchestrator calls it in the merge path the instant the candidate
// lands, BEFORE it closes the bead, so the durable marker that distinguishes an integration
// from any other close is written exactly once per landing. It is a set (the value is always
// true), so re-stamping on an at-least-once redelivery of the same Result is idempotent. The
// value is stored as the JSON bool true, which metaBool reads back directly (see
// core.Issue.Integrated, specs/integration.md "Integrated vs. closed").
func (c *Client) StampIntegrated(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("beads: empty issue id")
	}
	args := []string{"update", id, "--set-metadata", MetadataKeyIntegrated + "=true"}
	if _, err := c.run(ctx, args); err != nil {
		return fmt.Errorf("beads: stamp integrated on %s: %w", id, err)
	}
	return nil
}

// Release returns an issue to the ready pool and clears its lease — used by the
// reconcile sweep to reset work stranded by a dead runner so JetStream redelivery
// can re-dispatch it.
func (c *Client) Release(ctx context.Context, id string) error {
	return c.setStatus(ctx, id, "open", "--unset-metadata", MetadataKeyLease)
}

// Reissue invalidates an in-flight issue made stale by a spec edit and returns it to the
// ready pool. It is the re-derive half of "recompile the delta" (see specs/specs-process.md):
// when a human refines a spec while work is underway, the agent cannot see the change (its
// slice was materialized once, at dispatch), so realignment must be structural — the
// orchestrator reissues the work so it is re-dispatched against the now-current slice. Like
// Release, the stale in-flight attempt's eventual Result is ignored because the issue is no
// longer in_progress. Unlike Release (dead-runner recovery, which keeps the pin so the
// SAME spec version is re-dispatched), Reissue clears the pinned spec hash too: the spec
// version itself changed, so the issue must carry no stale pin until the next dispatch
// re-resolves the edited slice and re-pins it.
func (c *Client) Reissue(ctx context.Context, id string) error {
	return c.setStatus(ctx, id, "open", "--unset-metadata", MetadataKeyLease, "--unset-metadata", MetadataKeySpecHash)
}

// Close marks an issue accepted. Acceptance is the orchestrator's decision after a
// passing gate, never an agent's self-report (producer != verifier — see
// specs/verification.md).
func (c *Client) Close(ctx context.Context, id string) error {
	return c.setStatus(ctx, id, "closed")
}

// Block dead-letters an issue: on a budget/retry breach or an unrecoverable
// escalation it is marked blocked for human triage via spec refinement. Emitting the
// DLQ alert event is the orchestrator's job (T1.19); this is the beads-side write. The
// reason — the orchestrator's one-line classification of why the work terminated — is
// stamped in the same transition (MetadataKeyDLQReason) so the dead-letter queue and the
// Resolve wizard can show *why* without re-deriving it; an empty reason stamps nothing.
func (c *Client) Block(ctx context.Context, id, reason string) error {
	if reason == "" {
		return c.setStatus(ctx, id, "blocked")
	}
	return c.setStatus(ctx, id, "blocked", "--set-metadata", MetadataKeyDLQReason+"="+reason)
}

// AwaitApproval parks an issue awaiting human approval of its integrate candidate (T2.10):
// it marks the issue blocked (so it surfaces in the escalation/DLQ view like any other work
// needing a human) and records the candidate ref it is parked on plus the provenance to
// replay on approval. Unlike a dead-letter this is recoverable: a later `harness approve`
// for this candidate resumes the merge, a reject routes a fix. candidateRef must be set
// (there is nothing to approve otherwise); parkedProv may be empty (a degraded provenance
// record, never a dropped park). See specs/configuration.md "human-approval".
func (c *Client) AwaitApproval(ctx context.Context, id, candidateRef, parkedProv string) error {
	if candidateRef == "" {
		return fmt.Errorf("beads: await approval on %s: empty candidate ref", id)
	}
	extra := []string{"--set-metadata", MetadataKeyCandidateRef + "=" + candidateRef}
	if parkedProv != "" {
		extra = append(extra, "--set-metadata", MetadataKeyParkedProv+"="+parkedProv)
	}
	return c.setStatus(ctx, id, "blocked", extra...)
}

// RecordApproval stamps a human's approval on a parked issue: who approved and which
// candidate sha they approved. It records the audit trail without changing status — the
// caller (the orchestrator's approval handler) drives the resume (re-merge → close) after
// recording, so the status moves blocked→closed through the merge path, not here. It is a
// metadata-only update, like PinSpecHash.
func (c *Client) RecordApproval(ctx context.Context, id, approvedRef, approver string) error {
	if id == "" {
		return fmt.Errorf("beads: empty issue id")
	}
	args := []string{"update", id,
		"--set-metadata", MetadataKeyApprovedRef + "=" + approvedRef,
		"--set-metadata", MetadataKeyApprover + "=" + approver}
	if _, err := c.run(ctx, args); err != nil {
		return fmt.Errorf("beads: record approval on %s: %w", id, err)
	}
	return nil
}

func (c *Client) setStatus(ctx context.Context, id, status string, extra ...string) error {
	if id == "" {
		return fmt.Errorf("beads: empty issue id")
	}
	// Every status change stamps state_entered_at atomically (a single bd update), so the
	// time-in-state anchor is set exactly once per transition for free — see MetadataKeyStateEntered.
	args := append([]string{"update", id, "--status", status}, extra...)
	args = append(args, stateEnteredNow()...)
	if _, err := c.run(ctx, args); err != nil {
		return fmt.Errorf("beads: set issue %s status=%s: %w", id, status, err)
	}
	return nil
}

// Apply writes a batch of validated child-issue proposals: it creates each child
// (carrying its role in metadata, per MetadataKeyRole) and adds its blocked-by edges.
// bd rejects any edge that would close a cycle, which is the acyclicity guarantee for
// the issue DAG (see specs/architecture.md).
//
// It runs in two phases — create ALL the children first, then add ALL the edges —
// because a proposal may depend on a sibling proposed in the same batch (a
// decomposition planner emits an ordered set of children at once, before any has an
// ID). Each DependsOn entry is resolved against the batch's Key→ID map: an entry that
// matches a sibling's Proposal.Key becomes that sibling's freshly assigned ID;
// anything else is treated as an existing issue ID. Creating every child before any
// edge means forward references (A depends on a sibling created later) resolve too.
//
// Before creating anything, Apply checks that every dependency target which is not a
// same-batch sibling key already exists, so a hostile proposal cannot plant a dangling
// edge to a fabricated id (closing the bd-1.0.4 foreign-prefix gap — see the check
// below).
//
// Apply is all-or-nothing: if any create or edge fails, every issue created in this
// call is deleted, so the orchestrator can safely retry. It returns the created issues
// with their assigned IDs, in proposal order.
func (c *Client) Apply(ctx context.Context, proposals []core.Proposal) ([]core.Issue, error) {
	keyToIndex := map[string]int{}
	for i, p := range proposals {
		if strings.TrimSpace(p.Issue.Title) == "" {
			return nil, fmt.Errorf("beads: proposal %d has an empty title", i)
		}
		if p.Issue.Role == "" {
			return nil, fmt.Errorf("beads: proposal %d (%q) has an empty role", i, p.Issue.Title)
		}
		for _, dep := range p.DependsOn {
			if dep == "" {
				return nil, fmt.Errorf("beads: proposal %d (%q) has an empty dependency id", i, p.Issue.Title)
			}
		}
		if p.Key != "" {
			if _, dup := keyToIndex[p.Key]; dup {
				return nil, fmt.Errorf("beads: proposal %d (%q) reuses local key %q", i, p.Issue.Title, p.Key)
			}
			keyToIndex[p.Key] = i
		}
	}

	// Referential-integrity check: every dependency target that is not a same-batch
	// sibling key must already exist in the store. The harness verifies this itself
	// through the read path rather than trusting `bd dep add` to reject a dangling edge:
	// bd 1.0.4 validates a same-prefix target but treats a foreign-prefix id as an
	// unchecked external (federation) reference and silently accepts it, so an untrusted
	// proposal could otherwise plant an edge to a fabricated id (e.g. "other-123") and
	// corrupt the work graph — exactly the kind of mutation single-writer validation must
	// reject (see specs/architecture.md, specs/security.md). The check is
	// prefix-independent — Get resolves any id against the local store and errors on a
	// miss regardless of prefix — and runs before any issue is created, so an illegal
	// proposal fails the whole batch with nothing to roll back. A sibling key is satisfied
	// by construction (its issue is created in Phase 1), so it is skipped here exactly as
	// Phase 2 resolves it from the batch. Targets are deduplicated so a batch that fans out
	// to one shared dependency probes it once.
	checked := map[string]bool{}
	for i, p := range proposals {
		for _, dep := range p.DependsOn {
			if _, sibling := keyToIndex[dep]; sibling {
				continue
			}
			if checked[dep] {
				continue
			}
			if _, err := c.Get(ctx, dep); err != nil {
				return nil, fmt.Errorf("beads: proposal %d (%q) depends on unknown issue %q: %w", i, p.Issue.Title, dep, err)
			}
			checked[dep] = true
		}
	}

	created := make([]core.Issue, 0, len(proposals))
	rollback := func() {
		for _, is := range created {
			_, _ = c.run(ctx, []string{"delete", is.ID, "--force"})
		}
	}

	// Phase 1: create every child, recording each declared Key's assigned ID so a
	// later edge can resolve a sibling reference (including a forward one).
	keyToID := map[string]string{}
	for i, p := range proposals {
		id, err := c.create(ctx, p.Issue)
		if err != nil {
			rollback()
			return nil, fmt.Errorf("beads: create proposal %d (%q): %w", i, p.Issue.Title, err)
		}
		issue := p.Issue
		issue.ID = id
		created = append(created, issue)
		if p.Key != "" {
			keyToID[p.Key] = id
		}
	}

	// Phase 2: add the blocked-by edges, resolving sibling Keys to assigned IDs.
	for i, p := range proposals {
		for _, dep := range p.DependsOn {
			target := dep
			if id, ok := keyToID[dep]; ok {
				target = id
			}
			if _, err := c.run(ctx, []string{"dep", "add", created[i].ID, target}); err != nil {
				rollback()
				return nil, fmt.Errorf("beads: add dependency %s depends-on %s: %w", created[i].ID, target, err)
			}
		}
	}
	return created, nil
}

// create makes one issue and returns its assigned ID. bd create --json emits a
// single issue object (not an array, unlike ready/show).
func (c *Client) create(ctx context.Context, issue core.Issue) (string, error) {
	args := []string{"create", issue.Title, "--json"}
	if issue.Body != "" {
		args = append(args, "--description", issue.Body)
	}
	if issue.Role != "" || issue.Attempt > 0 || issue.Base != "" || issue.TraceMap != "" || issue.Spec != "" ||
		issue.SpentTokens > 0 || issue.SpentUSD > 0 || issue.SpentWall > 0 || issue.EpicID != "" ||
		issue.TestsSoul != "" || issue.ImplementSoul != "" {
		meta := map[string]any{}
		if issue.Role != "" {
			meta[MetadataKeyRole] = issue.Role
		}
		// Only stamp the spec path when set; a seed without --spec carries none and the
		// orchestrator dispatches with an empty slice (the worktree tree is the fallback).
		if issue.Spec != "" {
			meta[MetadataKeySpec] = issue.Spec
		}
		// Only stamp retries when nonzero so a fresh issue's metadata stays {"role":...}
		// (a 0 generation is the absence of the key, decoded back as 0 by metaInt).
		if issue.Attempt > 0 {
			meta[MetadataKeyRetries] = issue.Attempt
		}
		// Only stamp the base when set; a seeded issue carries none and the orchestrator
		// falls back to the pipeline base (main) when building the Brief.
		if issue.Base != "" {
			meta[MetadataKeyBase] = issue.Base
		}
		// Only stamp the traceability-map hash when set; it appears once the author-tests
		// stage produces an issue and is threaded forward from there.
		if issue.TraceMap != "" {
			meta[MetadataKeyTraceMap] = issue.TraceMap
		}
		// Only stamp cumulative spend when nonzero so a fresh/next-stage issue's metadata
		// stays minimal (absence decodes back as 0). These are threaded forward by the
		// orchestrator's on_failure route, accumulating across the retry chain.
		if issue.SpentTokens > 0 {
			meta[MetadataKeySpentTokens] = issue.SpentTokens
		}
		if issue.SpentUSD > 0 {
			meta[MetadataKeySpentUSD] = issue.SpentUSD
		}
		// Cumulative wall is threaded like the token/dollar spend; stored as a Go-duration
		// string so it round-trips through metadata legibly (metaDuration parses it back).
		if issue.SpentWall > 0 {
			meta[MetadataKeySpentWall] = issue.SpentWall.String()
		}
		// The epic root id threads forward like Base so every issue of an epic shares it; a
		// root seed carries none (epicOf supplies its own id). Stamped when set so a root's
		// metadata stays minimal.
		if issue.EpicID != "" {
			meta[MetadataKeyEpicID] = issue.EpicID
		}
		// The producing souls thread forward like TraceMap so the producer≠verifier identities
		// survive across the stages of an epic and across on_failure retries; stamped when set so
		// a freshly seeded issue (no stage has run yet) carries neither.
		if issue.TestsSoul != "" {
			meta[MetadataKeyTestsSoul] = issue.TestsSoul
		}
		if issue.ImplementSoul != "" {
			meta[MetadataKeyImplementSoul] = issue.ImplementSoul
		}
		b, err := json.Marshal(meta)
		if err != nil {
			return "", err
		}
		args = append(args, "--metadata", string(b))
	}
	// Selector tags ride in beads labels (one `key=value` label each), distinct from the
	// role/base/etc. that ride in metadata above, so a soul selector never collides with
	// the role binding (see core.Issue.Tags, MetadataKeyRole). bd's --labels is
	// comma-separated; keys are sorted so the label list is deterministic (stable provenance
	// and stable test assertions). parseLabels is the inverse on the read path.
	if len(issue.Tags) > 0 {
		args = append(args, "--labels", formatLabels(issue.Tags))
	}
	out, err := c.run(ctx, args)
	if err != nil {
		return "", err
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &created); err != nil {
		return "", fmt.Errorf("beads: decode created issue: %w", err)
	}
	if created.ID == "" {
		return "", fmt.Errorf("beads: created issue has no id")
	}
	return created.ID, nil
}

// formatLabels encodes selector tags as bd's comma-separated `key=value` label list,
// sorted by key for a deterministic, stable encoding (see create, parseLabels).
func formatLabels(tags map[string]string) string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + tags[k]
	}
	return strings.Join(parts, ",")
}
