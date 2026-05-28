package artifact_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/artifact"
)

func newStore(t *testing.T) (*artifact.FilesStore, string) {
	t.Helper()
	root := t.TempDir()
	s, err := artifact.NewFilesStore(root)
	if err != nil {
		t.Fatalf("NewFilesStore: %v", err)
	}
	return s, root
}

func mustPut(t *testing.T, s artifact.Store, kind string, content []byte) string {
	t.Helper()
	ref, err := s.Put(context.Background(), kind, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ref.Kind != kind {
		t.Fatalf("ref kind = %q, want %q", ref.Kind, kind)
	}
	return ref.Hash
}

func getAll(t *testing.T, s artifact.Store, hash string) []byte {
	t.Helper()
	rc, err := s.Get(context.Background(), hash)
	if err != nil {
		t.Fatalf("Get(%s): %v", hash, err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return got
}

func TestPutGetRoundTrip(t *testing.T) {
	s, _ := newStore(t)
	content := []byte("the replayable decision trail")

	hash := mustPut(t, s, "transcript", content)

	// Hash is the self-describing content address of the bytes.
	want := artifact.HashPrefix + hex.EncodeToString(sha256OfBytes(content))
	if hash != want {
		t.Fatalf("hash = %q, want %q", hash, want)
	}
	if got := getAll(t, s, hash); !bytes.Equal(got, content) {
		t.Fatalf("round trip = %q, want %q", got, content)
	}
}

func TestPutDeduplicates(t *testing.T) {
	s, root := newStore(t)
	content := []byte("identical evidence")

	h1 := mustPut(t, s, "gate-output", content)
	h2 := mustPut(t, s, "gate-output", content)
	if h1 != h2 {
		t.Fatalf("identical content gave different hashes: %q vs %q", h1, h2)
	}

	// Content addressing means one file on disk, no temp leftovers.
	var files int
	if err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files++
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if files != 1 {
		t.Fatalf("stored file count = %d, want 1 (dedup + no temp leftover)", files)
	}
}

func TestPutDistinctContentDistinctHash(t *testing.T) {
	s, _ := newStore(t)
	if mustPut(t, s, "log", []byte("a")) == mustPut(t, s, "log", []byte("b")) {
		t.Fatal("distinct content produced the same hash")
	}
}

func TestPutEmptyContent(t *testing.T) {
	s, _ := newStore(t)
	hash := mustPut(t, s, "diff", []byte{})
	if got := getAll(t, s, hash); len(got) != 0 {
		t.Fatalf("empty content round trip = %q, want empty", got)
	}
}

func TestPutNilReader(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.Put(context.Background(), "x", nil); err == nil {
		t.Fatal("Put with nil content: want error, got nil")
	}
}

func TestGetNotFound(t *testing.T) {
	s, _ := newStore(t)
	absent := artifact.HashPrefix + strings.Repeat("0", sha256.Size*2)
	_, err := s.Get(context.Background(), absent)
	if !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("Get(absent) error = %v, want ErrNotFound", err)
	}
}

func TestHas(t *testing.T) {
	s, _ := newStore(t)
	hash := mustPut(t, s, "transcript", []byte("present"))

	has, err := s.Has(context.Background(), hash)
	if err != nil || !has {
		t.Fatalf("Has(present) = %v, %v; want true, nil", has, err)
	}

	absent := artifact.HashPrefix + strings.Repeat("a", sha256.Size*2)
	has, err = s.Has(context.Background(), absent)
	if err != nil || has {
		t.Fatalf("Has(absent) = %v, %v; want false, nil", has, err)
	}
}

// Hashes ride in an agent-produced Result and are untrusted, so a malformed hash
// must be rejected before it can escape the store root via path traversal.
func TestMalformedHashRejected(t *testing.T) {
	s, _ := newStore(t)
	bad := []string{
		"",
		"deadbeef",                           // no prefix
		"sha256:",                            // empty digest
		"sha256:../../../../etc/passwd",      // traversal attempt
		"sha256:" + strings.Repeat("z", 64),  // right length, not hex
		"sha256:" + strings.Repeat("a", 63),  // too short
		"sha256:" + strings.Repeat("a", 65),  // too long
		"md5:" + strings.Repeat("a", 64),     // wrong algorithm prefix
		"sha256:/" + strings.Repeat("a", 63), // contains a slash
	}
	for _, h := range bad {
		if _, err := s.Get(context.Background(), h); err == nil {
			t.Errorf("Get(%q): want error, got nil", h)
		}
		if _, err := s.Has(context.Background(), h); err == nil {
			t.Errorf("Has(%q): want error, got nil", h)
		}
	}
}

func TestContextCanceled(t *testing.T) {
	s, _ := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Put(ctx, "x", bytes.NewReader([]byte("y"))); err == nil {
		t.Error("Put with canceled ctx: want error")
	}
	if _, err := s.Get(ctx, artifact.HashPrefix+strings.Repeat("0", 64)); err == nil {
		t.Error("Get with canceled ctx: want error")
	}
	if _, err := s.Has(ctx, artifact.HashPrefix+strings.Repeat("0", 64)); err == nil {
		t.Error("Has with canceled ctx: want error")
	}
}

func TestNewFilesStore(t *testing.T) {
	if _, err := artifact.NewFilesStore(""); err == nil {
		t.Error("NewFilesStore(\"\"): want error")
	}
	// Creates a not-yet-existing nested root.
	root := filepath.Join(t.TempDir(), "nested", "artifacts")
	if _, err := artifact.NewFilesStore(root); err != nil {
		t.Fatalf("NewFilesStore(nested): %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("store root not created: %v", err)
	}
}

func sha256OfBytes(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}
