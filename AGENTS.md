# AGENTS.md — prc-otelkit

Shared Go library for Position Review Copilot observability: RED metrics
middleware (`metrics`) and the Prometheus `/metrics` handler it serves.
Module: `github.com/creatortsv/prc-otelkit`. Do not rename the module path
without lead sign-off — all seven Go services depend on this API.

## Standing mandates (apply to every agent working in this repo)

1. **Research with codebase-memory MCP first.** Query the knowledge graph
   (`search_graph`, `trace_path`, `get_code_snippet`) for structure and impact
   before reading files manually or writing code.
2. **Route specialized work through specialist subagents:** architecture →
   `software-architect`; security → security subagents; DB →
   `database-optimizer`; code review → `code-reviewer`.

## Conventions

- Stdlib-first. The single third-party dependency is
  `github.com/prometheus/client_golang` — mandated by standards §13
  (Prometheus metrics) and ratified for this module in
  [ADR-0001](docs/adr/0001-otelkit-foundations.md). Any new dependency needs a
  board-approved ADR.
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
go test ./metrics -bench .      # middleware overhead vs bare mux
```

CI matrix and status-check names: `.github/workflows/ci.yml`.
Docs: [README.md](README.md) · [docs/architecture.md](docs/architecture.md) ·
[docs/usage.md](docs/usage.md) · [docs/adr/0001-otelkit-foundations.md](docs/adr/0001-otelkit-foundations.md).
