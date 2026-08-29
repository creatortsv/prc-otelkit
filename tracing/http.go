package tracing

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Attribute keys follow the OpenTelemetry HTTP semantic-convention stable
// names so traces remain greppable by ecosystem tooling; they are declared
// locally rather than imported from a semconv subpackage whose path changes
// with every SDK release (ADR-0002).
const (
	attrHTTPMethod     = "http.request.method"
	attrHTTPRoute      = "http.route"
	attrHTTPStatusCode = "http.response.status_code"
	attrURLPath        = "url.path"
)

// unmatchedRoute is the span-name route segment for requests that matched no
// registered ServeMux pattern — the same constant semantics as the metrics
// package's `route` label.
const unmatchedRoute = "unmatched"

// Middleware returns HTTP server middleware that continues the incoming W3C
// trace (extracting traceparent from request headers), starts the server
// span around next, and propagates the context into the handler. Wrap the
// service mux — alongside metrics.Middleware — in main:
//
//	http.ListenAndServe(addr, tracing.Middleware(metrics.Middleware(mux)))
//
// The span is named `METHOD route` with route resolved from
// http.Request.Pattern after the mux has matched (e.g. "GET /users/{id}");
// requests matching no pattern are named `METHOD unmatched`. Both match the
// metrics route label, so trace and metric dimensions agree per endpoint.
// The span is marked with the error status code when the response status is
// >= 500; 4xx remain successful spans, mirroring the RED contract where
// error classification is metric-side.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := tracer().Start(ctx, r.Method, trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String(attrHTTPMethod, r.Method),
				attribute.String(attrURLPath, r.URL.Path),
			))
		defer span.End()

		// Derive the traced request up front: the ServeMux records the
		// matched pattern on the request value it receives, so the route is
		// read from this derived request after the handler returns.
		req := r.WithContext(ctx)
		rw := &statusResponseWriter{ResponseWriter: w}
		next.ServeHTTP(rw, req)

		route := req.Pattern
		if route == "" {
			route = r.Method + " " + unmatchedRoute
		}
		span.SetName(route)
		span.SetAttributes(
			attribute.String(attrHTTPRoute, route),
			attribute.Int(attrHTTPStatusCode, rw.statusOrOK()),
		)
		if rw.statusOrOK() >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, http.StatusText(rw.statusOrOK()))
		}
	})
}

// NewTransport wraps an http.RoundTripper so outgoing requests carry a
// client span with the W3C traceparent header injected, continuing the trace
// of the context the request was built with. A nil base falls back to
// http.DefaultTransport:
//
//	client := &http.Client{Transport: tracing.NewTransport(nil)}
//
// Underlying transport configuration (timeouts, dialer tuning) belongs to
// the service: pass its transport here rather than replacing it.
func NewTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return otelhttp.NewTransport(base,
		otelhttp.WithPropagators(propagator),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}))
}

// statusResponseWriter captures the status code written by the wrapped
// handler while passing everything through, including Flusher.
type statusResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.status = code
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// statusOrOK returns the captured status, defaulting to 200 for handlers
// that returned without writing anything (Go answers 200 implicitly).
func (w *statusResponseWriter) statusOrOK() int {
	if !w.wroteHeader {
		return http.StatusOK
	}
	return w.status
}
