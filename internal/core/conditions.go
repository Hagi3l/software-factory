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

	// PostconditionTestsRed is the reserved "tests must be red" proof for the
	// author-tests stage: the gate runs the acceptance tests against the stage's own
	// candidate, which must FAIL because no implementation exists yet. It proves the
	// test author produced real, executing acceptance tests that genuinely fail — not a
	// vacuous, always-green, or non-compiling suite — so a wasted implement attempt is
	// never spent on a bad test candidate (producer ≠ verifier applied to the author
	// stage; see specs/verification.md, specs/workflow.md). Like the red→green proof it
	// has no `checks` entry of its own — it reuses the CheckAcceptanceTests command, run
	// once against the candidate — and is the natural complement to the implementor's
	// red→green proof, which later re-confirms the same candidate is red as its base.
	PostconditionTestsRed = "tests-red"

	// CheckAcceptanceTests is the registry key for the project's acceptance-test
	// command (e.g. `go test ./...`). It is the command a command-check stage runs to
	// grade a candidate green, and the same command the red→green and tests-red proofs
	// run. Keeping the acceptance tests under one key makes that command a single source
	// of truth shared by the `qa` stage's `tests-pass` check, the `implement` stage's
	// red→green proof, and the `author-tests` stage's tests-red proof (see
	// specs/configuration.md).
	CheckAcceptanceTests = "tests-pass"
)
