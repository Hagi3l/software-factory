package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Loxstomper/harness/internal/artifact"
	"github.com/Loxstomper/harness/internal/beads"
	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/controlroom/wizard"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/spec"
)

// wizardSeeder is the composition-root implementation of wizard.Seeder (T4.14): the consent-gated
// write seam the control-room Create-Task wizard calls on APPROVE. It performs the four durable
// writes the wizard's conversation engine deliberately cannot (the wizard package imports neither
// git, beads, the artifact store, nor config, staying a self-contained conversation unit):
//
//  1. write the drafted spec markdown into the repository's specs/ tree (validated for path
//     safety, cross-link integrity, and issue coverage first);
//  2. write the decisions sidecar — the finalized agreed ledger items — beside the specs, so the
//     "why" behind the spec is versioned in git (specs-process.md: git history of the sidecar IS
//     the decision-evolution log);
//  3. store the conversation transcript in the artifact store as replayable provenance;
//  4. commit (1)+(2) to git, then create the seed issues through beads.Apply — the SAME
//     single-writer, referential-integrity-checked path the orchestrator writes children through
//     (CLAUDE.md single-writer invariant) — so a wizard-seeded issue is written exactly as the
//     orchestrator would, and a hostile/garbled draft cannot plant an illegal work item.
//
// Ordering: validate → write files → store transcript → git commit → create issues. The spec is
// committed before the issues exist so a created issue's spec reference always resolves on disk.
// A failure after the commit (a beads fault creating issues) leaves the spec committed without
// issues — recoverable by re-seeding; it is surfaced to the human, never silently swallowed.
//
// Under integration.mode: epic the order of the last two steps swaps and the commit retargets
// (T7.5, specs/integration.md "The epic branch"): the seed issue is created first to learn the
// epic-root id (the epic id), then the spec is committed onto a fresh epic/<epic_id> branch cut
// from main — *not* onto main — so main stays quiescent until the epic's terminal merge fires one
// atomic landing. The spec files are still written to the working tree before the issue exists
// (so the orchestrator, which resolves the spec slice by reading the working tree, sees the
// feature's spec the moment the issue is dispatchable, even though it is committed only on the
// epic branch). A one-active-epic consent gate (ensureNoActiveEpic) runs first: epic mode v1
// refuses to open a second feature while one is in flight.
type wizardSeeder struct {
	cfg   *config.Config
	repo  string
	bd    *beads.Client
	store artifact.Store
	log   *slog.Logger
	// git runs a git subcommand in the repo; a seam so tests drive a temp repo deterministically.
	git func(ctx context.Context, args ...string) (string, error)
}

// newWizardSeeder builds a seeder over the run's repo, beads client, and artifact store. It is
// constructed only when the requirements planner is configured (so APPROVE has a backing repo);
// a standalone `harness serve` wires no seeder and the wizard shows APPROVE disabled.
func newWizardSeeder(cfg *config.Config, repo string, bd *beads.Client, store artifact.Store, log *slog.Logger) *wizardSeeder {
	if log == nil {
		log = slog.New(slog.NewTextHandler(noopWriter{}, nil))
	}
	return &wizardSeeder{cfg: cfg, repo: repo, bd: bd, store: store, log: log, git: gitRunner(repo)}
}

// noopWriter discards log output (mirrors the default-logger pattern used elsewhere).
type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }

// gitRunner returns a git subcommand runner rooted at repo. Output is captured (combined) so a
// failure carries git's own diagnostic, and trimmed on success so callers get a clean value
// (e.g. a bare commit sha from rev-parse).
func gitRunner(repo string) func(ctx context.Context, args ...string) (string, error) {
	return func(ctx context.Context, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...) // #nosec G204 -- fixed git binary, repo-scoped; args are trusted, harness-built (not agent input).
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out)), nil
	}
}

