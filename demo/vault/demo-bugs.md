# Demo bugs — observed live-run failures

A running log of failures seen while running `demo/vault/run.sh` on a real feature.
Each entry is symptom → impact → root cause → evidence → fix direction, plus any
manual recovery used to keep a live run going. These are **harness** bugs surfaced by
the demo, not vault-app bugs.

---

## BUG-1 — Intermediate stage closes are reverted; a dependent sibling never dispatches (pipeline stalls) — FIXED (T15.3 was insufficient; real fix below)

**First seen:** 2026-07-06, share-link epic (`vault-bpc`), children `generate` (`vault-qkq`
lineage) and `reveal` (`vault-532`).

### Correction (2026-07-07) — the real root cause, and the fix that actually holds
**BUG-1 recurred with T15.3 in the running binary** (share-link epic `vault-tbh`, run at
14:36–14:46; binary built 14:30, well after the T15.3 commit). `vault-0cg`'s close was reverted
and `vault-273` (blocked on it) never dispatched — the identical stall. So the T15.3 diagnosis
below (*concurrent* `bd` processes racing on the jsonl round-trip) is **wrong**, and serializing
`bd` processes cannot fix it.

Proven against the live Dolt server's own versioned history (`dolt_history_issues`):

```
04:39:51.862   vault-0cg  status=closed        ← the close COMMITTED to Dolt
04:39:52.478   vault-0cg  status=in_progress   ← REVERTED by the next bd call's import
```

The reverting call (`04:39:52.478`, the harness's *own* next serialized write — the claim of the
successor `vault-c3n`) begins with a `bd: create N issue(s)` commit: **bd auto-imports
`issues.jsonl` → Dolt at the start of every write, even in `--server` mode.** The reverted row's
`updated_at` rolls back to a *pre-close* value that matches the on-disk jsonl — so the import
re-applied a stale jsonl over the committed close.

Why jsonl is stale is an **asymmetry**, not a race: `bd config` shows auto-IMPORT runs on every
write **unthrottled**, but auto-EXPORT (Dolt → jsonl) is **throttled** (`export.interval`, default
**60s**). A stage advance is create-successor + close-predecessor, and a whole epic runs in a few
minutes — so within one 60s window `issues.jsonl` freezes at an early snapshot while every
subsequent write reimports it, reverting any close whose issue is still present in that frozen
snapshot. This reproduces with a **single writer, no concurrency** (`bd close A; bd update B`
reverts A to `open`, 3/3), which is why T15.3's process-serialization changed nothing.

**Real fix (applied):** `bd config set export.auto false` immediately after `bd init --server`
in `demo/vault/run.sh`, making the warm Dolt server the sole source of truth — the orchestrator
reads/writes it via the server and never consumes `issues.jsonl` during a run, so no reimport can
clobber a write. Verified: default `export.auto` reverts a close-then-write burst 3/3; with
`export.auto=false` the close survives 3/3. Only valid in `--server` mode (in file/no-db mode
jsonl *is* the store — never disable export there). The T15.3 serialization is retained as
single-writer hygiene but is **not** what fixes BUG-1. `--skip-hooks` remains necessary (it closes
the git-triggered reimport vector); `export.auto false` closes the second, fatal one.

