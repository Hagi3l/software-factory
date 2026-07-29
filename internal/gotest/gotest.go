// Package gotest is the `go test -json` adapter: it turns a test run's ndjson event
// stream into compact, language-neutral core.Findings. It is the first of the per-tool
// check adapters described in specs/verification.md ("Findings: structured evidence, not
// the grade") — a pure, dependency-free parser with no model and no TCB exposure.
//
// The whole point is signal density. A raw `go test` dump is mostly noise (per-test RUN
// lines, PASS lines, elapsed times, multi-hundred-line goroutine stacks) and that noise is
// exactly what dilutes an agent's context and buries the one line that matters. CLAUDE.md
// tells a *human* to reach for `jq` and "if jq fails, check the .stderr file"; this parser
// owns those edge cases so the agent never has to:
//
//   - a plain test failure becomes one finding anchored at the `foo_test.go:NN` the test
//     printed, with the assertion body as Detail;
//   - a compile/build failure (the stream is not even well-formed test JSON) surfaces the
//     *compiler error* as a finding — that IS the signal, and a non-JSON line must never
//     crash the parser;
//   - a data race keeps its `WARNING: DATA RACE` stanza verbatim (the interleaving is the
//     evidence);
//   - a panic or timeout keeps the message and the triggering test but drops the goroutine
//     dump (pure noise for the agent).
//
// Jitter stripping is done here (T9.1 deferred it to parse time): no Elapsed, no
// timestamps, no run-to-run-varying text ever enters a finding, so an unchanged re-run
// yields byte-identical findings — the load-bearing property for prefix caching and the
// "findings not shrinking across attempts" signal. This package does not sort; core.Findings
// .Format() sorts, but the output here is deterministic given identical input.
package gotest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"

	"github.com/Loxstomper/software-factory/internal/core"
)

// testEvent mirrors the `go test -json` (cmd/test2json) event shape. Only the fields we
// use are kept; Time and Elapsed are deliberately *omitted* even though they are in the
// stream, because they are jitter and must never reach a finding. ImportPath/build-output
// fields carry the compile-failure signal (T9.2 build-failure edge case).
type testEvent struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
	Output  string `json:"Output"`
	// ImportPath is set on build-output/build-fail events (a package build error), where
	// Package is empty — this is how a compile failure announces itself in the stream.
	ImportPath string `json:"ImportPath"`
}

// Parse turns a `go test -json` ndjson stream into findings. A clean pass yields an empty
// (nil) Findings. The parse is total: malformed/non-JSON lines never panic — they are the
// compile-error edge case and are captured as evidence rather than discarded.
func Parse(stdout []byte) core.Findings {
	return parseReader(bytes.NewReader(stdout))
}

// the key under which we accumulate a test's output lines: a (package, test) pair, since
// the same test name can appear in sibling packages within one `./...` run.
type testKey struct {
	pkg  string
	test string
}

func parseReader(r *bytes.Reader) core.Findings {
	var findings core.Findings

	// Per-test accumulated output, in arrival order, so a failure's Detail is the test's
	// own body and nothing else (go test interleaves tests, but tags each line with its
	// Test, so keying by (pkg,test) de-interleaves correctly).
	output := map[testKey][]string{}
	// reported tracks which (pkg,test) already produced a finding off an explicit `fail`
	// event, so the package-level sweep below does not double-report them.
	reported := map[testKey]bool{}
	// emitOrder preserves first-seen order of tests so the package-level sweep is
	// deterministic (map iteration is not).
	var emitOrder []testKey
	seen := map[testKey]bool{}
	// Build errors accumulate separately: they arrive on build-output events that carry no
	// Package/Test, only an ImportPath, before any test event.
	var buildErr []string
	// Lines that did not parse as JSON at all — a raw build failure printed straight to
	// stdout (`# pkg` / `./x.go:3: undefined: Foo`). These ARE the signal; we keep them so
	// a broken build reads as a finding, never as a crash and never as a misleading green.
	var rawNonJSON []string

	sc := bufio.NewScanner(r)
	// go test lines are short, but a single very long Output line (a big assertion diff)
	// can exceed bufio's default 64KiB; raise the cap so we never silently truncate.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev testEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			// Not a test event. This is the compile-failure-as-raw-text case: keep the
			// line verbatim as evidence rather than dropping it or crashing.
			rawNonJSON = append(rawNonJSON, string(line))
			continue
		}

		switch ev.Action {
		case "build-output":
			// A package failed to build; the compiler error rides on these events. Strip
			// the RUN/marker chrome but keep the location-bearing diagnostic lines — that
			// error is the signal a raw dump buries under a "build failed" one-liner.
			if t := strings.TrimRight(ev.Output, "\n"); t != "" {
				buildErr = append(buildErr, t)
			}
		case "output":
			if ev.Test != "" {
				k := testKey{ev.Package, ev.Test}
				if !seen[k] {
					seen[k] = true
					emitOrder = append(emitOrder, k)
				}
				output[k] = append(output[k], ev.Output)
			}
		case "fail":
			if ev.Test == "" {
				// Package-level fail with no test. If the package failed to build, the
				// build error is already captured; otherwise this is a fail with no
				// surviving per-test event (e.g. a timeout kills the process before the
				// test gets its own fail) — handled after the loop from accumulated output.
				continue
			}
			k := testKey{ev.Package, ev.Test}
			findings = append(findings, failureFinding(ev.Test, output[k]))
			reported[k] = true
		}
	}

	// Package-level sweep: a timeout/panic that kills the test binary never emits a
	// per-test `fail` event (the process dies first), so the only trace is the test's
	// accumulated output ending in a `panic:` / `test timed out`. Recover those — a raw
	// dump would otherwise leave the failure as a bare "FAIL\tpkg" with the signal buried
	// in the goroutine stack. Iterate in first-seen order for determinism.
	for _, k := range emitOrder {
		if reported[k] {
			continue
		}
		if panicTimeoutLine(output[k]) == "" {
			continue
		}
		findings = append(findings, panicTimeoutFinding(k.test, output[k]))
		reported[k] = true
	}

	// A build failure (structured build-output events) — surface the compiler error. We do
	// this once for the whole run: the dependent tests are not-run, and re-discovering the
	// broken build per-tool is exactly the noise the precondition (T9.4) removes.
	if len(buildErr) > 0 {
		findings = append(findings, buildFinding(buildErr))
	}
	// A raw, non-JSON build failure printed straight to stdout. Same signal, different
	// shape (no test2json wrapper at all) — own it so it never crashes or reads as green.
	if len(rawNonJSON) > 0 {
		findings = append(findings, rawBuildFinding(rawNonJSON))
	}

	return findings
}
