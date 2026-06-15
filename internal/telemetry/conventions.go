// Package telemetry is the harness's OpenTelemetry layer. It is the single source of
// truth for the span and metric *schema* — the span names, attribute keys, and metric
// names every component emits — separate from the instrumentation scattered across the
// broker, runner, gate, and orchestrator. Keeping the schema in one place is the point:
// it is the contract the control room's budget, provenance, and replay views read
// (specs/observability.md, specs/control-room.md T4.10/T4.11), so it must have one
// definition to evolve rather than literals duplicated at each emit site.
//
// The provider setup that exports this schema (off / stdout / OTLP) lives in
// telemetry.go; this file is data only.
package telemetry

// ScopeName is the instrumentation scope every harness span and metric is emitted
// under, so a trace backend can attribute them to this codebase.
const ScopeName = "github.com/Loxstomper/harness"

// Span names. An invocation is one trace (specs/observability.md, "an invocation is a
// trace"): the runner opens SpanInvocation as the root, SpanBoot covers sandbox
// provisioning beneath it, and the broker's per-call SpanLLMTurn / SpanToolCall hang
// off it — parented in-process while the agent loop is co-located with the runner;
// cross-process trace-context propagation over NATS arrives with distribution (T5.8).
// SpanGateRun is the verification span; it is correlated to its invocation by issue id
// rather than span parentage because the gate runs in the trusted orchestrator, a
// distinct trace from the untrusted producer (producer ≠ verifier).
const (
	SpanInvocation = "invocation"
	SpanBoot       = "boot"
	SpanLLMTurn    = "llm-turn"
	SpanToolCall   = "tool-call"
	SpanGateRun    = "gate-run"
)

// Attribute keys. Harness-specific keys are namespaced under "harness." so they never
// collide with OTel's own semantic conventions. These keys are the join columns the
// control-room views key off — e.g. provenance traces a commit → issue → soul → model
// via AttrIssueID/AttrSoul/AttrModel, and the budget view groups cost by AttrModel.
const (
	AttrComponent      = "harness.component" // emitting component: orchestrator|runner|broker|gate
	AttrIssueID        = "harness.issue.id"
	AttrIssueRole      = "harness.issue.role"
	AttrEpicID         = "harness.epic.id"
	AttrInvocationID   = "harness.invocation.id"
	AttrSoul           = "harness.soul"
	AttrModel          = "harness.model"
	AttrBase           = "harness.base"
	AttrResultStatus   = "harness.result.status"
	AttrSandboxBackend = "harness.sandbox.backend"
	AttrSandboxProfile = "harness.sandbox.profile"
	AttrSandboxImage   = "harness.sandbox.image" // resolved concrete artifact (image/rootfs) — pins toolchain bytes in provenance
	AttrSandboxID      = "harness.sandbox.id"
	AttrStopReason     = "harness.llm.stop"
	AttrToolCalls      = "harness.llm.tool_calls"
	AttrTokenKind      = "harness.token.kind" // input|output|cache_read|cache_write
	// Per-turn token counts carried as span attributes on an llm-turn (the per-kind
	// MetricTokens counter is the aggregate; these are the single-turn breakdown a replay
	// reads).
	AttrInputTokens      = "harness.tokens.input"
	AttrOutputTokens     = "harness.tokens.output"
	AttrCacheReadTokens  = "harness.tokens.cache_read"
	AttrCacheWriteTokens = "harness.tokens.cache_write"
	AttrToolName       = "harness.tool.name"  // the brokered tool, e.g. git-push
	AttrGitBranch      = "harness.git.branch"
	AttrGitCommit      = "harness.git.commit"
	AttrCandidateRef   = "harness.candidate.ref"
	AttrGatePassed     = "harness.gate.passed"
	AttrGateChecksRun  = "harness.gate.checks_run"
	AttrHTTPStatus     = "harness.http.status" // upstream status of a brokered package fetch
)

// Token-kind attribute values for AttrTokenKind on the token-throughput counter. The
// four kinds are priced differently (specs/models.md ModelCost), so the cost view needs
// them split.
const (
	TokenKindInput      = "input"
	TokenKindOutput     = "output"
	TokenKindCacheRead  = "cache_read"
	TokenKindCacheWrite = "cache_write"
)

// Component attribute values for AttrComponent.
const (
	ComponentOrchestrator = "orchestrator"
	ComponentRunner       = "runner"
	ComponentBroker       = "broker"
	ComponentGate         = "gate"
)

// ToolGitPush is the AttrToolName value for the candidate-branch push — the one tool
// call the broker mediates (workspace tools run unbrokered inside the sandbox and are
// invisible to the collector by design; the broker sees only egress).
const ToolGitPush = "git-push"

// ToolPackageFetch is the AttrToolName value for a brokered Go module-proxy fetch — the
// supply-chain egress the broker mediates and logs (specs/security.md Control 2).
const ToolPackageFetch = "package-fetch"

// Metric names. Three families — latency, throughput, and cost — the budget view
// renders (specs/control-room.md T4.10). Durations are recorded in seconds (the OTel
// convention); the token and cost counters are monotonic sums.
const (
	MetricInvocations        = "harness.invocations"         // counter: completed invocations by role+status (throughput)
	MetricInvocationDuration = "harness.invocation.duration" // histogram, seconds (latency)
	MetricLLMTurns           = "harness.llm.turns"           // counter: model turns by model (throughput)
	MetricLLMTurnDuration    = "harness.llm.turn.duration"   // histogram, seconds (latency)
	MetricTokens             = "harness.tokens"              // counter by model+token.kind (cost input)
	MetricCostUSD            = "harness.cost.usd"            // counter, dollars by model (cost)
	MetricGateRuns           = "harness.gate.runs"           // counter by passed (throughput)
	MetricGateDuration       = "harness.gate.run.duration"   // histogram, seconds (latency)
)
