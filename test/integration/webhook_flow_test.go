//go:build integration

package integration

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	stripesdk "github.com/stripe/stripe-go/v86"

	"github.com/mlkad/stripe-payment-service/internal/domain"
	"github.com/mlkad/stripe-payment-service/internal/handler"
	repo "github.com/mlkad/stripe-payment-service/internal/repository/postgres"
	"github.com/mlkad/stripe-payment-service/internal/service"
	paystripe "github.com/mlkad/stripe-payment-service/internal/stripe"
)

const testWebhookSecret = "whsec_TESTSECRETdonotuseanywhereelse00"

// sign produces a Stripe-Signature header for payload. Stripe signs
// "<timestamp>.<raw body>" with HMAC-SHA256 and renders it as "t=...,v1=...",
// so this is the exact computation ConstructEvent reverses.
func sign(t *testing.T, payload []byte, at time.Time) string {
	t.Helper()
	ts := at.Unix()
	mac := hmac.New(sha256.New, []byte(testWebhookSecret))
	fmt.Fprintf(mac, "%d.%s", ts, payload)
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

func newWebhookStack(t *testing.T) (*service.WebhookService, http.Handler) {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := paystripe.New(paystripe.Config{
		SecretKey:        "sk_test_notusedforsubscriptionevents",
		WebhookSecret:    testWebhookSecret,
		APIVersion:       stripesdk.APIVersion,
		WebhookTolerance: 5 * time.Minute,
	}, log)
	if err != nil {
		t.Fatalf("build stripe client: %v", err)
	}

	svc := service.NewWebhookService(
		repo.NewUserRepo(pool),
		repo.NewSubscriptionRepo(pool),
		repo.NewWebhookRepo(pool, 5*time.Minute),
		client, log,
	)

	checkout, err := service.NewCheckoutService(repo.NewUserRepo(pool), client, service.CheckoutConfig{
		SuccessURL: "https://example.test/ok",
		CancelURL:  "https://example.test/cancel",
	}, log)
	if err != nil {
		t.Fatalf("build checkout service: %v", err)
	}

	mux := http.NewServeMux()
	handler.NewStripeHandler(svc, checkout, log).Register(mux)
	return svc, mux
}

// subscriptionEvent renders a customer.subscription.* payload in the shape the
// pinned API version produces: the billing period lives on the item, not on the
// subscription.
func subscriptionEvent(eventID, eventType, subID, customerID string, status domain.SubscriptionStatus, created time.Time) []byte {
	periodStart := created.Add(-24 * time.Hour).Unix()
	periodEnd := created.Add(30 * 24 * time.Hour).Unix()

	object := map[string]any{
		"id":                   subID,
		"object":               "subscription",
		"customer":             customerID,
		"status":               string(status),
		"cancel_at_period_end": false,
		"metadata":             map[string]string{},
		"items": map[string]any{
			"object": "list",
			"data": []any{map[string]any{
				"id":                   "si_Test0001",
				"object":               "subscription_item",
				"quantity":             1,
				"current_period_start": periodStart,
				"current_period_end":   periodEnd,
				"price": map[string]any{
					"id":          "price_Pro001",
					"object":      "price",
					"currency":    "usd",
					"unit_amount": 2900,
				},
			}},
		},
	}
	if status == domain.SubscriptionCanceled {
		object["canceled_at"] = created.Unix()
	}

	body, _ := json.Marshal(map[string]any{
		"id":          eventID,
		"object":      "event",
		"api_version": stripesdk.APIVersion,
		"created":     created.Unix(),
		"livemode":    false,
		"type":        eventType,
		"data":        map[string]any{"object": object},
	})
	return body
}

func postWebhook(t *testing.T, h http.Handler, payload []byte, signature string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(payload)))
	if signature != "" {
		req.Header.Set("Stripe-Signature", signature)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func ledgerRowCount(t *testing.T, eventID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM processed_webhooks WHERE event_id = $1`, eventID).Scan(&n); err != nil {
		t.Fatalf("count ledger rows: %v", err)
	}
	return n
}

func ledgerStatus(t *testing.T, eventID string) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status::text FROM processed_webhooks WHERE event_id = $1`, eventID).Scan(&status); err != nil {
		t.Fatalf("read ledger status: %v", err)
	}
	return status
}

// seedUserWithCustomer creates a user already linked to a Stripe customer, so
// subscription events can resolve an owner without a checkout session.
func seedUserWithCustomer(t *testing.T, email, customerID string) *domain.User {
	t.Helper()
	u := &domain.User{Email: email, StripeCustomerID: &customerID}
	if err := repo.NewUserRepo(pool).CreateUser(context.Background(), u); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return u
}

