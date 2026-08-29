package metrics

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// counterSeries is one exposed http_requests_total series.
type counterSeries struct {
	route  string
	method string
	status string
	value  float64
}

// gatherCounters returns all http_requests_total series whose route contains
// routeFragment, failing the test when the metric family is absent.
func gatherCounters(t *testing.T, routeFragment string) []counterSeries {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var out []counterSeries
	for _, mf := range families {
		if mf.GetName() != "http_requests_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if !strings.Contains(labels["route"], routeFragment) {
				continue
			}
			out = append(out, counterSeries{
				route:  labels["route"],
				method: labels["method"],
				status: labels["status"],
				value:  m.GetCounter().GetValue(),
			})
		}
	}
	return out
}

// assertCounters fails unless the gathered series for routeFragment equal
// want exactly — same set of label triples, same values.
func assertCounters(t *testing.T, routeFragment string, want []counterSeries) {
	t.Helper()
	got := gatherCounters(t, routeFragment)
	if len(got) != len(want) {
		t.Fatalf("series count for route fragment %q: got %v (%d), want %v (%d)",
			routeFragment, got, len(got), want, len(want))
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g.route == w.route && g.method == w.method && g.status == w.status {
				if g.value != w.value {
					t.Fatalf("series %s %s %s: value = %v, want %v", g.route, g.method, g.status, g.value, w.value)
				}
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("series %+v not found among %v", w, got)
		}
	}
}

// histogramBuckets returns the exposed cumulative bucket counts of
// http_request_duration_seconds for the given route, keyed by the le upper
// bound; fails the test when the series is absent.
func histogramBuckets(t *testing.T, route string) map[float64]uint64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != "http_request_duration_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "route" && lp.GetValue() == route {
					out := map[float64]uint64{}
					h := m.GetHistogram()
					for _, b := range h.GetBucket() {
						out[b.GetUpperBound()] = b.GetCumulativeCount()
					}
					return out
				}
			}
		}
	}
	t.Fatalf("no histogram series for route %q", route)
	return nil
}

// serve runs one request through h and returns the recorder.
func serve(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMiddlewareRouteLabelIsServeMuxPattern(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := Middleware(mux)

	// Two distinct concrete paths of one route — the label must collapse
	// onto the single pattern, not explode per path.
	for _, path := range []string{"/users/1", "/users/2"} {
		if rec := serve(t, h, http.MethodGet, path); rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status = %d, want %d", path, rec.Code, http.StatusOK)
		}
	}

	assertCounters(t, "users", []counterSeries{
		{route: "GET /users/{id}", method: http.MethodGet, status: "200", value: 2},
	})
}

func TestMiddlewareRecordsUnmatchedAsConstantRoute(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /known", func(w http.ResponseWriter, _ *http.Request) {})
	h := Middleware(mux)

	if rec := serve(t, h, http.MethodGet, "/nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("GET /nope: status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	assertCounters(t, unmatchedRoute, []counterSeries{
		{route: unmatchedRoute, method: http.MethodGet, status: "404", value: 1},
	})
}

func TestMiddlewareCapturesStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /created", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("GET /implicit", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("GET /failure", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("DELETE /gone", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	h := Middleware(mux)

	if rec := serve(t, h, http.MethodGet, "/created"); rec.Code != http.StatusCreated {
		t.Fatalf("/created: status = %d", rec.Code)
	}
	if rec := serve(t, h, http.MethodGet, "/implicit"); rec.Code != http.StatusOK {
		t.Fatalf("/implicit: status = %d", rec.Code)
	}
	if rec := serve(t, h, http.MethodGet, "/failure"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("/failure: status = %d", rec.Code)
	}
	if rec := serve(t, h, http.MethodDelete, "/gone"); rec.Code != http.StatusNoContent {
		t.Fatalf("/gone: status = %d", rec.Code)
	}

	assertCounters(t, "created", []counterSeries{
		{route: "GET /created", method: http.MethodGet, status: "201", value: 1},
	})
	assertCounters(t, "implicit", []counterSeries{
		{route: "GET /implicit", method: http.MethodGet, status: "200", value: 1},
	})
	assertCounters(t, "failure", []counterSeries{
		{route: "GET /failure", method: http.MethodGet, status: "500", value: 1},
	})
	assertCounters(t, "gone", []counterSeries{
		{route: "DELETE /gone", method: http.MethodDelete, status: "204", value: 1},
	})
}

func TestMiddlewareRecordsPanickingHandler(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})
	h := Middleware(mux)

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("panic did not propagate: middleware must not swallow handler panics")
			}
		}()
		serve(t, h, http.MethodGet, "/boom")
	}()

	// No status was written before the panic, so the implicit-200 rule
	// applies; the request must still be counted.
	assertCounters(t, "boom", []counterSeries{
		{route: "GET /boom", method: http.MethodGet, status: "200", value: 1},
	})
}

func TestMiddlewareDurationHistogramBucketsMatchContract(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /probe", func(w http.ResponseWriter, _ *http.Request) {})
	h := Middleware(mux)

	if rec := serve(t, h, http.MethodGet, "/probe"); rec.Code != http.StatusOK {
		t.Fatalf("/probe: status = %d", rec.Code)
	}

	buckets := histogramBuckets(t, "GET /probe")
	if len(buckets) != len(requestDurationBuckets) {
		t.Fatalf("bucket count = %d, want %d", len(buckets), len(requestDurationBuckets))
	}
	for _, want := range requestDurationBuckets {
		if _, ok := buckets[want]; !ok {
			t.Fatalf("bucket le=%v missing; got %v", want, buckets)
		}
	}
	// The §14 budget lines must be observable as buckets.
	for _, budget := range []float64{0.3, 1.0} {
		if _, ok := buckets[budget]; !ok {
			t.Fatalf("§14 budget line le=%v missing from buckets", budget)
		}
	}
}

func TestHandlerServesDefaultRegistry(t *testing.T) {
	// Seed one series via the middleware so the payload is non-empty.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /seed", func(w http.ResponseWriter, _ *http.Request) {})
	if rec := serve(t, Middleware(mux), http.MethodGet, "/seed"); rec.Code != http.StatusOK {
		t.Fatalf("/seed: status = %d", rec.Code)
	}

	rec := serve(t, Handler(), http.MethodGet, "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics: status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("/metrics content type = %q, want text/plain exposition format", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"http_requests_total",
		"http_request_duration_seconds_bucket",
		`route="GET /seed"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/metrics body missing %q", want)
		}
	}
}

func BenchmarkMiddleware(b *testing.B) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /bench", func(w http.ResponseWriter, _ *http.Request) {})
	h := Middleware(mux)

	req := httptest.NewRequest(http.MethodGet, "/bench", nil)
	rec := httptest.NewRecorder()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec.Body.Reset()
		h.ServeHTTP(rec, req)
	}
}

func BenchmarkBareMux(b *testing.B) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /bench", func(w http.ResponseWriter, _ *http.Request) {})

	req := httptest.NewRequest(http.MethodGet, "/bench", nil)
	rec := httptest.NewRecorder()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec.Body.Reset()
		mux.ServeHTTP(rec, req)
	}
}
