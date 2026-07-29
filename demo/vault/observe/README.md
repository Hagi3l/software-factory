# Vault demo — OpenObserve assets

Provisioning assets for the `OPENOBSERVE=1` path of [`../run.sh`](../run.sh). Used only when
you want to **see the whole telemetry record land in one backend** — all three OTel signals
the harness exports (traces + logs + metrics), authenticated, on one OTLP/gRPC endpoint.

This exists because the vault demo targets a *security* audience: the point isn't a trace
waterfall (Jaeger already does that) but proving that the **complete, tamper-evident record**
of agent behaviour ships — including the trusted-side `slog` (as trace-correlated OTel logs) and the cost/token/gate metrics — to a real, authenticated backend. Jaeger is
trace-only and refuses metrics/logs; OpenObserve is a single binary that ingests all three.

## `completeness-dashboard.json`

A four-panel OpenObserve dashboard — one chart per signal, plus a per-record log table —
POSTed to OO's REST API (`POST /api/{org}/dashboards`) by `run.sh` after the container is healthy:

| Panel | Signal | Source | Query |
|-------|--------|--------|-------|
| Traces — spans / interval | traces | `default` stream | `count(*)` over `histogram(_timestamp)` |
| Logs — records / interval | logs | `default` stream | `count(*)` over `histogram(_timestamp)` |
| Metrics — invocations | metrics | `factory_invocations` stream | PromQL `factory_invocations` |
| Pipeline — log records | logs | `default` stream | a `table` panel: `SELECT _timestamp, COALESCE(factory_issue_id, issue) AS issue, COALESCE(factory_issue_role, role) AS role, factory_soul, factory_attempt, body … ORDER BY _timestamp DESC` |

The **Pipeline** table renders every trusted-side `slog` record as columns (issue / role / soul /
attempt / event), newest-first, **unfiltered**. The `COALESCE`s exist because the harness emits
logs under *two* attribute conventions that never co-occur on one record (see
[`pipeline-savedview.json`](#pipeline-savedviewjson) below), so coalescing keeps `issue`/`role`
populated on lifecycle *and* broker rows.

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
  streams: OTel's dotted instrument names map to underscores, so `factory.invocations` (see
  `internal/telemetry/conventions.go`) becomes the stream/series `factory_invocations`. Sibling
  series: `factory_cost_usd`, `factory_tokens`, `factory_gate_runs`, `factory_llm_turns`,
  `factory_invocation_duration`, `factory_llm_turn_duration`, `factory_gate_run_duration`.

## `pipeline-savedview.json`

A Logs **Saved View** named **Pipeline**, POSTed to `POST /api/{org}/savedviews` by `run.sh`. It
opens the Logs explorer (Logs → Saved Views → **Pipeline**) straight into columns — `issue`,
`factory_issue_id`, `role`, `factory_issue_role`, `factory_soul`, `factory_attempt`, `body` —
instead of the raw JSON source, so the pipeline is legible without hand-picking fields. Same
**best-effort** discipline as the dashboard (a failed POST logs a note; add the columns by hand).

**Why two `issue`/`role` columns.** The harness emits logs under *two* attribute conventions: the
orchestrator/agent **lifecycle** logs use plain `slog` keys (`issue`, `role`), while the broker +
instrumented logs use the telemetry-schema keys (`factory_issue_id`, `factory_issue_role`). They
**never co-occur** on one record. The dashboard's `table` panel can `COALESCE` them into one
column; the raw Logs explorer can't, so the saved view carries both and each row populates
whichever its emitter used. (Unifying the lifecycle logs onto the telemetry-schema keys would
collapse these to a single column everywhere and is the cleaner fix — an observability-spec
follow-up, since [`specs/observability.md`](../../../specs/observability.md) calls for *one schema
across all three signals*.)

**Why it's an opaque blob.** A saved view is a snapshot of OpenObserve's *frontend* search state
(query + time range + the `resultGrid` column set), not a documented API shape — so this file was
**captured from a real OO view** and the columns swapped in, rather than authored by hand. A
bumped `OPENOBSERVE_IMAGE` can drift the shape; re-capture if the POST starts failing.

## Credentials & auth

`run.sh` boots OO with a fixed root user (`ZO_ROOT_USER_EMAIL`/`ZO_ROOT_USER_PASSWORD`) and
derives the ingestion token as `base64(email:password)` — exactly the string OO's own
*Ingestion* page shows. It is exported as `OTEL_OTLP_AUTH` (`Basic <token>`), which the
materialized infra overlay references as `authorization: ${OTEL_OTLP_AUTH}`. The harness
expands it host-side at export time (`config.OTelConfig.ResolveHeaders`), so the credential
lives in the environment, never in config — the same key discipline as the model API key, and
what `software-factory validate`'s credential-header rule enforces.
