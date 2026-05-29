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
