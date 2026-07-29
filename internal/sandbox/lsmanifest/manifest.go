// Package lsmanifest defines the language-server manifest a sandbox profile image
// bakes in: the languageId -> language-server launch-command mapping the agent's
// LSP-backed semantic tools (Phase 6, T6.1) resolve at runtime.
//
// Why a manifest rather than the tool hard-coding a server per language: the semantic
// tools (find_symbol, references, rename, ...) are language-NEUTRAL; which concrete
// server backs a file is an IMAGE fact, not a tool fact (specs/components/sandbox.md
// "Per-language language server"). The image exposes its servers at a fixed launch
// convention -- this manifest, at ManifestPath -- so the tool layer stays
// image-agnostic and never grows a per-language branch. Because the manifest is baked
// into the image, the image digest already pinned in provenance also pins the
// language-server versions, making semantic results reproducible.
//
// This file is the single source of truth for BOTH the format and the shipped
// content: the same language-servers.json is embedded here (parsed and validated by
// Go, against the canonical Go types) and COPYd into the image at ManifestPath by
// deploy/go-toolchain.Dockerfile. One file, so the format the tools resolve and the
// file the image carries can never drift.
package lsmanifest

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

// ManifestPath is the fixed in-image location of the language-server manifest -- the
// "known path" launch convention the semantic tools read (sandbox.md). Absolute,
// because it is resolved inside the sandbox independent of the working directory.
const ManifestPath = "/etc/factory/language-servers.json"

// currentVersion gates the on-disk format. A future incompatible change bumps this so
// an older reader fails loudly on parse rather than silently misreading.
const currentVersion = 1

//go:embed language-servers.json
var embedded []byte

// Manifest is the parsed languageId -> server mapping baked into a profile image.
type Manifest struct {
	Version int      `json:"version"`
	Servers []Server `json:"servers"`
}

// Server is one language's entry: how to launch its server and which files it serves.
type Server struct {
	// LanguageID is the LSP languageId (e.g. "go"). Unique within a manifest.
	LanguageID string `json:"languageId"`
	// Extensions are the file suffixes (".go") this server handles. Each extension
	// maps to exactly one server within a manifest, so a file resolves unambiguously.
	Extensions []string `json:"extensions"`
	// Command is the argv that launches the server, resolved on the image PATH
	// (e.g. ["gopls","serve"]). Non-empty; element 0 is the executable.
	Command []string `json:"command"`
	// RootMarkers are filenames that identify the workspace root for this language
	// (e.g. "go.mod"). Advisory for the session manager (T6.1); may be empty.
	RootMarkers []string `json:"rootMarkers,omitempty"`
}

// Parse decodes and validates a manifest. Unknown fields are rejected so a typo'd key
// in the baked file is a loud error, not a silently-ignored setting.
func Parse(data []byte) (*Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("lsmanifest: decode: %w", err)
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Manifest) validate() error {
	if m.Version != currentVersion {
		return fmt.Errorf("lsmanifest: unsupported version %d (want %d)", m.Version, currentVersion)
	}
	if len(m.Servers) == 0 {
		return fmt.Errorf("lsmanifest: no servers defined")
	}
	seenID := make(map[string]bool, len(m.Servers))
	seenExt := make(map[string]string)
	for i := range m.Servers {
		s := &m.Servers[i]
		if strings.TrimSpace(s.LanguageID) == "" {
			return fmt.Errorf("lsmanifest: server %d has empty languageId", i)
		}
		if seenID[s.LanguageID] {
			return fmt.Errorf("lsmanifest: duplicate languageId %q", s.LanguageID)
		}
		seenID[s.LanguageID] = true
		if len(s.Command) == 0 || strings.TrimSpace(s.Command[0]) == "" {
			return fmt.Errorf("lsmanifest: server %q has empty command", s.LanguageID)
		}
		if len(s.Extensions) == 0 {
			return fmt.Errorf("lsmanifest: server %q has no extensions", s.LanguageID)
		}
		for _, e := range s.Extensions {
			if !strings.HasPrefix(e, ".") {
				return fmt.Errorf("lsmanifest: server %q extension %q must start with '.'", s.LanguageID, e)
			}
			if prev, ok := seenExt[e]; ok {
				return fmt.Errorf("lsmanifest: extension %q claimed by both %q and %q", e, prev, s.LanguageID)
			}
			seenExt[e] = s.LanguageID
		}
	}
	return nil
}

// Embedded returns the shipped manifest -- the same bytes baked into the image at
// ManifestPath. It is parsed and validated on every call (cheap); a failure here is a
// build-time bug in language-servers.json, pinned by TestEmbeddedValid.
func Embedded() (*Manifest, error) { return Parse(embedded) }

// EmbeddedBytes returns the raw shipped manifest bytes -- exactly what the image
// carries at ManifestPath. Lets callers (and the build) assert image/Go parity.
func EmbeddedBytes() []byte { return embedded }

// ResolveExtension returns the server for a file by its extension (case-insensitive),
// if one is registered. This is the in-sandbox lookup the semantic tools use to pick a
// server for the file under the cursor.
func (m *Manifest) ResolveExtension(filename string) (*Server, bool) {
	ext := path.Ext(filename)
	if ext == "" {
		return nil, false
	}
	for i := range m.Servers {
		for _, e := range m.Servers[i].Extensions {
			if strings.EqualFold(e, ext) {
				return &m.Servers[i], true
			}
		}
	}
	return nil, false
}

// ResolveLanguageID returns the server for an LSP languageId, if registered.
func (m *Manifest) ResolveLanguageID(id string) (*Server, bool) {
	for i := range m.Servers {
		if m.Servers[i].LanguageID == id {
			return &m.Servers[i], true
		}
	}
	return nil, false
}
