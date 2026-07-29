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
const ScopeName = "github.com/Loxstomper/software-factory"

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

// Attribute keys. Factory-specific keys are namespaced under "factory." so they never
// collide with OTel's own semantic conventions. These keys are the join columns the
// control-room views key off — e.g. provenance traces a commit → issue → soul → model
// via AttrIssueID/AttrSoul/AttrModel, and the budget view groups cost by AttrModel.
//
// Cardinality rule (binding on every emitter — spans, metric dimensions, and log
// records alike). All three OTel signals share this schema so they join in a backend
// ("show the spans AND logs AND metrics for this issue/soul/stage"), but a metric
// labeled by an unbounded id spawns one time series per value and melts the backend.
// So:
//
//   - The high-cardinality identifiers — AttrIssueID, AttrEpicID, AttrInvocationID —
//     are TRACE and LOG attributes only. NEVER use them as a metric dimension; on a
//     trace/log they are exactly the drill-down key you want.
//   - Metric dimensions stay BOUNDED: AttrIssueRole (a fixed role set), AttrModel,
//     AttrSoul, AttrTokenKind, AttrGatePassed, AttrComponent, and AttrAttempt (a
//     small-integer retry count capped by the budget). These have few distinct values,
//     so their cartesian product of time series stays small.
//
// See specs/observability.md "Correlation: one schema across all three signals".
const (
	AttrComponent    = "factory.component" // emitting component: orchestrator|runner|broker|gate
	AttrIssueID      = "factory.issue.id"
	AttrIssueRole    = "factory.issue.role"
	AttrEpicID       = "factory.epic.id"
	AttrInvocationID = "factory.invocation.id"
	AttrSoul         = "factory.soul"
	AttrModel        = "factory.model"
	// AttrSubContext labels an llm-turn with the model stream it belongs to within one
	// invocation: "parent" (the soul's model) or "explorer" (the explore tool's pinned cheap
	// model). Bounded (two values), so it is safe as a metric dimension; it lets the budget /
	// verification views separate frontier spend from helper-loop spend (T12.2).
	AttrSubContext     = "factory.sub_context"
	AttrBase           = "factory.base"
	AttrResultStatus   = "factory.result.status"
	AttrSandboxBackend = "factory.sandbox.backend"
	AttrSandboxProfile = "factory.sandbox.profile"
	AttrSandboxImage   = "factory.sandbox.image" // resolved concrete artifact (image/rootfs) — pins toolchain bytes in provenance
	AttrSandboxID      = "factory.sandbox.id"
	AttrStopReason     = "factory.llm.stop"
	AttrToolCalls      = "factory.llm.tool_calls"
	AttrTokenKind      = "factory.token.kind" // input|output|cache_read|cache_write
	// Per-turn token counts carried as span attributes on an llm-turn (the per-kind
	// MetricTokens counter is the aggregate; these are the single-turn breakdown a replay
	// reads).
	AttrInputTokens      = "factory.tokens.input"
	AttrOutputTokens     = "factory.tokens.output"
	AttrCacheReadTokens  = "factory.tokens.cache_read"
	AttrCacheWriteTokens = "factory.tokens.cache_write"
	AttrToolName         = "factory.tool.name"  // tool invoked, e.g. git-push (egress) or edit_file (workspace)
	AttrToolError        = "factory.tool.error" // tool returned an error result (e.g. a failed compile), not a loop-fatal error
	AttrToolTurn         = "factory.tool.turn"  // 1-based agent-loop turn the tool call belongs to
	AttrGitBranch        = "factory.git.branch"
	AttrGitCommit        = "factory.git.commit"
	AttrCandidateRef     = "factory.candidate.ref"
	AttrGatePassed       = "factory.gate.passed"
	AttrGateChecksRun    = "factory.gate.checks_run"
	AttrHTTPStatus       = "factory.http.status" // upstream status of a brokered package fetch
	// AttrAttempt is the per-issue retry count (0 on the first try, incremented on each
	// re-dispatch up to the budget cap). It is a bounded small integer, so unlike the
	// unbounded ids above it is safe as a metric dimension — a retries-by-stage panel —
	// as well as a trace/log attribute. See the cardinality rule above.
	AttrAttempt = "factory.attempt"
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
	MetricInvocations        = "factory.invocations"         // counter: completed invocations by role+status (throughput)
	MetricInvocationDuration = "factory.invocation.duration" // histogram, seconds (latency)
	MetricLLMTurns           = "factory.llm.turns"           // counter: model turns by model (throughput)
	MetricLLMTurnDuration    = "factory.llm.turn.duration"   // histogram, seconds (latency)
	MetricTokens             = "factory.tokens"              // counter by model+token.kind (cost input)
	MetricCostUSD            = "factory.cost.usd"            // counter, dollars by model (cost)
	MetricGateRuns           = "factory.gate.runs"           // counter by passed (throughput)
	MetricGateDuration       = "factory.gate.run.duration"   // histogram, seconds (latency)

	// Context discipline (specs/components/agent.md "Tool-result aging") — what the agent
	// loop removed from the model's view, by role (a bounded dimension). Bytes, not
	// tokens: the loop never tokenizes, and bytes are the honest measurable proxy.
	MetricContextElidedResults = "factory.context.elided.results" // counter: tool results aged out, by role
	MetricContextElidedBytes   = "factory.context.elided.bytes"   // counter: content bytes saved, by role
)
