package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/Loxstomper/harness/internal/core"
	"github.com/Loxstomper/harness/internal/gotest"
	"github.com/Loxstomper/harness/internal/model"
	"github.com/Loxstomper/harness/internal/sandbox"
)

// run_tests is the implementor's fast inner-loop self-check (see specs/components/agent.md
// "Verification (self-check)" and specs/verification.md "Producer self-checks are feedback,
// not grades"). It wraps `go test -json <scope>` and returns the gate's own compact
// core.Findings.Format() string instead of the raw multi-thousand-line test dump.
//
// Why findings, not the raw dump: the implementor today runs tests through the generic
// `run` tool, which feeds the entire noisy log (per-test RUN/PASS lines, elapsed times,
// goroutine stacks) straight into the conversation history — the very history whose growth
// blows the model's context window. gotest.Parse owns that noise so only the signal-dense
// findings (one `file:line: message` per failure, the assertion body as detail) ever reach
// the agent. This is the headline Phase 9 inner-loop context win.
//
// Why zero-trust: this runs in the *untrusted producing sandbox*. It is feedback for the
// agent to fix its own work fast, never a grade — only the independent re-run in a fresh
// orchestrator-controlled sandbox grades the candidate (producer != verifier; see
// specs/verification.md). To keep the two from drifting they resolve the same findings
// shape: "I tested it" and "the gate tests it" share one result type.
//
// Why harvest the raw json: the compact findings are what the agent sees, but the full
// `go test -json` stream is kept as evidence so a human (or a forensic re-derivation) can
// drill into a finding. The agent has no network and cannot write to the content-addressed
// artifact store directly, so — exactly like the trace map and the transform log — the raw
// bytes accumulate on the agent side in a ledger that the trusted runner harvests into the
// store at teardown (under ArtifactKindGateEvidence, the same kind the gate's captured
// stdout/stderr uses). The hash returned to the agent inline is the store's own content
// address (sha256 of the bytes), computed locally; because the store is content-addressed
// the runner's later Put under that same kind yields a byte-identical hash, so the citation
// the agent already holds resolves once the bytes are harvested.

// defaultTestScope is the package pattern run_tests uses when the agent supplies none. It
// is deliberately the whole module so a scopeless self-check is still correct (never a
// false green from testing nothing); the tool description steers the agent to narrow it to
// the changed packages for speed. The *gate* — a different concern — always runs ./....
const defaultTestScope = "./..."

// TestEvidenceLedger accumulates the raw `go test -json` streams the run_tests self-check
// produces across one invocation, so the trusted runner can harvest them into the
// content-addressed artifact store after the sandbox is gone. It is the read-side analog of
// the TransformLedger: run_tests lives in a different constructor than the terminal
// lifecycle tools, so a single shared ledger — built once per invocation and handed to the
// tool — is how the bulky evidence a self-check produced reaches the harvest.
//
// Entries are keyed by their content-address hash (the store's own address), so a re-run
// that produces byte-identical json stores nothing twice — the same idempotence the
// content-addressed store gives. All methods are nil-safe so the tool can hold a nil ledger
// (no harvest sink) without a guard at every call site. Safe for concurrent use.
type TestEvidenceLedger struct {
	mu sync.Mutex
	// raw maps a content-address hash to the exact bytes addressed by it. A map (not a
	// slice) deduplicates identical re-runs and lets the harvester Put each unique stream
	// once under its own hash.
	raw map[string][]byte
}

// NewTestEvidenceLedger builds an empty ledger for one invocation.
func NewTestEvidenceLedger() *TestEvidenceLedger { return &TestEvidenceLedger{} }

