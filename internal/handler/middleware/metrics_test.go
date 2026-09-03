package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mlkad/stripe-payment-service/internal/metrics"
)

// gather renders the registry the way a scrape would.
func gather(t *testing.T, m *metrics.Registry) string {
	t.Helper()
	families, err := m.Gatherer().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var b strings.Builder
	for _, f := range families {
		for _, metric := range f.GetMetric() {
			b.WriteString(f.GetName())
			for _, l := range metric.GetLabel() {
				b.WriteString(" " + l.GetName() + "=" + l.GetValue())
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// The label must be the route pattern, never the path.
//
// A path with an id in it produces one time series per id. That is unbounded,
// and an unbounded label is how a metrics endpoint takes down the process it
// was added to observe.
func TestMetricsLabelsByRoutePatternNotPath(t *testing.T) {
	m := metrics.New()

	r := chi.NewRouter()
	r.Use(Metrics(m))
	r.Get("/api/v1/users/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	for _, id := range []string{"alice", "bob", "carol", "dave"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/users/"+id, nil)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}

	rendered := gather(t, m)
	for _, id := range []string{"alice", "bob", "carol", "dave"} {
		if strings.Contains(rendered, "/api/v1/users/"+id) {
			t.Fatalf("the raw path became a label value; four requests would be four "+
				"time series:\n%s", rendered)
		}
	}
	if !strings.Contains(rendered, "route=/api/v1/users/{id}") {
		t.Errorf("route pattern label missing:\n%s", rendered)
	}
}

// An unmatched request has no pattern. Using its path would let anyone mint
// time series by requesting random URLs.
func TestMetricsCollapsesUnmatchedRoutes(t *testing.T) {
	m := metrics.New()

	r := chi.NewRouter()
	r.Use(Metrics(m))
	// A real route has to exist. chi skips the middleware chain entirely on a
	// router with no routes registered, which no actual router is - but it
	// makes a test written without one silently observe nothing.
	r.Get("/real", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })

	for _, path := range []string{"/nope", "/also-nope", "/../etc/passwd", "/random-" + strings.Repeat("x", 40)} {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	rendered := gather(t, m)
	if strings.Contains(rendered, "also-nope") || strings.Contains(rendered, "xxxxx") {
		t.Errorf("an unmatched path became a label:\n%s", rendered)
	}
	if !strings.Contains(rendered, "route=unmatched") {
		t.Errorf("unmatched requests were not collapsed:\n%s", rendered)
	}
}

func TestMetricsRecordsStatusAndMethod(t *testing.T) {
	m := metrics.New()

	r := chi.NewRouter()
	r.Use(Metrics(m))
	r.Get("/ok", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	r.Post("/boom", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ok", nil))
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/boom", nil))

	rendered := gather(t, m)
	for _, want := range []string{"method=GET", "method=POST", "status=200", "status=500"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("missing %q:\n%s", want, rendered)
		}
	}
}

// The gauge has to return to zero, or a burst of traffic leaves the service
// looking permanently saturated.
func TestMetricsInFlightGaugeReturnsToZero(t *testing.T) {
	m := metrics.New()

	r := chi.NewRouter()
	r.Use(Metrics(m))
	r.Get("/x", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	for i := 0; i < 5; i++ {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	}

	families, err := m.Gatherer().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "sps_http_requests_in_flight" {
			continue
		}
		if got := f.GetMetric()[0].GetGauge().GetValue(); got != 0 {
			t.Errorf("in-flight gauge = %v after all requests completed, want 0", got)
		}
		return
	}
	t.Error("in-flight gauge was never registered")
}

// A panic must still be counted, or the metric that matters most goes missing
// exactly when something is wrong.
func TestMetricsRecordsPanickedRequests(t *testing.T) {
	m := metrics.New()
	log, _ := capture()

	r := chi.NewRouter()
	r.Use(AccessLog(log))
	r.Use(Metrics(m))
	r.Use(Recoverer(log))
	r.Get("/panic", func(http.ResponseWriter, *http.Request) { panic("boom") })

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/panic", nil))

	if rendered := gather(t, m); !strings.Contains(rendered, "status=500") {
		t.Errorf("a panicked request was not recorded as 500:\n%s", rendered)
	}
}

// Two registries must be independent, or a test that instruments something
// pollutes the next one.
func TestRegistriesAreIndependent(t *testing.T) {
	first, second := metrics.New(), metrics.New()
	first.IncAuth("login", "success")

	if strings.Contains(gather(t, second), "sps_auth_attempts_total") {
		t.Error("a metric recorded on one registry appeared in another")
	}
}

func TestRegistryIncludesRuntimeCollectors(t *testing.T) {
	rendered := gather(t, metrics.New())
	for _, want := range []string{"go_goroutines", "go_memstats"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("%s is not collected; goroutine and GC data are the first "+
				"things to look at when a Go service misbehaves", want)
		}
	}
}
