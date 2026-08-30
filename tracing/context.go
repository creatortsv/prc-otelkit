package tracing

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// tracer is the package-wide tracer resolved from the global provider on
// every use, so instrumentation installed by Init is picked up regardless of
// ordering (the global provider proxies late wiring). Before Init the
// provider is a no-op and spans are simply not recorded.
const scopeName = "github.com/creatortsv/prc-otelkit/tracing"

func tracer() trace.Tracer {
	return otel.GetTracerProvider().Tracer(scopeName)
}

// TraceID returns the hex W3C trace id carried by ctx, or "" when the
// context holds no valid (sampled or unsampled) span context. The value
// matches Loki's `trace_id` derived field, which links log lines to traces
// in Tempo (prc-docs/operations/observability.md).
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}