// Seed commits an approved draft. See wizardSeeder for the ordering and rationale.
func (s *wizardSeeder) Seed(ctx context.Context, req wizard.SeedRequest) (wizard.SeedResult, error) {
	if err := s.validate(req); err != nil {
		return wizard.SeedResult{}, err
	}

	// Consent gate: epic mode v1 admits one active epic at a time (specs/integration.md "v1 scope").
	// Refuse a second feature while one is in flight, reporting the in-flight feature — done before
	// any write so a refused approval leaves the repo and beads untouched.
	if s.epicMode() {
		if err := s.ensureNoActiveEpic(ctx); err != nil {
			return wizard.SeedResult{}, err
		}
	}

	// 1. write the spec files.
	written := make([]string, 0, len(req.Specs)+1)
	for _, sp := range req.Specs {
		clean, _ := s.cleanSpecPath(sp.Path) // validated above
		full := filepath.Join(s.repo, filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			return wizard.SeedResult{}, fmt.Errorf("create spec dir for %q: %w", clean, err)
		}
		if err := os.WriteFile(full, []byte(sp.Content), 0o600); err != nil {
			return wizard.SeedResult{}, fmt.Errorf("write spec %q: %w", clean, err)
		}
		written = append(written, clean)
	}

	// 2. store the transcript (before the commit, so its hash can be cited in the decisions
	// sidecar and the commit message). Empty transcript is tolerated — store nothing, cite nothing.
	var transcriptRef string
	if len(req.Transcript) > 0 {
		ref, err := s.store.Put(ctx, core.ArtifactKindTranscript, bytes.NewReader(req.Transcript))
		if err != nil {
			return wizard.SeedResult{}, fmt.Errorf("store conversation transcript: %w", err)
		}
		transcriptRef = ref.Hash
	}

	// 3. write the decisions sidecar.
	sidecar := decisionsSidecarPath(req.Specs)
	sidecarFull := filepath.Join(s.repo, filepath.FromSlash(sidecar))
	if err := os.MkdirAll(filepath.Dir(sidecarFull), 0o750); err != nil {
		return wizard.SeedResult{}, fmt.Errorf("create decisions dir: %w", err)
	}
	if err := os.WriteFile(sidecarFull, []byte(decisionsSidecar(req.Summary, req.Decisions, transcriptRef)), 0o600); err != nil {
		return wizard.SeedResult{}, fmt.Errorf("write decisions sidecar: %w", err)
	}
	written = append(written, sidecar)

	// 4. commit the spec + sidecar, then create the seed issues via the single-writer path. Epic
	// mode swaps the order — create first to learn the epic-root id, then commit onto the epic
	// branch (see seedEpic and the wizardSeeder doc comment).
	message := commitMessage(req, transcriptRef, sidecar)
	if s.epicMode() {
		return s.seedEpic(ctx, req, written, transcriptRef, sidecar, message)
	}
	commit, err := s.commit(ctx, written, message)
	if err != nil {
		return wizard.SeedResult{}, err
	}
	created, err := s.createIssues(ctx, req, sidecar, transcriptRef)
	if err != nil {
		return wizard.SeedResult{Commit: commit, TranscriptRef: transcriptRef}, fmt.Errorf("create seed issues (spec already committed as %s): %w", short(commit), err)
	}
	return wizard.SeedResult{Commit: commit, TranscriptRef: transcriptRef, Issues: created}, nil
}

