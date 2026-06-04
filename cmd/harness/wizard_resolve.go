package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Loxstomper/harness/internal/controlroom/wizard"
	"github.com/Loxstomper/harness/internal/core"
)

// Resolve commits a Resolve-mode draft (T4.15, specs/control-room.md "Create and Resolve are the
// same component"): it writes the refined spec(s), stores the conversation provenance, commits,
// and returns the dead-lettered issue to the ready pool so it is re-dispatched against the
// now-clarified spec. wizardSeeder implements both wizard.Seeder (Create) and wizard.Resolver
// (Resolve) — one composition-root seam, two consent-gated write paths sharing the spec-write and
// commit machinery (validateSpecFiles, write, store, decisionsSidecar, commit).
//
// Resolve creates no new seed issues: the human re-entry invariant resolves stuck work by
// *refining the spec*, and the orchestrator's recompile-the-delta sweep then re-pins and reissues
// the rest of the affected in-flight work and re-derives merged work on its next tick (the blast
// radius the wizard previewed). The reopen is the one extra write Resolve makes beyond Create's
// spec commit: a blocked issue is neither in_progress nor closed, so the recompile sweep does not
// touch it — Resolve must explicitly return it to the ready pool (Reissue, which also clears the
// stale spec pin so the next dispatch re-resolves the edited slice).
//
// Ordering mirrors Seed: validate → write specs → store transcript → write decisions sidecar →
// commit → reopen. The spec is committed before the reopen so the re-dispatched issue resolves
// the clarified spec on disk; a reopen failure after the commit is surfaced (the spec is
// committed, recoverable) rather than silently swallowed.
func (s *wizardSeeder) Resolve(ctx context.Context, req wizard.ResolveRequest) (wizard.ResolveResult, error) {
	if _, err := s.validateSpecFiles(req.Specs); err != nil {
		return wizard.ResolveResult{}, err
	}

	// 1. write the refined spec files (overwriting the existing ones being clarified).
	written := make([]string, 0, len(req.Specs)+1)
	for _, sp := range req.Specs {
		clean, _ := s.cleanSpecPath(sp.Path) // validated above
		full := filepath.Join(s.repo, filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			return wizard.ResolveResult{}, fmt.Errorf("create spec dir for %q: %w", clean, err)
		}
		if err := os.WriteFile(full, []byte(sp.Content), 0o600); err != nil {
			return wizard.ResolveResult{}, fmt.Errorf("write spec %q: %w", clean, err)
		}
		written = append(written, clean)
	}

	// 2. store the conversation transcript (provenance, like Create).
	var transcriptRef string
	if len(req.Transcript) > 0 {
		ref, err := s.store.Put(ctx, core.ArtifactKindTranscript, bytes.NewReader(req.Transcript))
		if err != nil {
			return wizard.ResolveResult{}, fmt.Errorf("store conversation transcript: %w", err)
		}
		transcriptRef = ref.Hash
	}

	// 3. write the decisions sidecar (the "why" behind the refinement; git history is the log).
	sidecar := decisionsSidecarPath(req.Specs)
	sidecarFull := filepath.Join(s.repo, filepath.FromSlash(sidecar))
	if err := os.MkdirAll(filepath.Dir(sidecarFull), 0o750); err != nil {
		return wizard.ResolveResult{}, fmt.Errorf("create decisions dir: %w", err)
	}
	if err := os.WriteFile(sidecarFull, []byte(decisionsSidecar(req.Summary, req.Decisions, transcriptRef)), 0o600); err != nil {
		return wizard.ResolveResult{}, fmt.Errorf("write decisions sidecar: %w", err)
	}
	written = append(written, sidecar)

	// 4. commit the refined spec + sidecar.
	commit, err := s.commit(ctx, written, resolveCommitMessage(req, transcriptRef, sidecar))
	if err != nil {
		return wizard.ResolveResult{}, err
	}

	// 5. reopen the dead-lettered issue against the clarified spec. Reissue returns it to the
	// ready pool and clears the stale spec pin so the next dispatch re-resolves the edited slice
	// — exactly what the recompile sweep does to a drifted in-flight issue, but a blocked issue
	// must be reopened explicitly (the sweep only acts on in_progress/closed). It is the last
	// step: a failure here leaves the spec committed (recoverable — re-run Resolve), surfaced
	// rather than swallowed.
	res := wizard.ResolveResult{Commit: commit, TranscriptRef: transcriptRef}
	if req.IssueID != "" {
		if err := s.bd.Reissue(ctx, req.IssueID); err != nil {
			return res, fmt.Errorf("reopen dead-lettered issue %s (spec already committed as %s): %w", req.IssueID, short(commit), err)
		}
		res.ReopenedIssue = req.IssueID
	}
	return res, nil
}

// resolveCommitMessage builds the Resolve commit's message: a `specs:` subject from the summary,
// then the provenance body recording that it refines the spec to resolve an escalation and which
// dead-lettered issue it reopens. Like the harness's other commits it carries no co-author trailer.
func resolveCommitMessage(req wizard.ResolveRequest, transcriptRef, sidecar string) string {
	subject := strings.TrimSpace(req.Summary)
	if subject == "" {
		subject = "refine spec via Resolve wizard"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "specs: %s\n\n", subject)
	b.WriteString("Refined via the Resolve wizard (human-approved consent gate, T4.15) to resolve an escalation.\n")
	if req.IssueID != "" {
		fmt.Fprintf(&b, "Reopens: %s\n", req.IssueID)
	}
	fmt.Fprintf(&b, "Decisions: %s\n", sidecar)
	if transcriptRef != "" {
		fmt.Fprintf(&b, "Transcript: %s\n", transcriptRef)
	}
	return b.String()
}
