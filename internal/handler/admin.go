package handler

import (
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/mlkad/stripe-payment-service/internal/metrics"
)

// NewAdminRouter serves the operational surface: metrics and a liveness probe.
//
// Deliberately a separate handler from the public API, mounted on its own
// listener. /metrics is not a public endpoint: it publishes request rates,
// error counts, in-flight concurrency and business volume - how many
// subscriptions, how many payments failed, how many events are stuck. That is a
// map of the system for anyone probing it, and a free traffic-analysis feed for
// a competitor.
//
// Keeping it off the public router means it cannot be reached by adding a path,
// only by reaching a different port that the reverse proxy never exposes.
func NewAdminRouter(m *metrics.Registry, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /metrics", promhttp.HandlerFor(m.Gatherer(), promhttp.HandlerOpts{
		// A broken collector should degrade the scrape, not the process.
		ErrorHandling: promhttp.ContinueOnError,
		ErrorLog:      slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		// Scrapes are cheap but not free, and a stuck one holding a connection
		// open is worse than a missed sample.
		Timeout: 10 * 1e9,
	}))

	// Duplicated from the public router on purpose: an operator probing the
	// admin port should not have to reach the public one to ask whether the
	// process is alive.
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	return mux
}