// An unverified body must never reach the idempotency ledger.
//
// If the claim ran before signature verification, anyone could POST a guessed
// event id, settle it, and cause Stripe's genuine delivery to be discarded as a
// duplicate - an unauthenticated denial of service against billing state. This
// test fails if that ordering is ever reversed.
func TestWebhook_ForgedPayloadNeverReachesLedger(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	payload := subscriptionEvent("evt_Forged001", "customer.subscription.updated",
		"sub_Forged01", "cus_Forged01", domain.SubscriptionActive, time.Now())

	for _, tc := range []struct {
		name      string
		signature string
	}{
		{"no signature header", ""},
		{"garbage signature", "t=1,v1=deadbeef"},
		{"valid shape, wrong secret", "t=" + fmt.Sprint(time.Now().Unix()) + ",v1=" + strings.Repeat("a", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postWebhook(t, h, payload, tc.signature)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if n := ledgerRowCount(t, "evt_Forged001"); n != 0 {
				t.Fatalf("forged payload created %d ledger row(s); the claim ran before verification", n)
			}
		})
	}
}

// A signature that verifies but whose timestamp is outside the tolerance window
// is a replay of a genuine request. It must be rejected on the same path.
func TestWebhook_ReplayOutsideToleranceIsRejected(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	payload := subscriptionEvent("evt_Replay001", "customer.subscription.updated",
		"sub_Replay01", "cus_Replay01", domain.SubscriptionActive, time.Now())
	stale := time.Now().Add(-30 * time.Minute)

	rec := postWebhook(t, h, payload, sign(t, payload, stale))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if n := ledgerRowCount(t, "evt_Replay001"); n != 0 {
		t.Errorf("replayed payload created %d ledger row(s)", n)
	}
}

// The body is read whole before it can be trusted, so the size cap is the only
// thing between an anonymous POST and the heap.
func TestWebhook_OversizedBodyIsRejected(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	// Well past the 256 KiB handler limit.
	payload := []byte(`{"padding":"` + strings.Repeat("x", 512<<10) + `"}`)
	rec := postWebhook(t, h, payload, sign(t, payload, time.Now()))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

func TestWebhook_SubscriptionLifecycle(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)
	seedUserWithCustomer(t, "lifecycle@example.com", "cus_Life0001")
	ctx := context.Background()
	subs := repo.NewSubscriptionRepo(pool)

	now := time.Now().UTC().Truncate(time.Second)

	// created: no local row yet, so the service inserts one from the event.
	created := subscriptionEvent("evt_Life0001", "customer.subscription.created",
		"sub_Life0001", "cus_Life0001", domain.SubscriptionActive, now)
	if rec := postWebhook(t, h, created, sign(t, created, now)); rec.Code != http.StatusOK {
		t.Fatalf("created: status = %d body = %s", rec.Code, rec.Body)
	}

	got, err := subs.GetSubscriptionByStripeID(ctx, "sub_Life0001")
	if err != nil {
		t.Fatalf("subscription was not created: %v", err)
	}
	if got.Status != domain.SubscriptionActive {
		t.Errorf("status = %q, want active", got.Status)
	}
	if got.UnitAmount == nil || *got.UnitAmount != 2900 {
		t.Errorf("unit_amount = %v, want 2900", got.UnitAmount)
	}
	if got.CurrentPeriodEnd.Before(got.CurrentPeriodStart) {
		t.Error("billing period was not read from the subscription item")
	}

	// updated: newer event advances the status.
	later := now.Add(time.Hour)
	updated := subscriptionEvent("evt_Life0002", "customer.subscription.updated",
		"sub_Life0001", "cus_Life0001", domain.SubscriptionPastDue, later)
	if rec := postWebhook(t, h, updated, sign(t, updated, later)); rec.Code != http.StatusOK {
		t.Fatalf("updated: status = %d body = %s", rec.Code, rec.Body)
	}
	if got, _ = subs.GetSubscriptionByStripeID(ctx, "sub_Life0001"); got.Status != domain.SubscriptionPastDue {
		t.Errorf("status = %q, want past_due", got.Status)
	}

	// deleted: terminal, and canceled_at must be populated to satisfy
	// subscriptions_canceled_at_chk even though Stripe may omit it.
	last := now.Add(2 * time.Hour)
	deleted := subscriptionEvent("evt_Life0003", "customer.subscription.deleted",
		"sub_Life0001", "cus_Life0001", domain.SubscriptionActive, last)
	if rec := postWebhook(t, h, deleted, sign(t, deleted, last)); rec.Code != http.StatusOK {
		t.Fatalf("deleted: status = %d body = %s", rec.Code, rec.Body)
	}
	got, _ = subs.GetSubscriptionByStripeID(ctx, "sub_Life0001")
	if got.Status != domain.SubscriptionCanceled {
		t.Errorf("status = %q, want canceled", got.Status)
	}
	if got.CanceledAt == nil {
		t.Error("canceled_at is null on a canceled subscription")
	}
}

