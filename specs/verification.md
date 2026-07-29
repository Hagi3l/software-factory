# Verification

How the factory trusts its own output **with no human reviewing code**. This is
the keystone of the whole design — if verification is weak, "no human in the loop"
is reckless rather than autonomous.

See also: [workflow.md](workflow.md), [specs-process.md](specs-process.md),
[integration.md](integration.md), [architecture.md](architecture.md).

---

## The problem

A spec says, in prose, "reject negative quantities with a 400." The QA gate has
nothing to check that against unless *something* turns that sentence into an
executable test. With a human reviewer, the human is the oracle for whether the
implementation matches intent. **Remove the human and you inherit the oracle
problem:** nothing inherently proves the tests faithfully encode the spec, and the
tests become the contract the entire factory trusts.

This spec describes the structural controls that shrink that gap to something
defensible.

---

## Producer ≠ verifier

The governing principle, applied at three levels:

1. **Tests are authored independently of the implementation.** A dedicated
   `author-tests` stage, run by a *different* soul than `implement`, writes the
   acceptance tests. This is the defence against **correlated errors**: if the
   implementor also wrote the tests, the same misreading produces a wrong
   implementation *and* a test that agrees with it, and the gate passes green on a
   bug.
2. **Gates run in a fresh [verification sandbox](glossary.md#verification-sandbox)**,
   controlled by the orchestrator, distinct from the producing agent's sandbox. An
   untrusted process must never report its own grade.
3. **Acceptance is applied by the orchestrator**, not self-declared by the agent.

```
plan → author-tests → implement → qa
          (soul A)      (soul B)    (gate, clean sandbox)
       writes failing  makes tests  runs tests + mutation
       acceptance tests  pass        + scanners, independently
```

The `implement` agent *sees* the tests — that is intended (TDD: the tests are the
spec made executable). The independence that matters is **author ≠ implementor**,
so the tests aren't written to match a particular (possibly wrong) implementation.

### The separation is recorded, not just enforced

Soul selection is per-stage and otherwise transient — the orchestrator picks a soul
when it dispatches and the choice is gone once the [Brief](glossary.md#brief) is sent.
But "author ≠ implementor" is only *demonstrable* after the fact if both identities
survive, so the orchestrator **stamps the producing soul onto the issue as each stage
advances**: the `author-tests` soul and the `implement` soul are recorded (threaded
forward exactly like the [traceability map](#traceability-map) hash and preserved
across `on_failure` retries) and cited together on the merge trailer
(`Tests-Soul:` alongside `Soul:`, see [security.md](security.md)). This turns
producer ≠ verifier from an invariant the harness merely *upholds* into one a human
can *see* — both at the merge commit and, live, in the
[verification view](control-room.md). There is no separate "verifier soul" to record:
the `qa` [gate](glossary.md#gate) is orchestrator-controlled and runs in a clean
[verification sandbox](glossary.md#verification-sandbox), not as an agent with a soul —
so the meaningful, recordable comparison is the two *producing* souls, with the gate's
independence carried structurally by the sandbox boundary.

### The verifier's egress is deny-all, widened only for package fetch

The verification sandbox reaches nothing by default — no model calls, no git push, no
events — so an untrusted candidate cannot smuggle its grade out or pull in anything that
would let it cheat the gate. The one principled exception is **package fetch**: a candidate
that adds a brand-new dependency must be *buildable* in the verifier, or it could never be
re-gated even though the producing agent's own sandbox fetched the dep fine. So when a
deployment allowlists the [package proxy](security.md#control-2--supply-chain-mediation),
the verifier is granted the **same** `package-proxy` egress and nothing else — one opt-in
that covers producer and verifier together. This widens the verifier's *reach*, not what it
*trusts*: `go.sum` (which the candidate carries) pins the exact module bytes the producer
fetched, the checksum DB is proxied through the same chokepoint, and the `qa` scanners run
post-fetch — so the verifier builds against the identical, pinned dependency set, with the
fetch logged at the broker like any other egress. Both producer and verifier route through
the *one* host-side fetcher, so what they permit can never drift.

### Model diversity is configured, not mandated

Soul independence (above) is enforced by the harness. *Model* independence —
running the verifier on a different model **family** than the producer, so the two
don't share correlated blind spots (N-version diversity) — is a **configuration
capability**, not a built-in mechanism. The pieces already exist: a role maps to a
set of souls (`selector`, see [configuration.md](configuration.md)) and each soul
names its own model/tier (per-role model tiers), so a user who wants a
different-family reviewer simply points the `qa` soul at one. This is consistent
with the config-is-the-pipeline principle: the harness *enables and recommends*
diversity but leaves the model assignment to the user who configures the pipeline.

Because same-family producer/verifier is weaker independence, config validation
**should warn** (non-fatal) when a verifier role shares a model family with the
producer — keyed on family, not just an identical model id. **Family is the
organization that trained the weights** (the correlated-blind-spot unit), *not* the
provider adapter that serves them: behind an aggregating gateway (e.g. OpenRouter,
where every model is one `openai-compat` provider/endpoint) the vendor still lives in
the model slug, so the family is derived from the slug's `vendor/` prefix
(`deepseek/deepseek-v4-pro` → `deepseek`), falling back to the provider tag only for a
bare-slug model with no inferable vendor, and overridable by an explicit `family:` on
the registry entry ([configuration.md](configuration.md)). Keying on the gateway alone
would both miss genuine same-family overlap and falsely flag a deepseek-vs-anthropic
split that happens to share one gateway. The warning's natural home is `harness
validate` (so yaml-only users see it); a control-room tooltip is a complementary
surface once a souls/config view exists.

The reasoning extends only *weakly* to the
[`explore` tool](components/agent.md#explore--distilled-comprehension) on the verify path,
and the decision (2026-07-02) is that a **shared, same-family explorer is acceptable
there**. The theoretical risk is correlated attention-steering: a distiller frames how its
caller thinks, so a verifier sharing the producer's explorer could inherit its navigation
blind spots. But explore is additive and never load-bearing by contract
([components/agent.md](components/agent.md)): the verdict comes from the gate's
deterministic checks, which have no attention to steer; the verifier keeps every raw
comprehension tool and the candidate is re-graded in a fresh sandbox regardless; a weak or
blinkered explore answer costs a wasted search, never a false pass. Mandating family
diversity for the *helper* would buy a second-order independence gain at the price of a
second cheap-model entry plus stage-scoped selector routing the harness has no other use
for. Deployments wanting a stricter posture have two config-only options: a second
`explorer` soul on a different family with a distinguishing selector, or **no explore on
the verify path at all** (the verifier reads raw — fully independent). `harness validate`
does **not** advise on explorer family overlap — the family-overlap advisory covers the
producer/verifier *main* models, where the model *is* the grader's judgment, not a
recoverable helper.

### Producer self-checks are feedback, not grades

Nothing stops — and the [implementor persona](components/agent.md) is expected to
encourage — an agent running the gate's own checks *inside its own sandbox* before it
calls `submit`: lint, build, and the acceptance tests are reachable through the `run`
tool, and a candidate that fails them is far cheaper to fix in-loop than to bounce back
through a whole `qa` round-trip (a fresh sandbox, the full check suite, possibly a
retry-budget hit). Catching a trivially-fixable lint failure at the gate instead of at
the keyboard is wasted compute, so self-checking is welcome.

It grants **zero trust**. A producer self-check runs in the *untrusted* producing
sandbox and is never read as a grade — the agent could skip it, misreport it, or run it
against a tampered tree. Only the independent re-run in the fresh
[verification sandbox](glossary.md#verification-sandbox) advances the transition. The
self-check lowers the *expected cost* of clearing the gate; it never *is* the gate.
This is the same `done` = *candidate ready, not accepted* boundary, seen from the
producer's side: **the agent checks itself for speed; the transition logic checks it for
trust.**

To stop the two from drifting, the producer's self-check and the gate **resolve the same
[check registry](configuration.md) command** — one `golangci-lint run`, one `tests-pass`
— authored once and run in two places for two different reasons: fast feedback in the
producer's sandbox, authoritative grading in the clean one. Because it is byte-for-byte
the same command, "I linted it" and "the gate lints it" can never quietly mean different
things.

The producer reaches these through the same **structured** surface the gate produces:
`run_tests` for the fast inner-loop check and `run_gate` for the full pre-`submit` suite,
each returning the same [findings](glossary.md#finding) the gate would — so the producer
works against a compact, signal-dense result instead of a raw dump (and stops blowing its
own context window on a noisy test log), and "I checked it" and "the gate checks it" share
not just one command but one result shape. It is still zero trust: the tool runs in the
untrusted sandbox, only the independent re-run grades.

---

## The trust mechanisms

Because specs are **pure prose** (see [specs-process.md](specs-process.md)), the
test author's interpretation carries the full correctness burden. These mechanisms
make the tests themselves trustworthy:

### Red→green proof
Require the acceptance tests to **fail against the pre-implementation base** and
**pass against the implementation**. This single mechanical check — which TDD
normally leaves to a human's eye — proves a test is not vacuously green and
actually exercises the behaviour. It is cheap and kills a class of fake-passing
tests.

Mechanically, the gate provisions **two fresh verification sandboxes** — one seeded
at the candidate branch, one at the base ref the candidate branched from — and runs
the project's acceptance-test command (the `tests-pass` command from the
[check registry](configuration.md)) in each. The proof passes iff that command
**fails on the base** (red) and **passes on the candidate** (green); a base that
passes means the tests don't exercise the change and the candidate is rejected even
though its own tests are green. The reserved proof has no command of its own — it
reuses `tests-pass`, so the acceptance tests stay a single source of truth — and the
evidence record captures both runs for audit. The base ref the candidate branched from
holds the failing tests but not the implementation: the orchestrator threads the
verified [`author-tests`](workflow.md) candidate as the `implement` issue's base (see
base threading in [workflow.md](workflow.md)), so red→green is the natural gate on
`implement`.

### Tests-red proof
The complement to red→green, gating [`author-tests`](workflow.md). It requires the
acceptance tests to **fail against the author-tests candidate** — the branch holding
the freshly written tests but no implementation — so an `implement` attempt is never spent
making an always-green suite (one that asserts nothing) pass. Like red→green it has no
command of its own: it reuses the `tests-pass` command, run **once** against the candidate,
and passes iff that command **fails** (nonzero exit). It is the
[producer ≠ verifier](#producer--verifier) principle applied to the author-tests stage,
and the natural complement to the implementor's red→green proof, which later re-confirms
that same candidate is red as `implement`'s base before requiring the implementation to
turn it green.

**Nonzero exit is necessary but not sufficient in a compiled, statically-typed language.**
A failing exit proves the suite did not pass — but not that it *ran*. In Go (or any
compiled, typed language) a test that references a symbol the implementation has not defined
yet **fails to compile**, which also exits nonzero, so `tests-red` would pass on a suite
that never executed a single assertion. That is a *vacuous* red: it certifies nothing about
whether the tests encode behaviour, and it lets an under-defined contract flow downstream —
where the implementor, forced to make red→green's green compile, ends up *inventing* the API
surface itself, quietly taking over the interface design that is the test author's job. (In
a dynamically-typed language the same test *executes* and fails at runtime, so the red is
genuine and this gap never arises.)

**The compiled-language complement is a `compiles` companion check** — not gate
language-awareness. A compiled-language target pairs `tests-red` with an ordinary check that
the tree, *including the test binaries*, builds (for Go: `go build ./... && go test
-run='^$' ./...`) and lists **both** on the `author-tests` postcondition. The conjunction
"**compiles ∧ tests-fail**" is the genuine executing red: the suite built and an assertion
failed. This keeps the kernel language-neutral — `compiles` is a normal exit-0-pass command
and the target's build system, not the harness, defines what "compiles" means, so a compiled
target opts in and a dynamic one does not. It also forces the test author to commit the
minimal compiling **API skeleton** (signatures and types, no logic), which the implementor
inherits via base threading as a precise, compiler-checked contract; and it strengthens
red→green, since a provably-compiling base makes red→green's "fails on base" a failed
*assertion* rather than a build break.

### Mutation testing
Mutate the implementation and require the tests to **catch** the mutation; gate on
a minimum mutation score. This mechanically attacks the "weak test" problem
(tests that pass but assert nothing meaningful). It is expensive — and spending
compute to buy assurance is exactly the trade a no-human factory should make.

> **Decision:** red→green + mutation testing are **first-class postconditions**.
> The compute cost is accepted; it is what makes no-human-review defensible.

A mutation gate is expressed as a **metric-comparison postcondition** — `mutation>=0.8`:
a metric name, a comparison operator, and a threshold. It resolves to the measurement
command registered under its metric name in the [check registry](configuration.md)
(`checks: { mutation: … }`); the gate runs that command once against the candidate in the
clean sandbox and reads the score it prints (the trailing numeric token of stdout). The
check passes only when the command **ran cleanly (exit 0), emitted a parseable score, and
that score satisfies the comparison**. A nonzero exit (the tool could not measure) or
unparseable output **fails closed** — an unverifiable score is not a passing one, because
a gate that carries 100% of the assurance must never read "couldn't tell" as "fine."
Keeping the tool invocation in config (not hardcoded in the gate) keeps the gate
tool-agnostic: it grades a number, not a `gremlins` report, so swapping the mutation tool
is a config edit. The measured score and the comparison are recorded as gate evidence, so
a passed or failed mutation gate is auditable after the fact.

### Independent scanners
The `qa` gate also runs generic, spec-independent checks: SAST (e.g. `gosec`),
dependency/license/vulnerability scans, policy-as-code. Defence in depth — the
gate carries 100% of the assurance a human reviewer would otherwise provide, so it
should be many independent checks, not one.

These need no built-in check kind: a scanner is an **ordinary command check** (see the
[check registry](configuration.md)), graded on its exit code — `0` means the scanner ran
and found nothing (pass); any non-zero exit means it reported findings *or* could not run,
both of which **fail closed** (an unverifiable candidate is not a passing one). The kernel
ships three: `gosec` (SAST), `govulncheck` (known-vulnerability scan), and `license-scan`
(dependency/licence policy, e.g. `go-licenses check`). Each captures its report as gate
evidence, cited by name in the provenance trailer (`gosec@<hash>`) like every other check,
so a passed or failed scan is auditable down to the exact findings. Adding a fourth scanner
is a config edit — a new `checks:` entry plus the stage postcondition — because the gate
grades an exit code, not a particular tool's report.

Because the verification sandbox is [zero-network](security.md), a scanner that needs
reference data — the vulnerability database for `govulncheck`, licence metadata for
`license-scan` — must read it from data baked into the role's sandbox image, the same
offline guarantee the build relies on. A purely static analyser like `gosec` needs no such
data. The gate runs its checks **fail-fast by default** (it stops at the first failure), so
a `qa` run that trips a proof or a metric surfaces that and re-routes to `implement` to fix it.
Fail-fast is deliberate for the proof and measurement checks — a mutation score is
meaningless on a candidate whose tests are red, so there is no point measuring it.

The independent scanners are the exception. A failed scanner does **not** make the next one
meaningless — a `gosec` hit tells you nothing about whether `govulncheck` or `license-scan`
will also fire — so stopping at the first wastes the others and forces the candidate to
bounce once *per* scanner. Instead, the checks named in the config's **`independent_checks`**
list (see [configuration.md](configuration.md)) are run **past a failure**: one `qa` pass
records *every* independent finding at once, so the human triaging the dead-letter queue (or
the agent re-routed to `implement`) fixes them all in a single round-trip. The signal is
config, not code: only a command check (a scanner) may be listed, and `harness validate`
rejects a reserved proof or a metric's measurement command, so those always stay fail-fast.
The gate still stops at the first *non-independent* failure, and a candidate that trips any
check still fails the gate — aggregation changes only *how much* a single failing pass
reports, never the verdict. Each scanner's report is captured as gate evidence regardless, so
all the aggregated findings are auditable by hash.

### Traceability map
The test author emits, per test, the **spec heading + sentence** it claims to
encode. This does not prove faithfulness, but it makes interpretation *auditable*
when something slips, and it is the only window a human has into how the model read
the prose.

Mechanically, the author records each entry with a `trace_test` tool call (test name,
spec file, heading, sentence) as it writes the test — a non-terminal lifecycle tool that
accumulates, the same shape as `request_subtask`, folded into the terminal submit Result.
The runner **harvests** the accumulated map to the [artifact store](components/artifact-store.md)
as a stable, content-addressed document (kind `traceability-map`) and clears the structured
form from the result envelope, so the bulky map travels by hash, not inline — the same
discipline every large artifact (prompt, transcript, gate evidence) follows.

Because the map is produced at `author-tests` but the only provenance surface is the merge
commit at `integrate`, the orchestrator **threads its hash forward**, exactly as it threads
the candidate base: the `author-tests` map is stamped onto the `implement` issue it
produces, propagated across later agent stages, and preserved across `on_failure` retries,
so a re-implemented candidate still traces back to the same author's interpretation. At the
merge the trailer cites it as `Traceability: <hash>` (see [security.md](security.md)). A
change merged without an `author-tests` stage in its lineage simply carries `Traceability:
(none)` — self-describing, like a missing `Prompt-SHA`, never silently blank.

### The gate verdict is recorded, not just acted on

A gate run already produces a structured verdict internally — per-check pass/fail, the
red→green base-vs-candidate pair, the mutation score against its threshold, each
scanner's exit — but historically the orchestrator *acted* on it (accept / re-route)
and discarded everything but the final disposition. That is enough to advance the
graph and nothing more: the proof that justified the decision evaporated. Because the
gate carries 100% of the assurance a human reviewer would, the verdict that justified
a merge is exactly the thing worth keeping.

So the gate **harvests its verdict to the [artifact store](components/artifact-store.md)
as a single content-addressed record** (kind `gate-verdict`), the same discipline every
large artifact follows. The record holds, per check: its kind (command / red→green /
tests-red / metric), pass/fail, the measured score and comparison for a metric check,
and the base-and-candidate outcomes for a red→green proof — each still pointing at its
own captured-output evidence by hash. Individual check evidence is still cited in the
provenance trailer as before; the `gate-verdict` record is the *assembled* view of one
gate run, so the [verification view](control-room.md) can render the whole trust
argument — red→green, mutation score, scanners, and the producing-soul split — as a
**forensic snapshot** without the gate being live. It is recorded for every gate run,
pass or fail: a *rejected* candidate's verdict is as worth auditing as an accepted
one (it is what a human triaging the [dead-letter queue](control-room.md) needs).

### Findings: structured evidence, not the grade

Every check captures its tool's output, but a raw `go test` dump or a `govulncheck`
stream is mostly noise — and that noise is exactly what dilutes an agent's context and
buries the one line that matters. So a check is also a **thin per-tool adapter**: it runs
the tool in its machine-readable mode (`go test -json`, `golangci-lint --output.json`,
`gosec -fmt=json`, `govulncheck -json`) and parses the output into structured
**[findings](glossary.md#finding)** — each a `{file, line, severity, rule, message}` plus
a typed `detail` that preserves the one tool-specific thing that matters (a test's
assertion diff, a vulnerability's call path). This is the same canonical-interface /
thin-adapter split the [model layer](models.md) and the [semantic tools](components/agent.md)
already use — *provider adapter : model :: check adapter : tool output* — so the findings
*shape* is **language-neutral** while the parsing lives in a per-tool (per-language)
adapter; the kernel ships the Go adapters, and a command with no adapter still grades on
its exit code with its raw output as evidence.

The raw output still travels — it remains the gate-evidence artifact, by hash, as before.
The findings are the **compact, signal-dense, cache-stable** representation that enters the
agent's context, the [gate-verdict record](#the-gate-verdict-is-recorded-not-just-acted-on),
the [verification view](control-room.md), and a failed candidate's retry [Brief](glossary.md#brief).
The extraction is done **once, by the infra, not by the agent** — so even a weak producing
model gets a focused result whether or not it would have known to ask for one (the same
"the tool layer picks the mechanism, never the agent" stance as the semantic tools).
Findings are sorted deterministically and stripped of run-to-run jitter (elapsed times,
timestamps), so an unchanged re-run yields byte-identical findings — which keeps the
conversation prefix cacheable and lets a "findings not shrinking across attempts" signal
mean what it says.

**Grading is unchanged.** Pass/fail is still decided by the exit code (or the reserved
proof / metric comparison) exactly as above — findings are *evidence*, not the grade, so
the gate stays agnostic to any one tool's report. Grading the verdict *over* the findings
— a severity threshold like "fail on a high-severity `gosec` hit, treat the rest as
advisory" — is a deliberate **later refinement**, not part of this contract; today a check
that exits non-zero fails closed regardless of what its findings say.

### A check is tri-state; a broken build never reads as green

A check ends in one of three states, not two: **passed**, **failed**, or **not-run**. The
third matters because checks have a dependency — nothing else can run on a tree that does
not compile, and a scanner that never executed must never be recorded as "ran, found
nothing." A misleading green is worse than a red here, because [producer ≠ verifier](#producer--verifier)
cannot catch a candidate that merely *looks* clean. So the gate runs a **build
precondition** first; if it fails, the gate records the build error as a finding and marks
every dependent check **not-run**, instead of letting each tool independently rediscover
the broken build in its own error format. This is the fail-fast rule (above) made honest:
the precondition short-circuits the dependent checks while the
[independent scanners](#independent-scanners) still aggregate among themselves. Crucially,
**not-run never counts as a pass** — the gate still fails closed on anything that is not a
clean pass, so an unverified candidate is never accepted; the tri-state changes only what
the verdict *records* (we-checked-and-it's-clean vs. we-never-got-to-check), which is what
a human triaging the dead-letter queue, and the retry Brief, actually need.

---

## The residual risk (named honestly)

If the spec is genuinely ambiguous and **neither** agent notices — the test author
and the implementor quietly resolve it the *same wrong way* — it sails through
green and violates intent silently. The tests can't catch it because they encode
the same misreading.

There is **no fully automatic defence** against undetected shared ambiguity with
no human in the loop. The mitigations are:

- **Escalation on *detection*** — an agent that *notices* ambiguity must raise
  `needs-spec-clarification` rather than guess (see
  [specs-process.md](specs-process.md)).
- **The traceability map** — gives a human something to audit after the fact.

This residual risk is nonzero. It is the price of no human review, and specs
should be written with that in mind: ambiguity is a *correctness hazard*, not just
a style issue.

---

## OPEN questions

- Minimum mutation score and which mutation operators — the *expression* is decided (a
  `metric<op>threshold` postcondition with operators `>=`, `<=`, `==`, `>`, `<`, the
  threshold living in config) and the gate check kind is implemented. With the live `qa`
  stage landed, the kernel commits **`mutation>=0.8`** as its default — an 0.8 score
  with the `>=` operator — declared on the qa stage in `config/harness.yaml`. It remains
  config, so it is tunable per role/project; which mutation operators the tool exercises is
  the tool's own configuration, kept out of the gate (which grades only the resulting
  number).
- N-version diversity via a different-model reviewer soul is **resolved, not open**:
  it is a configuration capability — see "Model diversity is configured, not mandated"
  above — not a built-in mechanism. The non-fatal config-validation warning when a
  verifier shares a model family with the producer is **implemented** (`harness validate`
  surfaces it via `config.Warnings()`; the producer is the red→green-gated stage and its
  verifiers are the gate stages it produces). The complementary control-room tooltip
  remains a follow-up, pending a souls/config view.
