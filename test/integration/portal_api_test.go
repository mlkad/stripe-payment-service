//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func postPortal(t *testing.T, h http.Handler, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/portal", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// The portal URL authenticates its bearer as that Stripe customer, so the route
// has to be behind auth like every other billing endpoint.
func TestPortal_RequiresAuthentication(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	if rec := postPortal(t, h, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// A user who has never completed a checkout has no Stripe customer, so there is
// nothing to manage. 404 rather than an error: the UI should offer a checkout.
func TestPortal_WithoutBillingAccountIs404(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)
	u := seedUserWithCustomer(t, "noportal@example.com", "cus_NoPortal001")

	// Strip the customer link to model a user who never checked out.
	if _, err := pool.Exec(t.Context(),
		`UPDATE users SET stripe_customer_id = NULL WHERE id = $1`, u.ID); err != nil {
		t.Fatalf("unlink customer: %v", err)
	}

	rec := postPortal(t, h, tokenFor(t, u.ID))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", rec.Code, rec.Body)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %q", rec.Body)
	}
	if body["error"] == "" {
		t.Error("404 body has no error field")
	}
}

// The request carries no customer id, and there is no field it could carry one
// in. This is the whole security boundary for the portal: the URL grants
// access to a billing account, so the account must come from the token.
func TestPortal_TakesNoCustomerFromTheRequest(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	victim := seedUserWithCustomer(t, "victim@example.com", "cus_Victim00001")
	attacker := seedUserWithCustomer(t, "attacker@example.com", "cus_Attacker001")

	// Every way a caller might try to name someone else's account.
	for _, attempt := range []struct {
		name, path, body string
	}{
		{"query parameter", "/api/v1/portal?customer_id=cus_Victim00001", ""},
		{"query parameter with user id", "/api/v1/portal?user_id=" + victim.ID.String(), ""},
		{"json body", "/api/v1/portal", `{"customer_id":"cus_Victim00001"}`},
	} {
		t.Run(attempt.name, func(t *testing.T) {
			var r *http.Request
			if attempt.body == "" {
				r = httptest.NewRequest(http.MethodPost, attempt.path, nil)
			} else {
				r = httptest.NewRequest(http.MethodPost, attempt.path, strings.NewReader(attempt.body))
				r.Header.Set("Content-Type", "application/json")
			}
			r.Header.Set("Authorization", "Bearer "+tokenFor(t, attacker.ID))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)

			// Without Stripe credentials the call cannot succeed here; what
			// matters is that the victim's customer id never appears in the
			// response, whatever the outcome.
			if strings.Contains(rec.Body.String(), "cus_Victim00001") {
				t.Fatalf("the response referenced the victim's billing account: %s", rec.Body)
			}
			if rec.Code == http.StatusOK || rec.Code == http.StatusCreated {
				t.Errorf("status = %d: a portal session was opened without real Stripe credentials", rec.Code)
			}
		})
	}
}

func TestPortal_RejectsUnknownUser(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	// A valid token whose subject no longer exists.
	if rec := postPortal(t, h, tokenFor(t, uuid.New())); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
