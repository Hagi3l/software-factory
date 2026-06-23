package query

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/spec"
)

// statusInProgress and statusClosed are the beads statuses BlastRadius filters ListAll by,
// mirroring the orchestrator's spec-drift sweeps: recompileSpecDelta acts on in_progress work
// (reissue), recompileMergedDelta on closed work (spawn a re-derivation plan). Kept as local
// literals so the read model stays free of an orchestrator import.
const (
	statusInProgress = "in_progress"
	statusClosed     = "closed"
)

// ResolveContext is everything the Resolve-mode wizard pre-loads about a dead-lettered issue
// (specs/control-room.md "Create and Resolve are the same component"): the escalation that
// blocked it, the governing spec slice the human will refine, and the agent transcript that
// raised it. It is the read behind GET /resolve/{id}. The conversation itself is the wizard's
// job — this is the grounding the wizard and the human start from.
type ResolveContext struct {
	Issue core.Issue // the dead-lettered issue (id/title/role/spec/status/reason/attempt/spend)

	// Spec is the issue's governing spec path, echoed for convenience (== Issue.Spec). SpecSlice
	// is the resolved current content of that bounded slice — what the human reads and refines.
	// It is empty when the issue names no spec or the slice fails to resolve (a best-effort read,
	// never fatal — the wizard still opens, just without the slice pre-shown).
	Spec      string
	SpecSlice string

	// TranscriptHash is the artifact-store hash of the transcript that raised the escalation
	// (core.Issue.Transcript, stamped by the orchestrator for every disposition, T4.15), and
	// TranscriptAvailable reports whether it still resolves in the store. Empty hash means none
	// was harvested. The wizard surfaces a link to /artifact/{hash} (and /replay/{id}) so the
	// human can read exactly what the agent saw and said.
	TranscriptHash      string
	TranscriptAvailable bool
}

// ResolveContext assembles the escalation, the current spec slice, and the transcript reference
// for the Resolve wizard. Only an unreadable issue is fatal; the spec slice and transcript are
// best-effort grounding (a missing slice or unresolvable transcript degrades to empty rather
// than failing the page, mirroring IssueDetail). repo and depth are supplied by the caller (the
// server, from config) so the read model stays free of a config/filesystem-config dependency —
// the same threading StageOrder and BudgetCaps use.
func (r *Reader) ResolveContext(ctx context.Context, repo string, depth int, id string) (ResolveContext, error) {
	issue, err := r.issues.Get(ctx, id)
	if err != nil {
		return ResolveContext{}, fmt.Errorf("query: resolve context %s: %w", id, err)
	}
	rc := ResolveContext{Issue: issue, Spec: issue.Spec}

	if issue.Spec != "" {
		if slice, serr := spec.Resolve(repo, issue.Spec, depth); serr == nil {
			rc.SpecSlice = slice
		}
	}

	if issue.Transcript != "" {
		rc.TranscriptHash = issue.Transcript
		if r.arts != nil {
			if has, herr := r.arts.Has(ctx, issue.Transcript); herr == nil {
				rc.TranscriptAvailable = has
			}
		}
	}
	return rc, nil
}

// BlastItem is one in-flight issue a spec edit would re-pin and reissue: its identity plus the
// spec version it is currently pinned to (so the human sees it is about to change).
type BlastItem struct {
	ID       string
	Role     string
	Spec     string
	SpecHash string
}

// BlastGroup is one already-merged (epic, spec-path) unit a spec edit would re-derive: the
// orchestrator's merged-delta sweep spawns one fresh plan issue per such group, so the preview
// reports the group (epic + path + how many closed issues share it), not each closed issue.
type BlastGroup struct {
	Epic    string
	Spec    string
	Members int
}

// BlastRadius is the read-only preview of what editing the given spec paths would set in motion
// once committed (specs/control-room.md: "this change re-pins and reissues these 3 in-flight
// items" — the recompile-the-delta mechanism, specs/specs-process.md). It is the consequence the
// Resolve wizard shows at the moment of consent: the in-flight work the orchestrator's spec-drift
// sweep would reissue, and the merged work it would re-derive. EditedSpecs echoes which paths
// were assessed (so an empty draft reads as "nothing to assess yet").
type BlastRadius struct {
	EditedSpecs []string
	InFlight    []BlastItem
	Merged      []BlastGroup
}

