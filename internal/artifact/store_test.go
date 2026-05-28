package artifact_test

import (
	"strings"
	"testing"

	"github.com/Loxstomper/harness/internal/artifact"
	"github.com/Loxstomper/harness/internal/config"
)

func TestOpen(t *testing.T) {
	root := t.TempDir()

	tests := []struct {
		name    string
		cfg     config.ArtifactsConfig
		wantErr bool
		isFiles bool
	}{
		{name: "files backend", cfg: config.ArtifactsConfig{Backend: "files", Path: root}, isFiles: true},
		{name: "empty defaults to files", cfg: config.ArtifactsConfig{Path: root}, isFiles: true},
		{name: "files without path", cfg: config.ArtifactsConfig{Backend: "files"}, wantErr: true},
		{name: "s3 not implemented", cfg: config.ArtifactsConfig{Backend: "s3"}, wantErr: true},
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
		})
	}
}

func TestHashPrefix(t *testing.T) {
	if !strings.HasPrefix(artifact.HashPrefix, artifact.HashAlgorithm) {
		t.Fatalf("HashPrefix %q does not start with algorithm %q", artifact.HashPrefix, artifact.HashAlgorithm)
	}
}
