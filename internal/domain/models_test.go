package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

/* --- errors --------------------------------------------------------------- */

// Callers branch on errors.Is without importing pgx; that only works if the
// field errors keep unwrapping to ErrValidation.
func TestFieldErrorsUnwrapToErrValidation(t *testing.T) {
	single := FieldError{Field: "email", Detail: "is required"}
	if !errors.Is(single, ErrValidation) {
		t.Error("FieldError does not unwrap to ErrValidation")
	}
	if !strings.Contains(single.Error(), "email") {
		t.Errorf("FieldError text lost the field name: %s", single)
	}

	many := FieldErrors{single, {Field: "quantity", Detail: "must be positive"}}
	if !errors.Is(many, ErrValidation) {
		t.Error("FieldErrors does not unwrap to ErrValidation")
	}
	// The whole point of aggregating is fixing everything in one pass.
	for _, want := range []string{"email", "quantity"} {
		if !strings.Contains(many.Error(), want) {
			t.Errorf("aggregate lost %q: %s", want, many)
		}
	}
}

func TestSentinelErrorsAreDistinct(t *testing.T) {
	all := []error{ErrNotFound, ErrConflict, ErrValidation, ErrStaleEvent, ErrEventNotClaimed}
	for i, a := range all {
		for j, b := range all {
			if i != j && errors.Is(a, b) {
				t.Errorf("%v and %v are not distinguishable", a, b)
			}
		}
	}
}

/* --- status value objects ------------------------------------------------- */

// IsLive is the entitlement predicate and must match the partial indices
// idx_subscriptions_user_id_live and idx_subscriptions_current_period_end_live.
// Changing this set without changing those drops entitlement queries onto a
// sequential scan.
func TestSubscriptionStatusIsLiveMatchesTheIndexPredicate(t *testing.T) {
	live := map[SubscriptionStatus]bool{
		SubscriptionTrialing: true,
		SubscriptionActive:   true,
		SubscriptionPastDue:  true,
	}
	all := []SubscriptionStatus{
		SubscriptionIncomplete, SubscriptionIncompleteExpired, SubscriptionTrialing,
		SubscriptionActive, SubscriptionPastDue, SubscriptionCanceled,
		SubscriptionUnpaid, SubscriptionPaused,
	}
	for _, s := range all {
		if got := s.IsLive(); got != live[s] {
			t.Errorf("%q.IsLive() = %v, want %v", s, got, live[s])
		}
	}

	// past_due specifically: Stripe is still retrying, and revoking access
	// before dunning completes churns customers who would have paid.
	if !SubscriptionPastDue.IsLive() {
		t.Error("past_due is not live; dunning would lock out recoverable customers")
	}
}

func TestSubscriptionStatusValidity(t *testing.T) {
	for _, s := range []SubscriptionStatus{
		SubscriptionIncomplete, SubscriptionIncompleteExpired, SubscriptionTrialing,
		SubscriptionActive, SubscriptionPastDue, SubscriptionCanceled,
		SubscriptionUnpaid, SubscriptionPaused,
	} {
		if !s.Valid() {
			t.Errorf("%q is not valid", s)
		}
	}
	for _, s := range []SubscriptionStatus{"", "unknown", "ACTIVE", "active "} {
		if s.Valid() {
			t.Errorf("%q was accepted as a status", s)
		}
	}
}

func TestSubscriptionStatusIsTerminal(t *testing.T) {
	terminal := map[SubscriptionStatus]bool{
		SubscriptionCanceled: true, SubscriptionIncompleteExpired: true,
	}
	for _, s := range []SubscriptionStatus{
		SubscriptionActive, SubscriptionPastDue, SubscriptionCanceled,
		SubscriptionIncompleteExpired, SubscriptionPaused,
	} {
		if got := s.IsTerminal(); got != terminal[s] {
			t.Errorf("%q.IsTerminal() = %v, want %v", s, got, terminal[s])
		}
	}
}

func TestWebhookStatusValidity(t *testing.T) {
	for _, s := range []WebhookStatus{
		WebhookProcessing, WebhookSucceeded, WebhookFailed, WebhookSkipped,
	} {
		if !s.Valid() {
			t.Errorf("%q is not valid", s)
		}
	}
	if WebhookStatus("done").Valid() {
		t.Error("an unknown webhook status was accepted")
	}
}

/* --- User ----------------------------------------------------------------- */

