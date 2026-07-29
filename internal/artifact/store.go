package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Loxstomper/software-factory/internal/config"
	"github.com/Loxstomper/software-factory/internal/core"
)

// HashAlgorithm is the content-address digest. SHA-256 is the single algorithm
// the kernel uses; the prefix on every hash makes the address self-describing and
// leaves room to migrate without ambiguity (an old "sha256:" ref stays readable).
const (
	HashAlgorithm = "sha256"
	HashPrefix    = HashAlgorithm + ":"
)

// Artifact store backend identifiers, mirrored from the artifacts.backend config
// key (see specs/components/artifact-store.md). They are the single source of truth
// shared by Open and any future config validation.
const (
	BackendFiles = "files"
	BackendS3    = "s3"
)

// ErrNotFound is returned by Get when no content is stored under the given hash.
// It is a sentinel so callers can branch on a missing artifact (e.g. the control
// room showing "evidence harvested but pruned") versus an I/O failure.
var ErrNotFound = errors.New("artifact: not found")

// Store is the content-addressed home for the large evidence an invocation
// produces — transcripts, gate output, diffs — written before a sandbox is torn
// down and referenced thereafter by hash. Content addressing buys deduplication
// and tamper-evidence for free: a provenance record citing a hash cannot be
// silently altered (see specs/components/artifact-store.md).
//
// The backend is chosen by config (files for the single-host dev story, s3 for
// distributed deployments), the same pluggability principle as the sandbox.
type Store interface {
	// Put writes content and returns a content-addressed reference to it. The hash
	// is derived from the bytes alone; kind is caller-supplied metadata recorded on
	// the ref (e.g. "transcript", "gate-output", "diff"), not part of the address.
	// Put is idempotent: writing identical bytes again yields the same hash and
	// stores nothing new.
	Put(ctx context.Context, kind string, content io.Reader) (core.ArtifactRef, error)
	// Get opens the content stored under hash for reading. The caller closes the
	// returned reader. A hash with no stored content yields ErrNotFound.
	Get(ctx context.Context, hash string) (io.ReadCloser, error)
	// Has reports whether content with the given hash is present.
	Has(ctx context.Context, hash string) (bool, error)
}

// Open builds the artifact Store selected by config. An empty backend defaults to
// "files" (the single-binary dev default). It fails loud on an unknown backend or
// missing required setting — consistent with config validation being a startup gate.
func Open(cfg config.ArtifactsConfig) (Store, error) {
	switch cfg.Backend {
	case "", BackendFiles:
		if cfg.Path == "" {
			return nil, fmt.Errorf("artifact: files backend requires artifacts.path")
		}
		return NewFilesStore(cfg.Path)
	case BackendS3:
		return NewS3Store(cfg)
	default:
		return nil, fmt.Errorf("artifact: unknown backend %q (want %q or %q)", cfg.Backend, BackendFiles, BackendS3)
	}
}

// storeKey validates a content hash and returns its store-relative key,
// "<algo>/<ab>/<rest>" with forward slashes — the layout every backend shares (a path
// under the files root, an object key under the S3 bucket), sharded by the first
// digest byte so no single directory/prefix accumulates millions of entries. The
// strict shape check (the algorithm prefix plus exactly a SHA-256's worth of hex) is
// also the traversal guard: a hash arrives in an agent-produced Result and is therefore
// untrusted, so anything that is not pure hex (no '/', no '..') is rejected before it
// ever names a path or key. It is the single source of truth for both backends so their
// address layout — and their rejection of a malformed hash — can never drift.
func storeKey(hash string) (string, error) {
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
	return HashAlgorithm + "/" + digest[:2] + "/" + digest[2:], nil
}
