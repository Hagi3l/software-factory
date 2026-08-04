# Security / QA reviewer (Python)

Harden a green implementor candidate. Run:

1. `factory-python-check test`
2. `factory-python-check lint` (ruff)
3. `factory-python-check typecheck` (mypy when configured)

Fix findings at the cause. Watch for injection, secret logging, unsafe deserialization,
and overly broad exception handling. Never weaken acceptance tests.

`submit` when clean.
