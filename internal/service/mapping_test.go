package service

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	stripesdk "github.com/stripe/stripe-go/v86"

	"github.com/mlkad/stripe-payment-service/internal/domain"
)

func mustEvent(t *testing.T, id string, created time.Time) stripesdk.Event {
	t.Helper()
	return stripesdk.Event{ID: id, Created: created.Unix()}
}

/* --- subscription mapping ------------------------------------------------- */

// Since the 2025 API versions the price and the billing period live on the
// subscription item, not the subscription. Reading them from the wrong place
// writes zero timestamps into NOT NULL columns.
func TestSubscriptionFromReadsThePeriodOffTheItem(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	start, end := now.Add(-24*time.Hour), now.Add(30*24*time.Hour)

	sub := &stripesdk.Subscription{
		ID:       "sub_abc",
		Customer: &stripesdk.Customer{ID: "cus_abc"},
		Status:   stripesdk.SubscriptionStatusActive,
		Items: &stripesdk.SubscriptionItemList{Data: []*stripesdk.SubscriptionItem{{
			Quantity:           3,
			CurrentPeriodStart: start.Unix(),
			CurrentPeriodEnd:   end.Unix(),
			Price: &stripesdk.Price{
				ID: "price_abc", Currency: stripesdk.CurrencyUSD, UnitAmount: 2900,
				Product: &stripesdk.Product{ID: "prod_abc"},
			},
		}}},
	}

	userID := uuid.New()
	got, err := subscriptionFrom(userID, sub, mustEvent(t, "evt_abc", now))
	if err != nil {
		t.Fatalf("subscriptionFrom: %v", err)
	}

	if !got.CurrentPeriodStart.Equal(start) || !got.CurrentPeriodEnd.Equal(end) {
		t.Errorf("period = %v..%v, want %v..%v",
			got.CurrentPeriodStart, got.CurrentPeriodEnd, start, end)
	}
	if got.StripePriceID != "price_abc" {
		t.Errorf("price = %q, want price_abc", got.StripePriceID)
	}
	if got.Quantity != 3 {
		t.Errorf("quantity = %d, want 3", got.Quantity)
	}
	if got.UnitAmount == nil || *got.UnitAmount != 2900 {
		t.Errorf("unit_amount = %v, want 2900", got.UnitAmount)
	}
	if got.StripeProductID == nil || *got.StripeProductID != "prod_abc" {
		t.Errorf("product = %v, want prod_abc", got.StripeProductID)
	}
	if got.UserID != userID {
		t.Errorf("user id = %v, want %v", got.UserID, userID)
	}
	// The resulting entity has to satisfy the same rules the database enforces.
	if err := got.Validate(); err != nil {
		t.Errorf("mapped subscription fails its own validation: %v", err)
	}
}

// A subscription with no items has no price and no period, so it cannot be
// mapped. Better a clear error than a row of zero timestamps.
func TestSubscriptionFromRejectsMissingItems(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		name string
		sub  *stripesdk.Subscription
	}{
		{"nil items", &stripesdk.Subscription{ID: "sub_abc"}},
		{"empty items", &stripesdk.Subscription{
			ID: "sub_abc", Items: &stripesdk.SubscriptionItemList{}}},
		{"item without a price", &stripesdk.Subscription{
			ID:    "sub_abc",
			Items: &stripesdk.SubscriptionItemList{Data: []*stripesdk.SubscriptionItem{{}}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := subscriptionFrom(uuid.New(), tt.sub, mustEvent(t, "evt_abc", now)); err == nil {
				t.Error("a subscription with no usable item was mapped anyway")
			}
		})
	}
}

// A quantity of zero would violate subscriptions_quantity_positive_chk.
func TestSubscriptionFromDefaultsQuantity(t *testing.T) {
	now := time.Now()
	sub := &stripesdk.Subscription{
		ID: "sub_abc", Customer: &stripesdk.Customer{ID: "cus_abc"},
		Status: stripesdk.SubscriptionStatusActive,
		Items: &stripesdk.SubscriptionItemList{Data: []*stripesdk.SubscriptionItem{{
			Quantity:           0,
			CurrentPeriodStart: now.Add(-time.Hour).Unix(),
			CurrentPeriodEnd:   now.Add(time.Hour).Unix(),
			Price:              &stripesdk.Price{ID: "price_abc"},
		}}},
	}
	got, err := subscriptionFrom(uuid.New(), sub, mustEvent(t, "evt_abc", now))
	if err != nil {
		t.Fatalf("subscriptionFrom: %v", err)
	}
	if got.Quantity != 1 {
		t.Errorf("quantity = %d, want it defaulted to 1", got.Quantity)
	}
}

