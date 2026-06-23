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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// HashPrefix is the content-address scheme for a spec-slice hash. It mirrors the artifact
// store's addressing (see internal/artifact) deliberately — same SHA-256 scheme — so the
// hash is self-describing and migration-safe, though a spec slice is hashed for version
// pinning rather than stored. Kept here, not imported, so this package stays a leaf.
const HashPrefix = "sha256:"

// Hash returns the content address of a resolved spec slice: the SHA-256 of its bytes,
// prefixed with the algorithm. It is what the Brief pins and the orchestrator stores on the
// issue, so the exact spec version an agent worked against is recorded and a later edit to
// the governing spec is detectable as drift — re-resolve, re-hash, compare (T3.6/T3.7, see
// specs/specs-process.md). Resolve is deterministic, so the same slice always hashes
// identically. The empty slice (an issue naming no spec) hashes to "" — there is nothing to
// pin.
func Hash(slice string) string {
	if slice == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(slice))
	return HashPrefix + hex.EncodeToString(sum[:])
}

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
	order, contents, err := walk(root, ref, depth)
	if err != nil {
		return "", err
	}
	return render(order, contents), nil
}

// render concatenates the visited files into the slice document: each prefixed by its
// `<!-- spec: <path> -->` marker (the path slash-normalized so the bytes are stable across
// OSes), the body, and a trailing blank line. It is the single renderer shared by Resolve
// (the issue slice) and ResolveAmbient (the ambient prefix), so both content-address by the
// exact same byte layout — the load-bearing property for the spec-version pin (T3.6).
func render(order []string, contents map[string]string) string {
	var b strings.Builder
	for _, rel := range order {
		fmt.Fprintf(&b, "<!-- spec: %s -->\n", filepath.ToSlash(rel))
		b.WriteString(contents[rel])
		if !strings.HasSuffix(contents[rel], "\n") {
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// ResolveAmbient renders the ambient prefix: the repo-relative markdown paths in `paths`,
// read and emitted once each in listed order, in the same per-file form Resolve uses,
// skipping any path already present in `exclude` (the issue slice's members) and any
// duplicate within `paths` — so an ambient file that is also the governing spec or one of
// its neighbors is injected exactly once (specs/specs-process.md "Ambient specs").
//
// Unlike Resolve it does NOT follow cross-links: ambient files are deliberately leaves — a
// spec index of pointers and a conventions doc — and the agent reaches the rest on demand
// via read_file in its worktree, so cross-link reachability never inflates the prefix and
// spec_depth can stay low. Each path is confined under root exactly as Resolve confines link
// targets (a `../` escape is dropped). A path that fails to read (a typo, a not-yet-authored
// conventions file) is reported in `missing` and omitted — best-effort, never fatal, so the
// orchestrator dispatches with degraded context rather than wedging the issue. The result is
// deterministic, so it content-addresses stably as part of the hashed slice.
func ResolveAmbient(root string, paths, exclude []string) (slice string, missing []string) {
	excluded := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		excluded[e] = true
	}
	seen := make(map[string]bool, len(paths))
	var order []string
	contents := map[string]string{}
	for _, p := range paths {
		key := filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(p))))
		if key == "" || key == "." || seen[key] || excluded[key] {
			continue
		}
		seen[key] = true
		// walk at depth 0 yields just this file (no neighbors), reusing the confinement and
		// read logic — an unreadable or escaping path returns no entry, which we record as missing.
		ord, cts, err := walk(root, p, 0)
		if err != nil || len(ord) == 0 {
			missing = append(missing, key)
			continue
		}
		order = append(order, ord[0])
		contents[ord[0]] = cts[ord[0]]
	}
	return render(order, contents), missing
}

