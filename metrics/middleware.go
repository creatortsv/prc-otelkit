// Package metrics provides RED (Rate–Errors–Duration) HTTP instrumentation
// for Position Review Copilot services: a Prometheus middleware recording
// request rate and duration per route, plus the /metrics handler that serves
// the default registry.
//
// Metric contract (prc-docs/operations/observability.md):
//
//	http_requests_total{route,method,status}          — counter
//	http_request_duration_seconds_bucket{route,le}    — histogram
//
// The route label is the registered Go 1.22+ ServeMux pattern
// (http.Request.Pattern, e.g. "GET /users/{id}"), never the concrete request
// path — raw paths are unbounded label cardinality under live load. Requests
// the mux cannot match to any pattern are labeled "unmatched".
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// unmatchedRoute labels requests that matched no registered pattern
// (typically 404s). A constant keeps the route label cardinality bounded.
const unmatchedRoute = "unmatched"

// requestDurationBuckets are the histogram upper bounds, in seconds. The
// standards §14 budget lines — 0.3 (target) and 1.0 (hard limit) — are
// explicit buckets so the latency-budget dashboard reads them straight from
// the series. Low end resolves fast reads; high end keeps tail outliers
// visible.
var requestDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.3, 0.5, 0.75, 1.0, 1.5, 2.5, 5, 10}

var (
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total HTTP requests handled, by route pattern, method and status code.",
	}, []string{"route", "method", "status"})

	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request duration in seconds, by route pattern.",
		Buckets: requestDurationBuckets,
	}, []string{"route"})
)

// Middleware returns HTTP middleware recording RED metrics for every request
// passing through next. Wrap the service mux with it:
//
//	http.ListenAndServe(addr, metrics.Middleware(newMux()))
//
// The route label is resolved from http.Request.Pattern, which the ServeMux
// sets while matching; it is therefore read after next.ServeHTTP returns.
// Recording is deferred, so it also covers handlers that panic — net/http
// still aborts the connection and logs the panic exactly as it would without
// the middleware; no recovery is added here. A handler that returns without
// writing anything is recorded with the implicit 200.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &recordingResponseWriter{ResponseWriter: w}
		start := time.Now()
		defer func() {
			route := r.Pattern
			if route == "" {
				route = unmatchedRoute
			}
			requestsTotal.WithLabelValues(route, r.Method, strconv.Itoa(rw.statusOrOK())).Inc()
			requestDuration.WithLabelValues(route).Observe(time.Since(start).Seconds())
		}()
		next.ServeHTTP(rw, r)
	})
}

// recordingResponseWriter captures the status code written by the wrapped
// handler while passing everything through to the real writer.
type recordingResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *recordingResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.status = code
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *recordingResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Flush passes through for handlers that stream, keeping the wrapper
// transparent for http.Flusher consumers.
func (w *recordingResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// statusOrOK returns the captured status, defaulting to 200 for handlers
// that returned without writing anything (Go answers 200 implicitly).
func (w *recordingResponseWriter) statusOrOK() int {
	if !w.wroteHeader {
		return http.StatusOK
	}
	return w.status
}
