package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Loxstomper/harness/internal/checkfindings"
	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/model"
	"github.com/Loxstomper/harness/internal/sandbox"
)

// run_gate is the implementor's *full* pre-submit self-check — the producer self-check
// (specs/verification.md "Producer self-checks are feedback, not grades") generalized from
// run_tests' single inner-loop test pass (T9.3) to the whole command-check suite the gate
// will run: the acceptance tests plus the scanners (lint, SAST, vuln, license). It runs each
// configured check's command once in the *untrusted producing sandbox* and returns the gate's
// own compact core.Findings — never the raw multi-thousand-line dumps the scanners emit.
//
// Why it exists alongside run_tests: run_tests is the cheap, scope-narrowed loop the agent
// runs after every change; run_gate is the heavier "will the gate accept this?" pass run once
// before submit. Catching a lint or scanner failure here costs one sandbox command; catching
// it at the real gate costs a whole qa round-trip (a fresh sandbox, the full suite, possibly a
// retry-budget hit). So self-checking lowers the expected cost of clearing the gate.
//
// Why it is still zero-trust: it runs in the producing sandbox and is feedback, never a grade.
// The agent could skip it, tamper with the tree, or misreport it — so only the independent
// re-run in a fresh, orchestrator-controlled sandbox advances the transition (producer !=
// verifier). To keep the self-check and the gate from drifting they resolve the *same* check
// commands (operator config, passed in) and the *same* finding adapters (the shared
// internal/checkfindings leaf the gate also uses) — so "I checked it" and "the gate checks it"
// share one command and one result shape.
//
// What it deliberately omits: the red→green proof (it needs a second sandbox seeded at the
// base ref — only the gate has that) and the mutation metric (it grades a score against a
// threshold, not an exit code — running it here would misread the tool's exit-0 as a pass).
// config.Harness.CommandCheckCommands excludes both by construction, so run_gate only ever
// receives exit-code-graded command checks.
//
// Like run_tests, the raw output of every check is harvested (by content address) into the
// artifact store as evidence so a finding can be drilled into later; only the compact findings
// reach the agent's context.

// RunGateTool builds the run_gate self-check tool. checks maps each command check's registry
// name to its shell command (from config.Harness.CommandCheckCommands). ledger may be nil (no
// harvest sink), in which case the tool still returns findings and cites uncollected hashes.
func RunGateTool(sb sandbox.Sandbox, checks map[string]string, ledger *TestEvidenceLedger) Tool {
	return funcTool{
		def: model.ToolDef{
			Name: "run_gate",
			Description: "Run the project's full gate check suite (acceptance tests plus the lint/security/" +
				"vulnerability/license scanners) in one pass and return the parsed findings per check — not the " +
				"raw scanner dumps. Use this ONCE before you submit, as a final self-check; use run_tests for your " +
				"fast inner loop while iterating. This is feedback only: passing here is not acceptance — the " +
				"candidate is graded by an independent re-run after you submit.",
			Params: json.RawMessage(`{"type": "object", "properties": {}}`),
		},
		fn: func(ctx context.Context, _ json.RawMessage) (Outcome, error) {
			if len(checks) == 0 {
				return Outcome{Content: "run_gate: no command checks are configured for this project."}, nil
			}
			// Run in a stable order so an unchanged tree yields a byte-identical result (the same
			// cache-stability the findings themselves have).
			names := make([]string, 0, len(checks))
			for name := range checks {
				names = append(names, name)
			}
			sort.Strings(names)

			results := make([]gateCheckResult, 0, len(names))
			for _, name := range names {
				res, err := sb.Exec(ctx, sandbox.Command{Path: "sh", Args: []string{"-c", checks[name]}})
				if err != nil {
					return Outcome{}, fmt.Errorf("agent: run_gate exec %q: %w", name, err)
				}
				// Harvest the raw output as evidence (stdout, or stderr when the tool wrote only
				// there) and parse it with the SAME adapter the gate uses for this check name.
				raw := res.Stdout
				if len(raw) == 0 {
					raw = res.Stderr
				}
				results = append(results, gateCheckResult{
					name:     name,
					passed:   res.ExitCode == 0,
					exitCode: res.ExitCode,
					findings: checkfindings.Parse(name, res.Stdout, res.Stderr),
					evidence: ledger.record(raw),
				})
			}
			content, anyFailed := formatRunGate(results)
			return Outcome{Content: content, IsError: anyFailed}, nil
		},
	}
}

// gateCheckResult is one check's self-check outcome: whether its command exited clean, the
// findings its adapter parsed, and the content-address of its harvested raw output.
type gateCheckResult struct {
	name     string
	passed   bool
	exitCode int
	findings core.Findings
	evidence string
}

// formatRunGate renders the full self-check the agent sees: a one-line tally, then a block per
// check (pass/fail, exit code, and its compact findings — never the raw dump), then the
// evidence hashes so any finding can be drilled into. anyFailed is true when any check exited
// non-zero, so the caller marks the outcome an error (actionable feedback).
func formatRunGate(results []gateCheckResult) (string, bool) {
	passed := 0
	for _, r := range results {
		if r.passed {
			passed++
		}
	}
	anyFailed := passed != len(results)

	var b strings.Builder
	fmt.Fprintf(&b, "ran %d gate check(s): %d passed, %d failed.\n", len(results), passed, len(results)-passed)
	for _, r := range results {
		mark := "✓"
		if !r.passed {
			mark = "✗"
		}
		fmt.Fprintf(&b, "\n%s %s", mark, r.name)
		if !r.passed {
			fmt.Fprintf(&b, " (exit %d)", r.exitCode)
		}
		switch {
		case len(r.findings) > 0:
			fmt.Fprintf(&b, " — %d finding(s):\n%s\n", len(r.findings), r.findings.Format())
		case r.passed:
			b.WriteString(": clean\n")
		default:
			// Failed but no structured findings — a check with no adapter, or output the adapter
			// could not parse. The exit code is the signal; the raw output is in evidence.
			b.WriteString(": failed (see evidence)\n")
		}
	}
	b.WriteString("\n(raw output kept as evidence: ")
	for i, r := range results {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%s", r.name, r.evidence)
	}
	b.WriteString(")")
	return b.String(), anyFailed
}
