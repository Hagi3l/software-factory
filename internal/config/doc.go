// Package config defines the declarative schema for the harness — factory.yaml
// (DAG + policy), souls/*.yaml, and infra.<env>.yaml — together with the loader
// and the validator that rejects malformed configuration before anything runs.
//
// The system is config-driven, so validation is a startup gate, not a runtime
// concern: an unreachable DAG role, an undefined produces/on_failure target, or a
// missing persona file is a loud startup error rather than a surprise mid-run.
package config
