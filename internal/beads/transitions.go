package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	if _, err := c.run(ctx, args); err != nil {
		return time.Time{}, fmt.Errorf("beads: claim issue %s: %w", id, err)
	}
	return until, nil
}

// Release returns an issue to the ready pool and clears its lease — used by the
// reconcile sweep to reset work stranded by a dead runner so JetStream redelivery
// can re-dispatch it.
func (c *Client) Release(ctx context.Context, id string) error {
	return c.setStatus(ctx, id, "open", "--unset-metadata", MetadataKeyLease)
}

// Close marks an issue accepted. Acceptance is the orchestrator's decision after a
// passing gate, never an agent's self-report (producer != verifier — see
// specs/verification.md).
func (c *Client) Close(ctx context.Context, id string) error {
	return c.setStatus(ctx, id, "closed")
}

// Block dead-letters an issue: on a budget/retry breach or an unrecoverable
// escalation it is marked blocked for human triage via spec refinement. Emitting the
// DLQ alert event is the orchestrator's job (T1.19); this is the beads-side write.
func (c *Client) Block(ctx context.Context, id string) error {
	return c.setStatus(ctx, id, "blocked")
}

func (c *Client) setStatus(ctx context.Context, id, status string, extra ...string) error {
	if id == "" {
		return fmt.Errorf("beads: empty issue id")
	}
	args := append([]string{"update", id, "--status", status}, extra...)
	if _, err := c.run(ctx, args); err != nil {
		return fmt.Errorf("beads: set issue %s status=%s: %w", id, status, err)
	}
	return nil
}

// Apply writes a batch of validated child-issue proposals: it creates each child
// (carrying its role in metadata, per MetadataKeyRole) and adds its blocked-by
// edges. bd rejects any edge that would close a cycle, which is the acyclicity
// guarantee for the issue DAG (see specs/architecture.md) — note the current
// Proposal shape (a brand-new child depending only on existing issues) cannot itself
// form a cycle, so this is defense-in-depth that also covers any future shape that
// adds edges among existing issues. Apply is all-or-nothing: if any create or edge
// fails, every issue created in this call is deleted, so the orchestrator can safely
// retry. It returns the created issues with their assigned IDs, in proposal order.
func (c *Client) Apply(ctx context.Context, proposals []core.Proposal) ([]core.Issue, error) {
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
	}

	created := make([]core.Issue, 0, len(proposals))
	rollback := func() {
		for _, is := range created {
			_, _ = c.run(ctx, []string{"delete", is.ID, "--force"})
		}
	}

	for i, p := range proposals {
		id, err := c.create(ctx, p.Issue)
		if err != nil {
			rollback()
			return nil, fmt.Errorf("beads: create proposal %d (%q): %w", i, p.Issue.Title, err)
		}
		issue := p.Issue
		issue.ID = id
		created = append(created, issue)

		for _, dep := range p.DependsOn {
			if _, err := c.run(ctx, []string{"dep", "add", id, dep}); err != nil {
				rollback()
				return nil, fmt.Errorf("beads: add dependency %s depends-on %s: %w", id, dep, err)
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
	if issue.Role != "" {
		meta, err := json.Marshal(map[string]string{MetadataKeyRole: issue.Role})
		if err != nil {
			return "", err
		}
		args = append(args, "--metadata", string(meta))
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
