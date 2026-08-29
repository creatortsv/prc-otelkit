# prc-otelkit

Shared observability library for Position Review Copilot: **RED metrics**
(Rate · Errors · Duration) HTTP middleware with the Prometheus `/metrics`
handler, and **distributed tracing** (OTLP → Tempo, W3C `traceparent` over
HTTP and Kafka, `trace_id` in slog logs), used by all seven Go services.

Design rationale: [docs/adr/0001-otelkit-foundations.md](docs/adr/0001-otelkit-foundations.md) (Accepted) —
shared module over per-service duplication, `prometheus/client_golang` as the
ratified dependency, pattern-based route labels, §14 budget buckets.
Tracing design: [docs/adr/0002-otelkit-tracing.md](docs/adr/0002-otelkit-tracing.md) —
OpenTelemetry SDK, OTLP/HTTP export, W3C-only propagation, route-pattern
span naming.
Architecture overview: [docs/architecture.md](docs/architecture.md).
Service-team integration guide: [docs/usage.md](docs/usage.md).

## Metric contract

Published per `prc-docs/operations/observability.md`:

| Metric | Type | Labels |
| --- | --- | --- |
| `http_requests_total` | counter | `route`, `method`, `status` |
| `http_request_duration_seconds_bucket` | histogram (`_bucket`/`_sum`/`_count`) | `route`, `le` |

- `route` is the registered Go 1.22+ ServeMux pattern (`GET /users/{id}`) —
  never the concrete request path; unmatched requests are labeled
  `unmatched`.
- Histogram buckets include the standards §14 budget lines: **0.3** (target)
  and **1.0** (hard limit), so Grafana latency-budget panels read them
  straight from the series.
- Full bucket list (seconds): 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.3, 0.5,
  0.75, 1.0, 1.5, 2.5, 5, 10.

## Packages

| Package | Purpose |
| --- | --- |
| `metrics` | `Middleware` (RED recording around an `http.Handler`) + `Handler` (`/metrics` exposition) |
| `tracing` | `Init` (global tracer provider, OTLP/HTTP → Tempo), `Middleware` (server spans from W3C headers), `NewTransport` (client spans + injection), `Inject`/`Extract` (Kafka header propagation), `TraceID` + `NewLogHandler` (`trace_id` in slog) |

## Requirements

- Go 1.25+ (declared floor in `go.mod`; CI pins the 1.27 toolchain)
- Runtime deps (each with a dependency verdict in an ADR):
  - `github.com/prometheus/client_golang` — standards §13 mandate (ADR-0001)
  - `go.opentelemetry.io/otel`, `go.opentelemetry.io/otel/sdk`,
    `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp`,
    `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp`
    (standards §13 mandate, ADR-0002)

## Quick start

```go
import (
	"github.com/creatortsv/prc-otelkit/metrics"
	"github.com/creatortsv/prc-otelkit/tracing"
)

shutdown, err := tracing.Init(ctx, tracing.Config{ServiceName: "prc-auth"})
defer shutdown(ctx)

mux := http.NewServeMux()
mux.HandleFunc("GET /healthz", healthHandler)
mux.HandleFunc("GET /metrics", metrics.Handler().ServeHTTP)

http.ListenAndServe(addr, tracing.Middleware(metrics.Middleware(mux)))
```

Without an endpoint configured (no `OTLPEndpoint`, no
`OTEL_EXPORTER_OTLP_ENDPOINT`) tracing stays disabled and all helpers
degrade to zero-op — local development needs no Tempo.

## Development

```sh
go build ./...
go test ./... -race            # hermetic
go test ./metrics -bench .     # middleware overhead vs bare mux
gofmt -l . && go vet ./... && golangci-lint run ./... && govulncheck ./...
docker build -f docker/Dockerfile .   # smoke
```

CI (`.github/workflows/ci.yml`) runs the full standards §3 matrix:
`gofmt` · `go vet` · `golangci-lint` · `go test (race)` · `go build` ·
`govulncheck` · `docker build (smoke)`.

## Performance budget

Middleware overhead target < 5 ms p95 added per request (standards §14).
Measured with the in-repo benchmarks: metrics ~0.2 µs/op, 35 B, 2 allocs;
tracing middleware ~0.4 µs/op, 952 B, 13 allocs on an M1 Pro — three to four
orders of magnitude under budget. Re-run benchmarks when touching either hot
path.

## Delivery contract

- Adoption in services is via pinned version (`go get
  github.com/creatortsv/prc-otelkit@vX.Y.Z`), commit `go.mod`/`go.sum`,
  never `replace` directives.
- Breaking API or metric-contract changes ship as a new minor/major tag with
  a migration note in `docs/usage.md`.
