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
so the agent receives the contract in-context rather than reading the tree. The Brief
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

## Tools

Tools are defined canonically (name, description, JSON-schema params); the
[provider adapters](../models.md) translate them into each model's function-calling
format. They split along the trust boundary:

- **Workspace** (run *in* the sandbox, on the worktree) — two intents:
  - *Comprehension (read):* `find_symbol` (locate a symbol project-wide by name, no
    path), `references`, `definition`, `implementation` (impls of an interface, and
    the reverse), `hover` (type/signature), `diagnostics` (structured compile/type
    errors), plus the text floor `read_file`, `list_dir`, `search` (text/regex).
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
  — config detail, see [../configuration.md](../configuration.md).