// seedEpic completes an APPROVE under integration.mode: epic (T7.5, specs/integration.md "The
// epic branch"). It creates the seed issue first — the validate step guarantees exactly one, so
// it is the epic root and its id IS the epic id (core.EpicOf folds a root into its own epic) —
// then commits the already-written spec + sidecar onto a fresh epic/<epic_id> branch cut from
// main, never onto main. Holding main quiescent for the epic's duration is load-bearing: it is
// what lets the terminal merge land the whole feature in one commit (so one deploy fires per
// feature) and skip re-gating (the children already verified the combined tree on the epic
// branch). A failure committing the spec after the issue exists is surfaced (the issue is
// recoverable by re-seeding), never silently swallowed.
func (s *wizardSeeder) seedEpic(ctx context.Context, req wizard.SeedRequest, written []string, transcriptRef, sidecar, message string) (wizard.SeedResult, error) {
	created, err := s.createIssues(ctx, req, sidecar, transcriptRef)
	if err != nil {
		return wizard.SeedResult{TranscriptRef: transcriptRef}, fmt.Errorf("create seed issue: %w", err)
	}
	epicID := created[0].ID // the single seed issue is the epic root; its id is the epic id.
	commit, err := s.commitOntoEpic(ctx, epicID, written, message)
	if err != nil {
		return wizard.SeedResult{TranscriptRef: transcriptRef, Issues: created},
			fmt.Errorf("open epic branch %s with the spec (seed issue %s already created): %w", core.EpicBranch(epicID), epicID, err)
	}
	return wizard.SeedResult{Commit: commit, TranscriptRef: transcriptRef, Issues: created}, nil
}

// validate enforces the wizard's spec-authoring contract before any write (specs-process.md:
// "every link resolves; every spec maps to ≥1 issue") plus the consent gate's produces-legality
// check. It is pure except for os.Stat checks against the repo (to resolve links/refs to existing
// files), so the no-I/O parts are unit-testable and the file checks reflect the real tree.
func (s *wizardSeeder) validate(req wizard.SeedRequest) error {
	if len(req.Issues) == 0 {
		return errors.New("the draft has no seed issues to create")
	}
	// Epic mode lands one feature atomically, rooted at a single epic id (specs/integration.md
	// "v1 scope — one active epic"). The epic id is the root seed's own id, so a draft must seed
	// exactly one root issue; the autonomous decomposition planner fans it into children. More
	// than one root would mint multiple epics (each its own branch + terminal merge), defeating
	// the one-feature-one-landing contract.
	if s.epicMode() && len(req.Issues) != 1 {
		return fmt.Errorf("epic mode lands one feature atomically, so a draft must seed exactly one root issue (the planner decomposes it into children); got %d — consolidate them into a single seed issue", len(req.Issues))
	}
	batch, err := s.validateSpecFiles(req.Specs)
	if err != nil {
		return err
	}

	// Issue spec references resolve, and every *newly-created* drafted spec is referenced by
	// ≥1 seed issue. A drafted path that already exists on disk is an *edit* (folding intent
	// into a spec a domain already owns, or refreshing the specs/README.md index when the
	// spec-file set changes) — an edit seeds no new work, so it needs no backing issue
	// (specs-process.md "Editing the tree, not just appending to it"; the coverage rule binds
	// only new specs). This mirrors Resolve mode, which edits a spec without seeding anything.
	referenced := map[string]bool{}
	for _, is := range req.Issues {
		if is.Spec == "" {
			continue
		}
		rel, err := s.cleanSpecPath(is.Spec)
		if err != nil {
			return fmt.Errorf("seed issue %q: %w", is.Title, err)
		}
		if !batch[rel] && !s.fileExists(rel) {
			return fmt.Errorf("seed issue %q references spec %q, which is neither drafted nor present", is.Title, rel)
		}
		referenced[rel] = true
	}
	for sp := range batch {
		if s.fileExists(sp) {
			continue // editing an existing spec (incl. the README index) seeds no work
		}
		if !referenced[sp] {
			return fmt.Errorf("newly-drafted spec %q is not referenced by any seed issue (every new spec must map to ≥1 issue)", sp)
		}
	}

	// Produces-legality: every seed issue enters at a legal pipeline entry stage.
	for _, is := range req.Issues {
		if _, err := resolveSeedRole(s.cfg, is.Role); err != nil {
			return fmt.Errorf("seed issue %q: %w", is.Title, err)
		}
	}
	return nil
}

