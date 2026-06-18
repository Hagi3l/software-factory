# Vault demo run — observed issues (run started 2026-06-18 ~12:03)

Feature drafted: **"Add server-side uptime counter to the header"** (`specs/uptime.md`), epic root `vault-u2v`.
Tracking issues to discuss after the run completes (terminal merge → push → deploy).

## 1. Wizard drafted 2 root seed issues in epic mode
- **Symptom:** Approve failed — `epic mode ... must seed exactly one root issue ... got 2`.
- **Status:** User-resolved by asking the wizard to consolidate. Re-approved OK (`issues=1`).
- **Discuss:** Is this expected friction, or should the requirements-planner persona be
  hardened to always seed exactly one root in epic mode? It split at the *seed* level
  instead of leaving the split to decomposition. (`cmd/harness/wizard_seed.go:197`)

## 2. Redundant second planner pass on the epic root (Dolt read-your-writes lag)
- **Symptom:** After `accepted decomposition children=2` (12:20:49), `vault-u2v` was
  re-dispatched to the planner 1s later (12:20:50) and ran a full second pass (~4.5min,
  invocation `fcd6347706ed236a`).
- **Cause:** `acceptPlan` closes the plan issue, but the scheduler's next `bd.ready()`
  poll hadn't observed the close (Dolt server-mode eventual consistency; see
  `inflight.go` / CLAUDE.md).
- **Guardrail exists:** `acceptPlan`/`planHasChildren` idempotency guard (results.go:406-423,
  added for "the corruption observed in the demo run: 4 children for a 2-child feature")
  should close the 2nd pass via `plan already decomposed (children exist); closing without
  re-applying` — no duplicate children, but wasted spend (~$0.30) + confusing card.
- **Discuss:** Should the scheduler consult the in-flight projection (or a recently-closed
  set) before re-dispatching, to avoid the wasted pass entirely? CONFIRM the guard fired.

## 3. EPIC hero shows 0/3 but log says children=2
- **Symptom:** Board epic hero card reads `integrated 0/3` while decomposition logged
  `children=2`.
- **Discuss:** Counting nuance (denominator includes something extra) or did a child slip
  in? Confirm final child count is sane and not climbing.

## 4. Board card status: epic root shows "open"/"queued" while being worked
- **Symptom:** `vault-u2v` planner card showed `open` / `queued Nm` during active planner
  invocations; children correctly showed `in_progress`.
- **Likely related to #2** (lag between claim/close writes and the board's direct
  `bd list --all` read; the board, unlike the orchestrator, has no in-flight projection).
- **Discuss:** Should the board reconcile against the live feed / in-flight projection so a
  card mid-invocation never reads as `open`?

## 2 — UPDATE: guard fired, no corruption ✅
- 12:26:25 `plan already decomposed (children exist); closing without re-applying issue=vault-u2v`.
  The redundant 2nd planner pass closed cleanly via the idempotency guard. Cost: one wasted
  ~5min planner pass (spend). The underlying re-dispatch (lag) is still the root cause to discuss.

## 5. test-author failed on vault-ubk → retried as vault-x90
- 12:31:37 `runner: invocation harvested role=test-author issue=vault-ubk status=failed`
- 12:31:44 `dispatched issue=vault-x90 role=test-author attempt=1`
- **By design:** `route` (results.go:678) mints a NEW issue (attempt+1, copied title/body/spec)
  and closes the original. Not a bug.