// BlastRadius computes, read-only, which in-flight and already-merged issues a draft's spec
// edits would touch — the same predicate the orchestrator's recompile sweeps act on, run as a
// preview rather than a mutation. An issue is affected when its bounded spec slice *includes*
// one of the edited paths: the slice would then re-resolve to a different content hash and the
// sweep would reissue it (in-flight) or spawn a re-derivation plan (merged) — while an issue
// whose slice does not include the edit re-hashes unchanged and is left alone (this membership
// is exactly spec.Members, sharing the resolver's traversal, so the preview cannot diverge from
// what the sweep will do; see specs/specs-process.md "Spec drift"). It is best-effort: an issue
// with no spec or no pin, or whose slice fails to resolve (mid-edit), is skipped rather than
// guessed at — the same conservatism the sweep applies. repo and depth are supplied by the
// caller. editedPaths are the repo-relative spec paths the draft would write; ambient is the
// configured ambient_specs list (T3.14), which rides in EVERY slice — so editing one drifts all
// pinned work, exactly as the recompile sweeps now re-resolve ambient-aware.
func (r *Reader) BlastRadius(ctx context.Context, repo string, depth int, ambient, editedPaths []string) (BlastRadius, error) {
	edited := map[string]bool{}
	out := BlastRadius{}
	for _, p := range editedPaths {
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
		if clean == "" || clean == "." {
			continue
		}
		if !edited[clean] {
			edited[clean] = true
			out.EditedSpecs = append(out.EditedSpecs, clean)
		}
	}
	sort.Strings(out.EditedSpecs)
	if len(edited) == 0 {
		return out, nil
	}

	// An ambient spec is prepended to every issue's slice, so editing one drifts every pinned
	// issue regardless of its governing spec — short-circuit membership to true in that case so
	// the preview matches what the ambient-aware recompile sweeps will reissue/re-derive.
	ambientEdited := false
	for _, a := range ambient {
		if edited[filepath.ToSlash(filepath.Clean(filepath.FromSlash(a)))] {
			ambientEdited = true
			break
		}
	}

	all, err := r.issues.ListAll(ctx)
	if err != nil {
		return BlastRadius{}, fmt.Errorf("query: blast radius: %w", err)
	}

	// includesEdit reports whether the bounded slice rooted at the issue's spec includes any
	// edited path — memoized by spec path so a fan-out of issues sharing a spec resolves it once.
	// When an ambient file was edited it is unconditionally true (ambient rides in every slice).
	memo := map[string]bool{}
	includesEdit := func(specPath string) bool {
		if ambientEdited {
			return true
		}
		if hit, ok := memo[specPath]; ok {
			return hit
		}
		hit := false
		if members, merr := spec.Members(repo, specPath, depth); merr == nil {
			for _, m := range members {
				if edited[filepath.ToSlash(filepath.Clean(filepath.FromSlash(m)))] {
					hit = true
					break
				}
			}
		}
		memo[specPath] = hit
		return hit
	}

	// In-flight: the recompileSpecDelta analog. An in_progress issue with a pinned hash whose
	// slice includes the edit will be reissued.
	for _, i := range all {
		if i.Status != statusInProgress || i.Spec == "" || i.SpecHash == "" {
			continue
		}
		if includesEdit(i.Spec) {
			out.InFlight = append(out.InFlight, BlastItem{ID: i.ID, Role: i.Role, Spec: i.Spec, SpecHash: i.SpecHash})
		}
	}
	sort.Slice(out.InFlight, func(a, b int) bool { return out.InFlight[a].ID < out.InFlight[b].ID })

	// Merged: the recompileMergedDelta analog, grouped by (epic, spec-path). A group is affected
	// when its slice includes the edit and at least one closed member carries a pinned hash (an
	// unpinned member could not drift). One entry per group — that is the dedupe the per-(epic,
	// path) sweep makes across an epic's many closed issues.
	type key struct{ epic, spec string }
	groups := map[key]int{}
	pinned := map[key]bool{}
	for _, i := range all {
		if i.Status != statusClosed || i.Spec == "" {
			continue
		}
		k := key{epic: core.EpicOf(i), spec: i.Spec}
		groups[k]++
		if i.SpecHash != "" {
			pinned[k] = true
		}
	}
	for k, n := range groups {
		if !pinned[k] || !includesEdit(k.spec) {
			continue
		}
		out.Merged = append(out.Merged, BlastGroup{Epic: k.epic, Spec: k.spec, Members: n})
	}
	sort.Slice(out.Merged, func(a, b int) bool {
		if out.Merged[a].Spec != out.Merged[b].Spec {
			return out.Merged[a].Spec < out.Merged[b].Spec
		}
		return out.Merged[a].Epic < out.Merged[b].Epic
	})
	return out, nil
}
