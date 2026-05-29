package orchestrator

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/spec"
)

// TestRecompileSpecDeltaReissuesDriftedWork proves the heart of "recompile the delta" (T3.7):
// an in-flight issue pinned to the spec version it was briefed against is left alone while the
// spec is unchanged, but is reissued once a human edits that spec so the re-resolved slice no
// longer matches the pin. This is the structural drift detector — the agent never sees the
// edit, so realignment must be the orchestrator re-dispatching the work (see
// specs/specs-process.md). The sweep uses the real spec resolver against an on-disk fixture so
// the hash comparison is genuinely exercised, not faked.
func TestRecompileSpecDeltaReissuesDriftedWork(t *testing.T) {
	repo := t.TempDir()
	specPath := filepath.Join(repo, "specs", "orders.md")
	mustWriteSpec(t, specPath, "# Orders\nreject negatives\n")

	// Pin the issue to the original slice version, as dispatch (scheduleReady) would have.
	orig, err := spec.Resolve(repo, "specs/orders.md", 1)
	if err != nil {
		t.Fatalf("resolve original slice: %v", err)
	}
	pinned := spec.Hash(orig)

	o := orchWithRepo(repo, 1)
	bd := newFakeBeads()
	o.bd = bd
	bd.inflight = []core.Issue{{ID: "iss-1", Role: "implement", Status: statusInProgress, Spec: "specs/orders.md", SpecHash: pinned}}

	// No edit yet: the re-resolved hash matches the pin, so nothing is reissued.
	o.recompileSpecDelta(context.Background())
	if len(bd.reissued) != 0 {
		t.Fatalf("reissued %v with no spec change; want none", bd.reissued)
	}

	// A human refines the spec: the slice changes, so the pinned hash is now stale.
	mustWriteSpec(t, specPath, "# Orders\nreject negatives AND zero\n")
	o.recompileSpecDelta(context.Background())
	if len(bd.reissued) != 1 || bd.reissued[0] != "iss-1" {
		t.Fatalf("reissued = %v, want [iss-1] after the spec edit", bd.reissued)
	}
}

// TestRecompileSpecDeltaSkipsWithoutDriftSignal proves the sweep's best-effort discipline: it
// touches in-flight work only on a clear drift signal. An issue with no spec reference, an
// issue dispatched but not yet pinned (degraded, not drifted), and an issue whose spec no
// longer resolves (mid-edit/deleted — an ambiguous signal) are all left in_progress rather
// than disrupted, mirroring buildBrief's degradation discipline.
func TestRecompileSpecDeltaSkipsWithoutDriftSignal(t *testing.T) {
	repo := t.TempDir()
	mustWriteSpec(t, filepath.Join(repo, "specs", "orders.md"), "# Orders\nreject negatives\n")

	o := orchWithRepo(repo, 1)
	bd := newFakeBeads()
	o.bd = bd
	bd.inflight = []core.Issue{
		{ID: "no-spec", Role: "implement", Status: statusInProgress},                                                  // no spec ref -> nothing to diff
		{ID: "no-pin", Role: "implement", Status: statusInProgress, Spec: "specs/orders.md"},                          // dispatched, not yet pinned -> skip
		{ID: "gone", Role: "implement", Status: statusInProgress, Spec: "specs/gone.md", SpecHash: "sha256:deadbeef"}, // unresolvable -> leave alone
	}

	o.recompileSpecDelta(context.Background())
	if len(bd.reissued) != 0 {
		t.Fatalf("reissued %v; none should be reissued (no spec / no pin / unresolvable)", bd.reissued)
	}
}