// customer.subscription.deleted is terminal even when the object still reports
// an earlier status, which happens when a subscription is deleted mid-cycle.
func TestStatusUpdateFromForcesCanceledOnDelete(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	sub := &stripesdk.Subscription{
		ID:     "sub_abc",
		Status: stripesdk.SubscriptionStatusActive,
		Items: &stripesdk.SubscriptionItemList{Data: []*stripesdk.SubscriptionItem{{
			Quantity:           1,
			CurrentPeriodStart: now.Add(-time.Hour).Unix(),
			CurrentPeriodEnd:   now.Add(time.Hour).Unix(),
			Price:              &stripesdk.Price{ID: "price_abc"},
		}}},
	}

	got, err := statusUpdateFrom(sub, mustEvent(t, "evt_abc", now), true)
	if err != nil {
		t.Fatalf("statusUpdateFrom: %v", err)
	}
	if got.Status != domain.SubscriptionCanceled {
		t.Errorf("status = %q, want canceled on a delete event", got.Status)
	}
	// subscriptions_canceled_at_chk requires a timestamp; Stripe omits it on a
	// subscription deleted before its first invoice.
	if got.CanceledAt == nil {
		t.Error("canceled_at is nil; the database would reject this write")
	}
}

func TestStatusUpdateFromRejectsUnknownStatus(t *testing.T) {
	now := time.Now()
	sub := &stripesdk.Subscription{
		ID: "sub_abc", Status: "bananas",
		Items: &stripesdk.SubscriptionItemList{Data: []*stripesdk.SubscriptionItem{{
			Quantity: 1, Price: &stripesdk.Price{ID: "price_abc"},
		}}},
	}
	if _, err := statusUpdateFrom(sub, mustEvent(t, "evt_abc", now), false); err == nil {
		t.Error("an unknown Stripe status was mapped into the domain")
	}
}

func TestEnsureCanceledAt(t *testing.T) {
	fallback := time.Now().UTC()
	existing := fallback.Add(-time.Hour)

	var nilAt *time.Time
	ensureCanceledAt(domain.SubscriptionCanceled, &nilAt, fallback)
	if nilAt == nil || !nilAt.Equal(fallback) {
		t.Error("canceled without a timestamp did not get the fallback")
	}

	keep := &existing
	ensureCanceledAt(domain.SubscriptionCanceled, &keep, fallback)
	if !keep.Equal(existing) {
		t.Error("an existing canceled_at was overwritten")
	}

	var untouched *time.Time
	ensureCanceledAt(domain.SubscriptionActive, &untouched, fallback)
	if untouched != nil {
		t.Error("a non-canceled status was given a canceled_at")
	}
}

/* --- invoice mapping ------------------------------------------------------ */

// Since the 2025 API versions the subscription hangs off
// invoice.parent.subscription_details, not invoice.subscription.
func TestSubscriptionIDOfReadsTheParent(t *testing.T) {
	invoice := &stripesdk.Invoice{
		ID: "in_abc",
		Parent: &stripesdk.InvoiceParent{
			SubscriptionDetails: &stripesdk.InvoiceParentSubscriptionDetails{
				Subscription: &stripesdk.Subscription{ID: "sub_abc"},
			},
		},
	}
	if got := subscriptionIDOf(invoice); got != "sub_abc" {
		t.Errorf("subscriptionIDOf = %q, want sub_abc", got)
	}
}

// Line items are the fallback for invoices rendered before that move.
func TestSubscriptionIDOfFallsBackToLineItems(t *testing.T) {
	invoice := &stripesdk.Invoice{
		ID: "in_abc",
		Lines: &stripesdk.InvoiceLineItemList{Data: []*stripesdk.InvoiceLineItem{
			{},
			{Subscription: &stripesdk.Subscription{ID: "sub_from_line"}},
		}},
	}
	if got := subscriptionIDOf(invoice); got != "sub_from_line" {
		t.Errorf("subscriptionIDOf = %q, want sub_from_line", got)
	}
}

// A one-off or quote-generated invoice has no subscription. That is not an
// error, and the handler skips it.
func TestSubscriptionIDOfReturnsEmptyForOneOffInvoices(t *testing.T) {
	for _, tt := range []struct {
		name    string
		invoice *stripesdk.Invoice
	}{
		{"no parent, no lines", &stripesdk.Invoice{ID: "in_abc"}},
		{"parent without subscription details", &stripesdk.Invoice{
			ID: "in_abc", Parent: &stripesdk.InvoiceParent{}}},
		{"empty lines", &stripesdk.Invoice{
			ID: "in_abc", Lines: &stripesdk.InvoiceLineItemList{}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := subscriptionIDOf(tt.invoice); got != "" {
				t.Errorf("subscriptionIDOf = %q, want empty", got)
			}
		})
	}
}