- **Discuss:** WHY did the author-tests stage fail on vault-ubk ("Wire uptime into server:
  startTime, shell component, handler calls, httptest")? Pull the failure reason / gate
  evidence at the end — recurring author-tests failures would point at the test-author soul
  or the spec slice for that child.

## 6. Retry shows as a same-titled "duplicate" card (UX)
- A failed+retried task appears as two same-titled cards in the column (closed original +
  live retry, different ids). Reads as a duplicate to the operator.
- **Discuss:** Board could collapse attempts of one lineage (group by retry chain / title),
  or visually mark the closed-failed card as superseded.

## 3 — NOTE on the 0/3 count
- The 0/3 was visible at ~12:24, BEFORE the vault-x90 retry (12:31) existed. So the retry
  doesn't explain it. Candidates: epic summary denominator counts the epic root, or the first
  decomposition created an extra issue. Still to confirm at end.

## 5 — UPDATE: vault-ubk was a RUNAWAY, not a quiet fail
- 12:31:35 `agent: turn budget exhausted, stopping issue=vault-ubk max_turns=50`
- usage: input_tokens=**1,367,520**, cumulative chain spent **$0.82** before failing.
- The 50-turn cap (termination guarantee) worked. But the test-author soul got STUCK looping
  on the "Wire uptime into server" child. Risk: retry vault-x90 hits the same wall → could
  burn the epic budget on one child.
- **Discuss:** why does test-author loop on this child? Spec slice too big / ambiguous? The
  child bundles 4 things (startTime, shell component, handler calls, httptest) — maybe the
  planner's split is too coarse for one author-tests pass.

## 7. epicSummaries counts ALL closed issues as "integrated" (BUG)
- `query.go:176`: `if i.Status == statusClosed { s.Integrated++ }`, Total = all epic issues.
- Wrongly counts: (a) the closed **epic root** vault-u2v (closed after decomposition, never
  integrated); (b) **failure-routed** children (vault-ubk closed by `route()` on failure,
  superseded by vault-x90). Neither is an integration.
- Observed: hero shows **integrated 1/4** while ACTUAL integrated = 0. The "1" is the closed
  root. Will misread 2/4 once the board observes vault-ubk's close.
- **Fix direction:** count only issues that reached `integrate`/landed on the epic branch
  (status closed AND produced-through-integrate), not any closed status; exclude the root and
  failure-closed issues. Subsumes/clarifies issue #3 (the 0/3 → 1/4 count).

## 8. Control room beads reads killed under load → stale board (BUG/perf)
- 12:30:32 `ERROR controlroom: invocation read failed ... bd show issue vault-hfr: signal: killed`.
- The board's direct `bd list/show` is timing out / being killed despite warm server mode
  (README claims warm Dolt prevents this). The runaway vault-ubk + heavy polling overwhelmed it.
- Effect: board shows CLOSED vault-ubk as still `in_progress (12m35s)` — the "duplicate" the
  operator saw. Stale by ~2min, not the <1s read-your-writes lag.
- **Discuss:** (a) does control room need the orchestrator's in-flight projection / event
  feed instead of polling beads? (b) is warm Dolt actually keeping up, or is the runaway the
  trigger? (c) `signal: killed` = bd timeout? raise it / cache reads?

## ✅ SUCCESS STORY (for the demo narrative) — one child went end-to-end
- The `vault-hfr` lineage completed the FULL DAG and integrated onto `epic/vault-u2v`:
  spec(88300fd) → red tests vault-hfr(6275872) → implement vault-llf(cc24b86) → qa
  vault-80d (full **5-check** gate: tests-pass + golangci-lint + gosec + govulncheck +
  license-scan, ALL passed) → integrate merge 37fafff.
- The integrate step even **re-gated on `integration/vault-80d`** (5 checks again) before
  landing — the merge-queue re-verification. Merge commit carries full provenance
  (soul=vault-security, model, prompt_sha, 5 verified-check content hashes).
- **`main` never moved** (still at seed 00ecf64) — epic atomicity is CORRECT.
- This is the thesis working: producer≠verifier, red→green proven, independent security
  re-audit, provenance trailer, atomic epic. Lead with this in the talk.

## 5 — FINAL UPDATE: the retry ALSO nearly ran away (over-bundled child confirmed)
- vault-x90 (retry) completed at 12:50:28 but burned **1.35M input tokens, 39 turns**
  (cap is 50 — squeaked through). vault-ubk attempt burned 1.37M/50-cap.
- BOTH attempts of the same child churned ~1.35M tokens → **~$1.6 total on one child**.
- This is SYSTEMATIC, not a fluke: the child "Wire uptime into server: startTime, shell
  component, handler calls, httptest" is too coarse for one author-tests pass. The planner's
  split is the root cause. After x90 finally passed, it produced vault-76z (implement),
  still in flight when the run was stopped.

## 9. Log says "merged to main" for an epic child (MISLEADING LOG — not a merge bug)
- `results.go:637` logs `orchestrator: merged to main` when a child integrates — but in epic
  mode it merges onto `epic/<id>`, NOT main (verified: 37fafff is on epic/vault-u2v; main is
  still at seed). The actual terminal-merge-to-main path is separate (epic_completion.go:122
  `epic landed atomically`).
- **Impact:** operator/logs misread it as main advancing per-child, contradicting the whole
  epic story. The message should name the real target (epic branch vs main).

## 8 — CONFIRMED: 3× control-room beads reads killed under load
- `signal: killed` at 12:25:04 (budgets), 12:30:32 (status + invocation). The board's direct
  `bd list/show` is being killed (timeout/OOM) under concurrent load despite warm Dolt.

## Run outcome (stopped manually to save cost ~12:50+)
- Integrated onto epic branch: 1 of 2 real children (vault-hfr lineage). Other child
  (vault-ubk→x90→vault-76z) was in implement when stopped. Terminal merge to main never
  reached (epic didn't drain). Spend ~$2 (board showed $1.02 at 12:33, pre vault-x90's 1.35M).

---
(append new issues below as the run proceeds)
