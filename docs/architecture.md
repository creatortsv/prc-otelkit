# prc-otelkit — Architecture

Single-purpose module: HTTP RED instrumentation for the seven PRC Go
services. Two exported symbols only — `metrics.Middleware` and
`metrics.Handler` — everything else is package-private.

## Data flow

```
request → Middleware(recordingResponseWriter) → ServeMux → handler
                 │                                              │
                 │        status captured on WriteHeader/Write  │
                 ◄──────── after next.ServeHTTP returns ─────────┘
                 │
                 ├─ requestsTotal.WithLabelValues(route, method, status).Inc()
                 └─ requestDuration.WithLabelValues(route).Observe(dt)

/metrics → promhttp.Handler() → default registry → exposition format
```

Key mechanics:

- **Route label.** `http.Request.Pattern` is read *after* the wrapped mux
  returns — `ServeMux` sets it during matching, so an outer middleware sees
  the registered pattern (`"GET /users/{id}"`, method prefix included). When
  nothing matched (`Pattern == ""`, typically 404) the constant `unmatched`
  keeps cardinality bounded. No component of the label is derived from the
  concrete request path.
- **Status capture.** `recordingResponseWriter` intercepts `WriteHeader` and
  the first `Write` (implicit 200), passes everything else through,
  including `http.Flusher` for streaming handlers. It never swallows handler
  panics — recording is deferred, `net/http` abort behavior is untouched.
- **Registry.** Collectors register once per process on the default
  registry via `promauto`; `Handler()` returns `promhttp.Handler()` serving
  the same registry. Double registration is impossible because the
  collectors are package-level variables.

## Metric contract

Owned by `prc-docs/operations/observability.md`; the label sets and bucket
bounds are asserted by tests (`TestMiddlewareRouteLabelIsServeMuxPattern`,
`TestMiddlewareDurationHistogramBucketsMatchContract`), so drift from the
contract fails CI rather than surfacing in Grafana.

## Concurrency & performance

- Collector children are mutex-protected inside client_golang; recording
  adds ~0.2 µs/op and 2 allocations per request (see `BenchmarkMiddleware` vs
  `BenchmarkBareMux`) — the standards §14 middleware budget is < 5 ms p95.
- The only per-request work is: one `time.Now()` pair, one map-free label
  lookup, one counter increment, one histogram observation.

## Testing strategy

All tests are hermetic (stdlib `httptest` + registry gathers over
`client_model` protos): pattern-vs-path collapse, unmatched fallback, status
capture (201/implicit-200/500/204 + method dimension), panic recording,
bucket-set equality with the contract, and `/metrics` payload shape. Benchmarks
guard the overhead budget; CI runs them, but only `-race` correctness gates.

## Versioning

Semantic versioning; services pin exact tags (`go get ...@vX.Y.Z`). The
metric contract is stable within v0 while the walking skeleton exists;
breaking changes bump the major once services consume real versions.
