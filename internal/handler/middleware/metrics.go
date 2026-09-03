package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mlkad/stripe-payment-service/internal/metrics"
)

// Metrics records one observation per request.
//
// It sits inside the router so chi has already matched a route by the time the
// observation is taken. That matters: the label has to be the route *pattern*,
// not the request path.
//
// A raw path creates one time series per distinct URL. With an id in the path
// that is unbounded, and an unbounded label is how a metrics endpoint takes
// down the process it was added to observe - the registry grows without limit
// and every scrape gets slower. Prometheus calls this a cardinality explosion
// and it is the single most common way to break a service by instrumenting it.
func Metrics(m *metrics.Registry) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m.IncInFlight()
			defer m.DecInFlight()

			start := time.Now()
			rec := wrapWriter(w)

			next.ServeHTTP(rec, r)

			m.ObserveRequest(
				r.Method,
				routeLabel(r),
				strconv.Itoa(rec.status),
				time.Since(start).Seconds(),
				rec.written,
			)
		})
	}
}

// routeLabel returns the matched chi pattern.
//
// An unmatched request has no pattern, and using its path would let anyone
// mint new time series by requesting random URLs. They all collapse to one
// label instead.
func routeLabel(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if pattern := rctx.RoutePattern(); pattern != "" {
			return pattern
		}
	}
	return "unmatched"
}
