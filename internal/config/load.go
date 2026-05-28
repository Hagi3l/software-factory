package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/Loxstomper/harness/internal/core"
)

// Config is the fully-loaded configuration: the workflow (harness.yaml), the souls
// (souls/*.yaml), and the environment overlay (infra.<env>.yaml), plus the root
// directory they were loaded from so persona paths can be resolved. Loading only
// parses; the startup gate that rejects an illegal DAG is harness validate.
type Config struct {
	Root    string      // directory the config was loaded from
	Harness *Harness    // harness.yaml
	Souls   []core.Soul // souls/*.yaml, sorted by Name
	Infra   *Infra      // infra.<env>.yaml
}

// Load reads the full configuration rooted at dir for the named environment,
// expecting <dir>/harness.yaml, <dir>/souls/*.yaml, and <dir>/infra.<env>.yaml. It
// parses but does not validate cross-file references — that is harness validate's
// job (see specs/configuration.md).
func Load(dir, env string) (*Config, error) {
	harness, err := LoadHarness(filepath.Join(dir, "harness.yaml"))
	if err != nil {
		return nil, err
	}
	souls, err := LoadSouls(filepath.Join(dir, "souls"))
	if err != nil {
		return nil, err
	}
	infra, err := LoadInfra(filepath.Join(dir, fmt.Sprintf("infra.%s.yaml", env)))
	if err != nil {
		return nil, err
	}
	return &Config{Root: dir, Harness: harness, Souls: souls, Infra: infra}, nil
}

// PersonaPath resolves a soul's declared persona path against the config root.
// Absolute paths are returned unchanged. The result is the markdown file the agent
// boots its persona from; harness validate checks it exists.
func (c *Config) PersonaPath(s core.Soul) string {
	if filepath.IsAbs(s.Persona) {
		return s.Persona
	}
	return filepath.Join(c.Root, s.Persona)
}

// unmarshalStrict decodes YAML into out, rejecting unknown keys. Strictness is a
// safety feature, not pedantry: in an autonomous pipeline a typo'd key would
// silently fall back to a zero value and fail badly mid-run, so it must fail loud at
// load (see specs/configuration.md). An empty document leaves out at its zero value.
func unmarshalStrict(data []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}
