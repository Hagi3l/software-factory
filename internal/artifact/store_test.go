package artifact_test

import (
	"strings"
	"testing"

	"github.com/Loxstomper/software-factory/internal/artifact"
	"github.com/Loxstomper/software-factory/internal/config"
)

func TestOpen(t *testing.T) {
	root := t.TempDir()

	tests := []struct {
		name    string
		cfg     config.ArtifactsConfig
		wantErr bool
		isFiles bool
		isS3    bool
	}{
		{name: "files backend", cfg: config.ArtifactsConfig{Backend: "files", Path: root}, isFiles: true},
		{name: "empty defaults to files", cfg: config.ArtifactsConfig{Path: root}, isFiles: true},
		{name: "files without path", cfg: config.ArtifactsConfig{Backend: "files"}, wantErr: true},
		// s3 construction is network-free (minio.New dials lazily), so a well-formed s3
		// config opens successfully; only a missing bucket / endpoint+region fails here.
		{name: "s3 with endpoint", cfg: config.ArtifactsConfig{Backend: "s3", Bucket: "b", Endpoint: "http://127.0.0.1:9000"}, isS3: true},
		{name: "s3 with region", cfg: config.ArtifactsConfig{Backend: "s3", Bucket: "b", Region: "us-east-1"}, isS3: true},
		{name: "s3 without bucket", cfg: config.ArtifactsConfig{Backend: "s3", Region: "us-east-1"}, wantErr: true},
		{name: "s3 without endpoint or region", cfg: config.ArtifactsConfig{Backend: "s3", Bucket: "b"}, wantErr: true},
		{name: "unknown backend", cfg: config.ArtifactsConfig{Backend: "carrier-pigeon"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := artifact.Open(tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatal("Open: want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if tt.isFiles {
				if _, ok := s.(*artifact.FilesStore); !ok {
					t.Fatalf("Open returned %T, want *artifact.FilesStore", s)
				}
			}
			if tt.isS3 {
				if _, ok := s.(*artifact.S3Store); !ok {
					t.Fatalf("Open returned %T, want *artifact.S3Store", s)
				}
			}
		})
	}
}

func TestHashPrefix(t *testing.T) {
	if !strings.HasPrefix(artifact.HashPrefix, artifact.HashAlgorithm) {
		t.Fatalf("HashPrefix %q does not start with algorithm %q", artifact.HashPrefix, artifact.HashAlgorithm)
	}
}
