package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from a YAML Go-duration string such
// as "2h" or "30m". Budgets (harness.yaml) and sandbox limits (infra.<env>.yaml)
// are written this way; YAML has no native duration scalar, so the config package
// owns the canonical parse rather than scattering time.ParseDuration at use sites.
type Duration time.Duration

// Duration returns the value as a stdlib time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// UnmarshalYAML parses a duration string. A non-string node or an unparsable value
// is a loud error — a mistyped budget must fail at load, not silently become zero
// (an unbounded budget would defeat the termination guarantee, see specs/workflow.md).
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf(`duration must be a string like "2h": %w`, err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}
