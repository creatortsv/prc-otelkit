# ADR-0002: Distributed tracing dependencies (OpenTelemetry SDK, OTLP/HTTP exporter, otelhttp)

- Status: Proposed
- Date: 2026-08-30
- Deciders: Backend Engineer (Forge, VENA-151); ratification via PR dual-gate
  review (code-reviewer + security) with Backend Team Lead verdict. The
  requirement itself is pre-decided by standards §13 and the VENA-151 issue
  text ("OTLP export to Tempo, W3C traceparent over HTTP and Kafka").

## Context

Standards §13 mandates distributed tracing for every service: OpenTelemetry →
Tempo, W3C `traceparent` mandatory in both HTTP headers and Kafka message
headers, with a shared internal `otelkit` helper. Infra ships Tempo with OTLP
ingest (gRPC 4317 / HTTP 4318) and injects
`OTEL_EXPORTER_OTLP_ENDPOINT=http://tempo:4318` into all seven service
deployments (prc-infra `30-services.yaml`). The Loki contract derives a
`trace_id` field from service log lines and links it to Tempo, which fixes
the slog attribute key.

The Go SDK ships none of this: propagation, resource/sampler/exporter
wiring, and HTTP/Kafka carriers must come from the OpenTelemetry Go
modules. Hand-rolling the W3C encoding, OTLP protobuf wire format, and
batching processor was rejected as unmaintainable — the same reasoning that
ratified `prometheus/client_golang` in ADR-0001.

## Decision 1 — Dependencies (direct)

| Module | License | Purpose |
| --- | --- | --- |
| `go.opentelemetry.io/otel` | Apache-2.0 | Trace API, W3C `TraceContext` propagator |
| `go.opentelemetry.io/otel/sdk` | Apache-2.0 | TracerProvider, ParentBased sampling, BatchSpanProcessor, resource |
| `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp` | Apache-2.0 | OTLP-over-HTTP span export (local protocol per infra contract) |
| `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` | Apache-2.0 | Client-transport instrumentation (span + traceparent injection) |

Transitive additions come from these (notably `google.golang.org/grpc` and
`genproto` via the OTLP data model, already reached transitively through
`prometheus/client_golang`'s protobuf stack). All Apache-2.0/BSD/MIT.

Deliberately **not** included:

- **OTLP gRPC exporter** — the infra contract states OTLP HTTP is the local
  protocol and the collector tier also exposes HTTP ingest; gRPC export
  would add the full `google.golang.org/grpc` runtime as a direct dependency
  for zero deployment need. Revisit via follow-up ADR only if a protocol
  requirement appears.
- SDK manual instrumentation beyond this package — services consume the
  helpers here, not raw SDK surface.

## Decision 2 — SDK wiring: global provider, env-driven, no-op by default

`tracing.Init(ctx, Config)` installs the global `TracerProvider`
(`otel.SetTracerProvider`) with the service resource (`service.name` +
optional `service.version`, `deployment.environment.name`), a
`ParentBased(TraceIDRatioBased(ratio))` sampler, and a batch span processor
(default 5 s delay, bounded queue, drop-on-overflow — export never blocks
request handling, satisfying the §14 p95 budget).

No endpoint configured (`Config.OTLPEndpoint` empty **and**
`OTEL_EXPORTER_OTLP_ENDPOINT` unset) leaves the global provider untouched:
spans are created but never recorded, export never happens, and local
development works without Tempo. This is also why every helper in the
package is safe to call before/unwithout `Init`.

## Decision 3 — Propagation surface: W3C TraceContext only

One format everywhere: `traceparent`/`tracestate` via
`propagation.TraceContext`. Baggage is deliberately not propagated until a
cross-service baggage contract exists (consumers ignore unknown headers, so
adding it later is backward-compatible). The Kafka carrier
(`tracing.Inject` / `tracing.Extract`) works over a dependency-free
`Header{Key string; Value []byte}` mirror of the Kafka header shape, keeping
this package importable without a Kafka client; prc-eventkit maps
`kgo.RecordHeader` onto it (one-way dependency, no import cycle).

## Decision 4 — HTTP server middleware is otelkit-owned; client path uses otelhttp

The server span must be named after the matched route
(`http.Request.Pattern`, e.g. `GET /users/{id}`) to agree with the metrics
`route` label — but the pattern is only known after `ServeMux` matching,
while `otelhttp`'s server middleware names the span before the handler runs.
`tracing.Middleware` therefore starts the span itself (extract via W3C
propagator, span kind server, method/path attributes, error status for
responses ≥ 500) and renames it to the pattern post-handler, reading the
pattern off the derived request (`ServeMux` records it on the request value
it receives). `unmatched` mirrors the metrics constant.

Client instrumentation uses `otelhttp.NewTransport` (injection + client
span + HTTP client-facet attributes) with the same propagator — otelhttp
remains the baseline transport instrumentation per §13. Semantic-convention
attribute keys are declared as local constants (`http.request.method`,
`http.route`, `http.response.status_code`, `url.path`) instead of importing
a `semconv` subpackage whose import path changes with every SDK release;
values follow the current stable names and are revisited on adoption.

## Decision 5 — Logs carry `trace_id` via a slog handler wrapper

`tracing.NewLogHandler` wraps any `slog.Handler` and injects a `trace_id`
attribute from the record's context. The key string is a cross-repo
contract (Loki `derived_fields` → Tempo link in observability.md); it is
exported as `tracing.TraceIDKey` and must not be renamed.

## Consequences

- All seven services share one tracing wiring: `Init` in `main`, two
  middleware wraps, one transport wrap, one logger wrap — no per-service
  instrumentation drift.
- New direct dependencies (table above) supersede ADR-0001's
  "single dependency" statement for this module; the metrics contract is
  untouched (`tracing` imports nothing from `metrics`).
- Export is batched and non-blocking; dropped spans on overflow are
  acceptable by design (observability over durability).
- Tempo/collector-level tail sampling and PII scrubbing remain infra-side
  (stage/prod collector tier); this package does no attribute scrubbing.
- Overhead budget §14: middleware adds ~0.4 µs/op over the bare mux
  (in-repo benchmark), four orders of magnitude below the 5 ms p95 budget.
- Live Tempo verification (single trace across gateway → service → Kafka →
  consumer; Loki `trace_id` → trace link) is tracked in VENA-151 and
  requires the local cluster — the helper ships with hermetic unit tests
  asserting propagation semantics and real OTLP HTTP export.
