# Vault demo — OpenObserve assets

Provisioning assets for the `OPENOBSERVE=1` path of [`../run.sh`](../run.sh). Used only when
you want to **see the whole telemetry record land in one backend** — all three OTel signals
the harness exports (traces + logs + metrics), authenticated, on one OTLP/gRPC endpoint.

This exists because the vault demo targets a *security* audience: the point isn't a trace
waterfall (Jaeger already does that) but proving that the **complete, tamper-evident record**
of agent behaviour ships — including the trusted-side `slog` (as trace-correlated OTel logs,
T5.13) and the cost/token/gate metrics (T5.12) — to a real, authenticated backend. Jaeger is
trace-only and refuses metrics/logs; OpenObserve is a single binary that ingests all three.

## `completeness-dashboard.json`

A three-panel OpenObserve dashboard — one panel per signal — POSTed to OO's REST API
(`POST /api/{org}/dashboards`) by `run.sh` after the container is healthy:

| Panel | Signal | Source | Query |
|-------|--------|--------|-------|
| Traces — spans / interval | traces | `default` stream | `count(*)` over `histogram(_timestamp)` |
| Logs — records / interval | logs | `default` stream | `count(*)` over `histogram(_timestamp)` |
| Metrics — invocations | metrics | `harness_invocations` stream | PromQL `harness_invocations` |

The dashboard is **best-effort**: the JSON is pinned to OpenObserve's dashboard **v5** schema,
which evolves with the product. If the auto-POST fails (schema drift against a bumped
`OPENOBSERVE_IMAGE`), `run.sh` logs a note and continues — telemetry still lands, and you can
import this file from the UI (Dashboards → Import) or rebuild the panels by hand. Bump this
file in lockstep with the image tag.

### Why these queries

- **Traces & logs** both land in the `default` stream (stream *type* `traces` / `logs`
  disambiguates them), so a count-over-time per stream is the most schema-stable way to show
  arrival without depending on field names that vary across OTLP mappings.
- **Metrics** are queried via **PromQL**, because OO stores OTLP metrics as Prometheus-named
  streams: OTel's dotted instrument names map to underscores, so `harness.invocations` (see
  `internal/telemetry/conventions.go`) becomes the stream/series `harness_invocations`. Sibling
  series: `harness_cost_usd`, `harness_tokens`, `harness_gate_runs`, `harness_llm_turns`,
  `harness_invocation_duration`, `harness_llm_turn_duration`, `harness_gate_run_duration`.

## Credentials & auth

`run.sh` boots OO with a fixed root user (`ZO_ROOT_USER_EMAIL`/`ZO_ROOT_USER_PASSWORD`) and
derives the ingestion token as `base64(email:password)` — exactly the string OO's own
*Ingestion* page shows. It is exported as `OTEL_OTLP_AUTH` (`Basic <token>`), which the
materialized infra overlay references as `authorization: ${OTEL_OTLP_AUTH}`. The harness
expands it host-side at export time (`config.OTelConfig.ResolveHeaders`), so the credential
lives in the environment, never in config — the same key discipline as the model API key, and
what `harness validate`'s credential-header rule enforces.