// validateSpecFiles enforces the parts of the spec-authoring contract common to Create and
// Resolve: at least one spec, every path safe (relative, under specs/, .md) and unique, and
// every inline markdown link resolving to another drafted spec or an existing file — the same
// links the orchestrator traverses when it materializes a slice (specs-process.md, reusing
// spec.Links so "every link resolves" means precisely the links the orchestrator follows). It
// returns the set of cleaned drafted paths (the batch) for the caller's remaining checks. The
// issue-coverage and produces-legality checks are Create-only (Resolve writes a spec edit and
// reopens the stuck issue rather than seeding new work), so they stay in validate.
func (s *wizardSeeder) validateSpecFiles(specs []wizard.DraftSpec) (map[string]bool, error) {
	if len(specs) == 0 {
		return nil, errors.New("the draft has no spec files to author")
	}
	batch := map[string]bool{}
	for _, sp := range specs {
		clean, err := s.cleanSpecPath(sp.Path)
		if err != nil {
			return nil, err
		}
		if batch[clean] {
			return nil, fmt.Errorf("spec %q is drafted more than once", clean)
		}
		batch[clean] = true
	}
	for _, sp := range specs {
		clean, _ := s.cleanSpecPath(sp.Path)
		for _, link := range spec.Links(clean, sp.Content) {
			rel := filepath.ToSlash(link)
			if rel == ".." || strings.HasPrefix(rel, "../") {
				return nil, fmt.Errorf("spec %q links outside the repository (%q)", clean, rel)
			}
			if batch[rel] || s.fileExists(rel) {
				continue
			}
			return nil, fmt.Errorf("spec %q has a broken link to %q (not in the draft and not present in the repo)", clean, rel)
		}
	}
	return batch, nil
}

// cleanSpecPath normalizes and validates a drafted spec path: relative (not absolute), confined
// to the repo (no `../` escape), under the specs/ tree, and a .md file. It returns the cleaned,
// slash-form, repo-relative path used as the canonical key everywhere (batch set, issue refs,
// on-disk join).
func (s *wizardSeeder) cleanSpecPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("empty spec path")
	}
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("spec path %q must be relative to the repo", p)
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("spec path %q escapes the repository", p)
	}
	if clean != "specs" && !strings.HasPrefix(clean, "specs/") {
		return "", fmt.Errorf("spec path %q must live under specs/", p)
	}
	if !strings.HasSuffix(strings.ToLower(clean), ".md") {
		return "", fmt.Errorf("spec path %q must be a .md file", p)
	}
	return clean, nil
}

// fileExists reports whether a repo-relative path resolves to an existing (non-directory) file.
func (s *wizardSeeder) fileExists(rel string) bool {
	info, err := os.Stat(filepath.Join(s.repo, filepath.FromSlash(rel)))
	return err == nil && !info.IsDir()
}

// commit stages the written files and commits them with the given message, returning the commit
// sha. The git mechanics are shared by Create (Seed) and Resolve; the message — mode-specific
// provenance — is built by the caller (commitMessage / resolveCommitMessage).
func (s *wizardSeeder) commit(ctx context.Context, files []string, message string) (string, error) {
	if _, err := s.git(ctx, append([]string{"add", "--"}, files...)...); err != nil {
		return "", err
	}
	if _, err := s.git(ctx, "commit", "-m", message); err != nil {
		return "", err
	}
	sha, err := s.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return sha, nil
}

