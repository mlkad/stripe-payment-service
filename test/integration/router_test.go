//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func request(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestRouter_MountsTheExpectedRoutes(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
	}{
		{"liveness", http.MethodGet, "/livez", "", http.StatusOK},
		{"readiness", http.MethodGet, "/healthz", "", http.StatusOK},
		{"unknown path", http.MethodGet, "/nope", "", http.StatusNotFound},
		{"webhook rejects GET", http.MethodGet, "/webhook", "", http.StatusMethodNotAllowed},
		{"checkout rejects GET", http.MethodGet, "/api/v1/checkout", "", http.StatusMethodNotAllowed},
		{"webhook without signature", http.MethodPost, "/webhook", "{}", http.StatusBadRequest},
		{"checkout without a token", http.MethodPost, "/api/v1/checkout", `{"price_id":"price_X"}`, http.StatusUnauthorized},
		{"subscription without a token", http.MethodGet, "/api/v1/subscription", "", http.StatusUnauthorized},
		{"register is public", http.MethodPost, "/api/v1/auth/register", `{`, http.StatusBadRequest},
		{"login is public", http.MethodPost, "/api/v1/auth/login", `{`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := request(t, h, tt.method, tt.path, tt.body)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tt.wantStatus, rec.Body)
			}
		})
	}
}

// Every response the router can produce on its own - including the ones chi
// generates for unmatched routes - must use the same error envelope, or clients
// need two parsers.
func TestRouter_ErrorsUseOneEnvelope(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{"not found", http.MethodGet, "/nope"},
		{"method not allowed", http.MethodGet, "/webhook"},
		{"handler rejection", http.MethodPost, "/webhook"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := request(t, h, tc.method, tc.path, "")

			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("content-type = %q, want JSON", ct)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %q", rec.Body)
			}
			if body["error"] == "" {
				t.Errorf("body has no error field: %v", body)
			}
		})
	}
}

// The correlation id must be minted by the router and echoed back, so a caller
// can quote it in a bug report and an operator can find the request.
func TestRouter_EchoesACorrelationID(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	t.Run("minted when absent", func(t *testing.T) {
		rec := request(t, h, http.MethodGet, "/livez", "")
		if rec.Header().Get("X-Request-Id") == "" {
			t.Error("no X-Request-Id on the response")
		}
	})

	t.Run("inbound id survives", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/livez", nil)
		r.Header.Set("X-Request-Id", "trace-from-the-proxy")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)

		if got := rec.Header().Get("X-Request-Id"); got != "trace-from-the-proxy" {
			t.Errorf("X-Request-Id = %q, want the inbound value", got)
		}
	})
}

// Health responses must never be cached: a stale 200 sitting in a proxy keeps
// traffic flowing to an instance that has already lost its database.
func TestRouter_HealthIsNotCacheable(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	for _, path := range []string{"/livez", "/healthz"} {
		rec := request(t, h, http.MethodGet, path, "")
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s Cache-Control = %q, want no-store", path, got)
		}
	}
}
