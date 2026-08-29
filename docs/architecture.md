# prc-otelkit — Architecture

Shared observability kit for the seven PRC Go services, two packages:

- `metrics` — HTTP RED instrumentation. Two exported symbols only —
  `metrics.Middleware` and `metrics.Handler` — everything else is
  package-private.
- `tracing` — distributed tracing: global tracer-provider init (OTLP/HTTP →
  Tempo), HTTP server/client instrumentation, Kafka header propagation, and
  `trace_id`-enriched slog logging.

## Tracing data flow

```
incoming request
  │ traceparent extract (propagation.TraceContext)
  ▼
tracing.Middleware ── span START (kind server, name = method)
      │ derives req = r.WithContext(traced ctx)
      ▼
metrics.Middleware → ServeMux → handler
      │                             │
      │      req.Pattern set by ServeMux on the derived request
      ◄──────────────────────────────┘
span: SetName(route or METHOD unmatched), route/status attrs,
      error status for status ≥ 500, End

outgoing request → tracing.NewTransport (otelhttp) → client span + traceparent injected
kafka produce → Inject(ctx) → []Header (traceparent) appended to record
kafka consume → Extract(ctx, headers) → consumer span links the trace

Init: resource(service.name, version, env) + ParentBased sampler
      + BatchSpanProcessor (non-blocking) → OTLP/HTTP (otlptracehttp)
logs: NewLogHandler wraps slog.Handler → adds trace_id from ctx
```

Key mechanics:

- **No endpoint, no tracing.** With no `OTLPEndpoint` and no
  `OTEL_EXPORTER_OTLP_ENDPOINT`, `Init` never installs a provider; the
  global no-op provider makes every span and helper a zero-op. The global
  provider is resolved per use (late wiring is safe).
- **Route-pattern span naming.** `ServeMux` records the matched pattern on
  the request value it receives, so the middleware reads `Pattern` from the
  derived request after the handler returns and renames the span — agreeing
  with the metrics `route` label. Unmatched → `METHOD unmatched`.
- **Kafka carrier has no Kafka dependency.** `tracing.Header` mirrors the
  wire header shape; prc-eventkit maps `kgo.RecordHeader` onto it. This
  keeps the dependency one-way: eventkit → otelkit, never the reverse.
- **W3C TraceContext only.** One propagation format across HTTP and Kafka;
  baggage intentionally absent until a contract exists.
- **Export.** Batch span processor (default 5 s / 512 batch, bounded queue,
  drop on overflow) → OTLP/HTTP exporter to the endpoint from
  `Config.OTLPEndpoint` or the standard env var; URL parsing follows the
  OTel spec (`http://` = insecure, default signal path `/v1/traces`).

## Metrics data flow

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

The tracing counterpart (span naming, `trace_id` log key, W3C headers) is
owned by the same observability contract and ADR-0002; the tracing tests
assert continuation across HTTP and Kafka headers and real OTLP HTTP export
against an in-process `httptest` server.

## Concurrency & performance

- Collector children are mutex-protected inside client_golang; recording
  adds ~0.2 µs/op and 2 allocations per request (see `BenchmarkMiddleware` vs
  `BenchmarkBareMux`) — the standards §14 middleware budget is < 5 ms p95.
- The only per-request work is: one `time.Now()` pair, one map-free label
  lookup, one counter increment, one histogram observation.
- Tracing middleware adds ~0.4 µs/op (13 allocs) over the bare mux on the
  untraced fast path (no-op provider); traced spans record into the batch
  processor's bounded queue — export never runs on the request goroutine.

## Testing strategy

All tests are hermetic (stdlib `httptest` + registry gathers over
`client_model` protos + in-memory span exporters): pattern-vs-path collapse,
unmatched fallback, status capture (201/implicit-200/500/204 + method
dimension), panic recording, bucket-set equality with the contract, and
`/metrics` payload shape. Tracing tests cover W3C continuation (server and
client), Kafka header round-trip, malformed-header tolerance, log
enrichment, no-op degradation without an endpoint, and a real OTLP export
flush against a local `httptest` endpoint — still fully hermetic. Benchmarks
guard the overhead budget; CI runs them, but only `-race` correctness gates.

## Versioning

Semantic versioning; services pin exact tags (`go get ...@vX.Y.Z`). The
metric contract is stable within v0 while the walking skeleton exists;
breaking changes bump the major once services consume real versions.
