package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/Loxstomper/harness/internal/core"
)

// LoadSouls reads every *.yaml file in dir, unmarshalling each into a core.Soul —
// one file per soul (see specs/configuration.md). Results are sorted by Name so the
// set is deterministic regardless of directory iteration order. Persona paths are
// left as declared; resolve them against the config root with Config.PersonaPath.
//
// A missing directory is not an error — it yields no souls, leaving the "every role
// resolves to >=1 soul" check to harness validate, which can report it precisely.
func LoadSouls(dir string) ([]core.Soul, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("config: glob souls in %s: %w", dir, err)
	}
	sort.Strings(matches)

	souls := make([]core.Soul, 0, len(matches))
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("config: read soul file %s: %w", path, err)
		}
		var s core.Soul
		if err := unmarshalStrict(data, &s); err != nil {
			return nil, fmt.Errorf("config: parse soul file %s: %w", path, err)
		}
		souls = append(souls, s)
	}
	sort.Slice(souls, func(i, j int) bool { return souls[i].Name < souls[j].Name })
	return souls, nil
}