// ResolveWithAmbient assembles the full slice an agent is briefed against: the project's
// ambient specs (conventions + index, the same for every issue) prepended AHEAD of the
// issue-scoped bounded slice (ref plus its neighbors to depth), de-duplicated so an ambient
// file that is also the governing spec or a neighbor appears once. ref may be empty (a seed
// naming no spec) — then the slice is the ambient prefix alone, which is why ambient context
// reaches even spec-less work. The ambient prefix is the most stable text across all of a
// project's invocations, so placing it first maximizes the model layer's prompt-cache reuse.
//
// Missing ambient files are returned for the caller to log loudly and are omitted
// (best-effort); a missing *referenced* spec is still the fatal seed/planner fault Resolve
// surfaces. The result is deterministic and is exactly what the Brief content-hashes, so a
// conventions edit is pinned in provenance like a contract edit and the recompile-the-delta
// sweeps (which must re-resolve through this same function) re-hash identically — otherwise
// every sweep would see false drift. See specs/specs-process.md "Ambient specs", T3.6/T3.7.
func ResolveWithAmbient(root, ref string, depth int, ambient []string) (string, []string, error) {
	var order []string
	var contents map[string]string
	if strings.TrimSpace(ref) != "" {
		o, c, err := walk(root, ref, depth)
		if err != nil {
			return "", nil, err
		}
		order, contents = o, c
	}
	exclude := make([]string, len(order))
	for i, rel := range order {
		exclude[i] = filepath.ToSlash(rel)
	}
	ambientSlice, missing := ResolveAmbient(root, ambient, exclude)
	return ambientSlice + render(order, contents), missing, nil
}

// Members returns the root-relative slash paths of the files that make up the bounded slice
// rooted at ref — exactly the set Resolve concatenates, in the same breadth-first order. It
// shares Resolve's traversal (and its confinement, skip, and error rules), so "the slice
// includes path P" is answered by the same logic that builds the slice. The control room's
// Resolve-mode blast-radius preview (T4.15) uses it to answer, read-only, *which* in-flight
// and merged issues a spec edit would touch: an issue whose slice includes an edited path
// will re-resolve to a different hash and be reissued by the recompile-the-delta sweep,
// while one whose slice does not is left alone (see specs/specs-process.md "Spec drift",
// orchestrator recompileSpecDelta). Like Resolve, only the *referenced* file failing to read
// is an error; a broken/forward neighbor link is simply absent from the membership.
func Members(root, ref string, depth int) ([]string, error) {
	order, _, err := walk(root, ref, depth)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(order))
	for i, rel := range order {
		out[i] = filepath.ToSlash(rel)
	}
	return out, nil
}

// walk is the shared breadth-first traversal behind Resolve and Members: it visits the
// referenced file and its markdown neighbors to depth, returning the visited paths in BFS
// order (links taken in source order) and their contents. Keeping it the single traversal
// means the slice Resolve concatenates and the membership Members reports never diverge.
func walk(root, ref string, depth int) (order []string, contents map[string]string, err error) {
	if strings.TrimSpace(ref) == "" {
		return nil, nil, fmt.Errorf("spec: empty ref")
	}
	if depth < 0 {
		return nil, nil, fmt.Errorf("spec: negative depth %d", depth)
	}
	rootAbs, aerr := filepath.Abs(root)
	if aerr != nil {
		return nil, nil, fmt.Errorf("spec: resolve root %q: %w", root, aerr)
	}

	type node struct {
		rel string
		d   int
	}
	refClean := filepath.Clean(filepath.FromSlash(ref))
	queue := []node{{rel: refClean, d: 0}}
	seen := map[string]bool{}
	contents = map[string]string{}

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

		data, rerr := os.ReadFile(abs) // #nosec G304 -- abs is confined under rootAbs by the filepath.Rel check immediately above.
		if rerr != nil {
			if cur.rel == refClean {
				return nil, nil, fmt.Errorf("spec: read referenced file %q: %w", ref, rerr)
			}
			continue // a broken or forward neighbor link is skipped, not fatal
		}
		seen[cur.rel] = true
		order = append(order, cur.rel)
		contents[cur.rel] = string(data)
		if cur.d >= depth {
			continue
		}
		for _, nb := range Links(cur.rel, string(data)) {
			if !seen[nb] {
				queue = append(queue, node{rel: nb, d: cur.d + 1})
			}
		}
	}
	return order, contents, nil
}

// Links returns the root-relative paths of the local markdown files that content — a spec at
// root-relative path fromRel — links to via inline links, the exact set Resolve follows when
// building a slice. External URLs, same-file anchors, and non-.md targets are filtered out (see
// neighbor); each distinct target is returned once in source order. The wizard's link-integrity
// check (T4.14) reuses this so "every link resolves" means precisely the links the orchestrator
// will traverse when it materializes the slice — one source of truth for what a spec link *is*.
func Links(fromRel, content string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range linkRe.FindAllStringSubmatch(content, -1) {
		nb := neighbor(fromRel, m[1])
		if nb == "" || seen[nb] {
			continue
		}
		seen[nb] = true
		out = append(out, nb)
	}
	return out
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
