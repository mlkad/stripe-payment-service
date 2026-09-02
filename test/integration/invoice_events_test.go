//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	stripesdk "github.com/stripe/stripe-go/v86"

	"github.com/mlkad/stripe-payment-service/internal/domain"
	repo "github.com/mlkad/stripe-payment-service/internal/repository/postgres"
)

// invoiceEvent renders an invoice.payment_* payload in the shape the pinned API
// version produces: the subscription hangs off parent.subscription_details, not
// invoice.subscription.
func invoiceEvent(eventID, eventType, invoiceID, subID string, created time.Time, opts func(map[string]any)) []byte {
	object := map[string]any{
		"id":             invoiceID,
		"object":         "invoice",
		"billing_reason": "subscription_cycle",
		"attempt_count":  1,
		"period_start":   created.Add(-24 * time.Hour).Unix(),
		"period_end":     created.Add(30 * 24 * time.Hour).Unix(),
		"parent": map[string]any{
			"type": "subscription_details",
			"subscription_details": map[string]any{
				"subscription": map[string]any{"id": subID, "object": "subscription"},
			},
		},
	}
	if opts != nil {
		opts(object)
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

func readSubscription(t *testing.T, subID string) *domain.Subscription {
	t.Helper()
	sub, err := repo.NewSubscriptionRepo(pool).GetSubscriptionByStripeID(context.Background(), subID)
	if err != nil {
		t.Fatalf("read subscription %s: %v", subID, err)
	}
	return sub
}

func TestInvoice_PaymentFailedRecordsDunningState(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)
	_, base := seedSubscription(t, "sub_Dun0000001")

	at := base.Add(time.Hour)
	nextAttempt := at.Add(72 * time.Hour)
	payload := invoiceEvent("evt_Dun0000001", "invoice.payment_failed", "in_Dun0000001",
		"sub_Dun0000001", at, func(o map[string]any) {
			o["next_payment_attempt"] = nextAttempt.Unix()
			o["last_finalization_error"] = map[string]any{
				"code":         "card_declined",
				"decline_code": "insufficient_funds",
				"message":      "Your card has insufficient funds.",
			}
		})

	if rec := postWebhook(t, h, payload, sign(t, payload, at)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	sub := readSubscription(t, "sub_Dun0000001")
	if !sub.InDunning() {
		t.Fatal("subscription is not flagged as in dunning")
	}
	if sub.PaymentFailureCount != 1 {
		t.Errorf("failure count = %d, want 1", sub.PaymentFailureCount)
	}
	if sub.LastPaymentError == nil {
		t.Fatal("no failure reason recorded")
	}
	// The decline code is the actionable part; the generic message alone is not.
	if got := *sub.LastPaymentError; got == "" || got[:len("insufficient_funds")] != "insufficient_funds" {
		t.Errorf("last_payment_error = %q, want it to lead with the decline code", got)
	}
	if sub.NextPaymentAttemptAt == nil || !sub.NextPaymentAttemptAt.Equal(nextAttempt.Truncate(time.Second)) {
		t.Errorf("next_payment_attempt_at = %v, want %v", sub.NextPaymentAttemptAt, nextAttempt)
	}
	if sub.LatestInvoiceID == nil || *sub.LatestInvoiceID != "in_Dun0000001" {
		t.Errorf("latest_invoice_id = %v, want in_Dun0000001", sub.LatestInvoiceID)
	}
}

// The invoice handler must not invent a status transition. Stripe decides what
// a failed payment means and says so in customer.subscription.updated.
func TestInvoice_PaymentFailedDoesNotChangeStatus(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)
	_, base := seedSubscription(t, "sub_Status000001") // seeded active

	at := base.Add(time.Hour)
	payload := invoiceEvent("evt_Status00001", "invoice.payment_failed", "in_Status00001",
		"sub_Status000001", at, nil)
	if rec := postWebhook(t, h, payload, sign(t, payload, at)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	if sub := readSubscription(t, "sub_Status000001"); sub.Status != domain.SubscriptionActive {
		t.Errorf("status = %q, want active: the invoice handler decided a status transition on its own",
			sub.Status)
	}
}

func TestInvoice_PaymentSucceededClearsDunningAndAdvancesPeriod(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)
	_, base := seedSubscription(t, "sub_Recover00001")

	failedAt := base.Add(time.Hour)
	failure := invoiceEvent("evt_Recover0001", "invoice.payment_failed", "in_Recover0001",
		"sub_Recover00001", failedAt, nil)
	if rec := postWebhook(t, h, failure, sign(t, failure, failedAt)); rec.Code != http.StatusOK {
		t.Fatalf("failure: status = %d, body = %s", rec.Code, rec.Body)
	}
	if sub := readSubscription(t, "sub_Recover00001"); !sub.InDunning() {
		t.Fatal("dunning was not recorded")
	}

	paidAt := base.Add(2 * time.Hour)
	success := invoiceEvent("evt_Recover0002", "invoice.payment_succeeded", "in_Recover0002",
		"sub_Recover00001", paidAt, nil)
	if rec := postWebhook(t, h, success, sign(t, success, paidAt)); rec.Code != http.StatusOK {
		t.Fatalf("success: status = %d, body = %s", rec.Code, rec.Body)
	}

	sub := readSubscription(t, "sub_Recover00001")
	if sub.InDunning() {
		t.Error("payment_failed_at survived a successful payment")
	}
	if sub.PaymentFailureCount != 0 {
		t.Errorf("failure count = %d, want 0 after recovery", sub.PaymentFailureCount)
	}
	if sub.LastPaymentError != nil {
		t.Errorf("last_payment_error = %v, want nil after recovery", sub.LastPaymentError)
	}
	if sub.NextPaymentAttemptAt != nil {
		t.Errorf("next_payment_attempt_at = %v, want nil after recovery", sub.NextPaymentAttemptAt)
	}
	// The renewal invoice's period is what the subscription should now report.
	if !sub.CurrentPeriodEnd.After(base.Add(29 * 24 * time.Hour)) {
		t.Errorf("current_period_end = %v, want it advanced by the renewal", sub.CurrentPeriodEnd)
	}
}

func TestInvoice_ConsecutiveFailuresAccumulate(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)
	_, base := seedSubscription(t, "sub_Retry0000001")

	for i := 1; i <= 3; i++ {
		at := base.Add(time.Duration(i) * time.Hour)
		payload := invoiceEvent(
			"evt_Retry000000"+string(rune('0'+i)), "invoice.payment_failed",
			"in_Retry0000001", "sub_Retry0000001", at, nil)
		if rec := postWebhook(t, h, payload, sign(t, payload, at)); rec.Code != http.StatusOK {
			t.Fatalf("attempt %d: status = %d, body = %s", i, rec.Code, rec.Body)
		}
		if got := readSubscription(t, "sub_Retry0000001").PaymentFailureCount; got != int32(i) {
			t.Fatalf("after attempt %d: failure count = %d, want %d", i, got, i)
		}
	}
}

