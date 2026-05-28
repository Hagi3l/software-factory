# Control Room

The web UI. It is the human's entire window into the factory and their **only**
place to act. Two faces: a read-only observability surface, and the interactive
**wizard** through which humans author and refine intent.

See also: [observability.md](observability.md),
[specs-process.md](specs-process.md), [workflow.md](workflow.md),
[security.md](security.md).

---

## Stack

A server-driven hypermedia app over the Go backend — no SPA, no heavy client JS.

| Concern | Choice |
|---------|--------|
| Interactions | **htmx** (server-rendered HTML, swaps over the wire) |
| Local interactivity | **Alpine.js** (toggles, filters, small client state) |
| Templating | **templ** (typed Go components; compiled via `templ generate`) |
| CSS | **Tailwind** via the **standalone CLI** (single static binary, no Node) |
| Assets | **`embed.FS`** — htmx, Alpine, and compiled CSS embedded into the Go binary |
| Live updates | **SSE** (htmx SSE extension), fed from NATS |

The result is a single self-contained binary: no runtime asset serving, no
external toolchain at deploy time. The Tailwind CLI and `templ generate` are part
of the build (`go generate`), not the runtime.

---

## The views

| View | Purpose | Source |
|------|---------|--------|
| **Board** | kanban over beads issues by stage; live | beads + NATS (SSE) |
| **DAG** | the [issue dependency graph](glossary.md#issue-dependency-graph); blockers, merge order | beads → server-side SVG |
| **Activity feed** | what agents are doing right now | NATS events (SSE) |
| **Issue / invocation detail** | Brief, transcript, candidate diff, gate evidence, budget, retries | beads + [artifact store](components/artifact-store.md) + [trace](observability.md) |
| **Dead-letter queue** | escalations needing a human — *the action surface* | beads + artifact store |
| **Budgets** | token/$/wall-clock burn vs. caps, per epic/issue | beads + OTel metrics |
| **Provenance** | trace any merged commit back to issue→soul→model→prompt→evidence | git + artifact store |

### Rendering
- **Live:** NATS → SSE → htmx swaps; the board and feed update without refresh.
- **Historical/forensic:** plain server-rendered pages from the stores, with the
  structured timeline from the OTel trace backend. Supports **replay** of an
  invocation's decision trail (see [observability.md](observability.md)).
- **Graph viz:** render the DAG **server-side to SVG** (Go → DOT/Graphviz or d2)
  and embed it; hover/click-to-drill via Alpine + htmx on the SVG nodes. No
  client-side graph library.

---

## The wizard — the only human-in-the-loop surface

Launched from **"Create Task"** on the board. It is *not* a form and *not* an
open-ended chat — it is a **steered conversation with a live alignment ledger**:
the [requirements stage](workflow.md) realised with the trusted (non-sandboxed)
requirements planner driving toward aligned, testable intent, exactly like a
collaborative design discussion that converges and *then* authors specs. Its job is
threefold:

1. **Elicit testable intent.** Because [specs are pure prose](specs-process.md) and
   the acceptance criteria are the human's only correctness lever, the wizard
   actively probes for examples, edge cases, what-to-reject, and out-of-scope —
   converging on crisp criteria rather than wandering.
2. **Author and maintain `specs/`.** Output is markdown in the spec tree — new
   files, cross-links, and the README index kept consistent. The wizard owns spec
   link-integrity, not just issue creation.
3. **Gate on explicit human approval.** The human reviews the drafted spec + the
   seed issues and approves *before* anything is written. **That approval is the
   consent boundary** — everything past it is autonomous.

Data flow:

```
human ⇄ requirements planner (LLM, trusted, NOT sandboxed; streams over SSE)
      → drafts spec markdown + proposed seed issues
human → APPROVE
      → spec committed to git;  seed issues created via the orchestrator's
        single-writer path (validated, never written directly)
```

The planner runs no untrusted code (it converses and writes specs/issues), so it
is correctly outside the sandbox. Its conversation is itself an LLM interaction and
is therefore observable/replayable like any other.

---

## The alignment ledger

Alongside the conversation, a live **ledger** shows where you are — a lightly
structured list the planner maintains and you steer:

- **Forks become chips.** When the planner surfaces a decision, it renders the
  options as selectable chips (with the tradeoff); click one and it moves to
  *agreed*, or type a nuance and the planner folds it in. Freeform typing is always
  available.
- **Each item is agreed or open**, with a one-line rationale. The ledger is the
  shared "where are we" view a plain chat lacks — a *working aid*, not a durable
  object model.

**What gets stored is deliberately minimal.** The specs are the source of truth;
the ledger and conversation are *provenance*:

- the **conversation transcript** → the [artifact store](components/artifact-store.md),
  linked from the seed epic (replayable, the "why");
- the **finalized decisions** → a simple markdown sidecar in git (a short bulleted
  list, one line of rationale each, per epic/spec area).

Git history of that sidecar *is* the decision-evolution log — there is no
status/supersession machinery. Changing your mind later just means re-running the
wizard and editing the spec (and the decisions file along with it). Spec drift is
handled where it always is, by [spec-version pinning](specs-process.md) — not by a
parallel decision-recompile path.

---

## Create and Resolve are the same component

The [human re-entry invariant](specs-process.md) says stuck work is resolved by
*refining the spec* — which is the same operation as creating one. So **"Create
Task" and "Resolve" (from the DLQ) are one wizard, two entry modes:**

- **Create** → blank; elicit new intent.
- **Resolve** → pre-loaded with the escalation (`needs-spec-clarification`), the
  relevant [spec slice](glossary.md#spec-slice), and the agent transcript that
  raised it; the human edits the spec to resolve the ambiguity.

On the Resolve path the wizard shows the **spec diff and its blast radius** before
commit — "this change re-pins and reissues these 3 in-flight items" (the
recompile-the-delta mechanism in [specs-process.md](specs-process.md)) — so the
consequence of an edit is visible at the moment of consent.

This is the whole human interface: **guided spec authoring**, whether starting
fresh or unsticking dead-lettered work.

---

## OPEN questions

- **Decomposition preview:** dry-run the planner's breakdown ("this becomes ~5
  implement tasks") inside the wizard before approval. (Leaning defer.) — *under
  discussion.*
- **In-session ledger shape:** plain agreed/open checklist vs. the lightly-structured
  items described above (just enough to bind chips/confirm). Currently specified as
  the latter. — *minor, under discussion.*
- Auth / who may operate the control room — TBD.
