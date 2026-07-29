package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/Loxstomper/software-factory/internal/core"
)

// Config is the fully-loaded configuration: the workflow (factory.yaml), the souls
// (souls/*.yaml), and the environment overlay (infra.<env>.yaml), plus the root
// directory they were loaded from so persona paths can be resolved. Loading only
// parses; the startup gate that rejects an illegal DAG is software-factory validate.
type Config struct {
	Root    string      // directory the config was loaded from
	Harness *Harness    // factory.yaml
	Souls   []core.Soul // souls/*.yaml, sorted by Name
	Infra   *Infra      // infra.<env>.yaml
}

// Load reads the full configuration rooted at dir for the named environment,
// expecting <dir>/factory.yaml, <dir>/souls/*.yaml, and <dir>/infra.<env>.yaml. It
// parses but does not validate cross-file references — that is software-factory validate's
// job (see specs/configuration.md).
func Load(dir, env string) (*Config, error) {
	harness, err := LoadHarness(filepath.Join(dir, "factory.yaml"))
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
// boots its persona from; software-factory validate checks it exists.
func (c *Config) PersonaPath(s core.Soul) string {
	if filepath.IsAbs(s.Persona) {
		return s.Persona
	}
	return filepath.Join(c.Root, s.Persona)
}

// RequirementsPlannerPersonaPath resolves the requirements planner's persona path against
// the config root, mirroring PersonaPath for souls. It returns "" when no requirements
// planner is configured, so the composition root can skip building the wizard.
func (c *Config) RequirementsPlannerPersonaPath() string {
	if c.Harness == nil || c.Harness.RequirementsPlanner == nil {
		return ""
	}
	return c.requirementsPlannerPath(c.Harness.RequirementsPlanner.Persona)
}

// RequirementsPlannerPrefillPath resolves the optional prepared-requirement file
// (RequirementsPlanner.Prefill) against the config root, exactly like the persona path.
// It returns "" when no planner or no prefill is configured, so the wizard renders its
// composer unchanged.
func (c *Config) RequirementsPlannerPrefillPath() string {
	if c.Harness == nil || c.Harness.RequirementsPlanner == nil {
		return ""
	}
	return c.requirementsPlannerPath(c.Harness.RequirementsPlanner.Prefill)
}

// requirementsPlannerPath joins a planner-block relative path onto the config root ("" and
// absolute paths pass through) — the shared resolution rule for Persona and Prefill.
func (c *Config) requirementsPlannerPath(p string) string {
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Root, p)
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