// The decline code is the actionable part; the generic message alone tells a
// customer nothing they can act on.
func TestDeclineReasonPrefersTheDeclineCode(t *testing.T) {
	tests := []struct {
		name   string
		err    *stripesdk.Error
		wantIn string
	}{
		{"decline code wins", &stripesdk.Error{
			Code: "card_declined", DeclineCode: "insufficient_funds", Msg: "no funds",
		}, "insufficient_funds"},
		{"code when there is no decline code", &stripesdk.Error{
			Code: "expired_card", Msg: "card expired",
		}, "expired_card"},
		{"message alone", &stripesdk.Error{Msg: "something went wrong"}, "something went wrong"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := declineReason(&stripesdk.Invoice{LastFinalizationError: tt.err})
			if got == "" {
				t.Fatal("no reason produced")
			}
			if !contains(got, tt.wantIn) {
				t.Errorf("reason = %q, want it to contain %q", got, tt.wantIn)
			}
		})
	}

	if got := declineReason(&stripesdk.Invoice{}); got != "" {
		t.Errorf("reason = %q for an invoice with no error, want empty", got)
	}
}

/* --- small helpers -------------------------------------------------------- */

// Stripe uses 0 for "not set" on every unix timestamp, so a zero must become
// nil rather than 1970.
func TestUnixPtr(t *testing.T) {
	if unixPtr(0) != nil {
		t.Error("a zero timestamp became a time rather than nil")
	}
	if unixPtr(-1) != nil {
		t.Error("a negative timestamp became a time rather than nil")
	}

	now := time.Now().Unix()
	got := unixPtr(now)
	if got == nil || got.Unix() != now {
		t.Errorf("unixPtr(%d) = %v", now, got)
	}
	if got.Location() != time.UTC {
		t.Errorf("timestamp is in %v, want UTC", got.Location())
	}
}

func TestCustomerIDOf(t *testing.T) {
	if got := customerIDOf(nil); got != "" {
		t.Errorf("customerIDOf(nil) = %q, want empty", got)
	}
	if got := customerIDOf(&stripesdk.Customer{ID: "cus_abc"}); got != "cus_abc" {
		t.Errorf("customerIDOf = %q, want cus_abc", got)
	}
}

func TestOutcomeAcknowledge(t *testing.T) {
	// Everything except a genuine failure has to be acknowledged, or Stripe
	// redelivers an event that needs no work.
	for _, o := range []Outcome{OutcomeProcessed, OutcomeDuplicate, OutcomeSkipped, OutcomeStale} {
		if !o.Acknowledge() {
			t.Errorf("%q is not acknowledged; Stripe would keep redelivering it", o)
		}
	}
	if OutcomeFailed.Acknowledge() {
		t.Error("a failure is acknowledged; Stripe would never retry it")
	}
}

// The handled set is the service's subscription list. A typo here means events
// are silently skipped, so pin the exact contents.
func TestHandledEventTypes(t *testing.T) {
	want := []string{
		"checkout.session.completed",
		"customer.subscription.created",
		"customer.subscription.updated",
		"customer.subscription.deleted",
		"customer.subscription.paused",
		"customer.subscription.resumed",
		"invoice.payment_failed",
		"invoice.payment_succeeded",
	}
	for _, e := range want {
		if !handledEventTypes[stripesdk.EventType(e)] {
			t.Errorf("%s is no longer handled", e)
		}
	}
	if len(handledEventTypes) != len(want) {
		t.Errorf("handled set has %d entries, want %d - update this test deliberately",
			len(handledEventTypes), len(want))
	}
	if handledEventTypes["invoice.created"] {
		t.Error("an unsubscribed event type is in the handled set")
	}
}

// SubscriptionView is what reaches a browser; the entity is not.
func TestSubscriptionViewWithholdsInternalFields(t *testing.T) {
	view := SubscriptionView{
		Status: domain.SubscriptionActive, IsActive: true,
		PriceID: "price_abc", Quantity: 1,
		CurrentPeriodEnd: time.Now(),
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, forbidden := range []string{
		"stripe_customer_id", "stripe_subscription_id", "user_id",
		"last_stripe_event", "last_invoice_event", "\"id\"",
	} {
		if contains(string(encoded), forbidden) {
			t.Errorf("SubscriptionView exposes %q:\n%s", forbidden, encoded)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}
