package tracing

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// captureExporter is a hermetic SpanExporter collecting finished spans in
// memory, used as a synchronous exporter in tests (no network, no batching).
type captureExporter struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (c *captureExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spans = append(c.spans, spans...)
	return nil
}

func (c *captureExporter) Shutdown(context.Context) error { return nil }

func (c *captureExporter) recorded() []sdktrace.ReadOnlySpan {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]sdktrace.ReadOnlySpan(nil), c.spans...)
}

// installCapture sets the global provider to a synchronous capturing one and
// registers cleanup restoring a no-op provider. Tests using it must not be
// parallel.
func installCapture(t *testing.T) *captureExporter {
	t.Helper()
	exp := &captureExporter{}
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	return exp
}

func TestInitRequiresServiceName(t *testing.T) {
	_, err := Init(context.Background(), Config{})
	if err == nil || !strings.Contains(err.Error(), "ServiceName") {
		t.Fatalf("Init() error = %v, want ServiceName required", err)
	}
}

func TestInitDisabledWithoutEndpoint(t *testing.T) {
	t.Setenv(envEndpoint, "")
	shutdown, err := Init(context.Background(), Config{ServiceName: "prc-test"})
	if err != nil {
		t.Fatalf("Init() error = %v, want nil", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v, want nil", err)
	}

	ctx := context.Background()
	_, span := tracer().Start(ctx, "never-recorded")
	if TraceID(ctx) != "" || span.SpanContext().IsValid() {
		t.Fatalf("no-op provider must not produce valid span contexts, got %s", span.SpanContext().TraceID())
	}
	span.End()
	if got := Inject(ctx); got != nil {
		t.Fatalf("Inject() with no-op provider = %v, want nil", got)
	}
}

func TestInitUsesEnvEndpointAndExports(t *testing.T) {
	var got []byte
	otlp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			t.Errorf("export path = %q, want /v1/traces", r.URL.Path)
		}
		if ct := r.Header.Get("content-type"); !strings.HasPrefix(ct, "application/x-protobuf") {
			t.Errorf("content-type = %q, want application/x-protobuf", ct)
		}
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer otlp.Close()

	t.Setenv(envEndpoint, otlp.URL)
	shutdown, err := Init(context.Background(), Config{ServiceName: "prc-test"})
	if err != nil {
		t.Fatalf("Init() error = %v, want nil", err)
	}
	_, span := tracer().Start(context.Background(), "exported")
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v, want nil", err)
	}
	if len(got) == 0 {
		t.Fatal("no OTLP request received on shutdown flush")
	}
}

func TestInitExplicitEndpointWiring(t *testing.T) {
	// Exporter construction is lazy (the HTTP client connects on flush), so
	// Init must succeed even with an unreachable endpoint; flush errors are
	// the service's shutdown concern, not a wiring failure.
	shutdown, err := Init(context.Background(), Config{
		ServiceName:    "prc-test",
		ServiceVersion: "v9.9.9",
		Environment:    "test",
		OTLPEndpoint:   "http://127.0.0.1:1",
		SampleRatio:    0.5,
	})
	if err != nil {
		t.Fatalf("Init() error = %v, want nil", err)
	}
	_ = shutdown(context.Background())
}

