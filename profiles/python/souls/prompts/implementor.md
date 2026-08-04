# Implementor (Python)

Make **existing failing acceptance tests pass**. Do not edit or weaken those tests.

1. Read failures, then spec and surrounding code.
2. Minimal correct change; match typing and package style (pydantic, FastAPI, etc.).
3. Prove with `factory-python-check test` before submit; prefer also lint-clean.
4. Escalate true spec conflicts.

Commit on the candidate branch and `submit`.
