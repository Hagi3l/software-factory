// Command factory is the entry point for the autonomous software factory.
//
// It exposes the three operator-facing subcommands that drive the bootstrap kernel
// (see specs/bootstrap.md, IMPLEMENTATION_PLAN.md T1.21):
//
//	software-factory validate  — load + validate the config (the startup gate)
//	software-factory seed       — author a spec and create a seed issue (CLI stand-in for the
//	                     requirements wizard) via the single-writer beads path
//	software-factory run        — run an in-process orchestrator + one runner over embedded
//	                     NATS until interrupted; this is the spec -> merged-commit loop
//	software-factory approve    — approve a parked integrate candidate (the trusted-dev / TCB-review
//	software-factory reject       gate, T2.10); publishes the human's decision over NATS to the
//	                     single-writer orchestrator. Needs `software-factory run --nats-addr`.
//	software-factory serve      — start the control-room web server (the human's window); serves
//	                     the embedded UI until interrupted
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
	case "approve":
		err = cmdApprove(rest)
	case "reject":
		err = cmdReject(rest)
	case "serve":
		err = cmdServe(rest)
	case "sandbox-goproxy":
		err = cmdSandboxGoproxy(rest)
	case "version", "-v", "--version":
		fmt.Fprintf(os.Stdout, "software-factory %s\n", version)
		return 0
	case "help", "-h", "--help":
		usage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "software-factory: unknown command %q\n\n", cmd)
		usage(os.Stderr)
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "software-factory %s: %v\n", cmd, err)
		return 1
	}
	return 0
}

// usage prints the command summary. It goes to stdout for an explicit `help` and to
// stderr for a usage error, so piping `factory help` stays clean.
func usage(w *os.File) {
	fmt.Fprint(w, `software-factory — a secure, autonomous software factory

usage:
  software-factory validate [--config DIR] [--env ENV]
  software-factory seed     --title TITLE [--role ROLE] [--description TEXT] [--spec PATH]
                   [--config DIR] [--env ENV] [--repo DIR] [--bd PATH]
  software-factory run      [--config DIR] [--env ENV] [--repo DIR] [--bd PATH] [--serve-addr HOST:PORT] [--nats-addr HOST:PORT]
  software-factory approve  [--nats URL] [--approver WHO] [--repo DIR] [--bd PATH] <issue>
  software-factory reject   [--nats URL] [--approver WHO] [--repo DIR] [--bd PATH] <issue>
  software-factory serve    [--addr HOST:PORT]
  software-factory version

internal (run inside the sandbox by the image entrypoint, not by operators):
  software-factory sandbox-goproxy [--broker NET:ADDR] [--addr HOST:PORT]

Run a subcommand with -h for its flags.
`)
}