// createIssues writes the seed issues through beads.Apply — the single-writer path. Each issue
// carries its resolved entry role and its structured spec reference (so the orchestrator can
// resolve the Brief slice), a provenance footer linking the decisions sidecar + transcript, and
// the batch's Key/DependsOn edges (resolved by Apply). EpicID is left empty: a seed issue is the
// root of its own epic (the orchestrator's epicOf falls back to the issue's own id).
func (s *wizardSeeder) createIssues(ctx context.Context, req wizard.SeedRequest, sidecar, transcriptRef string) ([]wizard.SeededIssue, error) {
	proposals := make([]core.Proposal, len(req.Issues))
	for i, is := range req.Issues {
		role, err := resolveSeedRole(s.cfg, is.Role)
		if err != nil {
			return nil, err
		}
		specRef := ""
		if is.Spec != "" {
			specRef, _ = s.cleanSpecPath(is.Spec) // validated
		}
		proposals[i] = core.Proposal{
			Key:       is.Key,
			DependsOn: is.DependsOn,
			Issue: core.Issue{
				Title: is.Title,
				Body:  issueBody(is.Body, sidecar, transcriptRef),
				Role:  role,
				Spec:  specRef,
			},
		}
	}
	created, err := s.bd.Apply(ctx, proposals)
	if err != nil {
		return nil, err
	}
	out := make([]wizard.SeededIssue, len(created))
	for i, is := range created {
		out[i] = wizard.SeededIssue{ID: is.ID, Title: is.Title, Role: is.Role}
	}
	return out, nil
}

// epicMode reports whether the run lands verified work atomically per epic (integration.mode:
// epic). It reads the validated config the wizard shares with the orchestrator; an absent block
// reads as per-item via Harness.Mode() (which also tolerates a nil Harness).
func (s *wizardSeeder) epicMode() bool {
	return s.cfg != nil && s.cfg.Harness.Mode() == config.IntegrationEpic
}

// ensureNoActiveEpic enforces the v1 one-active-epic rule (specs/integration.md "v1 scope — one
// active epic", specs/control-room.md): under integration.mode: epic the wizard refuses to open a
// second feature while one is still in flight, so main stays quiescent for an epic's duration
// (what lets the terminal merge land the feature in one commit and skip re-gating, T7.4). It reads
// every issue (beads.ListAll, which surfaces closed issues `bd list` hides) and groups by epic
// (core.EpicOf — a descendant's threaded id, or a root's own id). An epic is active if any member
// is not yet closed (work in flight) OR its subtree has drained but its terminal merge has not yet
// landed it on main (the window between the last child closing and the slow sweep firing). The
// drained-but-unlanded clause is load-bearing: seeding a second epic in that window would cut it
// from a main that does not yet contain the first feature, and the first's terminal merge — whose
// tree is the epic branch's — would then revert the second. The refusal names the in-flight
// feature so the human sees what blocks them, and runs before any write so a refusal is inert.
func (s *wizardSeeder) ensureNoActiveEpic(ctx context.Context) error {
	all, err := s.bd.ListAll(ctx)
	if err != nil {
		return fmt.Errorf("check for an in-flight epic: %w", err)
	}
	byEpic := map[string][]core.Issue{}
	for _, is := range all {
		byEpic[core.EpicOf(is)] = append(byEpic[core.EpicOf(is)], is)
	}
	// Deterministic order so the reported feature is stable when more than one epic is active.
	epics := make([]string, 0, len(byEpic))
	for epic := range byEpic {
		epics = append(epics, epic)
	}
	sort.Strings(epics)
	for _, epic := range epics {
		members := byEpic[epic]
		inFlight := false
		for _, is := range members {
			if is.Status != "closed" { // beads' terminal status; see core.Issue.Status doc.
				inFlight = true
				break
			}
		}
		if inFlight {
			return fmt.Errorf("epic mode runs one feature at a time: %s is still in flight; finish or abandon it before seeding another feature", s.epicFeatureName(epic, members))
		}
		landed, err := s.epicLandedOnMain(ctx, epic)
		if err != nil {
			return err
		}
		if !landed {
			return fmt.Errorf("epic mode runs one feature at a time: %s has drained but is awaiting its terminal merge to main; wait for it to land before seeding another feature", s.epicFeatureName(epic, members))
		}
	}
	return nil
}

