# prc-otelkit — Usage for service teams

Two-line integration per service. Assumes Go 1.22+ `ServeMux` route patterns
(all PRC services use them). Part 1 covers metrics, part 2 tracing.

## 1. Add the dependency (pinned)

```sh
go get github.com/creatortsv/prc-otelkit@v0.1.1
```

Commit `go.mod` and `go.sum`. Never use `replace` directives for shared PRC
modules.

## 2. Wire the middleware and the endpoint

```go
import "github.com/creatortsv/prc-otelkit/metrics"

func newMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthHandler)
	mux.HandleFunc("GET /metrics", metrics.Handler().ServeHTTP)
	return metrics.Middleware(mux)
}

func main() {
	// ... unchanged
	http.ListenAndServe(addr, newMux())
}
```

That is the whole integration: RED metrics recording around every route plus
the `/metrics` endpoint exposing the default registry.

## What you get

- `http_requests_total{route,method,status}` — counter per route pattern.
  `method` is normalized: standard method tokens pass through verbatim,
  anything else (arbitrary tokens are accepted by `net/http`) is collapsed
  into `"other"` so the label cardinality stays bounded.
- `http_request_duration_seconds_bucket{route,le}` (+ `_sum`, `_count`) —
  histogram per route pattern, buckets include the §14 budget lines 0.3 s
  (target) and 1.0 s (hard limit).
- `route` is your registered pattern (e.g. `GET /users/{id}`); requests that
  match nothing are labeled `unmatched` — this also absorbs method-mismatch
  405s and redirects, which the mux answers without matching a pattern.
  Never register catch-all routes like `/` if you want meaningful
  per-endpoint budgets.

## Adding business metrics later (§13)

The default registry is shared. Register your own collectors in your service
package and they are served by the same `metrics.Handler()`:

```go
var reviewsGenerated = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "prc_reviews_generated_total",
	Help: "Position reviews generated, by plan tier.",
}, []string{"plan_tier"})
```

Prefix service-specific metric names with `prc_<service>_`.

## Conventions & gotchas

- Keep route patterns specific; two registrations matching the same path
  (e.g. `GET /a` and `GET /{x}`) split traffic across two route labels —
  prefer one explicit pattern per endpoint.
- Handler panics are recorded as status `500` (a panicking request is an
  error, even when no status was written — recording the implicit 200 would
  undercount the error-rate panel) and then propagate exactly as before —
  `net/http` still aborts and logs. This module adds no recovery middleware;
  recover-at-the-edge is a separate service decision.
- Streaming/SSE handlers work — the recording writer passes `Flush` through,
  and `Hijack`/`Unwrap` reach the underlying writer (WebSocket upgrades and
  `http.ResponseController` keep working).
- Versions pinned in CI: Go 1.27.0, golangci-lint v2.13.2, govulncheck v1.7.0.
- Pod annotations for Prometheus scraping (`prometheus.io/scrape` etc.) are
  DevOps-owned in `prc-infra/k8s/30-services.yaml`; backend only guarantees
  the `/metrics` endpoint.

# 2. Tracing (OTLP → Tempo, W3C traceparent)

## Init once in main

```go
import "github.com/creatortsv/prc-otelkit/tracing"

func main() {
	ctx := context.Background()
	shutdown, err := tracing.Init(ctx, tracing.Config{
		ServiceName: "prc-auth",        // required — Tempo service.name
		Environment: os.Getenv("ENV"),  // optional resource attribute
		// OTLPEndpoint falls back to OTEL_EXPORTER_OTLP_ENDPOINT,
		// which prc-infra injects into every deployment. Empty on both
		// sides disables tracing entirely (local dev without Tempo).
	})
	if err != nil { log.Fatal(err) }
	defer shutdown(ctx) // flushes pending spans on exit
	// ...
}
```

The export path is batched and non-blocking (default batch processor:
5 s delay, bounded queue, drop on overflow) — spans never block requests.
Sampling defaults to "everything"; set `SampleRatio` (0, 1] to thin out
root spans — children follow their parent.

## Wrap mux, client, and logger

```go
// server spans: continue incoming traceparent, span named "METHOD route"
handler := tracing.Middleware(metrics.Middleware(mux))

// client spans: inject traceparent into outgoing requests
client := &http.Client{Transport: tracing.NewTransport(nil)} // or wrap your own transport

// logs: every record under a traced context carries trace_id (Loki link)
logger := slog.New(tracing.NewLogHandler(slog.NewJSONHandler(os.Stdout, nil)))
```

Order matters: `tracing.Middleware` outermost so the trace context wraps the
whole RED recording. The `trace_id` key is a cross-repo contract (Loki
derived field → Tempo link); never set it manually — `NewLogHandler` is its
only writer.

## Kafka via prc-eventkit

Producers inject and consumers extract automatically once the service's
prc-eventkit version includes propagation (producer adds W3C headers to the
record, consumer extracts them around the handler) — no code needed in
services. For header plumbing outside prc-eventkit:

```go
headers := tracing.Inject(ctx)        // nil when the ctx is untraced
ctx = tracing.Extract(ctx, headers)   // continue the trace on the other side
```

## What you get in Tempo

- One trace per user request: gateway → service spans linked by W3C
  `traceparent`, continuing through Kafka into consumers via message
  headers.
- Spans named `GET /users/{id}`-style, agreeing with the metrics `route`
  label; unmatched requests named `METHOD unmatched`.
- Server spans marked with error status when the response status is ≥ 500.
- `service.name` from `Config.ServiceName` (+ optional version/environment
  attributes); sampling via `Config.SampleRatio` (ParentBased).

## Gotchas

- Call `defer shutdown(ctx)` — without it the last batch of spans may be
  lost on exit.
- Always close response bodies of clients wrapped with
  `tracing.NewTransport`: the client span ends when the body is closed (an
  `otelhttp` contract), and an unclosed body leaks an open span.
- 4xx responses are not span errors (matches the RED contract where error
  classification is metric-side); 5xx are.
- The endpoint URL is parsed like the OTel spec's env var: `http://` means
  insecure plaintext to Tempo/collector; `https` uses TLS.

