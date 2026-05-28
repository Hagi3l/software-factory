// Command harness is the entry point for the autonomous software factory.
//
// It exposes the three operator-facing subcommands that drive the bootstrap kernel
// (see specs/bootstrap.md, IMPLEMENTATION_PLAN.md T1.21):
//
//	harness validate  — load + validate the config (the startup gate)
//	harness seed       — author a spec and create a seed issue (CLI stand-in for the
//	                     requirements wizard) via the single-writer beads path
//	harness run        — run an in-process orchestrator + one runner over embedded
//	                     NATS until interrupted; this is the spec -> merged-commit loop
//
// This is the composition root: it is the one place that wires every internal
// package together. Nothing here enforces a guarantee — the components it assembles
// (orchestrator, runner/broker, sandbox, gate) do — so its job is faithful wiring
// and fail-loud startup.
package main

import (
	"fmt"
	"os"
)

// version is the build version, overridden at link time by the Makefile via
// -ldflags. It reports "dev" for unstamped local builds.
var version = "dev"

func main() {
	os.Exit(dispatch(os.Args[1:]))
}

// dispatch routes a subcommand to its handler and maps the outcome to a process
// exit code: 0 success, 1 a command error (a failed validation, a bad config, a run
// that crashed), 2 a usage error (unknown/missing command). It is separated from
// main so tests can drive the CLI without exiting the test process.
func dispatch(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "validate":
		err = cmdValidate(rest)
	case "seed":
		err = cmdSeed(rest)
	case "run":
		err = cmdRun(rest)
	case "version", "-v", "--version":
		fmt.Fprintf(os.Stdout, "harness %s\n", version)
		return 0
	case "help", "-h", "--help":
		usage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "harness: unknown command %q\n\n", cmd)
		usage(os.Stderr)
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "harness %s: %v\n", cmd, err)
		return 1
	}
	return 0
}

// usage prints the command summary. It goes to stdout for an explicit `help` and to
// stderr for a usage error, so piping `harness help` stays clean.
func usage(w *os.File) {
	fmt.Fprint(w, `harness — secure, autonomous software factory

usage:
  harness validate [--config DIR] [--env ENV]
  harness seed     --title TITLE [--role ROLE] [--description TEXT] [--spec PATH]
                   [--config DIR] [--env ENV] [--repo DIR] [--bd PATH]
  harness run      [--config DIR] [--env ENV] [--repo DIR]
                   [--gate-build CMD] [--gate-test CMD] [--bd PATH]
  harness version

Run a subcommand with -h for its flags.
`)
}
