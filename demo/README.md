# Demo: a landing page, end to end

This is the smallest demo that still shows the harness doing its real job: a human
authors a spec, and sandboxed, independently-verified agents turn it into code merged to
`main` — driven by a **hosted model** (`deepseek/deepseek-v4-flash` via OpenRouter), so
all you need is an API key.

The task is intentionally tiny — produce a static `index.html` landing page — but the
pipeline is faithful: one soul writes a **failing** acceptance test from the spec, a
*different* soul makes it pass, the gate proves the red→green transition in a clean
sandbox, and the trusted layer merges. That producer ≠ verifier gate is the whole point;
the demo just shrinks everything around it.

```
spec (you author it)
  └─ author-tests   demo-test-author writes acceptance.sh (greps index.html) — proven RED
       └─ implement   demo-implementor writes index.html — proven RED→GREEN in a clean sandbox
            └─ integrate   orchestrator merges to main (autonomous; no approval step)
```

## What's here

```
demo/
  run.sh                       # turnkey: build → scaffold scratch repo → seed → run
  templates/landing-page.md    # the spec the script seeds
  config/
    harness.yaml               # mini DAG (author-tests → implement → integrate), shell gate
    infra.dev.yaml             # hosted model via OpenRouter; reuses the go-toolchain image
    souls/
      test-author.yaml + prompts/test-author.md    # writes the failing acceptance.sh
      implementor.yaml + prompts/implementor.md     # writes index.html to pass it
```

It's a self-contained config, separate from the harness's own `config/`, so it can't
disturb the real pipeline. The target repo it builds is a throwaway created in a temp
dir — nothing is written into this repository.

## Prerequisites

- **Docker** running (the sandbox backend).
- An **OpenRouter API key** in `OPENAI_API_KEY` (the openai-compat adapter sends it as the
  bearer token). The default model is `deepseek/deepseek-v4-flash`; override with `MODEL=`
  (see below). Any model you pick must support function calling — the agent loop drives the
  model through structured tool calls, and a model that can't do that won't get through a
  single stage.
- **beads** (`bd`) on your `PATH` (or pass `BD=/path/to/bd`).
- Go + `make` (to build the `harness` binary).

The `go-toolchain` sandbox image is built automatically on first run if it's missing
(it's reused here only for its `bash`/`grep`/`git` — no Go runs in the shell gate).

## Run it

```bash
export OPENAI_API_KEY='sk-or-...'   # your OpenRouter key
./demo/run.sh
```

Then open the control room at <http://127.0.0.1:8080> and watch the **Board** and
**Activity** views. When the issue reaches `integrate` it merges to `main` of the scratch
repo (printed at startup). Open that `index.html` in a browser, and
`git -C <scratch> log` shows the provenance trailer tracing the merge back to the issue,
soul, model, and evidence.

### Common overrides

```bash
MODEL='deepseek/deepseek-v4' ./demo/run.sh          # use a different OpenRouter model slug
MODEL_ENDPOINT='https://openrouter.ai/api/v1' ./demo/run.sh
SERVE_ADDR='127.0.0.1:9000' ./demo/run.sh
```

`run.sh` only rewrites the config when you override `MODEL`/`MODEL_ENDPOINT` (into a temp
copy); otherwise it uses `demo/config/` directly. The model name is sent verbatim as the
request's model field, so it must be a valid OpenRouter slug.

## What to expect

- **It costs a little.** The run makes real API calls; with `deepseek/deepseek-v4-flash`
  the whole landing-page run is well under a cent. The dollar cap in
  `config/harness.yaml` (`policy.budget`) and the token/retry caps bound spend; the `cost`
  block in `config/infra.dev.yaml` is what turns recorded tokens into the USD accounting.
- **If it flails immediately** — the agent emits prose instead of tool calls, or no
  candidate branch ever appears — that's almost certainly the tool-calling / streaming
  handshake, not the task. Check the Activity feed for malformed tool calls, and confirm
  your model actually supports function calling. The budget/retry caps mean a flailing run
  terminates safely (it dead-letters) rather than running forever.
- **If a stage dead-letters with a wall-budget reason** on slow hardware, raise
  `limits.wall` in `config/infra.dev.yaml` (per-invocation) and/or `policy.budget.wall`
  in `config/harness.yaml` (cumulative).
- **Dead-lettered work** shows up in the control room's Dead-letter view with the reason.
  The intended fix is to refine the spec (`templates/landing-page.md`) and re-run — the
  harness's one human lever is the spec, never the agent's code.

## Tweaking the demo

- **See the approval gate instead of auto-merge.** Switch `policy.profile` to
  `trusted-dev` in `config/harness.yaml`, add `postcondition: [human-approved]` to the
  `integrate` stage, and run with `--nats-addr 127.0.0.1:4222`; then approve with
  `harness approve --nats nats://127.0.0.1:4222 <issue>` in another shell.
- **Change the page.** Edit `templates/landing-page.md`. Keep the acceptance criteria
  literal (specific elements/strings) so the grep-based gate stays unambiguous.
- **Add a qa stage.** Re-introduce a `qa` stage with `postcondition: [tests-pass]` and a
  `security`-style soul to demonstrate an extra independent re-gate (see the shipped
  `config/harness.yaml` for the shape).

## How it maps to the real config

This demo drops three things the shipped `config/harness.yaml` has, purely to keep the
smallest surface for a first run — none of it changes the architecture:

| Shipped | Demo | Why |
|---|---|---|
| `plan` decomposition stage | _(none — seed enters at `author-tests`)_ | one tiny file needs no decomposition, and planning adds surface for no benefit here |
| `qa` stage + gosec/govulncheck/mutation/license | _(none)_ | Go/security checks are meaningless for static HTML and aren't in the image; the merge-queue re-gate still re-verifies |
| `trusted-dev` profile + `human-approved` + `tcb_paths` | `autonomous`, no TCB globs | the HTML diff touches no TCB path, so it merges hands-free — fewer moving parts for a first run |

Everything else — the zero-network sandbox, the broker-mediated model call, the red→green
proofs, single-writer beads, provenance trailers, budgets as the termination guarantee —
is exactly the real machinery.