// epicLandedOnMain reports whether an epic's atomic terminal merge has advanced main: a merge
// commit citing the epic id in its provenance trailer is present on main. It mirrors the merger's
// idempotency grep (MergeEpic) so the consent gate and the merge queue agree on "landed". An empty
// result means not yet landed; a git error (e.g. no main ref) is surfaced rather than guessed.
func (s *wizardSeeder) epicLandedOnMain(ctx context.Context, epic string) (bool, error) {
	out, err := s.git(ctx, "log", "refs/heads/main", "--fixed-strings", "--grep=Issue: "+epic+" |", "--format=%H", "-n", "1")
	if err != nil {
		return false, fmt.Errorf("check whether epic %s has landed on main: %w", epic, err)
	}
	return strings.TrimSpace(out) != "", nil
}

// epicFeatureName is a human-facing label for an in-flight epic in the consent-gate refusal: the
// root issue's title (the member whose id is the epic id) with the id, or the bare id when the
// root is not in the snapshot.
func (s *wizardSeeder) epicFeatureName(epic string, members []core.Issue) string {
	for _, is := range members {
		if is.ID == epic {
			if t := strings.TrimSpace(is.Title); t != "" {
				return fmt.Sprintf("%q (%s)", t, epic)
			}
		}
	}
	return epic
}

// commitOntoEpic commits the approved spec + sidecar onto a fresh epic/<epic_id> branch cut from
// main, leaving main and the working tree's HEAD untouched (T7.5, specs/integration.md "The epic
// branch"). It is done entirely with git plumbing: stage the spec files, snapshot the resulting
// tree (main's tree + the spec), write a commit with main as its sole parent, and point the epic
// branch at it — so neither main moves nor does the checkout switch branches. The working-tree
// spec files are then unstaged but left on disk: the orchestrator resolves an issue's spec slice
// by reading the repo's working tree, so the feature's spec must be readable there for the whole
// epic even though it is committed only on the epic branch (the children's sandboxes still base
// off main — code, not the spec, which reaches the agent via its Brief — and integrate their
// candidates onto this branch). main exists in any real integration repo; absent it the epic
// branch cannot be cut, which is surfaced as an error rather than guessed.
func (s *wizardSeeder) commitOntoEpic(ctx context.Context, epicID string, files []string, message string) (string, error) {
	branch := core.EpicBranch(epicID)
	ref := "refs/heads/" + branch
	mainTip, err := s.git(ctx, "rev-parse", "--verify", "refs/heads/main")
	if err != nil {
		return "", fmt.Errorf("epic mode requires an initialized main branch to cut %s from: %w", branch, err)
	}
	if _, err := s.git(ctx, append([]string{"add", "--"}, files...)...); err != nil {
		return "", err
	}
	tree, err := s.git(ctx, "write-tree")
	if err != nil {
		return "", err
	}
	commit, err := s.git(ctx, "commit-tree", tree, "-p", mainTip, "-m", message)
	if err != nil {
		return "", err
	}
	if _, err := s.git(ctx, "update-ref", ref, commit); err != nil {
		return "", err
	}
	// Unstage just the spec files (mixed reset of those paths); the working-tree copies remain on
	// disk for spec resolution, and main's index/tree is left as it was.
	if _, err := s.git(ctx, append([]string{"reset", "-q", "--"}, files...)...); err != nil {
		return "", err
	}
	return commit, nil
}

// issueBody appends a provenance footer to a seed issue's body linking the decisions sidecar and
// the conversation transcript — the "why" behind the work, reachable from the issue the human
// seeded (specs-process.md: the transcript and decisions are the spec's provenance).
func issueBody(body, sidecar, transcriptRef string) string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(body, "\n"))
	b.WriteString("\n\n---\nSeeded via the Create-Task wizard.\n")
	fmt.Fprintf(&b, "Decisions: %s\n", sidecar)
	if transcriptRef != "" {
		fmt.Fprintf(&b, "Transcript: %s\n", transcriptRef)
	}
	return strings.TrimSpace(b.String())
}

