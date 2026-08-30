package tracing

import (
	"context"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Header is one Kafka message header entry. It is a dependency-free mirror
// of the on-the-wire Kafka header shape (franz-go's kgo.RecordHeader has the
// same Key/Value fields) so this package stays usable by both prc-eventkit
// and header-generic tooling without importing a Kafka client.
type Header struct {
	Key   string
	Value []byte
}

// propagator is the single W3C propagation format used across PRC:
// traceparent (and tracestate) only. Baggage is deliberately not propagated
// until a cross-service baggage contract exists; extending later is
// backward-compatible because consumers ignore unknown headers.
var propagator = propagation.TraceContext{}

// Inject returns the W3C trace-context headers for ctx as Kafka message
// headers, or nil when ctx carries no valid span context — producers then
// send nothing, matching the contract that untraced work stays untraced.
// Combine the result into the record's existing headers:
//
//	record.Headers = append(record.Headers, kgoRecordHeaders(tracing.Inject(ctx))...)
func Inject(ctx context.Context) []Header {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return nil
	}
	carrier := &headerCarrier{headers: make([]Header, 0, 2)}
	propagator.Inject(ctx, carrier)
	return carrier.headers
}

// Extract returns a context derived from ctx with the trace context found in
// the Kafka message headers, so the consumer-side span started by the caller
// links to the producer-side trace (single trace across the queue hop).
// Missing or malformed headers yield the input context unchanged.
func Extract(ctx context.Context, headers []Header) context.Context {
	return propagator.Extract(ctx, &headerCarrier{headers: headers})
}

// headerCarrier adapts []Header to propagation.TextMapCarrier. Set replaces
// any previous value for the key — W3C headers are single-valued.
type headerCarrier struct {
	headers []Header
}

func (c *headerCarrier) Get(key string) string {
	for _, h := range c.headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

func (c *headerCarrier) Set(key, value string) {
	for i, h := range c.headers {
		if h.Key == key {
			c.headers[i].Value = []byte(value)
			return
		}
	}
	c.headers = append(c.headers, Header{Key: key, Value: []byte(value)})
}

func (c *headerCarrier) Keys() []string {
	keys := make([]string, len(c.headers))
	for i, h := range c.headers {
		keys[i] = h.Key
	}
	return keys
}
