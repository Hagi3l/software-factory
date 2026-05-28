// Package gate runs verification in a fresh, orchestrator-controlled sandbox that
// is distinct from the producer's: a clean checkout of the candidate branch, a
// build and test run, reporting pass/fail back to the orchestrator.
//
// Producer never equals verifier — grading an artifact in the sandbox that produced
// it would let untrusted code certify itself, so the gate's separateness is the
// guarantee that makes no-human-review trustworthy.
package gate
