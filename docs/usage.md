# prc-otelkit — Usage for service teams

Two-line integration per service. Assumes Go 1.22+ `ServeMux` route patterns
(all PRC services use them).

## 1. Add the dependency (pinned)

```sh
go get github.com/venomai-ltd/prc-otelkit@v0.1.0
```

Commit `go.mod` and `go.sum`. Never use `replace` directives for shared PRC
modules.

## 2. Wire the middleware and the endpoint

```go
import "github.com/venomai-ltd/prc-otelkit/metrics"

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
- `http_request_duration_seconds_bucket{route,le}` (+ `_sum`, `_count`) —
  histogram per route pattern, buckets include the §14 budget lines 0.3 s
  (target) and 1.0 s (hard limit).
- `route` is your registered pattern (e.g. `GET /users/{id}`); requests that
  match nothing are labeled `unmatched`. Never register catch-all routes like
  `/` if you want meaningful per-endpoint budgets.

## Adding business metrics later (§13)

The default registry is shared. Register your own collectors in your service
package and they are served by the same `metrics.Handler()`:

```go
var reviewsGenerated = promauto.NewCounter(prometheus.CounterOpts{
	Name: "prc_reviews_generated_total",
	Help: "Position reviews generated, by plan tier.",
}, []string{"plan_tier"})
```

Prefix service-specific metric names with `prc_<service>_`.

## Conventions & gotchas

- Keep route patterns specific; two registrations matching the same path
  (e.g. `GET /a` and `GET /{x}`) split traffic across two route labels —
  prefer one explicit pattern per endpoint.
- Handler panics are recorded (with the status written before the panic) and
  then propagate exactly as before — `net/http` still aborts and logs. This
  module adds no recovery middleware; recover-at-the-edge is a separate
  service decision.
- Streaming/SSE handlers work — the recording writer passes `Flush` through.
- Versions pinned in CI: Go 1.27.0, golangci-lint v2.13.2, govulncheck v1.7.0.
- Pod annotations for Prometheus scraping (`prometheus.io/scrape` etc.) are
  DevOps-owned in `prc-infra/k8s/30-services.yaml`; backend only guarantees
  the `/metrics` endpoint.