// Redelivery must not double-count. The event ledger is what makes this
// idempotent, not anything in the invoice handler.
func TestInvoice_RedeliveryDoesNotDoubleCount(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)
	_, base := seedSubscription(t, "sub_Idem00000001")

	at := base.Add(time.Hour)
	payload := invoiceEvent("evt_Idem00000001", "invoice.payment_failed", "in_Idem00000001",
		"sub_Idem00000001", at, nil)
	signature := sign(t, payload, at)

	for i := 0; i < 3; i++ {
		if rec := postWebhook(t, h, payload, signature); rec.Code != http.StatusOK {
			t.Fatalf("delivery %d: status = %d", i, rec.Code)
		}
	}

	if got := readSubscription(t, "sub_Idem00000001").PaymentFailureCount; got != 1 {
		t.Errorf("failure count = %d after three deliveries of one event, want 1", got)
	}
}

// The two event streams have separate ordering cursors. An invoice event must
// not advance the subscription cursor, or the customer.subscription.updated
// carrying the real status gets rejected as stale and silently dropped.
func TestInvoice_DoesNotStarveTheSubscriptionEventStream(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)
	_, base := seedSubscription(t, "sub_Cursor000001")

	// Invoice event at T+5.
	invoiceAt := base.Add(5 * time.Hour)
	inv := invoiceEvent("evt_Cursor00001", "invoice.payment_failed", "in_Cursor00001",
		"sub_Cursor000001", invoiceAt, nil)
	if rec := postWebhook(t, h, inv, sign(t, inv, invoiceAt)); rec.Code != http.StatusOK {
		t.Fatalf("invoice event: status = %d, body = %s", rec.Code, rec.Body)
	}

	// Subscription event at T+4, i.e. older than the invoice event but newer
	// than anything on its own cursor. It must still be applied.
	subAt := base.Add(4 * time.Hour)
	subEvent := subscriptionEvent("evt_Cursor00002", "customer.subscription.updated",
		"sub_Cursor000001", "cus_Cursor000001", domain.SubscriptionPastDue, subAt)
	rec := postWebhook(t, h, subEvent, sign(t, subEvent, subAt))
	if rec.Code != http.StatusOK {
		t.Fatalf("subscription event: status = %d, body = %s", rec.Code, rec.Body)
	}

	sub := readSubscription(t, "sub_Cursor000001")
	if sub.Status != domain.SubscriptionPastDue {
		t.Errorf("status = %q, want past_due: the invoice event advanced the subscription cursor "+
			"and starved the event carrying the real status", sub.Status)
	}
	// And the invoice event's own effect survived.
	if !sub.InDunning() {
		t.Error("dunning state was lost")
	}
}

