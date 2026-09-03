// Package metrics defines the Prometheus instruments and the registry that
// holds them.
//
// The registry is built here rather than using prometheus.DefaultRegisterer, so
// nothing can register a metric into this process by importing a library, and a
// test can build a second Registry without collector-already-registered panics.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Registry holds every instrument this service exports.
type Registry struct {
	reg *prometheus.Registry

	// HTTP
	requestsTotal    *prometheus.CounterVec
	requestDuration  *prometheus.HistogramVec
	requestsInFlight prometheus.Gauge
	responseSize     *prometheus.HistogramVec

	// Webhooks
	webhookEvents   *prometheus.CounterVec
	webhookDuration *prometheus.HistogramVec

	// Ledger and dunning, set by the background workers.
	ledgerUnsettled      *prometheus.GaugeVec
	subscriptionsDunning prometheus.Gauge
	payloadsUnpurged     prometheus.Gauge

	// Auth
	authAttempts   *prometheus.CounterVec
	tokenRefreshes *prometheus.CounterVec

	// Stripe
	stripeRequests *prometheus.CounterVec
}

// New builds the registry with the Go runtime and process collectors attached.
//
// Those two are worth their cost: goroutine count, GC pause and open file
// descriptors are the first things to look at when a Go service misbehaves,
// and adding them later means adding them during an incident.
func New() *Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m := &Registry{reg: reg}

	m.requestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sps", Subsystem: "http", Name: "requests_total",
		Help: "HTTP requests by route pattern, method and status class.",
	}, []string{"method", "route", "status"})

	m.requestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "sps", Subsystem: "http", Name: "request_duration_seconds",
		Help: "HTTP request latency by route pattern.",
		// Tuned for this service rather than the defaults: most routes answer
		// in single-digit milliseconds, and the interesting tail is the webhook
		// path, which calls Stripe and can legitimately take seconds.
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 25},
	}, []string{"method", "route", "status"})

	m.requestsInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "sps", Subsystem: "http", Name: "requests_in_flight",
		Help: "HTTP requests currently being served.",
	})

	m.responseSize = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "sps", Subsystem: "http", Name: "response_size_bytes",
		Help:    "HTTP response size.",
		Buckets: prometheus.ExponentialBuckets(64, 4, 8),
	}, []string{"route"})

	m.webhookEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sps", Subsystem: "webhook", Name: "events_total",
		Help: "Stripe webhook deliveries by event type and outcome.",
	}, []string{"event_type", "outcome"})

	m.webhookDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "sps", Subsystem: "webhook", Name: "processing_duration_seconds",
		Help:    "Time to process one webhook delivery, excluding signature verification.",
		Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"event_type"})

	m.ledgerUnsettled = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "sps", Subsystem: "webhook", Name: "ledger_unsettled",
		Help: "Webhook events not yet settled, by state. dead_lettered > 0 needs a human.",
	}, []string{"state"})

	m.subscriptionsDunning = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "sps", Subsystem: "billing", Name: "subscriptions_in_dunning",
		Help: "Subscriptions with an outstanding payment failure.",
	})

	m.payloadsUnpurged = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "sps", Subsystem: "retention", Name: "payloads_past_window",
		Help: "Webhook payloads held past their retention window. Sustained non-zero is a compliance gap.",
	})

	m.authAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sps", Subsystem: "auth", Name: "attempts_total",
		Help: "Authentication attempts by operation and outcome.",
	}, []string{"operation", "outcome"})

	m.tokenRefreshes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sps", Subsystem: "auth", Name: "token_refreshes_total",
		Help: "Refresh token exchanges. outcome=reuse_detected is a theft signal.",
	}, []string{"outcome"})

	m.stripeRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sps", Subsystem: "stripe", Name: "requests_total",
		Help: "Outbound Stripe API calls by operation and outcome.",
	}, []string{"operation", "outcome"})

	reg.MustRegister(
		m.requestsTotal, m.requestDuration, m.requestsInFlight, m.responseSize,
		m.webhookEvents, m.webhookDuration,
		m.ledgerUnsettled, m.subscriptionsDunning, m.payloadsUnpurged,
		m.authAttempts, m.tokenRefreshes, m.stripeRequests,
	)
	return m
}

// Gatherer exposes the registry to the /metrics handler.
func (m *Registry) Gatherer() prometheus.Gatherer { return m.reg }

/* --- recording helpers ----------------------------------------------------
 *
 * Call sites use these rather than touching the vectors, so a label set cannot
 * drift between two places that record the same metric.
 */

// ObserveRequest records one HTTP request.
//
// route must be the chi route pattern, never the raw path. A path with an id in
// it would create one time series per id, and an unbounded label is how a
// metrics endpoint takes down the process it is meant to observe.
func (m *Registry) ObserveRequest(method, route, status string, seconds float64, bytes int64) {
	m.requestsTotal.WithLabelValues(method, route, status).Inc()
	m.requestDuration.WithLabelValues(method, route, status).Observe(seconds)
	m.responseSize.WithLabelValues(route).Observe(float64(bytes))
}

func (m *Registry) IncInFlight() { m.requestsInFlight.Inc() }
func (m *Registry) DecInFlight() { m.requestsInFlight.Dec() }

func (m *Registry) ObserveWebhook(eventType, outcome string, seconds float64) {
	m.webhookEvents.WithLabelValues(eventType, outcome).Inc()
	m.webhookDuration.WithLabelValues(eventType).Observe(seconds)
}

// SetLedger publishes the sweeper's view of the backlog.
func (m *Registry) SetLedger(processing, retryable, deadLettered int64) {
	m.ledgerUnsettled.WithLabelValues("processing").Set(float64(processing))
	m.ledgerUnsettled.WithLabelValues("retryable").Set(float64(retryable))
	m.ledgerUnsettled.WithLabelValues("dead_lettered").Set(float64(deadLettered))
}

func (m *Registry) SetDunning(n int64)            { m.subscriptionsDunning.Set(float64(n)) }
func (m *Registry) SetPayloadsPastWindow(n int64) { m.payloadsUnpurged.Set(float64(n)) }

func (m *Registry) IncAuth(operation, outcome string) {
	m.authAttempts.WithLabelValues(operation, outcome).Inc()
}

func (m *Registry) IncTokenRefresh(outcome string) {
	m.tokenRefreshes.WithLabelValues(outcome).Inc()
}

func (m *Registry) IncStripe(operation, outcome string) {
	m.stripeRequests.WithLabelValues(operation, outcome).Inc()
}
