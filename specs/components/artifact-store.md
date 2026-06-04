# Artifact Store

A durable, content-addressed home for the *large* evidence an invocation produces:
full agent transcripts, gate outputs, mutation reports, diffs. It exists because
[sandboxes are ephemeral](sandbox.md) — anything worth inspecting after an agent
dies must be saved before teardown.

See also: [runner.md](runner.md), [../observability.md](../observability.md),
[../security.md](../security.md), [../verification.md](../verification.md).

---

## Why it's separate from beads and git

[beads](orchestrator.md) holds work *state*; git holds *code* + provenance
trailers. Neither is the right home for multi-megabyte transcripts and reports.
Inlining them would bloat the work graph and the commit history. So large evidence
lives here, and beads / the provenance trailer reference it **by hash**.

This is the piece that makes [provenance](../security.md) and
[replay](../observability.md) actually work: the `Prompt-SHA` and evidence hashes
in a commit trailer are pointers *into* this store.

---

## What it holds

- Full agent transcripts (the replayable decision trail).
- Gate evidence: test output, red→green proof, mutation reports, scanner findings.
- The **gate verdict** (kind `gate-verdict`): the assembled, per-check result of one
  gate run — pass/fail, red→green base/candidate, mutation score vs. threshold,
  scanner exits — recorded for every run, pass or fail, so the
  [verification view](../control-room.md) can render the trust argument forensically.
  The bulky per-check *output* it references stays the separate gate-evidence entries
  above; this record is the index over them. See [verification.md](../verification.md).
- Candidate diffs and rejected attempts.
- The [test↔spec traceability map](../verification.md).
- Requirements-[wizard](../control-room.md) conversation transcripts (the "why"
  behind a spec; the [finalized decisions](../specs-process.md) themselves live in
  git, not here).

Content is **content-addressed** (referenced by hash), which gives deduplication
and tamper-evidence for free: a provenance record that cites a hash cannot be
silently altered.

---

## Harvest before teardown

The [runner](runner.md) is responsible for writing artifacts to the store **before
it destroys the sandbox**. The harvest step is part of the invocation lifecycle:

```
agent terminates → runner harvests branch + Result + transcript + gate evidence
                 → writes large artifacts to the store (content-addressed)
                 → Result/beads reference them by hash
                 → destroy sandbox
```

Miss this window and the evidence is gone with the sandbox.

---

## Pluggable backend

Like the [sandbox](sandbox.md), the backend is an interface chosen by config:

```yaml
artifacts:
  backend: files          # files | s3
  path: ./.harness/artifacts        # files backend
  # backend: s3                      # s3/minio backend:
  # bucket: harness-artifacts        #   the shared object bucket (must already exist)
  # endpoint: minio.internal:9000    #   MinIO/non-AWS host[:port] (http:// = plaintext dev)
  # region: us-east-1                #   required when endpoint is empty (derives the AWS endpoint)
```

- **files** — content-addressed local files. Simplest; fits the single-binary,
  single-host dev story.
- **s3 / minio** — for distributed deployments where runners on many hosts and the
  control room must share storage. Speaks plain S3, so it serves AWS S3 and any
  S3-compatible service (MinIO) identically. Objects use the same sharded
  content-address layout (`sha256/<ab>/<rest>`) the files backend does, so a hash means
  the same thing on either backend. Credentials come from the environment, never config
  (the same posture as model API keys); the bucket is an operator prerequisite — the
  backend reads and writes it but never creates it.

Same pluggability principle as the sandbox: dev runs local, production runs
distributed, no code change.

---

## OPEN questions

- **Retention tiers** — how long to keep full transcripts vs. compact evidence;
  likely policy in `harness.yaml` alongside budgets.
- Whether artifacts should be **signed** as part of provenance — see
  [../security.md](../security.md).
- Garbage collection of artifacts for dead-lettered/abandoned work — TBD.
