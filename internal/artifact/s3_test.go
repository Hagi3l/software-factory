package artifact_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/Loxstomper/software-factory/internal/artifact"
	"github.com/Loxstomper/software-factory/internal/config"
)

// --- Integration test: real MinIO in Docker, skipped when unavailable ---
//
// This is the real verification for the S3 backend: it drives the *actual* minio-go
// client against a live S3-compatible server, proving the full Store contract
// (content-addressed round-trip, dedup, not-found, malformed-hash rejection, large
// streamed content) behaves identically to the files backend — which is the whole point
// of the pluggable backend (dev runs local, production runs distributed, no code change).

// startMinIO boots a throwaway MinIO container on a free loopback port and returns its
// endpoint + root credentials. It skips the test (rather than failing) when Docker or
// the image is unavailable, so the suite stays green on a host without them.
func startMinIO(t *testing.T) (endpoint, accessKey, secretKey string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH; skipping minio integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skipf("docker daemon not reachable; skipping: %v", err)
	}

	// A free loopback port for the container's S3 API (9000 inside).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	accessKey, secretKey = "minioadmin", "minioadmin"
	name := fmt.Sprintf("factory-minio-test-%d", port)
	out, err := exec.Command("docker", "run", "-d",
		"--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:9000", port),
		"-e", "MINIO_ROOT_USER="+accessKey,
		"-e", "MINIO_ROOT_PASSWORD="+secretKey,
		"minio/minio", "server", "/data",
	).CombinedOutput()
	if err != nil {
		t.Skipf("could not start minio (image pull/run failed); skipping: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	return fmt.Sprintf("127.0.0.1:%d", port), accessKey, secretKey
}

func TestS3StoreIntegration(t *testing.T) {
	endpoint, ak, sk := startMinIO(t)
	ctx := context.Background()

	// Setup client: wait for the server to come up, then create the bucket the store
	// expects to exist (the backend never creates buckets — that is an operator step).
	setup, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(ak, sk, ""),
		Secure: false,
	})
	if err != nil {
		t.Fatalf("minio setup client: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := setup.ListBuckets(ctx); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Skipf("minio never became ready: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	const bucket = "factory-artifacts"
	if err := setup.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("make bucket: %v", err)
	}

	// Build the store under test via the production Open path; creds come from the env,
	// exactly as a real deployment supplies them (never from config).
	t.Setenv("AWS_ACCESS_KEY_ID", ak)
	t.Setenv("AWS_SECRET_ACCESS_KEY", sk)
	store, err := artifact.Open(config.ArtifactsConfig{
		Backend:  "s3",
		Bucket:   bucket,
		Endpoint: "http://" + endpoint, // http:// selects the plaintext dev transport
	})
	if err != nil {
		t.Fatalf("Open s3: %v", err)
	}
	if _, ok := store.(*artifact.S3Store); !ok {
		t.Fatalf("Open returned %T, want *artifact.S3Store", store)
	}

	t.Run("round-trip", func(t *testing.T) {
		content := []byte("the replayable decision trail\n")
		hash := mustPut(t, store, "transcript", content)
		if !strings.HasPrefix(hash, artifact.HashPrefix) {
			t.Fatalf("hash %q lacks %q prefix", hash, artifact.HashPrefix)
		}
		if got := getAll(t, store, hash); !bytes.Equal(got, content) {
			t.Fatalf("round-trip = %q, want %q", got, content)
		}
		ok, err := store.Has(ctx, hash)
		if err != nil || !ok {
			t.Fatalf("Has after Put = (%v, %v), want (true, nil)", ok, err)
		}
	})

	t.Run("dedup same content same hash", func(t *testing.T) {
		c := []byte("identical bytes")
		h1 := mustPut(t, store, "gate-evidence", c)
		h2 := mustPut(t, store, "transcript", c) // different kind, same bytes
		if h1 != h2 {
			t.Fatalf("dedup: %q != %q", h1, h2)
		}
	})

	t.Run("distinct content distinct hash", func(t *testing.T) {
		h1 := mustPut(t, store, "x", []byte("alpha"))
		h2 := mustPut(t, store, "x", []byte("beta"))
		if h1 == h2 {
			t.Fatalf("distinct content shares hash %q", h1)
		}
	})

	t.Run("get missing is ErrNotFound", func(t *testing.T) {
		// Well-formed hash, never stored.
		missing := artifact.HashPrefix + strings.Repeat("0", 64)
		_, err := store.Get(ctx, missing)
		if !errors.Is(err, artifact.ErrNotFound) {
			t.Fatalf("Get(missing) = %v, want ErrNotFound", err)
		}
		ok, err := store.Has(ctx, missing)
		if err != nil || ok {
			t.Fatalf("Has(missing) = (%v, %v), want (false, nil)", ok, err)
		}
	})

	t.Run("malformed hash rejected", func(t *testing.T) {
		// The untrusted-hash traversal guard (storeKey) must reject before any S3 call.
		for _, bad := range []string{
			"",
			"deadbeef",
			"sha256:../../etc/passwd",
			"sha256:" + strings.Repeat("z", 64),
			"sha256:" + strings.Repeat("a", 63),
		} {
			if _, err := store.Get(ctx, bad); err == nil {
				t.Errorf("Get(%q) accepted a malformed hash", bad)
			}
			if _, err := store.Has(ctx, bad); err == nil {
				t.Errorf("Has(%q) accepted a malformed hash", bad)
			}
		}
	})

	t.Run("large streamed content", func(t *testing.T) {
		// ~5 MiB exercises the temp-file streaming + a multipart-sized upload, the path a
		// real multi-megabyte transcript takes.
		big := bytes.Repeat([]byte("0123456789abcdef"), 5*1024*1024/16)
		hash := mustPut(t, store, "transcript", big)
		if got := getAll(t, store, hash); !bytes.Equal(got, big) {
			t.Fatalf("large round-trip mismatch: got %d bytes, want %d", len(got), len(big))
		}
	})
}
