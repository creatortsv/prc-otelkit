# ADR-0001: prc-otelkit foundations — shared RED metrics module, dependency, label design

- Status: Accepted
- Date: 2026-08-30
- Deciders: Backend Engineer (Rivet) per VENA-143; shared-module direction
  ratified by `backend-architect` verdict dated 2026-08-30 (parent VENA-141 /
  VENA-143 issue record); EngManager review via PR gates.

## Context

Standards §13 mandates Prometheus metrics for every service; the §14
performance budget (target 300 ms, hard limit 1 s p95) requires per-endpoint
latency visibility. Infra is done (Prometheus `kubernetes-pods` scrape job
awaits annotations; RED + latency-budget Grafana dashboards provisioned), but
the backend publishes nothing today — the metrics contract blocks the §14
per-endpoint budget view and the first live-load run.

The metric contract (`prc-docs/operations/observability.md`) fixes the
families: `http_requests_total{route,method,status}` counter and
`http_request_duration_seconds_bucket{route,le}` histogram. Duplicating that
implementation in seven services is seven chances to drift from the contract.

## Decision 1 — Shared module, mirroring the ratified prc-eventkit pattern

`prc-otelkit` is a separate shared-kit repo (module
`github.com/creatortsv/prc-otelkit`) with the same layout as `prc-eventkit`:
own AGENTS.md, own CI matrix (standards §3 Go-services row), MIT license,
docs + ADRs, conventional commits, PR-only merges. Services adopt via pinned
`go get` (commit `go.mod`/`go.sum`; never `replace` directives).

## Decision 2 — Dependency: prometheus/client_golang (single, ratified)

| Module | License | Verdict |
| --- | --- | --- |
| `github.com/prometheus/client_golang` v1.24.x (+ transitive `client_model`, `common`, `procfs`, `protobuf`, `x/sys`, `beorn7/perks`, `cespare/xxhash`, `goautoneg`) | Apache-2.0 (procfs: MIT; goautoneg: BSD-3; x/sys: BSD-3) | Ratified for this module: the issue names `promhttp`, and standards §13 mandates Prometheus as the metrics backend |

Not on the §5 allowlist, but §13 is itself a standard mandate — metrics
exposition requires the Prometheus client; hand-rolling the exposition
format and histogram implementation was rejected as unmaintainable. This is
the module's only direct dependency; no others may be added without a
board-approved ADR.

## Decision 3 — `route` label is the ServeMux pattern, never the path

`http.Request.Pattern` (set by Go 1.22+ `ServeMux` during matching, readable
after the wrapped handler returns) is used verbatim as the `route` label —
e.g. `GET /users/{id}`. Raw request paths are unbounded label cardinality
under live load and would blow up Prometheus memory and query cost. Requests
matching no pattern get the constant `unmatched` route label. Uniqueness of
patterns is the service's obligation (one explicit pattern per endpoint,
documented in usage.md).

## Decision 4 — Histogram buckets carry the §14 budget lines

Bucket bounds (seconds): 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, **0.3**, 0.5,
0.75, **1.0**, 1.5, 2.5, 5, 10. The 0.3 (target) and 1.0 (hard limit) lines
exist as buckets so `histogram_quantile` and the provisioned
latency-budget dashboard can read budget adherence directly from the series
without re-recording. Low end (5 ms) resolves fast reads; high end (10 s)
keeps pathological tails visible.

## Decision 5 — Default registry, package-level collectors, two-call API

`metrics.Middleware(next)` wraps a handler; `metrics.Handler()` serves the
default registry. Package-level `promauto` collectors guarantee single
registration per process and keep the integration to two lines in `main`.
Consequence: one RED instance per process (correct for the service model);
tests isolate via distinct route label values.

Recording is deferred: it also covers panicking handlers (status written
before the panic, implicit 200 otherwise), while panic propagation and
connection abort stay exactly `net/http` behavior — no recovery middleware
is introduced here. Response-writer wrapper passes `Flush` through so
streaming handlers keep working.

## Consequences

- All seven services gain `/metrics` with identical label semantics; the
  dashboards can assume the contract.
- Adoption is mechanical (usage.md); per-service PRs stay tiny.
- Overhead budget (< 5 ms p95 per §14) is met by ~3 orders of magnitude;
  guarded by in-repo benchmarks.
- Future tracing work (§13 `otelkit` helper, W3C `traceparent`) belongs to
  this same module family — a `tracing` package can be added here without
  touching the metrics contract.