// commitMessage builds the spec commit's message: a `specs:` subject from the summary, then the
// provenance body. The harness's own commits do not carry a co-author trailer.
func commitMessage(req wizard.SeedRequest, transcriptRef, sidecar string) string {
	subject := strings.TrimSpace(req.Summary)
	if subject == "" {
		subject = "author spec via Create-Task wizard"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "specs: %s\n\n", subject)
	b.WriteString("Authored via the Create-Task wizard (human-approved consent gate, T4.14).\n")
	fmt.Fprintf(&b, "Decisions: %s\n", sidecar)
	if transcriptRef != "" {
		fmt.Fprintf(&b, "Transcript: %s\n", transcriptRef)
	}
	if len(req.Issues) > 0 {
		b.WriteString("\nSeed issues:\n")
		for _, is := range req.Issues {
			role := is.Role
			if role == "" {
				role = "entry"
			}
			fmt.Fprintf(&b, "- %s (%s)\n", is.Title, role)
		}
	}
	return b.String()
}

// decisionsSidecar renders the finalized-decisions markdown sidecar (specs-process.md, T4.13/T4.14):
// the summary, the agreed ledger items with their one-line rationales, and the transcript link. Git
// history of this file is the decision-evolution record — there is no separate status machinery. It
// is shared by Create (authoring) and Resolve (refining): a re-run for the same spec area overwrites
// the sidecar, and the git diff is the decision-evolution log.
func decisionsSidecar(summary string, decisions []wizard.DecisionRecord, transcriptRef string) string {
	var b strings.Builder
	title := strings.TrimSpace(summary)
	if title == "" {
		title = "Decisions"
	}
	fmt.Fprintf(&b, "# Decisions: %s\n\n", title)
	b.WriteString("_Provenance for the spec authored or refined via the wizard. The spec is the source\n")
	b.WriteString("of truth; this records the decisions behind it. Git history of this file is the\n")
	b.WriteString("decision-evolution log — there is no separate status or supersession machinery._\n\n")
	if len(decisions) == 0 {
		b.WriteString("_No structured decisions were recorded for this work._\n")
	} else {
		for _, d := range decisions {
			// A deferred fork (T4.27) was knowingly left open, not decided — record it as such so
			// the sidecar carries both what was decided and what was set aside (pre-context for the
			// needs-spec-clarification escalation a defer may later raise).
			point := d.Point
			if d.Deferred {
				point = "Deliberately left open: " + d.Point
			}
			if r := strings.TrimSpace(d.Rationale); r != "" {
				fmt.Fprintf(&b, "- %s — %s\n", point, r)
			} else {
				fmt.Fprintf(&b, "- %s\n", point)
			}
		}
	}
	if transcriptRef != "" {
		fmt.Fprintf(&b, "\nConversation transcript: `%s`\n", transcriptRef)
	}
	return b.String()
}

// decisionsSidecarPath keys the sidecar by the first drafted spec's area (its base name), so the
// decisions live beside the spec they explain — "per epic/spec area" (control-room.md). Re-running
// the wizard for the same spec area overwrites it; git history preserves the evolution.
func decisionsSidecarPath(specs []wizard.DraftSpec) string {
	base := "task"
	if len(specs) > 0 {
		name := filepath.Base(filepath.FromSlash(specs[0].Path))
		name = strings.TrimSuffix(name, filepath.Ext(name))
		if slug := slugify(name); slug != "" {
			base = slug
		}
	}
	return "specs/decisions/" + base + ".md"
}

// slugify lowercases and reduces a string to a filesystem-safe slug (alnum runs joined by single
// hyphens). Empty when nothing survives.
func slugify(s string) string {
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		default:
			if !prevHyphen && b.Len() > 0 {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// short trims a sha for human-readable error/log text.
func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
