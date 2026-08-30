package metrics

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// promErrorLog adapts slog to promhttp.HandlerOpts's ErrorLog interface so
// registry or exposition errors surface in service logs instead of being
// silently swallowed or leaking error detail into the metrics response.
type promErrorLog struct {
	logger *slog.Logger
}

func (l promErrorLog) Println(v ...interface{}) {
	l.logger.Error("promhttp: metrics exposition error", "detail", fmt.Sprint(v...))
}

// Handler returns the /metrics handler serving the default registry — the
// same registry Middleware records into. Register it on the service mux:
//
//	mux.HandleFunc("GET /metrics", metrics.Handler().ServeHTTP)
func Handler() http.Handler {
	return promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{
		// A broken collector must not blank the whole exposition for every
		// consumer; failing series are skipped and the error is logged.
		ErrorHandling: promhttp.ContinueOnError,
		ErrorLog:      promErrorLog{logger: slog.Default()},
	})
}