func TestUserValidate(t *testing.T) {
	tests := []struct {
		name    string
		user    User
		wantErr bool
		field   string
	}{
		{"valid", User{Email: "ada@example.com"}, false, ""},
		{"empty email", User{Email: ""}, true, "email"},
		{"no at sign", User{Email: "not-an-email"}, true, "email"},
		{"whitespace in email", User{Email: "ada @example.com"}, true, "email"},
		{"internal newline", User{Email: "ada\n@example.com"}, true, "email"},
		{"internal tab", User{Email: "ada\t@example.com"}, true, "email"},
		{"too short", User{Email: "a@"}, true, "email"},
		{"too long", User{Email: strings.Repeat("a", 320) + "@example.com"}, true, "email"},
		{"wrong customer prefix", User{Email: "a@b.com", StripeCustomerID: ptr("sub_wrong")}, true, "stripe_customer_id"},
		{"good customer prefix", User{Email: "a@b.com", StripeCustomerID: ptr("cus_ok")}, false, ""},
		{"blank metadata key", User{Email: "a@b.com", Metadata: map[string]string{"  ": "v"}}, true, "metadata"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.user.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatal("invalid user was accepted")
				}
				if !errors.Is(err, ErrValidation) {
					t.Errorf("err = %v, want ErrValidation", err)
				}
				if !strings.Contains(err.Error(), tt.field) {
					t.Errorf("err = %q, want it to name %q", err, tt.field)
				}
				return
			}
			if err != nil {
				t.Errorf("valid user was rejected: %v", err)
			}
		})
	}
}

// Surrounding whitespace is repaired, not rejected, and the repair has to land
// on the entity: the repository persists u.Email, so validating a trimmed copy
// would let " ada@example.com" through and store it verbatim.
func TestUserValidateNormalisesEmailInPlace(t *testing.T) {
	for _, raw := range []string{
		"  ada@example.com  ",
		"ada@example.com\n",
		"\tada@example.com\t",
	} {
		u := User{Email: raw}
		if err := u.Validate(); err != nil {
			t.Errorf("Validate(%q) = %v, want it normalised and accepted", raw, err)
			continue
		}
		if u.Email != "ada@example.com" {
			t.Errorf("Validate(%q) left Email as %q, want it trimmed in place", raw, u.Email)
		}
	}
}

// PasswordHash must never serialise. The json:"-" tag is the last line of
// defence if a handler ever returns the entity instead of a DTO.
func TestUserNeverSerialisesItsSecrets(t *testing.T) {
	digest := "$2a$12$averysecretdigestvalue"
	deleted := time.Now()
	u := User{
		ID:           uuid.New(),
		Email:        "ada@example.com",
		PasswordHash: &digest,
		DeletedAt:    &deleted,
	}

	encoded, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{digest, "password_hash", "deleted_at"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("User JSON contains %q:\n%s", forbidden, encoded)
		}
	}
	if !strings.Contains(string(encoded), "ada@example.com") {
		t.Errorf("User JSON lost the email:\n%s", encoded)
	}
}

func TestUserIsDeleted(t *testing.T) {
	if (&User{}).IsDeleted() {
		t.Error("a live user reports as deleted")
	}
	now := time.Now()
	if !(&User{DeletedAt: &now}).IsDeleted() {
		t.Error("a soft-deleted user reports as live")
	}
}

/* --- Subscription --------------------------------------------------------- */

func validSubscription() Subscription {
	now := time.Now().UTC()
	return Subscription{
		UserID:               uuid.New(),
		StripeSubscriptionID: "sub_abc",
		StripeCustomerID:     "cus_abc",
		StripePriceID:        "price_abc",
		Status:               SubscriptionActive,
		Quantity:             1,
		CurrentPeriodStart:   now,
		CurrentPeriodEnd:     now.Add(30 * 24 * time.Hour),
	}
}

func TestSubscriptionValidate(t *testing.T) {
	now := time.Now().UTC()

	tests := []struct {
		name   string
		mutate func(*Subscription)
		field  string
	}{
		{"nil user", func(s *Subscription) { s.UserID = uuid.Nil }, "user_id"},
		{"bad subscription prefix", func(s *Subscription) { s.StripeSubscriptionID = "abc" }, "stripe_subscription_id"},
		{"bad customer prefix", func(s *Subscription) { s.StripeCustomerID = "abc" }, "stripe_customer_id"},
		{"bad price prefix", func(s *Subscription) { s.StripePriceID = "abc" }, "stripe_price_id"},
		{"unknown status", func(s *Subscription) { s.Status = "bananas" }, "status"},
		{"zero quantity", func(s *Subscription) { s.Quantity = 0 }, "quantity"},
		{"negative quantity", func(s *Subscription) { s.Quantity = -1 }, "quantity"},
		{"uppercase currency", func(s *Subscription) { s.Currency = ptr("USD") }, "currency"},
		{"wrong length currency", func(s *Subscription) { s.Currency = ptr("dollars") }, "currency"},
		{"negative amount", func(s *Subscription) { s.UnitAmount = ptr(int64(-1)) }, "unit_amount"},
		{"period end before start", func(s *Subscription) {
			s.CurrentPeriodEnd = s.CurrentPeriodStart.Add(-time.Hour)
		}, "current_period_end"},
		{"trial end before start", func(s *Subscription) {
			s.TrialStart, s.TrialEnd = ptr(now), ptr(now.Add(-time.Hour))
		}, "trial_end"},
		// Mirrors subscriptions_canceled_at_chk.
		{"canceled without a timestamp", func(s *Subscription) { s.Status = SubscriptionCanceled }, "canceled_at"},
		// Mirrors subscriptions_dunning_consistency_chk.
		{"failure count without a timestamp", func(s *Subscription) { s.PaymentFailureCount = 2 }, "payment_failed_at"},
		{"failure timestamp without a count", func(s *Subscription) { s.PaymentFailedAt = ptr(now) }, "payment_failed_at"},
		{"negative failure count", func(s *Subscription) { s.PaymentFailureCount = -1 }, "payment_failure_count"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := validSubscription()
			tt.mutate(&s)

			err := s.Validate()
			if err == nil {
				t.Fatal("invalid subscription was accepted")
			}
			if !errors.Is(err, ErrValidation) {
				t.Errorf("err = %v, want ErrValidation", err)
			}
			if !strings.Contains(err.Error(), tt.field) {
				t.Errorf("err = %q, want it to name %q", err, tt.field)
			}
		})
	}

	t.Run("valid", func(t *testing.T) {
		s := validSubscription()
		if err := s.Validate(); err != nil {
			t.Errorf("valid subscription was rejected: %v", err)
		}
	})

	t.Run("consistent dunning state is accepted", func(t *testing.T) {
		s := validSubscription()
		s.PaymentFailedAt, s.PaymentFailureCount = ptr(now), 2
		if err := s.Validate(); err != nil {
			t.Errorf("consistent dunning state was rejected: %v", err)
		}
	})
}

