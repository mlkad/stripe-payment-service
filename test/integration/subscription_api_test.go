//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/mlkad/stripe-payment-service/internal/domain"
	repo "github.com/mlkad/stripe-payment-service/internal/repository/postgres"
	"github.com/mlkad/stripe-payment-service/internal/service"
)

func getSubscription(t *testing.T, h http.Handler, userID string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/subscription?user_id="+url.QueryEscape(userID), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// A user who never subscribed is the normal first-visit state, not a fault: the
// UI renders pricing rather than an error, so the shape of this response
// matters as much as the status.
func TestSubscriptionAPI_NoSubscriptionIs404(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)
	u := seedUserWithCustomer(t, "nosub@example.com", "cus_NoSub0001")

	rec := getSubscription(t, h, u.ID.String())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %q", rec.Body)
	}
	if body["error"] == "" {
		t.Error("404 body has no error field")
	}
}

func TestSubscriptionAPI_RejectsMalformedUserID(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	for _, id := range []string{"", "not-a-uuid", "'; DROP TABLE users; --"} {
		if rec := getSubscription(t, h, id); rec.Code != http.StatusBadRequest {
			t.Errorf("user_id=%q: status = %d, want 400", id, rec.Code)
		}
	}
}

// The read model must not carry the Stripe customer id, internal row ids, or
// the event bookkeeping columns. Those are in domain.Subscription and have no
// business reaching a browser.
func TestSubscriptionAPI_ViewWithholdsInternalFields(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)
	sub, _ := seedSubscription(t, "sub_View00001")

	rec := getSubscription(t, h, sub.UserID.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body)
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body is not JSON: %q", rec.Body)
	}
	for _, leaked := range []string{
		"id", "user_id", "stripe_customer_id", "stripe_subscription_id",
		"last_stripe_event_id", "last_stripe_event_at", "metadata",
	} {
		if _, present := payload[leaked]; present {
			t.Errorf("read model exposes %q", leaked)
		}
	}
	if payload["status"] != "active" || payload["is_active"] != true {
		t.Errorf("status/is_active = %v/%v, want active/true", payload["status"], payload["is_active"])
	}
}

// A lapsed subscriber must be distinguishable from someone who never
// subscribed: the two need different messaging, so a canceled row still
// returns 200 rather than collapsing into the 404 case.
func TestSubscriptionAPI_CanceledSubscriptionStillReturns200(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)
	ctx := context.Background()
	sub, base := seedSubscription(t, "sub_Lapsed0001")

	canceledAt := base.Add(time.Hour)
	if _, err := repo.NewSubscriptionRepo(pool).UpdateSubscriptionStatus(ctx, repo.SubscriptionStatusUpdate{
		StripeSubscriptionID: "sub_Lapsed0001",
		Status:               domain.SubscriptionCanceled,
		CanceledAt:           &canceledAt,
		EventID:              "evt_Lapsed0001",
		EventCreatedAt:       canceledAt,
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	rec := getSubscription(t, h, sub.UserID.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a lapsed subscriber is not the same as a new one", rec.Code)
	}
	var view service.SubscriptionView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("body is not JSON: %q", rec.Body)
	}
	if view.Status != domain.SubscriptionCanceled {
		t.Errorf("status = %q, want canceled", view.Status)
	}
	if view.IsActive {
		t.Error("is_active = true for a canceled subscription")
	}
}

// CORS is what lets the browser read the response at all. An unlisted origin
// must get no headers - the request still runs, and the browser blocks it.
func TestCORS_OnlyListedOriginsAreAllowed(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	tests := []struct {
		name        string
		origin      string
		wantAllowed bool
	}{
		{"listed origin", "http://localhost:5173", true},
		{"unlisted origin", "https://evil.example.com", false},
		{"listed origin with a different port", "http://localhost:3000", false},
		{"listed origin over https", "https://localhost:5173", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/livez", nil)
			r.Header.Set("Origin", tt.origin)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)

			got := rec.Header().Get("Access-Control-Allow-Origin")
			if tt.wantAllowed && got != tt.origin {
				t.Errorf("Allow-Origin = %q, want %q", got, tt.origin)
			}
			if !tt.wantAllowed && got != "" {
				t.Errorf("Allow-Origin = %q for an unlisted origin; the browser would let it read the response", got)
			}
		})
	}
}

func TestCORS_PreflightIsAnswered(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	r := httptest.NewRequest(http.MethodOptions, "/api/v1/checkout", nil)
	r.Header.Set("Origin", "http://localhost:5173")
	r.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	if m := rec.Header().Get("Access-Control-Allow-Methods"); m == "" {
		t.Error("preflight carried no Allow-Methods")
	}
	// Without Vary, a shared cache could serve one origin's response to another.
	if v := rec.Header().Values("Vary"); len(v) == 0 {
		t.Error("preflight carried no Vary header")
	}
}