### Fix (T15.3, SUPERSEDED as the cause) — the trusted beads client serializes every `bd` invocation
The root cause is concurrent `bd` processes racing on the jsonl round-trip (one export
clobbering another's write). The fix makes serialization a property the trusted layer owns
instead of a precondition it *assumes* of the backend: `internal/beads/Client` now holds a
**per-store-directory lock** across every `bd` subprocess (`Client.mu`/`storeLock`, taken in
`Client.run`), so no two `bd` processes on the same store ever overlap — process-wide, across
the three Clients that share one repo (the orchestrator's writer, the wizard seeder, the
control-room reader). Create-successor and close-predecessor are then issued strictly
one-after-another (each a complete import→mutate→export against the previous export), so a
create can no longer clobber an adjacent close, and both readiness (`bd.ready()`) and epic-drain
(`sweepEpicCompletion`) read a durable store that stays correct — no stranding. This holds
regardless of whether the backend is a warm serialized engine or a per-call jsonl round-trip;
the warm server is now a *throughput* recommendation, not a correctness precondition (a slow
store can only add latency, never lose a write). Regression:
`internal/beads.TestClientSerializesInvocationsPerStore` (50×2 concurrent writes over two
same-dir Clients see max-concurrency 1). Spec: `specs/components/orchestrator.md`
"Durable-write loss". The `run.sh` warm-server question (fix direction (a)) is moot for
correctness and left as a throughput follow-up.

### Symptom
The `generate` slice built, gated, and integrated onto `epic/vault-bpc` cleanly
(commit `d8fc022`, all 5 gate checks green twice). Then the pipeline went **silent for
16+ minutes** with the harness process alive but idle (control room HTTP 200, ~0% CPU).
The `reveal` child (`vault-532`) sat `open`/"queued" and was **never dispatched**, so the
epic never drained and `main` never advanced.

### Impact
- A feature with two dependent children **hangs forever** after the first child
  integrates. No dead-letter, no error — a silent stall (worst kind for a live demo).
- Wasted spend: the store/projection divergence had already triggered one full **redo**
  of the generate slice earlier in the run (two near-identical candidate chains in git:
  `test(qkq)→feat(0mj)→security-qa` ×2), burning ~$12.50 twice.

### Root cause
`bd` is **not actually running in the warm `--server` mode** `run.sh` sets up. Every
`bd` invocation prints `auto-importing … issues.jsonl into empty database` and, on a
write, `auto-export`. So each `bd` process round-trips the entire work store through
`.beads/issues.jsonl`:

```
bd <cmd>:  import issues.jsonl → (empty) dolt DB  →  mutate  →  export DB → issues.jsonl
```

The orchestrator's beads client shells out to `bd` exactly like the CLI does
(`beads.New(WithBinary(bd), WithDir(repo))`, no `--server` flag — `cmd/harness/run.go`),
so **`issues.jsonl` is the effective shared source of truth**, not the dolt server
(which is running but unused for these calls).

On the **produce-next-stage** path the orchestrator fires **two `bd` processes
near-simultaneously** — create the successor issue, then close the current one
(`internal/orchestrator/results.go` `advance` → create child, then `accept` → `bd.Close`).
Their import→export round-trips **race** (classic lost update): the successor-create's
export lands last and **clobbers the close's status write**. Result: successors exist,
but the parent's `closed` write vanishes.

- `vault-yw0` (the *terminal* qa→integrate step, **no** concurrent create) closed and
  **stuck** — it had no create racing it.
- `vault-bpc`, `vault-qkq`, `vault-0mj` (each closed *while its successor was being
  created*) **reverted** to `open`/`in_progress`.

Then the second-order failure: **`scheduleReady` reads readiness from `bd.Ready()` (the
durable store), not the in-memory projection** (`internal/orchestrator/schedule.go:30`).
`vault-532` depends on `vault-qkq`; because `qkq`'s close never persisted, `bd.Ready()`
never returned `532`, so reveal was never dispatched — from the very first moment, not
just after generate integrated. The orchestrator's in-memory projection *did* mark the
lineage closed (that's how it advanced generate), which is why it went **idle** rather
than erroring: projection says "done", store says "qkq open, 532 blocked".

### Evidence
- Orchestrator log closed all four, e.g.
  `13:40:28 accepted vault-qkq produces=[implement]`,
  `13:44:06 accepted vault-0mj produces=[qa]`,
  `14:02:18 accepted vault-yw0 produces=[integrate]`.
- Durable store (`bd` / `.beads/issues.jsonl`) after the run showed `bpc` **open**, `qkq`
  **open**, `0mj` **in_progress**, `yw0` **closed** — and each reverted record's
  `updated_at` was its **dispatch** time, not the later close time (the close never wrote).
- `bd ready` returned `vault-bpc` (the closed-in-projection epic root), never `vault-532`.
- Every `bd` call logs `auto-importing … into empty database`; writes log
  `auto-export: git add failed … .beads … ignored by .gitignore`.
- **Controlled test:** a *single* `bd update <id> --status closed` **sticks** (verified
  stable for 8s with no other writer); three *back-to-back* closes reverted two of the
  three — confirming the race is between concurrent `bd` round-trips, not the
  orchestrator's read poll.

### Ruled out
- **Not** the beads git-hook revert documented in `run.sh` (post-checkout/post-merge
  re-importing a stale jsonl): `core.hooksPath` is unset and no hooks are installed
  (`--skip-hooks` held).

### Fix direction (not yet applied)
1. **Make `bd` actually use the warm server.** The whole point of `bd init --server` is
   one serialized engine; if regular `bd update`/`bd list`/`bd ready` bypass it and
   round-trip jsonl per call, concurrent writes lose. Confirm whether beads v1.0.4
   routes non-`dolt` subcommands to the sql-server, and pin it (env/flag/config) so the
   orchestrator's calls hit the server, not an empty embedded DB + jsonl.
2. **Serialize the create+close on the produce-next-stage path** (single beads
   transaction, or hold `createMu` across both) so a successor-create can't export over
   a sibling close. `accept`'s ordering comment already flags the Apply→Close window as
   non-atomic; the jsonl round-trip turns that window into a data-loss race.
3. **Consider making `scheduleReady` also trust the projection for readiness** (or
   reconcile the two), so a lost durable write can't strand a dependent sibling while the
   projection believes its blocker is closed.

### Manual recovery used (to finish the run live, no restart)
The stall was recoverable *without restarting* because the fast dispatch tick (2s) polls
`bd.Ready()` every pass:

1. Point reveal's base at the integrated epic branch (else it'd branch from `main` — no
   `shares` table — and fail its `compiles` postcondition):
   `bd -C <site> update vault-532 --set-metadata base=epic/vault-bpc`
2. Close the reverted lineage **one at a time** (a lone close sticks; batching re-triggers
   the race): `bd -C <site> update vault-bpc  --status closed`, then `vault-qkq`, then
   `vault-0mj`.
3. `bd ready` then returned `vault-532`; the next 2s tick dispatched reveal (test-author,
   Opus 4.8) branching from `epic/vault-bpc`.

Note (2)'s single-at-a-time discipline is itself a workaround for the same race.

### Recurrence in the same run (confirmed the predicted risk)
BUG-1 struck **twice more** on the reveal lineage, exactly as predicted — every
produce-next-stage transition is exposed:
- `vault-532` (reveal test-author) close reverted after producing `implement` (`vault-a3w`).
- `vault-a3w` (reveal implement) close reverted after producing `qa` (`vault-k4z`).

`vault-k4z` (the terminal qa→integrate step) closed and stuck (no concurrent create), and
reveal integrated onto `epic/vault-bpc` (commit `8d2aafd`) — so the **whole feature was on
the epic branch**, yet the run **stalled again**. The highest-impact manifestation is here:
`sweepEpicCompletion` requires *every* issue sharing the epic id to be `closed` in the
store (`internal/orchestrator/epic_completion.go:77`), so the two reverted statuses made
`drained=false` and the **terminal merge to `main` never fired** — a silent stall with the
entire verified feature sitting one merge away from landing. Recovery: lone-close
`vault-532` and `vault-a3w`; the next epic-completion sweep detected the drain and landed
`main` in one commit (`98bb972`), which the push watcher shipped to the public repo →
deploy. **Takeaway: a two-child epic hit the race 5×; only the terminal integrate steps
survived. This is not an edge case — it fires on essentially every stage advance.**

---

## BUG-2 — Terminal merge commit (the public repo's headline commit) has an empty provenance trailer — FIXED

**First seen:** 2026-07-06, same run — terminal commit `98bb972`.

### Symptom
The single machine-authored commit the terminal merge lands on `main` (and pushes to the
public repo) carries an all-`(none)` provenance trailer:
`Soul: (none) | Model: (none) | Tests-Soul: (none) | … | Verified: (none) | Transcript: (none)`.

### Impact
Cosmetic-but-material for the demo's thesis: this is the **first commit an audience sees**
on the public GitHub repo, and it's the one the README says *is* the accountability. An
empty trailer on the headline commit undercuts the "provenance by construction" story.

### Root cause (likely by-design, not a data-loss bug)
The terminal merge maps to the epic **root** issue (`vault-bpc`), which is a *plan* issue —
it has no implementing soul, model, or gate of its own, so there is nothing to stamp. The
real provenance lives on the **child integration commits** (`d8fc022` generate, `8d2aafd`
reveal), which carry full trailers (soul, model, tests-soul, all 5 gate-check evidence
hashes, traceability, transcript, explorer-model) and are ancestors of `main`.

### Fix (T15.4)
`gitMerger.MergeEpic` now synthesizes an **epic-level** trailer on the merge commit instead
of stamping the bare `{Issue, Subject}` the sweep passes. It reads the epic branch itself —
the single source of truth — by walking `main..epic` and parsing each commit's trailer, which
recovers exactly the per-child provenance commits: their issue ids, their integration-commit
hashes, and the deduped union of the gate-check names that verified them. The headline commit
now renders

```
Issue: vault-bpc | Children: <gen>@<hash>,<rev>@<hash> | Verified: build,gosec,govulncheck,license-scan,test
```

— a genuine feature record that omits the inapplicable producer fields (`Soul`/`Model`/…)
rather than printing them as `(none)`. `Issue: <epic> |` is preserved, so the idempotency
grep still no-ops a repeated sweep. Purely additive; the per-child trailers reachable under
the merge's second parent remain the source of truth, and a git fault while aggregating
degrades to the bare layer rather than blocking the landing. New render/parse methods
`Provenance.FeatureTrailer`/`FeatureCommitMessage` + a `Children []string` field
(`internal/core/provenance.go`); aggregation in `internal/orchestrator/merge.go`
`featureProvenance`. Spec sharpened with the exact format ([integration.md](../../specs/integration.md)
"The terminal merge is a merge commit"). Tests: core
`TestFeatureTrailerOmitsProducerFieldsAndRoundTrips`; orchestrator
`TestFeatureProvenanceAggregatesChildren`, `TestFeatureProvenanceDegradesOnGitError`, and the
extended `TestGitMergerEpicTerminalMergeIntegration` (asserts no `(none)`/producer leak, both
children cited, hashes resolve on the epic branch).

---

## BUG-3 — Reported USD (and token totals) are ~5× too high on the OpenRouter path

**First seen:** 2026-07-06 — harness `closing_usd` figures didn't match the OpenRouter
dashboard (harness said the run cost ~$21; OpenRouter showed a fraction of that).

Two independent pricing defects, both **fixed** in this change:

### 3a — Cache double-count in the openai-compat adapter (the big one) — FIXED
**Symptom.** Every `closing_usd` on the OpenRouter path is ~5× the real charge (at 90%
cache), and `TotalTokens` is inflated too.

**Root cause.** OpenAI/OpenRouter report `prompt_tokens` as the **full** prompt count,
*including* the cached subset (`prompt_tokens_details.cached_tokens`). The canonical
`Usage` contract — honored by the Anthropic adapter, `ModelCost.USD`, and
`Usage.TotalTokens` — defines `InputTokens` as the **non-cached** tokens billed at full
rate, with `CacheReadTokens` priced/counted **separately and additively**. The openai
adapter set `InputTokens = prompt_tokens` (`internal/model/openai/openai.go`) **without
subtracting the cached subset**, so cached tokens were billed twice: once at the full
input rate (inside `InputTokens`) and again at the cache-read rate. Its own comment
claimed the divergence "is normalized away by the canonical Usage shape" — but the code
never did the subtraction, and two adapter tests (`TestFromCompletion`,
`TestFromCompletionMapsCacheWrite`) had **baked the bug in** by asserting
`InputTokens == prompt_tokens`.

At 90% cache the overstatement factor is
`(1 + 0.1·f) / ((1−f) + 0.1·f)` with `f=0.9` ≈ **5.3×** — the double-count almost exactly
cancels the prompt-cache discount it was supposed to reward.

**Fix.** `internal/model/openai/openai.go` — `InputTokens = prompt_tokens − cached_tokens`
(clamped ≥0). Cache-WRITE tokens come from a non-schema field reported *alongside*
`prompt_tokens`, not within it, so they are not subtracted. Updated the two tests that
pinned the old behavior and added `TestFromCompletionCacheAccountingNoDoubleCount` as the
regression guard (asserts the non-overlapping split, the `TotalTokens` reconciliation to
`prompt+completion`, and the priced USD). The Anthropic adapter was already correct
(Anthropic's `input_tokens` excludes cached).

**Secondary impact.** `policy.budget` USD enforcement was tripping ~5× early on the
openai-compat path — a run could dead-letter on budget with ~80% of its real dollar
allowance unspent. Token/cache *metrics* were correct; only the USD dimension and
`TotalTokens` were inflated.

### 3b — Sonnet-5 rate miscalibration in the demo config — FIXED
**Symptom.** On top of 3a, the Sonnet tier was over-priced by a further 1.5×.

**Root cause.** `demo/vault/config/infra.dev.yaml` listed
`anthropic/claude-sonnet-5` at `input 3 / output 15` per Mtok; OpenRouter's live price is
**`input 2 / output 10`**. (Audited all four models against openrouter.ai on 2026-07-06:
Opus `5/25`, DeepSeek `0.435/0.87`, Haiku `1/5` all matched; only Sonnet was off.)

**Fix.** Corrected to `input_per_mtok: 2, output_per_mtok: 10`, with cache rates rederived
at Anthropic's standard multipliers on the new base (read 0.1× → `0.2`, write 1.25× →
`2.5`), matching how the other entries are derived.

**Doc follow-up (not done):** `demo/vault/README.md` says DeepSeek is "~7–17× under
Sonnet" — that ratio was computed against the wrong `3/15`; at the corrected `2/10` it's
~5–12×. Update when convenient.

### Corrected cost of this run
With both fixes, the successful-path spend was **~$3.8**, not the ~$21 the harness
reported — reconciling with the OpenRouter dashboard:

| Stage | Model | Harness `closing_usd` | Corrected (3a+3b) |
|---|---|---|---|
| qkq test-author | Opus | $7.10 | $1.34 |
| 0mj implement | Sonnet | $4.80 | $0.56 |
| 532 test-author | Opus | ~$5.28 | $1.08 |
| a3w implement | Sonnet | $2.85 | $0.41 |
| yw0 security | DeepSeek | $0.57 | $0.11 |
| k4z security | DeepSeek | $0.19 | $0.04 |
| bpc planner | Opus | ~$0.75 | $0.30 |
| **Total** | | **~$21.5** | **~$3.8** |

### Open question — epic-mode dependent-sibling base
`vault-532` depended on `vault-qkq` (generate's **author-tests** issue). A closed blocker
satisfies the dependency, so in the intended (non-buggy) flow reveal would have become
ready when `qkq` closed at author-tests time — while generate was still being
*implemented* — and dispatched with `Base` defaulting to `main`
(`internal/orchestrator/schedule.go:133`), which has **no `shares` table** → reveal's
tests can't compile against generate's code. Nothing on the normal produce path repoints
a dependent sibling's base onto the epic branch (`RepointDependents` only runs on
route/conflict paths). Worth confirming whether epic-mode dependent siblings are meant to
base on `epic/<id>`; the manual recovery set it explicitly.

---

## BUG-4 — A silently-hung model stream blocks for minutes (no per-call timeout) — FIXED

**First seen:** 2026-07-06 — the `security` (DeepSeek) stage `vault-yw0` took ~18 min, far
longer than the ~3–4 min producer stages.

### Symptom
A single security invocation stalled for **~11 minutes** mid-run, then resumed. Not slow
per-turn work — a dead stop.

### Root cause
There was **no timeout on a model call**. The openai adapter consumes the streaming
completion with `for stream.Next()` (`internal/model/openai/openai.go`) and blocks on the
next chunk; when the OpenRouter→DeepSeek stream went silent, `Next()` blocked until the
upstream eventually dropped the connection — a **667-second gap ending in
`read: connection reset by peer`** (T14.2 then retried and succeeded in 32s). The only
backstop was the coarse ~18 min invocation wall, so a hung stream could burn most of the
window before anything noticed.

### Evidence
- One gap `13:49:12 → 14:00:19` = **667s**, terminating in a WARN
  `broker: transient model fault … connection reset by peer`.
- The *rest* of `yw0` was normal: **median per-turn latency 5.8s** (vs 5.1s Opus / 4–5s
  Sonnet). Strip the one stall and it was ~7 min — so ~60% of the "18 minutes" was this
  single hang, not model or guidance. (`k4z`, the other DeepSeek stage, was genuinely
  slower per-turn — ~20s/turn, ~1,700 output tokens/turn — but that is the model, not a
  stall, and is a separate latency characteristic, not a bug.)

### Fix
Added a per-model **`idle_timeout`** (`config.ModelProvider.IdleTimeout`), enforced by a
watchdog in the openai adapter's stream loop: a timer reset on every received chunk, so the
bound is **inter-chunk, not a total-call cap** — it kills a silent stall fast without ever
cutting a legitimately long, steadily-streaming high-output turn (e.g. `k4z`'s). On fire it
cancels the stream and returns an **explicit transient fault**, so the existing bounded
retry re-issues it rather than dead-lettering (a naive ctx-cancel would be misread as
terminal). Wired for `openai` + `openai-compat`; validation rejects it on native
`anthropic` (unwired). Set to `idle_timeout: 90s` on all four demo models. Regression tests:
a hung stream → transient fault in ~0.15s; a slow-but-alive stream (total > bound, gaps <
bound) → completes normally. Docs: `docs/configuration.md`, `specs/models.md`.

### Related tuning (not a bug)
The DeepSeek entry originally set **no `effort`**, so the verifier ran at the provider
default. It is now pinned `effort: high` + `effort_param: reasoning` (full deliberation on
the correctness-defining tier, mirroring the Opus roles) — which *increases* its wall-time,
the opposite of a latency fix; flip to `low`/`medium` if the security stage's speed matters
more than its depth.

**Open / unverified:** it is not confirmed that OpenRouter honors `reasoning: {effort}` for
`deepseek/deepseek-v4-pro` — if it doesn't, `effort: high` is a silent no-op. Verify on the
next run by watching whether DeepSeek's output-tokens/turn changes.
