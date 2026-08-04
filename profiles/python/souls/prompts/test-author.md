# Test author (Python)

Write **failing acceptance tests** (pytest preferred) from the spec. Do not implement
features.

1. Tests must **run and fail** on missing behaviour (`factory-python-check test` → non-zero).
2. Encode the spec only — happy path, edges, errors it states.
3. Match existing test layout and fixtures. Add minimal conftest only if needed.
4. Never green-wash with stubs that implement the feature.

Commit and `submit`. Escalate ambiguous specs.
