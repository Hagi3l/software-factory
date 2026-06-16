package wizard

import "strings"

// T4.32a: the draft panel renders a proposed spec edit as a line diff against the file
// currently on disk, instead of dumping the whole proposed file with no indication of what
// changed. For a brand-new file there is nothing to diff against, so the full proposed
// content is shown (as it always was). The on-disk content is plumbed in by the server (which
// holds the repo root); the diff itself is pure, in-memory, and unit-testable here — no git
// shell-out, because the proposed content is not committed anywhere (it is the planner's
// uncommitted draft), so the only thing to compare against is the working-tree file. This same
// renderer backs both Create and Resolve (they share draftSpecFiles), so Resolve mode shows the
// refinement diff for free.

// DiffKind classifies one rendered diff line.
type DiffKind int

const (
	// DiffContext is an unchanged line present in both the on-disk file and the proposal.
	DiffContext DiffKind = iota
	// DiffAdd is a line the proposal adds (present in the proposed content, not on disk).
	DiffAdd
	// DiffDel is a line the proposal removes (present on disk, not in the proposed content).
	DiffDel
)

// DiffLine is one line of a rendered spec diff: its change kind and verbatim text (no prefix
// char — the view supplies the +/- gutter and the tint).
type DiffLine struct {
	Kind DiffKind
	Text string
}

// SpecFileDiff is one proposed spec file prepared for the draft panel. IsNew is true when no
// file exists at Path yet (the draft creates it), in which case Content carries the full
// proposed markdown to show. For an edit (IsNew false) Diff carries the line-by-line delta of
// the proposed content against the on-disk file, so the operator sees what changed rather than
// re-reading the whole file. Exactly one of Content / Diff is populated.
type SpecFileDiff struct {
	Path    string
	IsNew   bool
	Content string     // full proposed content; shown when IsNew
	Diff    []DiffLine // line delta vs the on-disk file; shown when !IsNew
}

// SpecFileDiffs pairs each proposed DraftSpec with the file currently on disk to produce the
// per-file view the draft panel renders: a full-content view for a new file, a line diff for an
// edit. read returns the on-disk content of a repo-relative path and whether it exists; a read
// fault is surfaced by the caller as "does not exist" (ok=false), so a transient FS error
// degrades to showing the full proposed content rather than blanking the panel — an edit then
// renders exactly as a new file would, which is honest (we could not read the prior version).
func SpecFileDiffs(specs []DraftSpec, read func(path string) (content string, ok bool)) []SpecFileDiff {
	out := make([]SpecFileDiff, 0, len(specs))
	for _, sp := range specs {
		existing, ok := read(sp.Path)
		if !ok {
			out = append(out, SpecFileDiff{Path: sp.Path, IsNew: true, Content: sp.Content})
			continue
		}
		out = append(out, SpecFileDiff{Path: sp.Path, Diff: LineDiff(existing, sp.Content)})
	}
	return out
}

// LineDiff computes a line-level delta from oldText to newText via a longest-common-subsequence
// alignment, emitting every line as context/add/del (no hunking or elision — spec files are
// modest, human-authored markdown and the panel is collapsible, so showing the whole file with
// its changes inline is the most readable and avoids hunk-header machinery). Unchanged lines are
// DiffContext, lines only in newText are DiffAdd, lines only in oldText are DiffDel; identical
// inputs yield an all-context result (the panel then shows "no change", which is itself signal).
func LineDiff(oldText, newText string) []DiffLine {
	a := strings.Split(oldText, "\n")
	b := strings.Split(newText, "\n")
	n, m := len(a), len(b)

	// dp[i][j] = length of the LCS of a[i:] and b[j:]. Filled back-to-front so the forward
	// walk below can greedily reconstruct one optimal alignment.
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				dp[i][j] = dp[i+1][j+1] + 1
			case dp[i+1][j] >= dp[i][j+1]:
				dp[i][j] = dp[i+1][j]
			default:
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	out := make([]DiffLine, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, DiffLine{Kind: DiffContext, Text: a[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			out = append(out, DiffLine{Kind: DiffDel, Text: a[i]})
			i++
		default:
			out = append(out, DiffLine{Kind: DiffAdd, Text: b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, DiffLine{Kind: DiffDel, Text: a[i]})
	}
	for ; j < m; j++ {
		out = append(out, DiffLine{Kind: DiffAdd, Text: b[j]})
	}
	return out
}
