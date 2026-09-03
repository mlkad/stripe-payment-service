//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mlkad/stripe-payment-service/internal/domain"
	repo "github.com/mlkad/stripe-payment-service/internal/repository/postgres"
	"github.com/mlkad/stripe-payment-service/internal/worker"
)

func newRetention(t *testing.T, cfg worker.RetentionConfig) *worker.RetentionWorker {
	t.Helper()
	return worker.NewRetentionWorker(
		repo.NewWebhookRepo(pool, 5*time.Minute),
		repo.NewRefreshTokenRepo(pool), cfg,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// piiPayload is a Stripe event carrying the personal data a real one would:
// email, name, postal address, and card metadata.
func piiPayload(eventID string) []byte {
	body, _ := json.Marshal(map[string]any{
		"id": eventID, "object": "event", "type": "checkout.session.completed",
		"created": time.Now().Unix(), "livemode": false, "api_version": "2026-08-26.dahlia",
		"data": map[string]any{"object": map[string]any{
			"id":             "cs_test_PII0001",
			"object":         "checkout.session",
			"customer_email": "ada.lovelace@example.com",
			"customer_details": map[string]any{
				"email": "ada.lovelace@example.com",
				"name":  "Ada Lovelace",
				"phone": "+44 20 7946 0958",
				"address": map[string]any{
					"line1":       "12 Marylebone Road",
					"city":        "London",
					"postal_code": "NW1 5JD",
					"country":     "GB",
				},
			},
			"billing_details": map[string]any{
				"name":  "Ada Lovelace",
				"email": "ada.lovelace@example.com",
			},
			"payment_method_details": map[string]any{
				"card": map[string]any{"last4": "4242", "brand": "visa"},
			},
		}},
	})
	return body
}

// seedLedgerRow inserts a webhook row directly, with control over its age and
// status. received_at has no trigger on it, unlike updated_at.
func seedLedgerRow(t *testing.T, eventID string, status domain.WebhookStatus, age time.Duration, payload []byte) {
	t.Helper()
	processedAt := "now()"
	if status == domain.WebhookFailed || status == domain.WebhookProcessing {
		processedAt = "NULL"
	}
	lastError := "NULL"
	if status == domain.WebhookFailed {
		lastError = "'downstream timeout'"
	}

	_, err := pool.Exec(context.Background(), `
		INSERT INTO processed_webhooks
			(event_id, event_type, status, attempts, last_error, payload,
			 stripe_created_at, received_at, processed_at)
		VALUES ($1, 'checkout.session.completed', $2, 1, `+lastError+`, $3::jsonb,
		        now() - make_interval(secs => $4), now() - make_interval(secs => $4), `+processedAt+`)`,
		eventID, string(status), string(payload), age.Seconds())
	if err != nil {
		t.Fatalf("seed ledger row %s: %v", eventID, err)
	}
}

func storedPayload(t *testing.T, eventID string) string {
	t.Helper()
	var payload *string
	if err := pool.QueryRow(context.Background(),
		`SELECT payload::text FROM processed_webhooks WHERE event_id = $1`, eventID).Scan(&payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if payload == nil {
		return ""
	}
	return *payload
}

func purgedAt(t *testing.T, eventID string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT payload_purged_at FROM processed_webhooks WHERE event_id = $1`, eventID).Scan(&at); err != nil {
		t.Fatalf("read payload_purged_at: %v", err)
	}
	return at
}

// The whole point: no personal data survives the purge.
//
// Asserted by searching for each value rather than by checking a field list,
// so a field added to the seed - or a Stripe payload shape nobody anticipated -
// still has to disappear.
func TestRetention_RemovesPersonalDataFromSettledEvents(t *testing.T) {
	truncate(t)
	seedLedgerRow(t, "evt_Pii00000001", domain.WebhookSucceeded, 60*24*time.Hour, piiPayload("evt_Pii00000001"))

	result := newRetention(t, worker.RetentionConfig{
		SettledAfter: 30 * 24 * time.Hour, UnsettledAfter: 90 * 24 * time.Hour, BatchSize: 100,
	}).RunOnce(context.Background())

	if result.Settled != 1 {
		t.Fatalf("purged %d settled payload(s), want 1", result.Settled)
	}

	stored := storedPayload(t, "evt_Pii00000001")
	for _, secret := range []string{
		"ada.lovelace@example.com", "Ada Lovelace", "+44 20 7946 0958",
		"12 Marylebone Road", "London", "NW1 5JD",
		"customer_details", "billing_details", "payment_method_details", "4242",
	} {
		if strings.Contains(stored, secret) {
			t.Errorf("purged payload still contains %q:\n%s", secret, stored)
		}
	}

	// What survives has to be enough to say what the event was.
	var skeleton map[string]any
	if err := json.Unmarshal([]byte(stored), &skeleton); err != nil {
		t.Fatalf("skeleton is not JSON: %q", stored)
	}
	if skeleton["id"] != "evt_Pii00000001" || skeleton["type"] != "checkout.session.completed" {
		t.Errorf("skeleton lost the event identity: %v", skeleton)
	}
	if purgedAt(t, "evt_Pii00000001") == nil {
		t.Error("payload_purged_at was not stamped; there is no evidence minimisation ran")
	}
}

// A settled event inside its window keeps its payload, so a recent problem can
// still be investigated.
func TestRetention_KeepsRecentSettledPayloads(t *testing.T) {
	truncate(t)
	seedLedgerRow(t, "evt_Recent00001", domain.WebhookSucceeded, 24*time.Hour, piiPayload("evt_Recent00001"))

	newRetention(t, worker.RetentionConfig{
		SettledAfter: 30 * 24 * time.Hour, UnsettledAfter: 90 * 24 * time.Hour, BatchSize: 100,
	}).RunOnce(context.Background())

	if !strings.Contains(storedPayload(t, "evt_Recent00001"), "ada.lovelace@example.com") {
		t.Error("a payload inside its retention window was purged")
	}
	if purgedAt(t, "evt_Recent00001") != nil {
		t.Error("payload_purged_at was stamped on a row that should not have been touched")
	}
}

// The sweeper replays failed events from this column. A failed event older than
// the settled window must keep its payload, or retention silently destroys the
// recovery mechanism.
func TestRetention_KeepsFailedPayloadsTheSweeperStillNeeds(t *testing.T) {
	truncate(t)
	// Well past the 30-day settled window, well inside the 90-day outer bound.
	seedLedgerRow(t, "evt_Failed00001", domain.WebhookFailed, 45*24*time.Hour, piiPayload("evt_Failed00001"))

	result := newRetention(t, worker.RetentionConfig{
		SettledAfter: 30 * 24 * time.Hour, UnsettledAfter: 90 * 24 * time.Hour, BatchSize: 100,
	}).RunOnce(context.Background())

	if result.Total() != 0 {
		t.Fatalf("purged %d payload(s); a failed event inside the outer bound must keep its payload", result.Total())
	}
	if !strings.Contains(storedPayload(t, "evt_Failed00001"), "ada.lovelace@example.com") {
		t.Error("a replayable failed event lost its payload")
	}
}

// The outer bound is not optional. Without it, one permanently dead-lettered
// event holds personal data forever.
func TestRetention_PurgesUnresolvedEventsPastTheOuterBound(t *testing.T) {
	truncate(t)
	seedLedgerRow(t, "evt_Dead00000001", domain.WebhookFailed, 120*24*time.Hour, piiPayload("evt_Dead00000001"))

	result := newRetention(t, worker.RetentionConfig{
		SettledAfter: 30 * 24 * time.Hour, UnsettledAfter: 90 * 24 * time.Hour, BatchSize: 100,
	}).RunOnce(context.Background())

	if result.Unsettled != 1 {
		t.Fatalf("purged %d unresolved payload(s), want 1", result.Unsettled)
	}
	if strings.Contains(storedPayload(t, "evt_Dead00000001"), "ada.lovelace@example.com") {
		t.Error("an unresolved event past the outer bound kept its personal data")
	}
	// The ledger row itself must survive: event_id is the idempotency key.
	if got := ledgerStatus(t, "evt_Dead00000001"); got != "failed" {
		t.Errorf("ledger status = %q, want failed - the row must outlive its payload", got)
	}
}

// An in-flight claim must never lose its payload: the sweeper may be replaying
// it right now, and taking the payload away mid-replay turns a recoverable
// event into an unreplayable one.
func TestRetention_NeverTouchesInFlightClaims(t *testing.T) {
	truncate(t)
	seedLedgerRow(t, "evt_InFlight0001", domain.WebhookProcessing, 200*24*time.Hour, piiPayload("evt_InFlight0001"))

	result := newRetention(t, worker.RetentionConfig{
		SettledAfter: time.Hour, UnsettledAfter: time.Hour, BatchSize: 100,
	}).RunOnce(context.Background())

	if result.Total() != 0 {
		t.Fatalf("purged %d payload(s); a processing claim must never be touched", result.Total())
	}
	if !strings.Contains(storedPayload(t, "evt_InFlight0001"), "ada.lovelace@example.com") {
		t.Error("an in-flight claim lost its payload")
	}
}

// Retention must be idempotent: a second pass finds nothing and does not
// re-stamp rows it already handled.
func TestRetention_IsIdempotent(t *testing.T) {
	truncate(t)
	seedLedgerRow(t, "evt_Twice0000001", domain.WebhookSucceeded, 60*24*time.Hour, piiPayload("evt_Twice0000001"))

	w := newRetention(t, worker.RetentionConfig{
		SettledAfter: 30 * 24 * time.Hour, UnsettledAfter: 90 * 24 * time.Hour, BatchSize: 100,
	})
	if first := w.RunOnce(context.Background()); first.Total() != 1 {
		t.Fatalf("first pass purged %d, want 1", first.Total())
	}
	stamped := purgedAt(t, "evt_Twice0000001")

	if second := w.RunOnce(context.Background()); second.Total() != 0 {
		t.Errorf("second pass purged %d, want 0", second.Total())
	}
	if after := purgedAt(t, "evt_Twice0000001"); after == nil || !after.Equal(*stamped) {
		t.Error("a second pass re-stamped payload_purged_at")
	}
}

// A backlog larger than one batch has to be cleared in a single pass, or a
// deployment that ran without retention never catches up.
func TestRetention_ClearsABacklogLargerThanOneBatch(t *testing.T) {
	truncate(t)
	for i := 0; i < 25; i++ {
		seedLedgerRow(t, fmt.Sprintf("evt_Backlog%05d", i),
			domain.WebhookSucceeded, 60*24*time.Hour, piiPayload("evt_Backlog"))
	}

	result := newRetention(t, worker.RetentionConfig{
		SettledAfter: 30 * 24 * time.Hour, UnsettledAfter: 90 * 24 * time.Hour,
		BatchSize: 10, // three full batches plus a short one
	}).RunOnce(context.Background())

	if result.Total() != 25 {
		t.Errorf("purged %d of 25 in one pass; the backlog is not being drained", result.Total())
	}

	stats, err := newRetention(t, worker.RetentionConfig{
		SettledAfter: 30 * 24 * time.Hour, UnsettledAfter: 90 * 24 * time.Hour,
	}).Stats(context.Background())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.DueNow != 0 {
		t.Errorf("due_now = %d after a full pass, want 0", stats.DueNow)
	}
	if stats.Purged != 25 {
		t.Errorf("purged count = %d, want 25", stats.Purged)
	}
}

// The sweeper already handles a missing payload; this checks the two features
// agree once retention has actually run.
func TestRetention_PurgedEventIsSettledByTheSweeperNotRetriedForever(t *testing.T) {
	truncate(t)
	svc, _ := newWebhookStack(t)
	ctx := context.Background()

	seedLedgerRow(t, "evt_Purged000001", domain.WebhookFailed, 120*24*time.Hour, piiPayload("evt_Purged000001"))
	newRetention(t, worker.RetentionConfig{
		SettledAfter: 30 * 24 * time.Hour, UnsettledAfter: 90 * 24 * time.Hour, BatchSize: 100,
	}).RunOnce(ctx)

	// The skeleton is still valid JSON but carries no subscribed event type,
	// so the sweeper settles it rather than cycling.
	backdate(t, "evt_Purged000001", time.Hour)
	newSweeper(t, svc, worker.SweeperConfig{
		MaxAttempts: 5, BaseBackoff: time.Second, MaxBackoff: time.Minute,
	}).Sweep(ctx)

	if got := ledgerStatus(t, "evt_Purged000001"); got == "failed" {
		t.Error("a purged event is still cycling as failed instead of being settled")
	}
}