// InDunning is independent of IsLive: a past_due subscription is both, which is
// the point - access continues while Stripe retries, and the customer is told.
func TestSubscriptionInDunningIsIndependentOfIsLive(t *testing.T) {
	now := time.Now()
	s := validSubscription()
	s.Status = SubscriptionPastDue
	s.PaymentFailedAt, s.PaymentFailureCount = &now, 1

	if !s.IsLive() {
		t.Error("a past_due subscription is not live")
	}
	if !s.InDunning() {
		t.Error("a subscription with a payment failure is not in dunning")
	}

	healthy := validSubscription()
	if healthy.InDunning() {
		t.Error("a healthy subscription reports as in dunning")
	}
}

// LastStripeEventAt and the invoice cursor are bookkeeping, not API surface.
func TestSubscriptionWithholdsEventBookkeepingFromJSON(t *testing.T) {
	now := time.Now()
	id := "evt_abc"
	s := validSubscription()
	s.LastStripeEventID, s.LastStripeEventAt = &id, &now
	s.LastInvoiceEventID, s.LastInvoiceEventAt = &id, &now

	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{
		"last_stripe_event_id", "last_stripe_event_at",
		"last_invoice_event_id", "last_invoice_event_at",
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("Subscription JSON exposes %q:\n%s", forbidden, encoded)
		}
	}
}

/* --- ProcessedWebhook ----------------------------------------------------- */

func TestProcessedWebhookValidate(t *testing.T) {
	valid := ProcessedWebhook{
		EventID: "evt_abc", EventType: "customer.subscription.updated",
		StripeCreatedAt: time.Now(),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid webhook was rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*ProcessedWebhook)
		field  string
	}{
		{"bad event prefix", func(w *ProcessedWebhook) { w.EventID = "abc" }, "event_id"},
		{"blank type", func(w *ProcessedWebhook) { w.EventType = "   " }, "event_type"},
		{"zero created", func(w *ProcessedWebhook) { w.StripeCreatedAt = time.Time{} }, "stripe_created_at"},
		{"unknown status", func(w *ProcessedWebhook) { w.Status = "done" }, "status"},
		{"malformed payload", func(w *ProcessedWebhook) { w.Payload = []byte("{not json") }, "payload"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := valid
			tt.mutate(&w)
			err := w.Validate()
			if err == nil {
				t.Fatal("invalid webhook was accepted")
			}
			if !strings.Contains(err.Error(), tt.field) {
				t.Errorf("err = %q, want it to name %q", err, tt.field)
			}
		})
	}
}

// The raw payload carries PII and must never serialise out of the entity.
func TestProcessedWebhookWithholdsPayloadFromJSON(t *testing.T) {
	w := ProcessedWebhook{
		EventID: "evt_abc", EventType: "checkout.session.completed",
		StripeCreatedAt: time.Now(),
		Payload:         []byte(`{"customer_email":"ada@example.com"}`),
	}
	encoded, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "ada@example.com") {
		t.Errorf("ProcessedWebhook JSON leaked the payload:\n%s", encoded)
	}
}

func TestProcessedWebhookIsSettled(t *testing.T) {
	settled := map[WebhookStatus]bool{
		WebhookSucceeded: true, WebhookSkipped: true,
	}
	for _, s := range []WebhookStatus{
		WebhookProcessing, WebhookSucceeded, WebhookFailed, WebhookSkipped,
	} {
		w := ProcessedWebhook{Status: s}
		if got := w.IsSettled(); got != settled[s] {
			t.Errorf("%q.IsSettled() = %v, want %v", s, got, settled[s])
		}
	}
}

func ptr[T any](v T) *T { return &v }
