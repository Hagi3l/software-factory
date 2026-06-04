package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/Loxstomper/harness/internal/config"
	"github.com/Loxstomper/harness/internal/core"
)

// S3Store is the content-addressed S3/MinIO backend: the distributed-deployment store
// where runners on many hosts and the control room share one bucket, the same Store
// contract the files backend serves on a single host (see specs/components/artifact-store.md).
// Objects live under the same sharded key layout the files backend uses
// (sha256/<ab>/<rest>, via storeKey), so an address means the same thing on either
// backend. It speaks plain S3, so it works against AWS S3 and any S3-compatible service
// (MinIO is what dev tests against).
type S3Store struct {
	client *minio.Client
	bucket string
}

var _ Store = (*S3Store)(nil)

// NewS3Store builds an S3-backed store from config. It is **network-free**: minio.New
// only constructs a client (it dials lazily on the first operation), so this is safe in
// the network-free composition root, mirroring how the OTLP exporter defers its dial — a
// missing bucket or unreachable endpoint surfaces on the first Put/Get/Has (a harvest is
// best-effort and logged), not as a boot failure. Credentials come from the environment
// (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN), never from config —
// the same posture as model API keys.
func NewS3Store(cfg config.ArtifactsConfig) (*S3Store, error) {
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, errors.New("artifact: s3 backend requires artifacts.bucket")
	}

	endpoint, secure := cfg.Endpoint, true
	if endpoint == "" {
		// No explicit endpoint: derive the AWS regional one, which needs a region.
		if cfg.Region == "" {
			return nil, errors.New("artifact: s3 backend requires artifacts.endpoint or artifacts.region")
		}
		endpoint = "s3." + cfg.Region + ".amazonaws.com"
	} else if rest, ok := strings.CutPrefix(endpoint, "http://"); ok {
		// An explicit scheme picks TLS (http:// for a plaintext dev MinIO); minio.New
		// wants the bare host[:port], so strip it.
		endpoint, secure = rest, false
	} else if rest, ok := strings.CutPrefix(endpoint, "https://"); ok {
		endpoint, secure = rest, true
	}

	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewEnvAWS(),
		Secure: secure,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("artifact: build s3 client: %w", err)
	}
	return &S3Store{client: client, bucket: cfg.Bucket}, nil
}

// Put buffers content to a temp file while hashing it, then uploads it under its
// content address. The object key IS the hash, unknown until every byte is read, and
// minio needs the exact size up front — so a temp file both names the object and keeps
// a multi-megabyte transcript off the heap (the files backend streams to a temp file
// for the same reason). It is idempotent: identical content has the same key by
// definition, so an existing object is left untouched and nothing is re-uploaded.
func (s *S3Store) Put(ctx context.Context, kind string, content io.Reader) (core.ArtifactRef, error) {
	if err := ctx.Err(); err != nil {
		return core.ArtifactRef{}, err
	}
	if content == nil {
		return core.ArtifactRef{}, errors.New("artifact: Put requires non-nil content")
	}

	tmp, err := os.CreateTemp("", "s3put-*")
	if err != nil {
		return core.ArtifactRef{}, fmt.Errorf("artifact: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), content)
	if err != nil {
		return core.ArtifactRef{}, fmt.Errorf("artifact: write content: %w", err)
	}

	hash := HashPrefix + hex.EncodeToString(h.Sum(nil))
	ref := core.ArtifactRef{Kind: kind, Hash: hash}
	key, err := storeKey(hash)
	if err != nil {
		return core.ArtifactRef{}, err
	}

	// Idempotent: the key is the content address, so an object already at it holds these
	// exact bytes. Skip the upload (and the bandwidth) when it is present.
	if _, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err == nil {
		return ref, nil
	} else if !isNotFound(err) {
		return core.ArtifactRef{}, fmt.Errorf("artifact: stat %s: %w", hash, err)
	}

	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return core.ArtifactRef{}, fmt.Errorf("artifact: rewind temp file: %w", err)
	}
	if _, err := s.client.PutObject(ctx, s.bucket, key, tmp, size, minio.PutObjectOptions{
		ContentType: "application/octet-stream",
	}); err != nil {
		return core.ArtifactRef{}, fmt.Errorf("artifact: put %s: %w", hash, err)
	}
	return ref, nil
}

// Get opens the content stored under hash. The caller closes the reader. A missing key
// yields ErrNotFound. StatObject is issued first so a missing object surfaces here, not
// lazily on the first Read of the returned object (minio defers the GET request until
// the body is read, which would otherwise hide a not-found behind the io.ReadCloser).
func (s *S3Store) Get(ctx context.Context, hash string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, err := storeKey(hash)
	if err != nil {
		return nil, err
	}
	if _, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, hash)
		}
		return nil, fmt.Errorf("artifact: stat %s: %w", hash, err)
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("artifact: get %s: %w", hash, err)
	}
	return obj, nil
}

// Has reports whether content with the given hash is present.
func (s *S3Store) Has(ctx context.Context, hash string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	key, err := storeKey(hash)
	if err != nil {
		return false, err
	}
	if _, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("artifact: stat %s: %w", hash, err)
	}
	return true, nil
}

// isNotFound reports whether a minio error is a missing-object/bucket response — the S3
// analog of os.ErrNotExist, mapped to ErrNotFound / a false Has so callers branch on a
// pruned/absent artifact the same way they do on the files backend.
func isNotFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "NoSuchKey" || resp.Code == "NoSuchBucket" || resp.StatusCode == http.StatusNotFound
}
