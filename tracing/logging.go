package tracing

import (
	"context"
	"log/slog"
)

// TraceIDLogKey is the slog attribute key carrying the trace id. Loki's
// derived_fields configuration extracts exactly this key from service log
// lines and turns it into a Tempo link (observability contract), so the key
// string is a cross-repo contract — do not rename it.
const TraceIDKey = "trace_id"

// NewLogHandler wraps a slog.Handler so every record handled under a context
// carrying a span context is enriched with a `trace_id` attribute. Wrap the
// handler your service builds, before constructing the *slog.Logger:
//
//	logger := slog.New(tracing.NewLogHandler(slog.NewJSONHandler(os.Stdout, nil)))
//
// When the context has no valid span the record passes through unchanged, so
// logs emitted outside requests carry no key. Services must not set a
// trace_id attribute themselves — the wrapper is the single writer of the
// key, keeping one trace id per record.
func NewLogHandler(next slog.Handler) slog.Handler {
	return &traceLogHandler{next: next}
}

type traceLogHandler struct {
	next slog.Handler
}

func (h *traceLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *traceLogHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := TraceID(ctx); id != "" {
		r.AddAttrs(slog.String(TraceIDKey, id))
	}
	return h.next.Handle(ctx, r)
}

func (h *traceLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceLogHandler{next: h.next.WithAttrs(attrs)}
}

func (h *traceLogHandler) WithGroup(name string) slog.Handler {
	return &traceLogHandler{next: h.next.WithGroup(name)}
}