// Stripe redelivers for up to three days. A second delivery must be answered
// 2xx without repeating the work.
func TestWebhook_RedeliveryIsAcknowledgedNotReapplied(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)
	seedUserWithCustomer(t, "dupe@example.com", "cus_Dupe0001")

	now := time.Now().UTC().Truncate(time.Second)
	payload := subscriptionEvent("evt_Dupe0001", "customer.subscription.created",
		"sub_Dupe0001", "cus_Dupe0001", domain.SubscriptionActive, now)
	signature := sign(t, payload, now)

	first := postWebhook(t, h, payload, signature)
	if first.Code != http.StatusOK {
		t.Fatalf("first delivery: status = %d body = %s", first.Code, first.Body)
	}
	if !strings.Contains(first.Body.String(), string(service.OutcomeProcessed)) {
		t.Errorf("first delivery outcome = %s, want processed", first.Body)
	}

	second := postWebhook(t, h, payload, signature)
	if second.Code != http.StatusOK {
		t.Fatalf("redelivery: status = %d, want 200", second.Code)
	}
	if !strings.Contains(second.Body.String(), string(service.OutcomeDuplicate)) {
		t.Errorf("redelivery outcome = %s, want duplicate", second.Body)
	}

	var attempts int32
	if err := pool.QueryRow(context.Background(),
		`SELECT attempts FROM processed_webhooks WHERE event_id = 'evt_Dupe0001'`).Scan(&attempts); err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1: the redelivery re-claimed a settled event", attempts)
	}
}

// An event type outside the subscription list is recorded as skipped, not
// dropped: the ledger should show it was seen and deliberately ignored.
func TestWebhook_UnhandledEventTypeIsSkipped(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	now := time.Now().UTC().Truncate(time.Second)
	body, _ := json.Marshal(map[string]any{
		"id":          "evt_Unhandled1",
		"object":      "event",
		"api_version": stripesdk.APIVersion,
		"created":     now.Unix(),
		"livemode":    false,
		"type":        "invoice.payment_succeeded",
		"data":        map[string]any{"object": map[string]any{"id": "in_Test0001", "object": "invoice"}},
	})

	rec := postWebhook(t, h, body, sign(t, body, now))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := ledgerStatus(t, "evt_Unhandled1"); got != "skipped" {
		t.Errorf("ledger status = %q, want skipped", got)
	}

	var reason *string
	if err := pool.QueryRow(context.Background(),
		`SELECT last_error FROM processed_webhooks WHERE event_id = 'evt_Unhandled1'`).Scan(&reason); err != nil {
		t.Fatalf("read reason: %v", err)
	}
	if reason == nil || *reason == "" {
		t.Error("skip reason was not recorded for the operator")
	}
}

// Out-of-order delivery reaching the service through the real HTTP path: the
// older event must be acknowledged and discarded, not applied.
func TestWebhook_OutOfOrderEventIsAcknowledgedAndDiscarded(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)
	seedUserWithCustomer(t, "order@example.com", "cus_Order001")
	subs := repo.NewSubscriptionRepo(pool)

	now := time.Now().UTC().Truncate(time.Second)
	newer := subscriptionEvent("evt_Order002", "customer.subscription.updated",
		"sub_Order001", "cus_Order001", domain.SubscriptionPastDue, now.Add(time.Hour))
	if rec := postWebhook(t, h, newer, sign(t, newer, now)); rec.Code != http.StatusOK {
		t.Fatalf("newer event: status = %d body = %s", rec.Code, rec.Body)
	}

	older := subscriptionEvent("evt_Order001", "customer.subscription.updated",
		"sub_Order001", "cus_Order001", domain.SubscriptionActive, now)
	rec := postWebhook(t, h, older, sign(t, older, now))
	if rec.Code != http.StatusOK {
		t.Fatalf("older event: status = %d, want 200 - a stale event must not be retried", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), string(service.OutcomeStale)) {
		t.Errorf("outcome = %s, want stale", rec.Body)
	}

	got, err := subs.GetSubscriptionByStripeID(context.Background(), "sub_Order001")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Status != domain.SubscriptionPastDue {
		t.Errorf("status = %q, want past_due: the stale event was applied", got.Status)
	}
}

// A subscription for a customer this service has never seen must not fail the
// endpoint forever; it is skipped and retained in the ledger for reconciliation.
func TestWebhook_UnknownCustomerIsSkipped(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	now := time.Now().UTC().Truncate(time.Second)
	payload := subscriptionEvent("evt_Unknown001", "customer.subscription.updated",
		"sub_Unknown01", "cus_Unknown01", domain.SubscriptionActive, now)

	rec := postWebhook(t, h, payload, sign(t, payload, now))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := ledgerStatus(t, "evt_Unknown001"); got != "skipped" {
		t.Errorf("ledger status = %q, want skipped", got)
	}
}
