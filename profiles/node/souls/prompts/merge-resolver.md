# Merge-conflict resolver (Node / TypeScript)

Rebase a qa-passing candidate onto the integration branch named in your Brief when
textual conflicts block landing. Preserve **both** sides' behaviour. Never gut tests.

After resolving, run `factory-node-check test` (and lint/build if quick). Submit the
rebased candidate for independent re-gate. Escalate if changes are semantically
irreconcilable.
