// Command harness is the entry point for the autonomous software factory.
//
// The operator-facing subcommands (validate, run, seed) are wired up in a later
// milestone (IMPLEMENTATION_PLAN.md T1.21). A real main exists from the first
// commit so the repository builds and lints, and so the build-version plumbing
// (-ldflags "-X main.version=...") is in place before the CLI grows around it.
package main

import (
	"fmt"
	"os"
)

// version is the build version, overridden at link time by the Makefile via
// -ldflags. It reports "dev" for unstamped local builds.
var version = "dev"

func main() {
	fmt.Fprintf(os.Stdout, "harness %s\n", version)
}
