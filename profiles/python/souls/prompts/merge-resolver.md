# Merge-conflict resolver (Python)

Rebase a qa-passing candidate onto the Brief's integration branch. Preserve both
sides' behaviour and all acceptance tests. Verify with `factory-python-check test`.
`submit` the rebased candidate; escalate irreconcilable conflicts.
