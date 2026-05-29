package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
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
// DLQ alert event is the orchestrator's job (T1.19); this is the beads-side write.
func (c *Client) Block(ctx context.Context, id string) error {
	return c.setStatus(ctx, id, "blocked")
}

// ListStranded returns the IDs of in_progress issues whose lease has expired as of
// now (or that carry no lease at all). It is the input to the orchestrator's
// reconcile sweep: a runner that died mid-task leaves its issue in_progress, and the
// lease — not orchestrator memory — is the durable record of that, so a restarted
// orchestrator re-derives the stranded set by reading beads (see
// specs/components/orchestrator.md). The orchestrator releases each back to ready.
func (c *Client) ListStranded(ctx context.Context, now time.Time) ([]string, error) {
	out, err := c.run(ctx, []string{"list", "--status", "in_progress", "--json", "--limit", "0"})
	if err != nil {
		return nil, fmt.Errorf("beads: list in_progress: %w", err)
	}
	data := bytes.TrimSpace(out)
	if len(data) == 0 {
		return nil, nil
	}
	var raw []issueJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("beads: decode in_progress issues: %w", err)
	}
	var stranded []string
	for _, r := range raw {
		lease := metaString(r.Metadata, MetadataKeyLease)
		if lease == "" {
			// An in_progress issue with no lease is anomalous (Claim always stamps one);
			// treat it as stranded so it cannot wedge in_progress forever.
			stranded = append(stranded, r.ID)
			continue
		}
		until, perr := time.Parse(time.RFC3339, lease)
		if perr != nil || !until.After(now) {
			stranded = append(stranded, r.ID)
		}
	}
	return stranded, nil
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
		issue.SpentTokens > 0 || issue.SpentUSD > 0 {
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
