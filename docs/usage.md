# prc-otelkit — Usage for service teams

Two-line integration per service. Assumes Go 1.22+ `ServeMux` route patterns
(all PRC services use them).

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

## 3. Sentry error capture (v0.2.0+)

Two lines per service, plus one call inside the existing panic recoverer.
Bootstrap in `main`:

```go
import "github.com/creatortsv/prc-otelkit/sentry"

enabled, shutdown, err := sentry.Init(sentry.Config{
	DSN:         os.Getenv("SENTRY_DSN"),
	Environment: os.Getenv("SENTRY_ENVIRONMENT"),
	Release:     os.Getenv("SENTRY_RELEASE"), // deployed image tag
	ServiceName: "prc-auth",
})
if err != nil {
	logger.Error("sentry init failed", "err", err)
}
defer shutdown(context.Background())
```

An empty `SENTRY_DSN` disables the SDK — every helper becomes a no-op, so
the same wiring runs in every environment. The DSN arrives from a cluster
Secret (`sentry-dsns`), never from git; `SENTRY_RELEASE` is set by the
manifests to the image tag.

Capture inside the service recoverer (after you have recovered and before
writing the 500):

```go
if rec := recover(); rec != nil {
	sentry.CapturePanic(rec, r) // r contributes method+path only
	// ... existing 500 handling
}
```

`errors` surfaced outside a recoverer can use `sentry.CaptureError(err, r)`.

### Privacy contract (verified by test)

`sentry/sentry_test.go` pins the contract by asserting on the serialized
envelope sent to a fake ingestion endpoint:

- `SendDefaultPII` is hardwired `false` and not configurable.
- Request context reported is **method + path only** — no query string, no
  body, no cookies, no headers.
- Client-level `BeforeSend` additionally filters any request data through a
  header allowlist (`Accept`, `Content-Type`, `Content-Length`,
  `Accept-Encoding`, `Referer`, `User-Agent`) and clears user IP, so data
  attached by other code paths cannot leak.
- Sensitive breadcrumbs (categories `http`, `request`, `response`, `query`)
  are dropped before send.

Never call `sentry-go` directly from services and never set
`send_default_pii` — route all capture through this package.
