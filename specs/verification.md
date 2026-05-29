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
the freshly written tests but no implementation. This proves the test author produced
real, executing, non-vacuous tests that genuinely fail before an `implement` attempt is
spent making them pass, rather than an always-green suite that asserts nothing. Like
red→green it has no command of its own: it reuses the `tests-pass` command, run **once**
against the candidate, and passes iff that command **fails** (nonzero exit). It is the
[producer ≠ verifier](#producer--verifier) principle applied to the author-tests stage,
and the natural complement to the implementor's red→green proof, which later re-confirms
that same candidate is red as `implement`'s base before requiring the implementation to
turn it green.

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
data. The gate runs its checks fail-fast (it stops at the first failure), so today a `qa`
run that trips one scanner surfaces that finding and re-routes; running every independent
scanner to aggregate *all* findings in one pass is a natural refinement to weigh when the
`qa` stage that declares them lands.

### Traceability map
The test author emits, per test, the **spec heading + sentence** it claims to
encode. This does not prove faithfulness, but it makes interpretation *auditable*
when something slips, and it is the only window a human has into how the model read
the prose.

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

- Minimum mutation score and which mutation operators — still TBD; likely per-role
  config. The *expression* is now decided — a `metric<op>threshold` postcondition with
  operators `>=`, `<=`, `==`, `>`, `<`, the threshold living in config — and the gate
  check kind is implemented; only the concrete default score (and the operator-set the
  tool exposes) is unsettled, and is chosen when the `qa` stage that declares the
  postcondition lands (T2.9). The examples carry `mutation>=0.8` as a placeholder, not a
  committed default.
- Whether `qa` should include a second, *different-model* reviewer soul as an
  additional independent gate (N-version diversity) — candidate for defence in depth.
