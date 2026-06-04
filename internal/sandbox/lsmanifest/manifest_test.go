package lsmanifest

import "testing"

// TestEmbeddedValid pins the shipped manifest: it must parse, and (demo scope, T5.3 /
// Phase 6) carry the Go entry resolving .go to gopls. This is what guarantees the file
// the image bakes at ManifestPath is one the semantic tools can actually resolve.
func TestEmbeddedValid(t *testing.T) {
	m, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	if m.Version != currentVersion {
		t.Errorf("version = %d, want %d", m.Version, currentVersion)
	}
	s, ok := m.ResolveLanguageID("go")
	if !ok {
		t.Fatal("no go server in shipped manifest")
	}
	if len(s.Command) == 0 || s.Command[0] != "gopls" {
		t.Errorf("go command = %v, want gopls first", s.Command)
	}
	got, ok := m.ResolveExtension("internal/foo/bar.go")
	if !ok || got.LanguageID != "go" {
		t.Errorf("ResolveExtension(.go) = %v, %v; want go server", got, ok)
	}
	if len(EmbeddedBytes()) == 0 {
		t.Error("EmbeddedBytes empty")
	}
}

func TestResolveExtension(t *testing.T) {
	m, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded: %v", err)
	}
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"go file", "main.go", true},
		{"go file uppercase ext", "MAIN.GO", true},
		{"nested go", "a/b/c.go", true},
		{"templ rides text floor", "view.templ", false},
		{"css rides text floor", "app.css", false},
		{"no extension", "Makefile", false},
		{"unknown", "x.py", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := m.ResolveExtension(tc.path)
			if ok != tc.want {
				t.Errorf("ResolveExtension(%q) = %v, want %v", tc.path, ok, tc.want)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{"bad version", `{"version":2,"servers":[{"languageId":"go","extensions":[".go"],"command":["gopls"]}]}`},
		{"no servers", `{"version":1,"servers":[]}`},
		{"empty languageId", `{"version":1,"servers":[{"languageId":"","extensions":[".go"],"command":["gopls"]}]}`},
		{"empty command", `{"version":1,"servers":[{"languageId":"go","extensions":[".go"],"command":[]}]}`},
		{"blank command", `{"version":1,"servers":[{"languageId":"go","extensions":[".go"],"command":["  "]}]}`},
		{"no extensions", `{"version":1,"servers":[{"languageId":"go","extensions":[],"command":["gopls"]}]}`},
		{"extension without dot", `{"version":1,"servers":[{"languageId":"go","extensions":["go"],"command":["gopls"]}]}`},
		{"duplicate languageId", `{"version":1,"servers":[{"languageId":"go","extensions":[".go"],"command":["gopls"]},{"languageId":"go","extensions":[".g"],"command":["x"]}]}`},
		{"duplicate extension", `{"version":1,"servers":[{"languageId":"go","extensions":[".x"],"command":["gopls"]},{"languageId":"y","extensions":[".x"],"command":["y"]}]}`},
		{"unknown field", `{"version":1,"servers":[{"languageId":"go","extensions":[".go"],"command":["gopls"],"bogus":true}]}`},
		{"not json", `not json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.json)); err == nil {
				t.Errorf("Parse(%s) = nil error, want rejection", tc.name)
			}
		})
	}
}

func TestParseAccepts(t *testing.T) {
	const ok = `{"version":1,"servers":[{"languageId":"go","extensions":[".go"],"command":["gopls","serve"],"rootMarkers":["go.mod"]}]}`
	m, err := Parse([]byte(ok))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s, found := m.ResolveLanguageID("go")
	if !found {
		t.Fatal("go not resolved")
	}
	if len(s.RootMarkers) != 1 || s.RootMarkers[0] != "go.mod" {
		t.Errorf("rootMarkers = %v", s.RootMarkers)
	}
}
