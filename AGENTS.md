# AGENTS.md — prc-otelkit

Shared Go library for Position Review Copilot observability: RED metrics
middleware (`metrics`) with the Prometheus `/metrics` handler, and
distributed tracing (`tracing`: OTLP → Tempo, W3C traceparent over HTTP and
Kafka, `trace_id` in slog logs). Module:
`github.com/creatortsv/prc-otelkit`. Do not rename the module path
without lead sign-off — all seven Go services depend on this API.

## Standing mandates (apply to every agent working in this repo)

1. **Research with codebase-memory MCP first.** Query the knowledge graph
   (`search_graph`, `trace_path`, `get_code_snippet`) for structure and impact
   before reading files manually or writing code.
2. **Route specialized work through specialist subagents:** architecture →
   `software-architect`; security → security subagents; DB →
   `database-optimizer`; code review → `code-reviewer`.

## Conventions

- Stdlib-first. Ratified third-party dependencies (any new one needs a
  board-approved ADR):
  - `github.com/prometheus/client_golang` — mandated by standards §13
    (Prometheus metrics), ratified in
    [ADR-0001](docs/adr/0001-otelkit-foundations.md).
  - `go.opentelemetry.io/otel` (+ `otel/sdk`, OTLP/HTTP exporter,
    `otelhttp` contrib) — mandated by standards §13 (traces → Tempo, W3C
    traceparent), ratified in
    [ADR-0002](docs/adr/0002-otelkit-tracing.md). No direct gRPC dependency:
    OTLP/HTTP is the only export protocol until an ADR adds another.
- English identifiers, comments, commits. Conventional commits.
- Tests: stdlib `testing`, table-driven; fully hermetic (no external services
  — `go test ./...` must stay green offline).
- The `route` metric label is the registered ServeMux pattern
  (`http.Request.Pattern`), never the concrete request path — cardinality
  explosion under live load. Do not loosen this in code or tests.
- Histogram buckets must keep the standards §14 budget lines observable:
  0.3 (target) and 1.0 (hard limit) are explicit buckets.

## Run / test

```sh
go build ./...
go test ./... -race
go test ./metrics ./tracing -bench .   # middleware overhead vs bare mux
```

CI matrix and status-check names: `.github/workflows/ci.yml`.
Docs: [README.md](README.md) · [docs/architecture.md](docs/architecture.md) ·
[docs/usage.md](docs/usage.md) ·
[ADR-0001](docs/adr/0001-otelkit-foundations.md) ·
[ADR-0002](docs/adr/0002-otelkit-tracing.md).
