// Package tracing provides distributed-tracing instrumentation for Position
// Review Copilot services: global tracer-provider initialization exporting
// spans to Tempo over OTLP/HTTP, W3C trace-context propagation for HTTP
// (server middleware + client transport) and Kafka message headers, and a
// slog handler that embeds the current trace_id into every log line.
//
// Contract (prc-docs/operations/observability.md):
//
//   - Traces: OpenTelemetry → Tempo; W3C traceparent is mandatory in HTTP
//     headers and Kafka message headers.
//   - Logs: Loki derives `trace_id` from the slog JSON field of that name and
//     links it to the trace in Tempo.
//   - Services receive their exporter endpoint via the
//     OTEL_EXPORTER_OTLP_ENDPOINT environment variable (injected by infra
//     into every service deployment).
//
// Span naming mirrors the metrics contract: the registered ServeMux pattern
// (http.Request.Pattern, e.g. "GET /users/{id}"), never the concrete path.
package tracing

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// envEndpoint is the standard OTel variable carrying the exporter endpoint.
// Infra injects it (e.g. http://tempo:4318) into every service deployment;
// Config.OTLPEndpoint overrides it for explicit wiring.
const envEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"

// Resource attribute keys follow the OpenTelemetry semantic conventions.
// Plain constants keep the package free of per-release semconv subpackage
// churn (see ADR-0002).
const (
	attrServiceName    = "service.name"
	attrServiceVersion = "service.version"
	attrDeploymentEnv  = "deployment.environment.name"
)

// Config controls Init.
type Config struct {
	// ServiceName becomes the resource attribute service.name. Required —
	// Tempo trace search and per-service SLO views group on it.
	ServiceName string

	// ServiceVersion becomes the resource attribute service.version. Optional.
	ServiceVersion string

	// Environment becomes the resource attribute
	// deployment.environment.name (e.g. "local", "stage"). Optional.
	Environment string

	// OTLPEndpoint is the OTLP/HTTP base endpoint, e.g. "http://tempo:4318"
	// (the value infra injects as OTEL_EXPORTER_OTLP_ENDPOINT). Empty falls
	// back to that environment variable. When both are empty, tracing stays
	// disabled and Init succeeds with a no-op shutdown.
	OTLPEndpoint string

	// SampleRatio is the fraction of root spans sampled, in (0, 1]; values
	// outside the range are treated as 1. Children follow their parent
	// (ParentBased), so a downstream service cannot resurrect a span that
	// the entry point dropped. The zero value samples everything — the
	// local and stage default.
	SampleRatio float64
}

// Init installs a global TracerProvider (otel.SetTracerProvider) exporting
// spans through a non-blocking batch processor (default 5 s delay, bounded
// queue, drops on overflow — export never blocks request handling) to the
// configured OTLP/HTTP endpoint. It returns a shutdown function that flushes
// pending spans and releases the exporter; services must defer it in main.
//
// When no endpoint is configured the returned shutdown is a no-op and the
// global provider stays a no-op: spans are created but never recorded, so
// local development works without a Tempo instance and this package's
// helpers degrade to zero-op pass-throughs.
func Init(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	if cfg.ServiceName == "" {
		return nil, errors.New("tracing: ServiceName is required")
	}
	endpoint := cfg.OTLPEndpoint
	if endpoint == "" {
		endpoint = os.Getenv(envEndpoint)
	}
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	exporter, err := otlptracehttp.New(ctx, exporterOptions(endpoint)...)
	if err != nil {
		return nil, fmt.Errorf("tracing: OTLP/HTTP exporter: %w", err)
	}

	attrs := []attribute.KeyValue{attribute.String(attrServiceName, cfg.ServiceName)}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, attribute.String(attrServiceVersion, cfg.ServiceVersion))
	}
	if cfg.Environment != "" {
		attrs = append(attrs, attribute.String(attrDeploymentEnv, cfg.Environment))
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(attrs...))
	if err != nil {
		return nil, fmt.Errorf("tracing: resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRatio(cfg.SampleRatio)))),
		sdktrace.WithBatcher(exporter),
	)
	otel.SetTracerProvider(provider)

	return provider.Shutdown, nil
}

// exporterOptions translates a base endpoint URL (e.g.
// "http://tempo:4318") into OTLP/HTTP exporter options. WithEndpoint keeps
// the exporter's spec-defined default signal path (/v1/traces) appended,
// matching what Tempo and the OTel collector expose.
func exporterOptions(endpoint string) []otlptracehttp.Option {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return []otlptracehttp.Option{otlptracehttp.WithEndpointURL(endpoint)}
	}
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(u.Host)}
	if u.Scheme == "http" {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	if u.Path != "" && u.Path != "/" {
		opts = append(opts, otlptracehttp.WithURLPath(u.Path))
	}
	return opts
}

// sampleRatio clamps the configured ratio into (0, 1].
func sampleRatio(r float64) float64 {
	if r <= 0 || r > 1 {
		return 1
	}
	return r
}
