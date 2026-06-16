package wizard

import "testing"

// LineDiff aligns unchanged lines as context and reports inserted/removed lines, so the draft
// panel can show what an edit changes rather than the whole file (T4.32a).
func TestLineDiff(t *testing.T) {
	old := "# Spec\n\nKeep this.\nRemove this.\n"
	neu := "# Spec\n\nKeep this.\nAdd this.\n"
	got := LineDiff(old, neu)

	// Build a compact "<kind><text>" projection to assert the alignment exactly.
	var kinds []DiffKind
	var texts []string
	for _, ln := range got {
		kinds = append(kinds, ln.Kind)
		texts = append(texts, ln.Text)
	}
	// The shared prefix ("# Spec", "", "Keep this.") is context; the changed line is a del + add;
	// the trailing "" (both end in \n) is context.
	wantKinds := []DiffKind{DiffContext, DiffContext, DiffContext, DiffDel, DiffAdd, DiffContext}
	if len(kinds) != len(wantKinds) {
		t.Fatalf("got %d lines %v, want %d", len(kinds), texts, len(wantKinds))
	}
	for i := range wantKinds {
		if kinds[i] != wantKinds[i] {
			t.Errorf("line %d kind = %d (%q), want %d", i, kinds[i], texts[i], wantKinds[i])
		}
	}
	// The del carries the old line, the add carries the new line — verbatim, no gutter prefix.
	if texts[3] != "Remove this." {
		t.Errorf("del text = %q, want %q", texts[3], "Remove this.")
	}
	if texts[4] != "Add this." {
		t.Errorf("add text = %q, want %q", texts[4], "Add this.")
	}
}

// Identical inputs produce an all-context diff (no spurious add/del), so an unchanged file reads
// as "nothing changed" rather than a churn of removals and re-adds.
func TestLineDiffIdentical(t *testing.T) {
	s := "one\ntwo\nthree\n"
	for i, ln := range LineDiff(s, s) {
		if ln.Kind != DiffContext {
			t.Errorf("line %d kind = %d (%q), want context for identical input", i, ln.Kind, ln.Text)
		}
	}
}

// A pure insertion against empty old content is all-add; a pure deletion to empty is all-del.
func TestLineDiffInsertAndDelete(t *testing.T) {
	for _, ln := range LineDiff("", "a\nb") {
		if ln.Text != "" && ln.Kind != DiffAdd {
			t.Errorf("insert: line %q kind = %d, want add", ln.Text, ln.Kind)
		}
	}
	for _, ln := range LineDiff("a\nb", "") {
		if ln.Text != "" && ln.Kind != DiffDel {
			t.Errorf("delete: line %q kind = %d, want del", ln.Text, ln.Kind)
		}
	}
}

// SpecFileDiffs renders a brand-new file as full content (nothing to diff against) and an existing
// file as a line diff; a read fault (ok=false) degrades to the full-content path (T4.32a).
func TestSpecFileDiffs(t *testing.T) {
	specs := []DraftSpec{
		{Path: "specs/new.md", Content: "# New\n"},
		{Path: "specs/edit.md", Content: "# Edit\n\nv2\n"},
		{Path: "specs/unreadable.md", Content: "# X\n"},
	}
	onDisk := map[string]string{"specs/edit.md": "# Edit\n\nv1\n"}
	read := func(path string) (string, bool) {
		c, ok := onDisk[path] // "specs/new.md" and "specs/unreadable.md" both miss -> ok=false
		return c, ok
	}

	got := SpecFileDiffs(specs, read)
	if len(got) != 3 {
		t.Fatalf("got %d views, want 3", len(got))
	}

	// New file: full content, no diff.
	if !got[0].IsNew || got[0].Content != "# New\n" || got[0].Diff != nil {
		t.Errorf("new file view = %+v, want IsNew with Content and no Diff", got[0])
	}
	// Existing file: a diff, no full content.
	if got[1].IsNew || got[1].Content != "" || len(got[1].Diff) == 0 {
		t.Errorf("edit view = %+v, want !IsNew with a Diff and no Content", got[1])
	}
	var sawDel, sawAdd bool
	for _, ln := range got[1].Diff {
		if ln.Kind == DiffDel && ln.Text == "v1" {
			sawDel = true
		}
		if ln.Kind == DiffAdd && ln.Text == "v2" {
			sawAdd = true
		}
	}
	if !sawDel || !sawAdd {
		t.Errorf("edit diff missing the v1->v2 change: %+v", got[1].Diff)
	}
	// Unreadable existing file degrades to full-content (IsNew), not a blank panel.
	if !got[2].IsNew || got[2].Content != "# X\n" {
		t.Errorf("unreadable view = %+v, want IsNew fallback with full content", got[2])
	}
}
