package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Handler returns the /metrics handler serving the default registry — the
// same registry Middleware records into. Register it on the service mux:
//
//	mux.HandleFunc("GET /metrics", metrics.Handler().ServeHTTP)
func Handler() http.Handler {
	return promhttp.Handler()
}
