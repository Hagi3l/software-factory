package profile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectNode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "next.config.ts"), []byte(`export default {}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Best != "node" {
		t.Fatalf("Best = %q, want node (scores=%v evidence=%v)", r.Best, r.All, r.Evidence)
	}
}

func TestDetectPython(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(`[project]\nname="x"\n`), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Best != "python" {
		t.Fatalf("Best = %q, want python", r.Best)
	}
}

func TestDetectGo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/x\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Best != "go" {
		t.Fatalf("Best = %q, want go", r.Best)
	}
}

func TestListSorted(t *testing.T) {
	list := List()
	if len(list) < 3 {
		t.Fatalf("len = %d", len(list))
	}
	if list[0].Name != "go" || list[1].Name != "node" || list[2].Name != "python" {
		t.Fatalf("order = %v", []string{list[0].Name, list[1].Name, list[2].Name})
	}
}

func TestResolveConfigDir(t *testing.T) {
	p, err := ResolveConfigDir("/factory", "node")
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join("/factory", "profiles/node") {
		t.Fatalf("got %q", p)
	}
	if _, err := ResolveConfigDir("", "nope"); err == nil {
		t.Fatal("want error")
	}
}