func TestSampleRatio(t *testing.T) {
	tests := []struct {
		in   float64
		want float64
	}{
		{in: 0, want: 1},
		{in: -1, want: 1},
		{in: 1.5, want: 1},
		{in: 0.5, want: 0.5},
		{in: 1, want: 1},
	}
	for _, tt := range tests {
		if got := sampleRatio(tt.in); got != tt.want {
			t.Errorf("sampleRatio(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestTraceIDOnPlainContext(t *testing.T) {
	if got := TraceID(context.Background()); got != "" {
		t.Fatalf("TraceID(plain ctx) = %q, want empty", got)
	}
}

func TestMiddlewareContinuesIncomingTrace(t *testing.T) {
	exp := installCapture(t)
	const wantTrace = "0af7651916cd43dd8448eb211c80319c"

	var handlerTraceID string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		handlerTraceID = TraceID(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(Middleware(mux))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/users/42", nil)
	req.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-00f067aa0ba902b7-01")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = res.Body.Close()

	if handlerTraceID != wantTrace {
		t.Fatalf("handler TraceID = %q, want %q (trace continued from traceparent)", handlerTraceID, wantTrace)
	}

	spans := exp.recorded()
	if len(spans) != 1 {
		t.Fatalf("captured %d spans, want 1 (%v)", len(spans), spanNames(spans))
	}
	span := spans[0]
	if span.SpanContext().TraceID().String() != wantTrace {
		t.Errorf("span trace id = %s, want %s", span.SpanContext().TraceID(), wantTrace)
	}
	if span.Name() != "GET /users/{id}" {
		t.Errorf("span name = %q, want %q", span.Name(), "GET /users/{id}")
	}
	if span.SpanKind() != trace.SpanKindServer {
		t.Errorf("span kind = %v, want server", span.SpanKind())
	}
}

func TestMiddlewareUnmatchedRoute(t *testing.T) {
	exp := installCapture(t)

	srv := httptest.NewServer(Middleware(http.NotFoundHandler()))
	defer srv.Close()
	res, err := http.Get(srv.URL + "/nowhere")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = res.Body.Close()

	spans := exp.recorded()
	if len(spans) != 1 {
		t.Fatalf("captured %d spans, want 1", len(spans))
	}
	if span := spans[0]; span.Name() != "GET unmatched" {
		t.Errorf("span name = %q, want %q", span.Name(), "GET unmatched")
	}
}

func TestMiddlewareMarksServerError(t *testing.T) {
	exp := installCapture(t)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(Middleware(mux))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/boom")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = res.Body.Close()

	spans := exp.recorded()
	if len(spans) != 1 {
		t.Fatalf("captured %d spans, want 1", len(spans))
	}
	span := spans[0]
	if span.Status().Code != codes.Error {
		t.Errorf("span status = %v, want Error", span.Status().Code)
	}
	if got := attrValue(span, attrHTTPStatusCode); got != "500" {
		t.Errorf("status attr = %v, want 500", got)
	}
}

// attrValue returns the attribute value rendered as string, or "" when
// absent.
func attrValue(span sdktrace.ReadOnlySpan, key string) string {
	for _, a := range span.Attributes() {
		if string(a.Key) == key {
			return a.Value.String()
		}
	}
	return ""
}

// newQuietServer starts an httptest server whose panic logging is discarded:
// the middleware re-raises handler panics (mirroring net/http behavior), and
// the server logs them per connection — noise the tests need not print.
func newQuietServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func TestMiddlewarePanicProducesErrorSpan(t *testing.T) {
	exp := installCapture(t)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /panic", func(_ http.ResponseWriter, _ *http.Request) {
		panic(errors.New("boom"))
	})
	srv := newQuietServer(t, Middleware(mux))

	_, err := http.Get(srv.URL + "/panic")
	if err == nil {
		t.Fatal("request over panicking handler: expected connection error, got nil")
	}

	spans := exp.recorded()
	if len(spans) != 1 {
		t.Fatalf("captured %d spans, want 1", len(spans))
	}
	span := spans[0]
	// The finalizer must have run despite the panic: the span is named from
	// the matched pattern, classified like the RED middleware (500), and
	// carries the panic value as an exception event.
	if span.Name() != "GET /panic" {
		t.Errorf("span name = %q, want %q", span.Name(), "GET /panic")
	}
	if span.Status().Code != codes.Error {
		t.Errorf("span status = %v, want Error", span.Status().Code)
	}
	if got := attrValue(span, attrHTTPStatusCode); got != "500" {
		t.Errorf("status attr = %v, want 500", got)
	}
	if got := attrValue(span, attrHTTPRoute); got != "GET /panic" {
		t.Errorf("route attr = %v, want %q", got, "GET /panic")
	}
	var exceptionEvents int
	for _, ev := range span.Events() {
		if ev.Name == "exception" {
			exceptionEvents++
		}
	}
	if exceptionEvents != 1 {
		t.Errorf("exception events = %d, want 1 (panic value recorded)", exceptionEvents)
	}
}

func TestMiddlewareErrAbortHandlerAbortsAs499(t *testing.T) {
	exp := installCapture(t)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /abort", func(_ http.ResponseWriter, _ *http.Request) {
		panic(http.ErrAbortHandler)
	})
	srv := newQuietServer(t, Middleware(mux))

	_, err := http.Get(srv.URL + "/abort")
	if err == nil {
		t.Fatal("request over aborting handler: expected connection error, got nil")
	}

	spans := exp.recorded()
	if len(spans) != 1 {
		t.Fatalf("captured %d spans, want 1", len(spans))
	}
	span := spans[0]
	// Deliberate connection abort: classified 499 like the RED middleware
	// records it — not a server-error span.
	if span.Name() != "GET /abort" {
		t.Errorf("span name = %q, want %q", span.Name(), "GET /abort")
	}
	if got := attrValue(span, attrHTTPStatusCode); got != "499" {
		t.Errorf("status attr = %v, want 499", got)
	}
	if span.Status().Code == codes.Error {
		t.Error("span status = Error, want unset (client-closed request is not a server error)")
	}
}

func TestMiddlewareNormalizesMethod(t *testing.T) {
	exp := installCapture(t)

	srv := httptest.NewServer(Middleware(http.NotFoundHandler()))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), "PROPFIND", srv.URL+"/nowhere", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = res.Body.Close()

	spans := exp.recorded()
	if len(spans) != 1 {
		t.Fatalf("captured %d spans, want 1", len(spans))
	}
	span := spans[0]
	// An attacker-controlled method token must not leak into span names or
	// attributes — it collapses into the shared bounded "other" value.
	if span.Name() != "other unmatched" {
		t.Errorf("span name = %q, want %q", span.Name(), "other unmatched")
	}
	if got := attrValue(span, attrHTTPMethod); got != "other" {
		t.Errorf("method attr = %v, want %q", got, "other")
	}
}

