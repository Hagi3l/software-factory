package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Loxstomper/harness/internal/core"
)

// FilesStore is the content-addressed files backend: the simplest store, fitting
// the single-binary, single-host dev story. Content lives at
// <root>/sha256/<ab>/<rest>, sharded by the first byte of the digest so a single
// directory never accumulates millions of entries. See specs/components/artifact-store.md.
type FilesStore struct {
	root string
}

var _ Store = (*FilesStore)(nil)

// NewFilesStore opens (creating if needed) a files-backed store rooted at path.
func NewFilesStore(path string) (*FilesStore, error) {
	if path == "" {
		return nil, errors.New("artifact: files store requires a non-empty path")
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("artifact: create store root %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("artifact: resolve store root %s: %w", path, err)
	}
	return &FilesStore{root: abs}, nil
}

// Put streams content to a temp file in the store root while hashing it, then
// atomically renames it to its content address. Hashing and writing happen in one
// pass (the hash is unknown until every byte is read), and the rename is atomic on
// the same filesystem, so a crash mid-write never leaves a partial file under a
// valid hash. Identical content is deduplicated: if the destination already exists,
// the temp file is dropped — the bytes are identical by definition of the address.
func (s *FilesStore) Put(ctx context.Context, kind string, content io.Reader) (core.ArtifactRef, error) {
	if err := ctx.Err(); err != nil {
		return core.ArtifactRef{}, err
	}
	if content == nil {
		return core.ArtifactRef{}, errors.New("artifact: Put requires non-nil content")
	}

	tmp, err := os.CreateTemp(s.root, ".put-*")
	if err != nil {
		return core.ArtifactRef{}, fmt.Errorf("artifact: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), content); err != nil {
		return core.ArtifactRef{}, fmt.Errorf("artifact: write content: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return core.ArtifactRef{}, fmt.Errorf("artifact: close temp file: %w", err)
	}

	hash := HashPrefix + hex.EncodeToString(h.Sum(nil))
	ref := core.ArtifactRef{Kind: kind, Hash: hash}
	dest, err := s.pathFor(hash)
	if err != nil {
		return core.ArtifactRef{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return core.ArtifactRef{}, fmt.Errorf("artifact: create shard dir: %w", err)
	}
	if _, err := os.Stat(dest); err == nil {
		return ref, nil // already stored; the deferred cleanup removes the temp file
	}
	if err := os.Rename(tmpName, dest); err != nil {
		// A concurrent writer may have landed the same content between our Stat and
		// Rename. Identical hash means identical bytes, so treat that as success.
		if _, statErr := os.Stat(dest); statErr == nil {
			return ref, nil
		}
		return core.ArtifactRef{}, fmt.Errorf("artifact: commit content: %w", err)
	}
	committed = true
	return ref, nil
}

// Get opens the content stored under hash. The caller closes the reader.
func (s *FilesStore) Get(ctx context.Context, hash string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dest, err := s.pathFor(hash)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(dest)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, hash)
		}
		return nil, fmt.Errorf("artifact: open %s: %w", hash, err)
	}
	return f, nil
}

// Has reports whether content with the given hash is present.
func (s *FilesStore) Has(ctx context.Context, hash string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	dest, err := s.pathFor(hash)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(dest); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("artifact: stat %s: %w", hash, err)
	}
	return true, nil
}

// pathFor maps a content hash to its on-disk location. It validates the hash shape
// strictly — the algorithm prefix plus exactly a SHA-256's worth of hex — which is
// also the path-traversal guard: a hash arrives in an agent-produced Result and is
// therefore untrusted, so anything that is not pure hex (no '/', no '..') is
// rejected before it reaches the filesystem.
func (s *FilesStore) pathFor(hash string) (string, error) {
	digest, ok := strings.CutPrefix(hash, HashPrefix)
	if !ok {
		return "", fmt.Errorf("artifact: malformed hash %q (want %s<hex>)", hash, HashPrefix)
	}
	if len(digest) != sha256.Size*2 {
		return "", fmt.Errorf("artifact: malformed hash %q (wrong digest length)", hash)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", fmt.Errorf("artifact: malformed hash %q (not hex)", hash)
	}
	return filepath.Join(s.root, HashAlgorithm, digest[:2], digest[2:]), nil
}