// record stores one raw `go test -json` stream under its content-address hash and returns
// that hash. A nil ledger still returns the hash (so the tool can always cite evidence)
// but accumulates nothing — the self-check ran without a harvest sink. Nil-safe.
func (l *TestEvidenceLedger) record(raw []byte) string {
	hash := contentAddress(raw)
	if l == nil {
		return hash
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.raw == nil {
		l.raw = map[string][]byte{}
	}
	if _, ok := l.raw[hash]; !ok {
		// Copy: the caller's buffer may be reused (sandbox Exec output), and the ledger
		// must own the bytes it hands the harvester.
		cp := make([]byte, len(raw))
		copy(cp, raw)
		l.raw[hash] = cp
	}
	return hash
}

// Evidence returns the accumulated raw test streams keyed by content-address hash, for the
// runner to Put into the artifact store under ArtifactKindGateEvidence. Nil-safe; returns
// nil when nothing was recorded.
func (l *TestEvidenceLedger) Evidence() map[string][]byte {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.raw) == 0 {
		return nil
	}
	out := make(map[string][]byte, len(l.raw))
	for h, b := range l.raw {
		out[h] = b
	}
	return out
}

// contentAddress computes the artifact store's own content address for bytes: the sha256
// digest, hex-encoded, with the store's algorithm prefix. It mirrors artifact.FilesStore's
// Put exactly (sha256 of the bytes alone), so the hash run_tests returns to the agent now
// is the same one the runner's later Put resolves to — without the agent ever reaching the
// store (it has no network). Kept inline rather than importing internal/artifact so the
// untrusted agent package carries no dependency on the trusted store implementation.
func contentAddress(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// RunTestsTool builds the run_tests self-check tool. ledger may be nil (no harvest sink),
// in which case the tool still returns findings and a (uncollected) evidence hash.
func RunTestsTool(sb sandbox.Sandbox, ledger *TestEvidenceLedger) Tool {
	return funcTool{
		def: model.ToolDef{
			Name: "run_tests",
			Description: "Run the project's Go tests over a scope and return the parsed test findings " +
				"(file:line + message per failure), not the raw test log — so a noisy test run never floods " +
				"your context. Use this for your inner-loop self-check after a change; scope it to the packages " +
				"you touched (e.g. ./internal/foo/...) to keep it fast. This is feedback only: passing here is " +
				"not acceptance — the candidate is graded by an independent re-run after you submit.",
			Params: json.RawMessage(`{
				"type": "object",
				"properties": {
					"scope": {"type": "string", "description": "Go package pattern to test, relative to the worktree root (e.g. ./internal/foo/... or a single package). Defaults to the whole module (./...); narrow it to the changed packages for a faster self-check."}
				}
			}`),
		},
		fn: func(ctx context.Context, args json.RawMessage) (Outcome, error) {
			var a struct {
				Scope string `json:"scope"`
			}
			if bad := decodeArgs(args, &a); bad != nil {
				return *bad, nil
			}
			scope := a.Scope
			if scope == "" {
				scope = defaultTestScope
			}

			// `go test -json` over the scope. The scope is passed as a positional argument
			// (never interpolated into a shell line) so it can never inject a command; a
			// build failure or a non-JSON line is not an error here — gotest.Parse turns it
			// into a finding (that IS the signal). A non-zero exit is likewise expected (tests
			// red), so we do not surface it as a tool error: the findings carry the verdict.
			res, err := sb.Exec(ctx, sandbox.Command{Path: "go", Args: []string{"test", "-json", scope}})
			if err != nil {
				return Outcome{}, fmt.Errorf("agent: run_tests exec: %w", err)
			}

			// The raw json is the evidence; harvest it (by content-address hash) so a finding
			// can be drilled into later, and cite that hash to the agent inline.
			hash := ledger.record(res.Stdout)
			findings := gotest.Parse(res.Stdout)

			return Outcome{Content: formatRunTests(findings, hash), IsError: len(findings) > 0}, nil
		},
	}
}

// formatRunTests renders the self-check result the agent sees: the compact findings (or a
// clear "passed" line on zero findings), always followed by the evidence hash so the agent
// or a forensic reader can cite the raw `go test -json` stream. It never includes the raw
// dump — that is the whole point.
func formatRunTests(findings core.Findings, evidenceHash string) string {
	body := findings.Format()
	if body == "" {
		// A clean pass. Stating "0 findings" (not "0 tests") keeps the self-check honest:
		// it reports what the parser saw, the same shape a failing run reports.
		body = "tests passed: 0 findings"
	} else {
		body = fmt.Sprintf("%d finding(s):\n%s", len(findings), body)
	}
	return fmt.Sprintf("%s\n\n(raw test output kept as evidence: %s)", body, evidenceHash)
}