// fakeHijacker is a ResponseWriter that additionally implements http.Hijacker.
type fakeHijacker struct {
	http.ResponseWriter
	hijacked bool
}

func (f *fakeHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	f.hijacked = true
	return nil, nil, nil
}

func TestStatusResponseWriterHijackPassthrough(t *testing.T) {
	fh := &fakeHijacker{ResponseWriter: httptest.NewRecorder()}
	w := &statusResponseWriter{ResponseWriter: fh}
	if _, ok := any(w).(http.Hijacker); !ok {
		t.Fatal("statusResponseWriter does not satisfy http.Hijacker")
	}
	if _, _, err := w.Hijack(); err != nil {
		t.Fatalf("Hijack passthrough: %v", err)
	}
	if !fh.hijacked {
		t.Fatal("Hijack did not reach the underlying writer")
	}

	if _, _, err := (&statusResponseWriter{ResponseWriter: httptest.NewRecorder()}).Hijack(); err == nil {
		t.Fatal("Hijack on non-Hijacker writer: want error, got nil")
	}
}

func TestNewTransportInjectsTraceparent(t *testing.T) {
	exp := installCapture(t)

	var gotParent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotParent = r.Header.Get("traceparent")
	}))
	defer srv.Close()

	ctx, span := tracer().Start(context.Background(), "outgoing")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/echo", nil)
	span.End()
	res, err := (&http.Client{Transport: NewTransport(nil)}).Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	// The otelhttp client span ends when the response body is closed, so it
	// must be drained and closed before the captured spans are asserted.
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if !strings.HasPrefix(gotParent, "00-") {
		t.Fatalf("traceparent header = %q, want W3C traceparent", gotParent)
	}

	var clientSpan sdktrace.ReadOnlySpan
	for _, s := range exp.recorded() {
		if s.Name() == "GET /echo" {
			clientSpan = s
		}
	}
	if clientSpan == nil {
		t.Fatalf("client span %q not captured", "GET /echo")
	}
	if clientSpan.SpanContext().TraceID().String() != traceIDFromParent(gotParent) {
		t.Error("client span trace id does not match the injected traceparent")
	}
}

func TestInjectExtractRoundTrip(t *testing.T) {
	_ = installCapture(t)

	ctx, span := tracer().Start(context.Background(), "producer")
	headers := Inject(ctx)
	if len(headers) == 0 {
		t.Fatal("Inject() = nil, want headers for a valid span context")
	}
	var found bool
	for _, h := range headers {
		if h.Key == "traceparent" && len(h.Value) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("headers %v missing traceparent", headers)
	}

	got := Extract(context.Background(), headers)
	if TraceID(got) != TraceID(ctx) {
		t.Fatalf("Extract() trace id = %q, want %q", TraceID(got), TraceID(ctx))
	}
	span.End()
}

func TestInjectWithoutSpanIsNil(t *testing.T) {
	if got := Inject(context.Background()); got != nil {
		t.Fatalf("Inject() on untraced ctx = %v, want nil", got)
	}
}

func TestExtractMalformedHeadersYieldsInputContext(t *testing.T) {
	out := Extract(context.Background(), []Header{{Key: "traceparent", Value: []byte("garbage")}})
	if TraceID(out) != "" {
		t.Fatalf("Extract() with malformed header produced trace id %q, want none", TraceID(out))
	}
}

func TestLogHandlerAddsTraceID(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(NewLogHandler(slog.NewJSONHandler(&buf, nil)))

	ctx, span := tracer().Start(context.Background(), "logged")
	want := TraceID(ctx)
	logger.InfoContext(ctx, "hello")
	span.End()

	var rec map[string]any
	if err := json.Unmarshal([]byte(buf.String()), &rec); err != nil {
		t.Fatalf("log line is not JSON: %v", err)
	}
	if got, _ := rec["trace_id"].(string); got != want {
		t.Fatalf("trace_id = %q, want %q; line: %s", got, want, buf.String())
	}
}

func TestLogHandlerOutsideSpanLeavesRecordUntouched(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(NewLogHandler(slog.NewJSONHandler(&buf, nil)))
	logger.Info("outside")

	if strings.Contains(buf.String(), "trace_id") {
		t.Fatalf("unexpected trace_id in %s", buf.String())
	}
}

func spanNames(spans []sdktrace.ReadOnlySpan) []string {
	names := make([]string, len(spans))
	for i, s := range spans {
		names[i] = s.Name()
	}
	return names
}

func traceIDFromParent(parent string) string {
	parts := strings.Split(parent, "-")
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}
