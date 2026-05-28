package main

import (
	"flag"
	"fmt"
	"os"
)

// cmdValidate is the startup gate as a subcommand: it loads the config and runs the
// full cross-file validation, printing every problem at once (config.Validate
// accumulates them) so an operator fixes the config in one pass. A clean config
// prints a one-line summary and exits 0; any problem returns the error, which
// dispatch maps to exit 1. Validation being loud and up front is a safety feature —
// a typo'd key or an unresolvable role must never surface mid-run (see
// specs/configuration.md).
func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	dir := fs.String("config", "config", "config directory (harness.yaml, souls/, infra.<env>.yaml)")
	env := fs.String("env", "dev", "infra environment overlay to load (infra.<env>.yaml)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(*dir, *env)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "config %q (env %q): OK — %d stage(s), %d soul(s), %d model(s)\n",
		*dir, *env, len(cfg.Harness.DAG), len(cfg.Souls), len(cfg.Infra.Models))
	return nil
}
