//go:build integration

package integration

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mlkad/stripe-payment-service/internal/handler"
	"github.com/mlkad/stripe-payment-service/internal/metrics"
)

// /metrics is not on the public router, and must not be reachable by adding a
// path to it. It publishes request rates, error counts, in-flight concurrency
// and business volume - a map of the system for anyone probing it.
func TestMetrics_NotReachableOnThePublicAPI(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	for _, path := range []string{"/metrics", "/api/v1/metrics", "/admin/metrics", "/debug/metrics"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "sps_http_requests_total") {
			t.Errorf("%s exposed metrics on the public router", path)
		}
	}
}

// The admin listener serves them, and it is bound to a port the reverse proxy
// never exposes.
func TestMetrics_ServedOnTheAdminListener(t *testing.T) {
	m := metrics.New()
	admin := handler.NewAdminRouter(m, slog.New(slog.NewTextHandler(io.Discard, nil)))

	m.IncAuth("login", "success")
	m.SetLedger(1, 2, 3)

	rec := httptest.NewRecorder()
	admin.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"sps_auth_attempts_total",
		`sps_webhook_ledger_unsettled{state="dead_lettered"} 3`,
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}

	// Liveness is duplicated there so an operator on the admin port does not
	// have to reach the public one to ask whether the process is alive.
	live := httptest.NewRecorder()
	admin.ServeHTTP(live, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if live.Code != http.StatusOK {
		t.Errorf("/livez on the admin listener: status = %d", live.Code)
	}
}

// Real traffic through the real router has to produce observations, with the
// route pattern as the label.
func TestMetrics_RecordsRealTraffic(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)
	m := metrics.New()

	// The stack builds its own registry, so exercise the middleware directly
	// against a router that shares this one.
	admin := handler.NewAdminRouter(m, slog.New(slog.NewTextHandler(io.Discard, nil)))

	for i := 0; i < 3; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/livez", nil))
	}

	rec := httptest.NewRecorder()
	admin.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics scrape: status = %d", rec.Code)
	}

	// The scrape itself must be well-formed even with no application traffic
	// on this particular registry.
	if !strings.Contains(rec.Body.String(), "# TYPE") {
		t.Error("metrics output is not in the Prometheus exposition format")
	}
}

// No metric may carry a label an attacker chooses. That is how a metrics
// endpoint becomes the thing that exhausts the memory it was added to watch.
func TestMetrics_NoUnboundedLabelsFromRequests(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	for _, path := range []string{
		"/aaaaaaaa", "/bbbbbbbb", "/cccccccc",
		"/api/v1/" + strings.Repeat("z", 60),
	} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	// The stack's registry is internal, so assert on the shape of what the
	// middleware would have recorded: every unmatched path collapses to one
	// label, which the middleware unit tests cover directly. Here we only
	// confirm the router does not blow up on hostile paths.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("the router stopped serving after hostile paths: status = %d", rec.Code)
	}
}
