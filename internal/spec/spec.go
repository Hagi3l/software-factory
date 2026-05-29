// Package spec builds the bounded spec slice an agent is handed in its Brief: the
// referenced spec file plus its cross-linked markdown neighbors to a configured depth,
// concatenated into one document — deliberately NOT the whole specs/ tree, which would
// blow the context window and dilute focus (see specs/specs-process.md "Spec context
// horizon", specs/components/agent.md).
//
// Why a host-side resolver rather than letting the agent read the tree from its worktree:
// the slice is bounded *intent*, assembled by the trusted orchestrator from the issue's
// spec reference, so an agent gets exactly the contract it needs in-context and the slice
// can later be content-hashed for spec-version pinning (T3.6) — a stable, auditable input
// the agent cannot expand by wandering the tree.
package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// linkRe matches an inline markdown link's target: the `path` in `[text](path)`. Specs
// use inline links by convention (see specs/specs-process.md "Format"); reference-style
// links (`[text][ref]`) are not followed — a documented limitation, not a silent gap.
var linkRe = regexp.MustCompile(`\[[^\]]*\]\(([^)]+)\)`)

// Resolve assembles the bounded spec slice rooted at ref. root is the directory paths are
// resolved against (the repository root); ref is the slice's entry file as a slash path
// relative to root (e.g. "specs/orders.md"). depth bounds the link traversal: depth 0
// yields just the referenced file, depth 1 adds its directly-linked neighbors, and so on
// (breadth-first). Each reachable markdown file is emitted exactly once, prefixed by an
// `<!-- spec: <path> -->` marker naming its root-relative path, in breadth-first order
// with links taken in source order — so the result is deterministic and content-addresses
// stably for T3.6.
//
// Only local markdown links are followed: an external URL (`http://…`, `mailto:`), a
// pure same-file anchor (`#section`), or a non-`.md` target (a link to code) is skipped —
// a spec legitimately links to all of those, and none belongs in the slice. A neighbor
// link that does not resolve to a readable file is skipped (a spec may link ahead to a
// not-yet-written file); only the *referenced* file failing to read is an error, since an
// issue pointing at a missing spec is a seed/planner fault the caller must see. Link
// targets are confined to root: a `../`-traversal that would escape the repository is
// dropped, so a hostile spec link cannot pull arbitrary host files into agent context.
func Resolve(root, ref string, depth int) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", fmt.Errorf("spec: empty ref")
	}
	if depth < 0 {
		return "", fmt.Errorf("spec: negative depth %d", depth)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("spec: resolve root %q: %w", root, err)
	}

	type node struct {
		rel string
		d   int
	}
	refClean := filepath.Clean(filepath.FromSlash(ref))
	queue := []node{{rel: refClean, d: 0}}
	seen := map[string]bool{}
	var order []string
	contents := map[string]string{}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur.rel] {
			continue
		}

		abs := filepath.Join(rootAbs, cur.rel)
		// Confinement: a link that cleaned to a path outside root (via `../`) is refused so
		// it cannot read host files beyond the repository.
		if rel, rerr := filepath.Rel(rootAbs, abs); rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}

		data, rerr := os.ReadFile(abs)
		if rerr != nil {
			if cur.rel == refClean {
				return "", fmt.Errorf("spec: read referenced file %q: %w", ref, rerr)
			}
			continue // a broken or forward neighbor link is skipped, not fatal
		}
		seen[cur.rel] = true
		order = append(order, cur.rel)
		contents[cur.rel] = string(data)
		if cur.d >= depth {
			continue
		}
		for _, m := range linkRe.FindAllStringSubmatch(string(data), -1) {
			if nb := neighbor(cur.rel, m[1]); nb != "" && !seen[nb] {
				queue = append(queue, node{rel: nb, d: cur.d + 1})
			}
		}
	}

	var b strings.Builder
	for _, rel := range order {
		fmt.Fprintf(&b, "<!-- spec: %s -->\n", filepath.ToSlash(rel))
		b.WriteString(contents[rel])
		if !strings.HasSuffix(contents[rel], "\n") {
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// neighbor resolves a markdown link target found in the file at fromRel (root-relative)
// to another spec file's root-relative path, or "" if it is not a followable local
// markdown link. The link is resolved relative to the linking file's directory, matching
// how the cross-linked spec graph reads (see specs/specs-process.md).
func neighbor(fromRel, target string) string {
	target = strings.TrimSpace(target)
	// Drop a fragment (#anchor) or query so `agent.md#the-brief` resolves to agent.md.
	if i := strings.IndexAny(target, "#?"); i >= 0 {
		target = target[:i]
	}
	if target == "" {
		return "" // a pure same-file anchor links to no new file
	}
	if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
		return "" // external URL
	}
	if !strings.HasSuffix(strings.ToLower(target), ".md") {
		return "" // links to code or assets are not spec content
	}
	return filepath.Clean(filepath.Join(filepath.Dir(fromRel), filepath.FromSlash(target)))
}