// A stale invoice event is acknowledged and discarded, like any other.
func TestInvoice_StaleEventIsAcknowledgedAndDiscarded(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)
	_, base := seedSubscription(t, "sub_Stale0000001")

	newerAt := base.Add(3 * time.Hour)
	newer := invoiceEvent("evt_Stale0000002", "invoice.payment_failed", "in_Stale0000002",
		"sub_Stale0000001", newerAt, nil)
	if rec := postWebhook(t, h, newer, sign(t, newer, newerAt)); rec.Code != http.StatusOK {
		t.Fatalf("newer: status = %d", rec.Code)
	}

	olderAt := base.Add(time.Hour)
	older := invoiceEvent("evt_Stale0000001", "invoice.payment_succeeded", "in_Stale0000001",
		"sub_Stale0000001", olderAt, nil)
	rec := postWebhook(t, h, older, sign(t, older, olderAt))
	if rec.Code != http.StatusOK {
		t.Fatalf("older: status = %d, want 200 - a stale event must not be retried", rec.Code)
	}

	if sub := readSubscription(t, "sub_Stale0000001"); !sub.InDunning() {
		t.Error("the stale success cleared dunning state it should not have touched")
	}
}

// An invoice with no subscription (a one-off charge) is acknowledged, not
// treated as a fault.
func TestInvoice_WithoutSubscriptionIsSkipped(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	now := time.Now().UTC().Truncate(time.Second)
	body, _ := json.Marshal(map[string]any{
		"id":          "evt_OneOff000001",
		"object":      "event",
		"api_version": stripesdk.APIVersion,
		"created":     now.Unix(),
		"livemode":    false,
		"type":        "invoice.payment_succeeded",
		"data": map[string]any{"object": map[string]any{
			"id":             "in_OneOff000001",
			"object":         "invoice",
			"billing_reason": "manual",
		}},
	})

	rec := postWebhook(t, h, body, sign(t, body, now))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := ledgerStatus(t, "evt_OneOff000001"); got != "skipped" {
		t.Errorf("ledger status = %q, want skipped", got)
	}
}

// An invoice can arrive before the subscription row exists. The payload has no
// items, so there is nothing to create it from - acknowledge and let the
// subscription event do it.
func TestInvoice_ForUnknownSubscriptionIsSkipped(t *testing.T) {
	truncate(t)
	_, h := newWebhookStack(t)

	now := time.Now().UTC().Truncate(time.Second)
	payload := invoiceEvent("evt_Unknown00001", "invoice.payment_failed", "in_Unknown00001",
		"sub_Unknown00001", now, nil)

	rec := postWebhook(t, h, payload, sign(t, payload, now))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := ledgerStatus(t, "evt_Unknown00001"); got != "skipped" {
		t.Errorf("ledger status = %q, want skipped", got)
	}
}
