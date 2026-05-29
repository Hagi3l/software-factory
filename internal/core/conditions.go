package core

// Condition identifiers that are shared conventions between config validation (which
// accepts them as known postconditions) and the gate (which realizes them as checks).
// They live in core so neither config nor gate owns the spelling — the same reason
// CandidateBranch does — keeping the config-time and run-time halves of the
// postcondition→check bridge in agreement (see specs/configuration.md, specs/verification.md).
const (
	// PostconditionRedGreen is the reserved red→green proof: the gate runs the
	// acceptance tests against the pre-implementation base (which must FAIL) and the
	// candidate (which must PASS), proving the tests are not vacuously green and
	// actually exercise the new behavior (see specs/verification.md "Red→green proof").
	// Unlike a command-check postcondition it has no `checks` entry of its own; it
	// reuses the CheckAcceptanceTests command, run against both refs.
	PostconditionRedGreen = "tests-red-then-green"

	// CheckAcceptanceTests is the registry key for the project's acceptance-test
	// command (e.g. `go test ./...`). It is the command a command-check stage runs to
	// grade a candidate green, and the same command the red→green proof runs against
	// both refs. Keeping the acceptance tests under one key makes that command a single
	// source of truth shared by the `qa` stage's `tests-pass` check and the `implement`
	// stage's red→green proof (see specs/configuration.md).
	CheckAcceptanceTests = "tests-pass"
)
