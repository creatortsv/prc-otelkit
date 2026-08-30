package tracing

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace/noop"
)

// Benchmarks mirror the metrics package: they quantify the p95 overhead
// budget (standards §14: < 5 ms) each helper adds over the plain path. Run:
//
//	go test ./tracing -bench . -benchmem
//
// The default global provider is a no-op — the production shape for services
// without Tempo — so these numbers double as the untraced-fast-path ceiling.

// noopProvider restores the global no-op provider and is safe for tests
// running under -bench without the capture helpers' cleanup semantics.
func withNoopProvider(b *testing.B) {
	otel.SetTracerProvider(noop.NewTracerProvider())
}

func BenchmarkMiddleware(b *testing.B) {
	withNoopProvider(b)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped := Middleware(mux)
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func BenchmarkMiddlewareBareMux(b *testing.B) {
	withNoopProvider(b)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, _ *http.Request) {})
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		mux.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func BenchmarkInjectHeaders(b *testing.B) {
	_ = &captureExporter{}
	withNoopProvider(b)

	ctx, span := tracer().Start(context.Background(), "bench")
	defer span.End()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = Inject(ctx)
	}
}

func BenchmarkExtract(b *testing.B) {
	withNoopProvider(b)
	headers := []Header{{Key: "traceparent", Value: []byte("00-0af7651916cd43dd8448eb211c80319c-00f067aa0ba902b7-01")}}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = Extract(context.Background(), headers)
	}
}

func BenchmarkLogHandler(b *testing.B) {
	withNoopProvider(b)
	h := NewLogHandler(discardHandler{})
	ctx, span := tracer().Start(context.Background(), "benched")
	span.End()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = h.Handle(ctx, slog.NewRecord(time.Now(), slog.LevelInfo, "benchmark", 0))
	}
}

func BenchmarkTraceID(b *testing.B) {
	withNoopProvider(b)
	ctx, span := tracer().Start(context.Background(), "benched")
	defer span.End()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = TraceID(ctx)
	}
}

// discardHandler is a slog.Handler dropping every record, used to isolate
// wrapper overhead from encoder cost.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool { return true }
func (discardHandler) Handle(context.Context, slog.Record) error {
	return nil
}
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h discardHandler) WithGroup(string) slog.Handler      { return h }
