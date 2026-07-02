# Agent

An ephemeral, sandboxed process that performs exactly one work item, then dies. It
has a [Soul](#soul-vs-agent) (identity) and runs an agentic loop. It is **untrusted**.

See also: [runner.md](runner.md), [sandbox.md](sandbox.md),
[../verification.md](../verification.md), [../configuration.md](../configuration.md).

---

## Soul vs. Agent

Two separate concerns:

- A **Soul** is *identity* — declarative config (see
  [../configuration.md](../configuration.md)). It is **stateless**: no cross-task
  memory. All durable state lives in beads, git, and specs. A soul is reconstituted
  fresh on every invocation.
- An **Agent** is *behaviour* — the code that runs a soul against one work item.

```go
type Soul struct {
    Name    string   // e.g. "implementor-go"
    Role    string   // which DAG stage it serves, e.g. "implementor"
    Model   string   // e.g. "claude-opus-4-7"
    Persona string   // path to a markdown prompt file
    Tools   []string // capability names
    Sandbox string   // sandbox profile name
}

type Agent interface {
    Soul() Soul
    Handle(ctx context.Context, brief Brief) (Result, error)
}
```

Souls *fulfil* roles. A role may map to a set of souls, chosen per issue by a
`selector` (see [../configuration.md](../configuration.md)). Concurrency is
orthogonal: many invocations of the *same* soul run in parallel across runners;
you scale by adding runners, not souls.

---

## The Brief

The task envelope handed *into* the sandbox. Because the agent is stateless and
sandboxed, **the Brief is its entire knowledge of the world.**

```
Brief:
  issue:    { id, title, body, role }          # the work item
  spec:     resolved spec slice                 # referenced file + linked
                                                #   neighbours, bounded depth
  base:     git ref to branch from
  criteria: postconditions this node must satisfy
  soul:     { name, model, persona, tools }
```

The **spec slice** is bounded: the referenced spec file plus its linked neighbours
to a configured depth — *not* the whole `specs/` tree, which would blow context
and dilute focus. The orchestrator resolves it from the issue's structured spec
reference (a repo-relative path threaded forward across the epic) and embeds it here,
so the agent receives the contract in-context rather than reading the tree. When the
project declares [`ambient_specs`](../configuration.md), those files (conventionally the
spec index and the conventions doc) are **prepended** to the slice — the same for every
issue, so the agent always carries the project's map and conventions, not just one issue's
contract (see [specs-process.md](../specs-process.md#ambient-specs)). The Brief
also pins the slice's **content hash** (stored on the issue), recording the exact spec
version the work was derived from for drift detection. See
[../specs-process.md](../specs-process.md).

---

## The Result (output)

What the agent returns *out of* the sandbox. **Everything here is a proposal**;
the orchestrator validates and applies (see
[orchestrator.md](orchestrator.md)).

```
Result:
  status:   done | failed | needs-spec-clarification
  branch:   candidate branch ref + commits
  evidence: gate outputs, logs, prompt-sha     # large items by hash → artifact store
  proposes: [ child issues, with role + dependency edges ]   # emergent breadth
```

`done` means *candidate ready*, **not** accepted. Acceptance is a separate trusted
step gated independently. `needs-spec-clarification` routes to the human re-entry
loop — the agent has detected ambiguity and is **escalating, not guessing**.

**Correlation is stamped by the trusted layer, not self-reported.** A `failed` or
escalated Result carries no candidate branch, so the only reliable link back to the
work item is the issue id — and trusting a sandboxed agent to address its own work
would be a trust-boundary hole. The [runner](runner.md) stamps the issue id onto
the Result at harvest, from the trusted dispatch; the orchestrator correlates on
that, and a Result it cannot correlate is treated as poison and dropped.

---

## The inner loop

A single invocation runs a Go-native agentic loop that calls model APIs **directly**
— never by shelling out to an existing agentic CLI. The loop speaks the
[canonical model interface](../models.md), so any tool-calling model can drive it:

```
boot soul (persona, model, tools); build context from Brief
loop (bounded by budget / turns):
    request  := { system: soul.persona, messages, tools }
    response := model.generate(request)        # relayed via broker (below)
    if response has no tool calls and signals done → submit
    for each tool call: execute → tool result
    append response + tool results to messages
    enforce budget from response.usage → escalate/stop if exceeded
```

The model call is **relayed through the broker**: the agent emits a *canonical*
request to its runner, which holds the keys, selects the provider adapter for
`soul.model`, calls the API, and relays a canonical response back. The agent is
**provider-unaware** — no key, no knowledge of which provider answered. See
[../models.md](../models.md) and [runner.md](runner.md).

### Tool-result aging

The loop re-sends the whole conversation every turn, and old tool results dominate
a deep run's input. [Prompt caching](../models.md) makes that history cache-*cheap*
but not *smaller* or *cleaner* — and a `read_file` from before the soul edited that
file is **actively misleading** (stale context the model can quote confidently). So
the loop ages the history: once the conversation grows past a threshold, the
*model's view* of old tool-result content is replaced by a short deterministic stub
(`[read_file {"path":"foo.go"} — elided (round N); re-run the tool if needed]`).
This is safe *in this architecture specifically* because the worktree is durable
state inside the sandbox: every read is repeatable, so an elided result loses
nothing the model cannot recover with one call — elision cuts stale-context
hallucination risk, not just tokens.

The rules:

1. **Only tool-result content ages.** The Brief (the opening turn) and every
   assistant message — the soul's own plan and reasoning trail, which the
   running-plan persona convention depends on — are never touched. A tool result's
   error flag and identity survive; only its bulk content is stubbed.
2. **Batch cadence, not a sliding horizon.** The elision boundary always keeps the
   most recent K rounds intact and advances in batches of B rounds. A sliding
   horizon would re-edit the history every turn and invalidate the cached prefix
   every turn; batching keeps the view byte-stable between advances, paying one
   cache re-write per batch instead ([prompt caching](../models.md)).
3. **Small results are exempt.** A result under ~1 KiB keeps its content forever —
   the stub would cost as much as the content, and tiny results
   (`diagnostics: clean`, a lifecycle ack) are load-bearing signal.
4. **The aged view is derived, never stored.** It is computed as a pure function of
   the pristine message history each time a request is built; the loop's own
   history stays complete. Deterministic elision only — **no LLM summarization**
   (that would add a model call, a failure mode, and non-determinism in the cache
   prefix; a stub with re-read recovery is self-correcting).
5. **Evidence is unaffected in substance.** The transcript is recorded relay-side
   from the wire ([runner.md](runner.md)), so it faithfully shows what the model
   saw; every elided result's *full* content still appears at its first appearance
   (the turn that carried it as the new tail), and the prompt artifact / Prompt-SHA
   (the first request) never contains tool results. Replay renders each message at
   first appearance, so the rendered trail shows the full content.

The [explore](#explore--distilled-comprehension) sub-loop does **not** age — it is
bounded at a dozen turns on a cheap model, below any threshold worth managing.
Elision is observable: the loop counts results elided and bytes saved
([observability.md](../observability.md)).

## Tools

Tools are defined canonically (name, description, JSON-schema params); the
[provider adapters](../models.md) translate them into each model's function-calling
format. They split along the trust boundary:

- **Workspace** (run *in* the sandbox, on the worktree) — two intents:
  - *Comprehension (read):* `find_symbol` (locate a symbol project-wide by name, no
    path), `references`, `definition`, `implementation` (impls of an interface, and
    the reverse), `hover` (type/signature), `diagnostics` (structured compile/type
    errors), plus the text floor `read_file`, `list_dir`, `search` (text/regex). Each
    answers *one* question; for a broad, multi-step one there is `explore` — a distilled
    comprehension sub-loop (see [Explore](#explore--distilled-comprehension) below).
  - *Transformation (write):* `rename` (semantic, project-wide), `code_action` (apply
    the server's own fix — organise imports, add import, quickfix, extract), plus the
    text floor `edit_file`, `write_file`, `run` (build/test/lint/fmt).
  - *Verification (self-check):* `run_tests` (the project's tests) and `run_gate` (the
    full [check registry](../configuration.md) pre-`submit`) return the gate's parsed
    **[findings](../verification.md)** — `file:line` + message, raw output kept as
    evidence — instead of a raw dump. The infra parses the tool output, so even a weak
    model gets a focused result; the self-check is feedback, never a grade (see
    [verification.md](../verification.md) "Producer self-checks").
- **Lifecycle** (control the invocation, produce the Result): `submit` (candidate
  ready), `submit_plan` (a decomposition is ready — ends a planning task with the
  proposed children and **no** candidate branch), `escalate` (raise
  `needs-spec-clarification`), `request_subtask` (propose a child issue — emergent
  breadth; a child may name a sibling proposed in the same task via a local `key` to
  express an ordering edge, and may carry selector `tags` that pick which soul fulfills
  the child's role — see [configuration.md](../configuration.md)). The tools are
  universal; a soul's persona decides which it
  uses (only the planner calls `submit_plan`, only the test author calls `trace_test`).
- **Brokered I/O** is mostly implicit: package fetch is `run` reaching the vetted
  mirror through the broker's proxy; git push is performed by the runner on
  `submit`, not an agent tool. The explicit network-tool surface stays near-zero —
  exactly what the [security model](../security.md) wants.

A role's behaviour comes from its **persona + which tools are enabled**, not from a
different loop: planner, test-author, implementor, and security agents all run the
same loop.

### Semantic tools (LSP-backed)

The comprehension and transformation tools are **intent-first**: the agent states
*what* it wants (find this symbol, rename this), and the trusted tool layer picks
*how* — never the untrusted agent. Each resolves **LSP-first** against a language
server, with text as a floor. This makes "prefer semantic, fall back to grep/sed" a
structural property, not a persona nudge — the agent cannot pick the wrong mechanism
because it never picks the mechanism.

- **Language-neutral surface, per-language backing.** The tool contract is identical
  across languages; only the backing server differs — `gopls` in a `go-toolchain`
  sandbox, `tsserver` in `ts-toolchain`. The server lives in the profile image, reached
  over a fixed launch convention (see [sandbox.md](sandbox.md#per-language-language-server)).
  This is the same canonical-interface / thin-adapter split the [model layer](../models.md)
  uses — *provider adapter : model :: language server : semantic tool* — so the canonical
  tool list stays small and stable while language specificity lives in the image.
- **Reads degrade silently, writes degrade loudly.** A read (`references`,
  `find_symbol`) with no available server falls back to grep, results **labelled
  unverified**. A write must not degrade silently: a `rename` that quietly became a
  `sed` would corrupt string literals, comments, and substrings undetected, so on
  missing semantic support it **refuses, or text-renames with an explicit precision
  warning** (match count, files, comment/string hits).
- **Lazy session, kept in sync.** The server starts on the first semantic call and
  lives for the rest of the invocation; `edit_file`/`write_file` notify it of every
  change so `diagnostics`/`references` never read stale text. It is torn down with the
  sandbox.
- **Mechanism is recorded.** Whether a transformation ran semantically or via the text
  floor is stamped into the Result's evidence — provenance the gate and traceability
  map can weigh (a text-fallback rename warrants more suspicion than a semantic one).

### Explore — distilled comprehension

The comprehension tools above each answer *one* question. `explore` answers a **broad,
multi-step** one: the agent states a free-form question — *"where and how is per-soul
model selection resolved, and what touches provenance?"* — and gets back a compact
distilled answer instead of running the iterative search→read→refine itself. Its purpose
is context and cost. The intermediate reading — which would bloat the agent's window and
burn its (frontier) tokens — happens in a **nested loop on a cheap model**, and only the
residue returns. It is the intent-first move one level up: *provider adapter : model ::
language server : semantic tool :: explore loop : comprehension question* — the agent says
what it wants to understand, the trusted layer decides how to find out.

- **A tool, not a DAG citizen.** The implementation is a bounded child agent loop run
  **in-process inside the agent binary**: its read tools hit the parent's already-warm
  [LSP session](#semantic-tools-lsp-backed) in the same sandbox (no reseed, no cold
  server), its model calls broker out over the same channel, and it returns its answer up
  to the parent as the tool result — never leaving the sandbox. Reads never cross the trust
  boundary; only model calls do, exactly as always.
- **Free-form question in, structured answer out.** `explore` returns
  `{summary, anchors:[{path,line,why}], coverage: complete|partial-budget|partial-uncertain,
  leads:[…]}`. **Anchors are always required** — every claim is grounded in a `file:line`
  the parent can re-read, so the distiller is a *pointer generator*, not a source of truth.
  This neutralizes hallucination on the action path: **distill for navigation, precise-read
  for action** — the parent re-reads the anchor at full fidelity before editing against it
  (anchors are pointers only; snippets are not auto-inlined, which would re-inflate the
  context the tool exists to save). `coverage` tells the parent how far to trust a partial
  answer; `leads` lets it re-ask narrower rather than re-explore blind. The child terminates
  by calling its one lifecycle tool, `answer(...)`, or on its budget — the same two-rail
  termination the main loop has.
- **Reuses a [Soul](#soul-vs-agent), off the DAG.** The explorer is configured like any
  soul (a cheap `model`, an explore `persona`, the read-only allowlist) and runs the same
  loop — but it is **invoked as a tool by the runner** in response to the parent's call,
  synchronously, *not* scheduled by the orchestrator as a DAG node ("helper soul"; the
  reserved `explorer` role, see [../configuration.md](../configuration.md)). Its mini-Brief
  is the *question* + the worktree + the [ambient specs](../configuration.md) — and **not**
  the parent's conversation. Handing it the parent's context would defeat the point; handing
  it only the question would starve it of the project map.

Five rules keep a nested loop from reintroducing the problems a sandbox exists to contain:

1. **Read-only toolset, no exceptions** — only the comprehension subset above; no
   `edit`/`write`/`run`/`submit`/`escalate`/`request_subtask`. This single rule preserves
   single-writer (it can't touch beads), producer≠verifier (it can't produce a candidate),
   and stateless souls (it writes nothing, dies with the parent) at once.
2. **No recursion** — the explorer's allowlist omits `explore` itself, structurally (not a
   runtime guard). This is the fan-out backstop that keeps *budgets = termination* intact.
3. **Fixed sub-budget, under the parent-task ceiling** — the runner meters the explorer's
   own model stream against a fixed cap (`policy.explore_budget`,
   [../configuration.md](../configuration.md)); a breach ends the explore with a
   `partial-budget` answer, never the parent task.
4. **Model pinned by the trusted dispatch, not the agent** — the parent may *call* explore
   but cannot *choose what it runs on*; the runner enforces the configured explorer soul
   regardless of what the sandbox asks for (see [../models.md](../models.md),
   [../messaging.md](../messaging.md)). Otherwise an untrusted agent could tag a request to
   escape its tier.
5. **The explore transcript is first-class evidence** — hashed to the
   [artifact store](artifact-store.md) alongside the main transcript, the pinned model
   recorded, so *provenance by construction* holds: the exploration is auditable, not a
   hidden side-channel.

**Additive, never load-bearing.** The parent keeps every raw comprehension tool; `explore`
is a pure accelerant. A `partial-uncertain` answer, or an explorer error, just routes the
parent back to searching itself — a soul can even run with `explore` disabled at no cost to
correctness. And unlike compaction it does **not** fight [prompt caching](../models.md): the
parent's cacheable prefix is untouched — an explore call is one tool-call/result appended at
the tail. Its blast radius if the cheap model is itself hostile is nil beyond quality: the
explorer is read-only and its output is a set of *checkable* anchors handed to an
already-untrusted, independently-gated parent, so a lying explorer costs a rejected
candidate downstream, not a bad merge — it opens no new trust hole.

---

## Rules an agent must obey

- **Escalate, never invent intent.** On spec ambiguity/contradiction, return
  `needs-spec-clarification`. Do not silently make a product decision. See
  [../specs-process.md](../specs-process.md).
- **Never self-certify.** Producing green tests is not acceptance; the independent
  gate decides. See [../verification.md](../verification.md).
- **Never assume network.** All external access is via the broker; if it's not on
  the allowlist, it does not exist.
- **Cite the spec** for non-obvious decisions, to feed the traceability map.

---

## OPEN questions

- Per-role tool *enablement* defaults (which souls get `run`, network access, etc.)
  — config detail, see [../configuration.md](../configuration.md). `explore` is part of
  this surface: planner and implementor are the obvious beneficiaries (broad
  localization). Whether the **verify path** (qa/security) gets it is a per-deployment
  config call, and a *shared, same-family* explorer there is acceptable — explore is
  additive and never load-bearing, so the decision is recorded in
  [../verification.md](../verification.md); reading raw remains the stricter option.
