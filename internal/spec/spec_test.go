package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree materializes a map of slash-relative path -> content under a fresh tempdir
// and returns the root. It is the fixture for slice resolution: a small cross-linked spec
// graph the resolver walks.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestResolveDepthZeroIsJustTheFile(t *testing.T) {
	root := writeTree(t, map[string]string{
		"specs/a.md": "# A\nlinks [b](b.md) and [c](c.md)\n",
		"specs/b.md": "# B\n",
		"specs/c.md": "# C\n",
	})
	got, err := Resolve(root, "specs/a.md", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# A") {
		t.Errorf("slice missing referenced file content: %q", got)
	}
	if strings.Contains(got, "# B") || strings.Contains(got, "# C") {
		t.Errorf("depth 0 must not pull neighbors, got:\n%s", got)
	}
	if !strings.Contains(got, "<!-- spec: specs/a.md -->") {
		t.Errorf("slice missing the file marker: %q", got)
	}
}

func TestResolveDepthOnePullsDirectNeighbors(t *testing.T) {
	root := writeTree(t, map[string]string{
		"specs/a.md":     "# A\nsee [b](b.md) and [sub](sub/d.md)\n",
		"specs/b.md":     "# B\nlinks [c](c.md)\n", // c is depth 2, excluded at depth 1
		"specs/c.md":     "# C\n",
		"specs/sub/d.md": "# D\n",
	})
	got, err := Resolve(root, "specs/a.md", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# A", "# B", "# D", "<!-- spec: specs/sub/d.md -->"} {
		if !strings.Contains(got, want) {
			t.Errorf("depth-1 slice missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "# C") {
		t.Errorf("depth-1 must not reach the depth-2 file c.md:\n%s", got)
	}
}

func TestResolveSkipsExternalAnchorAndNonMarkdownLinks(t *testing.T) {
	root := writeTree(t, map[string]string{
		"specs/a.md": "# A\n[web](https://example.com) [anchor](#x) [code](../main.go) [mail](mailto:x@y.z) [real](b.md)\n",
		"specs/b.md": "# B\n",
		"main.go":    "package main\n",
	})
	got, err := Resolve(root, "specs/a.md", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# B") {
		t.Errorf("a real .md neighbor must be followed:\n%s", got)
	}
	if strings.Contains(got, "package main") {
		t.Errorf("a non-.md link (code) must not be pulled into the slice:\n%s", got)
	}
}

func TestResolveSkipsBrokenNeighborButErrorsOnMissingRef(t *testing.T) {
	root := writeTree(t, map[string]string{
		"specs/a.md": "# A\nlinks [gone](gone.md) and [b](b.md)\n",
		"specs/b.md": "# B\n",
	})
	// A dangling neighbor link (forward reference to an unwritten spec) is skipped, not fatal.
	got, err := Resolve(root, "specs/a.md", 1)
	if err != nil {
		t.Fatalf("a broken neighbor link must not fail resolution: %v", err)
	}
	if !strings.Contains(got, "# B") {
		t.Errorf("the resolvable neighbor must still appear:\n%s", got)
	}
	// But the referenced file itself missing is an error the caller must see.
	if _, err := Resolve(root, "specs/missing.md", 1); err == nil {
		t.Error("a missing referenced file must error")
	}
}

func TestResolveConfinesToRoot(t *testing.T) {
	root := writeTree(t, map[string]string{
		"specs/a.md": "# A\nescape [up](../../secret.md)\n",
	})
	// Plant a markdown file outside root that the traversal must refuse to read.
	outside := filepath.Join(filepath.Dir(root), "secret.md")
	if err := os.WriteFile(outside, []byte("# SECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)

	got, err := Resolve(root, "specs/a.md", 2)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "SECRET") {
		t.Errorf("a ../ link escaping root must not be followed:\n%s", got)
	}
}

func TestResolveEmitsEachFileOnceDeterministically(t *testing.T) {
	// a -> b, a -> c, b -> c (diamond): c must appear exactly once, and order is BFS
	// (a, then b, c in a's link order).
	root := writeTree(t, map[string]string{
		"specs/a.md": "# A\n[b](b.md) [c](c.md)\n",
		"specs/b.md": "# B\n[c](c.md)\n",
		"specs/c.md": "# C\n",
	})
	got, err := Resolve(root, "specs/a.md", 2)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(got, "<!-- spec: specs/c.md -->"); n != 1 {
		t.Errorf("c.md emitted %d times, want exactly 1:\n%s", n, got)
	}
	ia := strings.Index(got, "specs/a.md")
	ib := strings.Index(got, "specs/b.md")
	ic := strings.Index(got, "specs/c.md")
	if ia >= ib || ib >= ic {
		t.Errorf("want BFS order a<b<c, got positions a=%d b=%d c=%d", ia, ib, ic)
	}

	// Determinism: a second call yields byte-identical output (the hash T3.6 will pin).
	again, err := Resolve(root, "specs/a.md", 2)
	if err != nil {
		t.Fatal(err)
	}
	if again != got {
		t.Error("Resolve is not deterministic across calls")
	}
}

func TestResolveEmptyRefErrors(t *testing.T) {
	if _, err := Resolve(t.TempDir(), "", 1); err == nil {
		t.Error("empty ref must error")
	}
}

// Hash is the content address the Brief pins and the issue stores; it must be a stable,
// prefixed digest of the slice, with the empty slice mapping to no pin (T3.6).
func TestHash(t *testing.T) {
	if got := Hash(""); got != "" {
		t.Errorf("Hash(empty) = %q, want \"\" (nothing to pin)", got)
	}
	a := Hash("# A\nbody\n")
	if !strings.HasPrefix(a, HashPrefix) {
		t.Errorf("Hash = %q, want a %q-prefixed digest", a, HashPrefix)
	}
	if a != Hash("# A\nbody\n") {
		t.Error("Hash is not deterministic for identical content")
	}
	if a == Hash("# A\nbody changed\n") {
		t.Error("Hash must differ when the slice content differs (drift detection depends on it)")
	}
}

// TestResolveWithAmbientPrependsAndDedupes proves the ambient specs (T3.14) are injected ahead
// of the issue's bounded slice, in listed order, and de-duplicated against it: an ambient file
// that is also the governing spec or one of its neighbors appears exactly once, and the ambient
// prefix comes first (the cache-stable prefix). The whole document is one hashable slice.
func TestResolveWithAmbientPrependsAndDedupes(t *testing.T) {
	root := writeTree(t, map[string]string{
		"specs/README.md":      "# Index\n",
		"specs/conventions.md": "# Conventions\nno new modules\n",
		"specs/orders.md":      "# Orders\nsee [validation](validation.md)\n",
		"specs/validation.md":  "# Validation\n",
	})
	ambient := []string{"specs/README.md", "specs/conventions.md", "specs/orders.md"} // orders is also the governing spec
	got, missing, err := ResolveWithAmbient(root, "specs/orders.md", 1, ambient)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Errorf("no ambient file is missing, got %v", missing)
	}
	for _, want := range []string{"# Index", "# Conventions", "# Orders", "# Validation"} {
		if !strings.Contains(got, want) {
			t.Errorf("slice missing %q:\n%s", want, got)
		}
	}
	// Ambient prefix comes first: the index precedes the governing spec.
	if iIndex, iOrders := strings.Index(got, "specs/README.md"), strings.Index(got, "specs/orders.md"); iIndex < 0 || iIndex > iOrders {
		t.Errorf("ambient index must be prepended ahead of the issue slice (index at %d, orders at %d)", iIndex, iOrders)
	}
	// De-dup: orders.md is both an ambient entry and the governing spec — emitted once.
	if n := strings.Count(got, "<!-- spec: specs/orders.md -->"); n != 1 {
		t.Errorf("orders.md emitted %d times, want exactly 1 (de-duped against the slice):\n%s", n, got)
	}
}

// TestResolveWithAmbientNoSpecStillInjectsAmbient proves ambient context reaches even a
// spec-less seed (ref empty): the slice is the ambient prefix alone, and it hashes non-empty —
// which is why an ambient edit can drift spec-less in-flight work.
func TestResolveWithAmbientNoSpecStillInjectsAmbient(t *testing.T) {
	root := writeTree(t, map[string]string{"specs/conventions.md": "# Conventions\n"})
	got, _, err := ResolveWithAmbient(root, "", 1, []string{"specs/conventions.md"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "# Conventions") {
		t.Errorf("a spec-less issue must still get the ambient prefix:\n%s", got)
	}
	if Hash(got) == "" {
		t.Error("an ambient-only slice must hash non-empty (so it is pinned and drift-checked)")
	}
}

// TestResolveWithAmbientMissingFileIsBestEffort proves an unreadable ambient file is reported
// and omitted, never fatal — degraded context, not a dead pipeline — while a missing REFERENCED
// spec stays the fatal seed fault Resolve surfaces.
func TestResolveWithAmbientMissingFileIsBestEffort(t *testing.T) {
	root := writeTree(t, map[string]string{"specs/orders.md": "# Orders\n"})
	got, missing, err := ResolveWithAmbient(root, "specs/orders.md", 1, []string{"specs/gone.md", "specs/orders.md"})
	if err != nil {
		t.Fatalf("a missing ambient file must not fail resolution: %v", err)
	}
	if len(missing) != 1 || missing[0] != "specs/gone.md" {
		t.Errorf("missing = %v, want [specs/gone.md]", missing)
	}
	if !strings.Contains(got, "# Orders") {
		t.Errorf("the readable content must still appear:\n%s", got)
	}
	// A missing referenced spec is still fatal.
	if _, _, err := ResolveWithAmbient(root, "specs/missing.md", 1, nil); err == nil {
		t.Error("a missing referenced spec must error even with ambient configured")
	}
}

// TestResolveWithAmbientIsDeterministic guards the load-bearing property: the slice (and so its
// hash) is byte-identical across calls, so the recompile sweeps re-hash to the pinned value
// rather than seeing false drift every tick.
func TestResolveWithAmbientIsDeterministic(t *testing.T) {
	root := writeTree(t, map[string]string{
		"specs/conventions.md": "# Conventions\n",
		"specs/orders.md":      "# Orders\n",
	})
	a, _, err := ResolveWithAmbient(root, "specs/orders.md", 1, []string{"specs/conventions.md"})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := ResolveWithAmbient(root, "specs/orders.md", 1, []string{"specs/conventions.md"})
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("ResolveWithAmbient is not deterministic across calls")
	}
}

// TestResolveAmbientConfinesToRoot proves an ambient path escaping the repo (a `../` traversal)
// is dropped, not read — ambient files ride into untrusted-agent context, so a hostile config
// path cannot pull host files in (the same confinement Resolve gives link targets).
func TestResolveAmbientConfinesToRoot(t *testing.T) {
	root := writeTree(t, map[string]string{"specs/conventions.md": "# Conventions\n"})
	outside := filepath.Join(filepath.Dir(root), "secret.md")
	if err := os.WriteFile(outside, []byte("# SECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)

	got, missing := ResolveAmbient(root, []string{"../secret.md", "specs/conventions.md"}, nil)
	if strings.Contains(got, "SECRET") {
		t.Errorf("an escaping ambient path must not be read:\n%s", got)
	}
	if !strings.Contains(got, "# Conventions") {
		t.Errorf("the confined ambient file must still resolve:\n%s", got)
	}
	if len(missing) != 1 || missing[0] != "../secret.md" {
		t.Errorf("missing = %v, want the escaping path reported as missing", missing)
	}
}

// TestMembersAreTheSliceFiles proves Members returns exactly the files Resolve concatenates, in
// BFS order — the membership the Resolve-mode blast-radius preview tests "does this slice include
// the edited spec" against (T4.15). It must share Resolve's traversal so the preview cannot
// diverge from what the recompile sweep will reissue.
func TestMembersAreTheSliceFiles(t *testing.T) {
	root := writeTree(t, map[string]string{
		"specs/a.md": "# A\nsee [b](b.md)\n", // a links b
		"specs/b.md": "# B\n",
		"specs/c.md": "# C\n", // unrelated
	})
	got, err := Members(root, "specs/a.md", 1)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"specs/a.md", "specs/b.md"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Members = %v, want %v (the slice files in BFS order)", got, want)
	}
	// c.md is not reachable from a.md, so it is not a member — an edit to it would not drift a.
	for _, m := range got {
		if m == "specs/c.md" {
			t.Error("Members included an unreachable file")
		}
	}
}

// TestMembersDepthZeroIsJustTheFile proves depth bounds membership exactly as it bounds the
// slice: at depth 0 only the referenced file is a member, so an edit to a neighbor is not in the
// blast radius of a depth-0 issue.
func TestMembersDepthZeroIsJustTheFile(t *testing.T) {
	root := writeTree(t, map[string]string{
		"specs/a.md": "# A\nsee [b](b.md)\n",
		"specs/b.md": "# B\n",
	})
	got, err := Members(root, "specs/a.md", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "specs/a.md" {
		t.Errorf("Members(depth 0) = %v, want [specs/a.md] only", got)
	}
}

// TestMembersMissingReferencedFileErrors mirrors Resolve: an issue pointing at a missing spec is
// a fault the caller must see, not a silent empty membership.
func TestMembersMissingReferencedFileErrors(t *testing.T) {
	if _, err := Members(t.TempDir(), "specs/gone.md", 1); err == nil {
		t.Error("Members must error on an unreadable referenced file")
	}
}
