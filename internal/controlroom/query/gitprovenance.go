package query

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/Loxstomper/harness/internal/core"
)

// recordSep / fieldSep are ASCII RS/US — bytes that cannot occur in a commit hash or a
// provenance trailer — so they frame git log output unambiguously even though commit
// bodies are multi-line. git emits them via the %x1e/%x1f format placeholders.
const (
	recordSep = "\x1e"
	fieldSep  = "\x1f"
)

// GitProvenance reads merged-commit provenance back out of git history. It is the git
// third of the control room's three stores (beads = work state, git = provenance,
// artifact store = evidence; specs/observability.md): the orchestrator writes the
// provenance trailer onto each integration commit, and this reads it back to drive the
// provenance and issue-detail views. Parsing is delegated to core.ParseCommitMessage, the
// inverse of the same core.Provenance.Trailer the orchestrator renders — one format, no
// drift.
type GitProvenance struct {
	repo string
	ref  string
	run  func(ctx context.Context, args []string) ([]byte, error)
}

// GitOption configures a GitProvenance reader.
type GitOption func(*GitProvenance)

// WithRef sets the git ref whose history is read (default "refs/heads/main"). main is where
// integration commits land, so it is the provenance of record.
func WithRef(ref string) GitOption { return func(g *GitProvenance) { g.ref = ref } }

// NewGitProvenance builds a reader over the repo at the given path.
func NewGitProvenance(repo string, opts ...GitOption) *GitProvenance {
	g := &GitProvenance{repo: repo, ref: "refs/heads/main"}
	for _, o := range opts {
		o(g)
	}
	if g.run == nil {
		g.run = g.execGit
	}
	return g
}

// Recent returns up to limit most-recent commits on the ref that carry a provenance
// trailer, newest first. Commits without a recognizable trailer (a hand-authored or
// pre-provenance commit) are skipped rather than erroring — the read path is lenient. A
// ref that does not exist yet (a fresh repo with no merges) yields no commits, not an
// error, so the provenance view renders empty instead of failing.
func (g *GitProvenance) Recent(ctx context.Context, limit int) ([]MergedCommit, error) {
	if limit <= 0 {
		limit = 50
	}
	if !g.refExists(ctx) {
		return nil, nil
	}
	out, err := g.run(ctx, []string{"log", g.ref,
		"--format=%H" + fieldSep + "%B" + recordSep, "-n", fmt.Sprint(limit)})
	if err != nil {
		return nil, fmt.Errorf("query: git log %s: %w", g.ref, err)
	}
	var commits []MergedCommit
	for _, rec := range strings.Split(string(out), recordSep) {
		rec = strings.TrimSpace(rec)
		if rec == "" {
			continue
		}
		hash, body, ok := strings.Cut(rec, fieldSep)
		if !ok {
			continue
		}
		prov, found := core.ParseCommitMessage(body)
		if !found {
			continue
		}
		commits = append(commits, MergedCommit{Commit: strings.TrimSpace(hash), Provenance: prov})
	}
	return commits, nil
}

// ByIssue finds the integration commit that landed the given issue and parses its
// provenance. It matches on the trailer's "Issue: <id> |" form — the same fixed-string
// grep the merger uses for its merge-idempotency check (internal/orchestrator/merge.go) —
// so a substring of a longer id cannot match. found is false (with a nil error) when the
// issue has not been merged or the ref does not exist; an error is reserved for a real git
// fault.
func (g *GitProvenance) ByIssue(ctx context.Context, issueID string) (core.Provenance, bool, error) {
	if issueID == "" {
		return core.Provenance{}, false, fmt.Errorf("query: empty issue id")
	}
	if !g.refExists(ctx) {
		return core.Provenance{}, false, nil
	}
	out, err := g.run(ctx, []string{"log", g.ref, "--fixed-strings",
		"--grep=Issue: " + issueID + " |", "--format=%B" + recordSep, "-n", "1"})
	if err != nil {
		return core.Provenance{}, false, fmt.Errorf("query: git log grep %s: %w", issueID, err)
	}
	body := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(string(out)), recordSep))
	if body == "" {
		return core.Provenance{}, false, nil
	}
	prov, found := core.ParseCommitMessage(body)
	return prov, found, nil
}

// refExists reports whether the configured ref resolves. --verify --quiet makes git exit
// nonzero (and silent) for a missing ref, which surfaces here as a run error — treated as
// "absent" so a fresh repo reads as empty rather than failing.
func (g *GitProvenance) refExists(ctx context.Context) bool {
	_, err := g.run(ctx, []string{"rev-parse", "--verify", "--quiet", g.ref})
	return err == nil
}

// execGit runs git in the reader's repo. -C scopes git to the repo without changing the
// process working directory, so concurrent readers over different repos do not interfere.
func (g *GitProvenance) execGit(ctx context.Context, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", g.repo}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}
