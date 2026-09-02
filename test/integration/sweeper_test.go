//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	stripesdk "github.com/stripe/stripe-go/v86"

	"github.com/mlkad/stripe-payment-service/internal/domain"
	repo "github.com/mlkad/stripe-payment-service/internal/repository/postgres"
	"github.com/mlkad/stripe-payment-service/internal/service"
	"github.com/mlkad/stripe-payment-service/internal/worker"
)

func newSweeper(t *testing.T, svc *service.WebhookService, cfg worker.SweeperConfig) *worker.WebhookSweeper {
	t.Helper()
	return worker.NewWebhookSweeper(
		repo.NewWebhookRepo(pool, 5*time.Minute), svc,
		cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// seedFailedEvent puts a webhook in the ledger as failed, the way a handler
// error would, with a payload the sweeper can replay.
func seedFailedEvent(t *testing.T, eventID, subID, customerID string, status domain.SubscriptionStatus, created time.Time) {
	t.Helper()
	ctx := context.Background()
	hooks := repo.NewWebhookRepo(pool, 5*time.Minute)

	payload := subscriptionEvent(eventID, "customer.subscription.updated", subID, customerID, status, created)
	record := &domain.ProcessedWebhook{
		EventID:         eventID,
		EventType:       "customer.subscription.updated",
		StripeCreatedAt: created,
		Payload:         json.RawMessage(payload),
	}
	if _, err := hooks.TryClaimEvent(ctx, record); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if err := hooks.MarkEventFailed(ctx, eventID, context.DeadlineExceeded); err != nil {
		t.Fatalf("seed failure: %v", err)
	}
}

// backdate ages a ledger row so the sweeper considers it due.
//
// trg_processed_webhooks_set_updated_at overwrites any value written to
// updated_at - which is the property the stale-claim and backoff checks rely
// on, since it means freshness cannot be forged. The trigger has to be dropped
// for the length of the write, which only a test would ever do.
func backdate(t *testing.T, eventID string, age time.Duration) {
	t.Helper()
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("backdate: begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, stmt := range []string{
		`ALTER TABLE processed_webhooks DISABLE TRIGGER trg_processed_webhooks_set_updated_at`,
		`UPDATE processed_webhooks SET updated_at = now() - make_interval(secs => $2) WHERE event_id = $1`,
		`ALTER TABLE processed_webhooks ENABLE TRIGGER trg_processed_webhooks_set_updated_at`,
	} {
		var err error
		if strings.HasPrefix(stmt, "UPDATE") {
			_, err = tx.Exec(ctx, stmt, eventID, age.Seconds())
		} else {
			_, err = tx.Exec(ctx, stmt)
		}
		if err != nil {
			t.Fatalf("backdate: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("backdate: commit: %v", err)
	}
}

func ledgerAttempts(t *testing.T, eventID string) int32 {
	t.Helper()
	var n int32
	if err := pool.QueryRow(context.Background(),
		`SELECT attempts FROM processed_webhooks WHERE event_id = $1`, eventID).Scan(&n); err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	return n
}

// The point of the whole mechanism: Stripe gives up after three days, so a bug
// fixed on day four leaves every event from days one to three permanently
// unprocessed. The sweeper replays them from the stored payload.
func TestSweeper_RecoversAFailedEvent(t *testing.T) {
	truncate(t)
	svc, _ := newWebhookStack(t)
	seedUserWithCustomer(t, "sweep@example.com", "cus_Sweep000001")

	now := time.Now().UTC().Truncate(time.Second)
	seedFailedEvent(t, "evt_Sweep000001", "sub_Sweep000001", "cus_Sweep000001",
		domain.SubscriptionPastDue, now)

	backdate(t, "evt_Sweep000001", time.Hour)

	result := newSweeper(t, svc, worker.SweeperConfig{
		MaxAttempts: 5, BaseBackoff: time.Second, MaxBackoff: time.Minute,
	}).Sweep(context.Background())

	if result.Retried != 1 || result.Recovered != 1 {
		t.Fatalf("retried=%d recovered=%d, want 1 and 1", result.Retried, result.Recovered)
	}
	if got := ledgerStatus(t, "evt_Sweep000001"); got != "succeeded" {
		t.Errorf("ledger status = %q, want succeeded", got)
	}

	// And the business effect actually landed.
	sub, err := repo.NewSubscriptionRepo(pool).GetSubscriptionByStripeID(
		context.Background(), "sub_Sweep000001")
	if err != nil {
		t.Fatalf("subscription was not created by the replay: %v", err)
	}
	if sub.Status != domain.SubscriptionPastDue {
		t.Errorf("status = %q, want past_due", sub.Status)
	}
}

// A claim left in 'processing' by a crashed worker is stuck: TryClaimEvent
// only reclaims it if Stripe redelivers, and Stripe stops after three days.
func TestSweeper_ReclaimsAbandonedClaims(t *testing.T) {
	truncate(t)
	svc, _ := newWebhookStack(t)
	ctx := context.Background()

	hooks := repo.NewWebhookRepo(pool, 5*time.Minute)
	now := time.Now().UTC().Truncate(time.Second)
	record := &domain.ProcessedWebhook{
		EventID: "evt_Abandoned01", EventType: "customer.subscription.updated",
		StripeCreatedAt: now,
		Payload:         json.RawMessage(subscriptionEvent("evt_Abandoned01", "customer.subscription.updated", "sub_Abandoned01", "cus_Abandoned01", domain.SubscriptionActive, now)),
	}
	if _, err := hooks.TryClaimEvent(ctx, record); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// The worker dies here, leaving the row in 'processing'.
	backdate(t, "evt_Abandoned01", 30*time.Minute)

	result := newSweeper(t, svc, worker.SweeperConfig{
		StaleClaimAfter: time.Minute, MaxAttempts: 5,
		BaseBackoff: time.Hour, MaxBackoff: time.Hour, // no retry this pass
	}).Sweep(ctx)

	if result.Reclaimed != 1 {
		t.Fatalf("reclaimed = %d, want 1", result.Reclaimed)
	}
	if got := ledgerStatus(t, "evt_Abandoned01"); got != "failed" {
		t.Errorf("status = %q, want failed - a reclaimed claim must become retryable", got)
	}
}

// A claim that is still being worked on must not be stolen, or two workers
// process the same event at once.
func TestSweeper_LeavesFreshClaimsAlone(t *testing.T) {
	truncate(t)
	svc, _ := newWebhookStack(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	record := &domain.ProcessedWebhook{
		EventID: "evt_Fresh000001", EventType: "customer.subscription.updated",
		StripeCreatedAt: now, Payload: json.RawMessage(`{"id":"evt_Fresh000001","object":"event"}`),
	}
	if _, err := repo.NewWebhookRepo(pool, 5*time.Minute).TryClaimEvent(ctx, record); err != nil {
		t.Fatalf("claim: %v", err)
	}

	result := newSweeper(t, svc, worker.SweeperConfig{
		StaleClaimAfter: 10 * time.Minute, MaxAttempts: 5,
		BaseBackoff: time.Second, MaxBackoff: time.Minute,
	}).Sweep(ctx)

	if result.Reclaimed != 0 {
		t.Errorf("reclaimed = %d, want 0: a claim inside its window was stolen", result.Reclaimed)
	}
	if got := ledgerStatus(t, "evt_Fresh000001"); got != "processing" {
		t.Errorf("status = %q, want processing", got)
	}
}

// Backoff exists so a failing event does not get hammered every tick.
func TestSweeper_HonoursBackoff(t *testing.T) {
	truncate(t)
	svc, _ := newWebhookStack(t)
	seedUserWithCustomer(t, "backoff@example.com", "cus_Backoff00001")

	now := time.Now().UTC().Truncate(time.Second)
	seedFailedEvent(t, "evt_Backoff00001", "sub_Backoff00001", "cus_Backoff00001",
		domain.SubscriptionActive, now)

	// Just failed, so a long backoff means it is not due yet.
	result := newSweeper(t, svc, worker.SweeperConfig{
		MaxAttempts: 5, BaseBackoff: time.Hour, MaxBackoff: 2 * time.Hour,
	}).Sweep(context.Background())

	if result.Retried != 0 {
		t.Errorf("retried = %d, want 0 while the backoff has not elapsed", result.Retried)
	}
	if got := ledgerAttempts(t, "evt_Backoff00001"); got != 1 {
		t.Errorf("attempts = %d, want 1 - the sweeper consumed an attempt during backoff", got)
	}
}

// Past the attempt budget an event is left alone. Something is wrong that
// another attempt will not fix, and the operator needs to see it.
func TestSweeper_StopsAtMaxAttemptsAndReportsDeadLetters(t *testing.T) {
	truncate(t)
	svc, _ := newWebhookStack(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	// No matching user, so the replay will keep failing.
	seedFailedEvent(t, "evt_Dead00000001", "sub_Dead00000001", "cus_Dead00000001",
		domain.SubscriptionActive, now)
	if _, err := pool.Exec(ctx,
		`UPDATE processed_webhooks SET attempts = 5 WHERE event_id = $1`, "evt_Dead00000001"); err != nil {
		t.Fatalf("exhaust attempts: %v", err)
	}
	backdate(t, "evt_Dead00000001", 24*time.Hour)

	sweeper := newSweeper(t, svc, worker.SweeperConfig{
		MaxAttempts: 5, BaseBackoff: time.Second, MaxBackoff: time.Minute,
	})
	result := sweeper.Sweep(ctx)

	if result.Retried != 0 {
		t.Errorf("retried = %d, want 0 past the attempt budget", result.Retried)
	}
	if result.Stats.DeadLettered != 1 {
		t.Errorf("dead_lettered = %d, want 1", result.Stats.DeadLettered)
	}
	if got := ledgerAttempts(t, "evt_Dead00000001"); got != 5 {
		t.Errorf("attempts = %d, want 5 - a dead letter was retried anyway", got)
	}

	// And the health check says a human is needed.
	if _, err := sweeper.HealthCheck(ctx); err == nil {
		t.Error("HealthCheck reported healthy with a dead-lettered event")
	}
}

// A clean ledger must produce no alarm and no work.
func TestSweeper_QuietOnACleanLedger(t *testing.T) {
	truncate(t)
	svc, _ := newWebhookStack(t)
	ctx := context.Background()

	sweeper := newSweeper(t, svc, worker.SweeperConfig{
		MaxAttempts: 5, BaseBackoff: time.Second, MaxBackoff: time.Minute,
	})
	result := sweeper.Sweep(ctx)

	if result.Reclaimed != 0 || result.Retried != 0 {
		t.Errorf("did work on a clean ledger: reclaimed=%d retried=%d", result.Reclaimed, result.Retried)
	}
	if result.Stats.Unsettled() != 0 {
		t.Errorf("unsettled = %d, want 0", result.Stats.Unsettled())
	}
	if _, err := sweeper.HealthCheck(ctx); err != nil {
		t.Errorf("HealthCheck on a clean ledger: %v", err)
	}
}

// An event whose payload was pruned by the retention policy cannot be
// replayed. It must be settled rather than left cycling forever.
func TestSweeper_SettlesEventsWithNoStoredPayload(t *testing.T) {
	truncate(t)
	svc, _ := newWebhookStack(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	seedFailedEvent(t, "evt_NoPayload001", "sub_NoPayload001", "cus_NoPayload001",
		domain.SubscriptionActive, now)
	if _, err := pool.Exec(ctx,
		`UPDATE processed_webhooks SET payload = NULL WHERE event_id = $1`, "evt_NoPayload001"); err != nil {
		t.Fatalf("prune payload: %v", err)
	}
	backdate(t, "evt_NoPayload001", time.Hour)

	newSweeper(t, svc, worker.SweeperConfig{
		MaxAttempts: 5, BaseBackoff: time.Second, MaxBackoff: time.Minute,
	}).Sweep(ctx)

	if got := ledgerStatus(t, "evt_NoPayload001"); got != "skipped" {
		t.Errorf("status = %q, want skipped - an unreplayable event was left cycling", got)
	}
}

// Two sweepers running at once must not both process the same event.
func TestSweeper_ConcurrentSweepsDoNotDoubleProcess(t *testing.T) {
	truncate(t)
	svc, _ := newWebhookStack(t)
	seedUserWithCustomer(t, "race@example.com", "cus_SweepRace001")
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	seedFailedEvent(t, "evt_SweepRace001", "sub_SweepRace001", "cus_SweepRace001",
		domain.SubscriptionActive, now)
	backdate(t, "evt_SweepRace001", time.Hour)

	cfg := worker.SweeperConfig{MaxAttempts: 5, BaseBackoff: time.Second, MaxBackoff: time.Minute}
	results := make(chan worker.SweepResult, 4)
	for i := 0; i < 4; i++ {
		go func() { results <- newSweeper(t, svc, cfg).Sweep(ctx) }()
	}

	total := 0
	for i := 0; i < 4; i++ {
		total += (<-results).Retried
	}
	if total != 1 {
		t.Errorf("the event was retried %d times across concurrent sweeps, want exactly 1", total)
	}
	if got := ledgerAttempts(t, "evt_SweepRace001"); got != 2 {
		t.Errorf("attempts = %d, want 2 (one original failure plus one retry)", got)
	}
}

// failingSubscriptionEvent renders a subscription event with no items. The
// mapper needs items for the price and the billing period, so dispatch fails
// every time - which is what makes an event exhaust its retry budget.
func failingSubscriptionEvent(eventID, subID string, created time.Time) []byte {
	body, _ := json.Marshal(map[string]any{
		"id": eventID, "object": "event", "api_version": stripesdk.APIVersion,
		"created": created.Unix(), "livemode": false,
		"type": "customer.subscription.updated",
		"data": map[string]any{"object": map[string]any{
			"id": subID, "object": "subscription", "customer": "cus_NoItems00001",
			"status": "active",
			"items":  map[string]any{"object": "list", "data": []any{}},
		}},
	})
	return body
}

// An event must never be retried past its budget, however many sweepers are
// running.
//
// This asserts the invariant, not the mechanism. ClaimEventForRetry re-checks
// attempts inside the UPDATE as defence in depth, but neutering that check does
// not make this test fail: the first claim flips the row to 'processing' well
// before the other sweepers reach their own claim, so the window where a stale
// listing could over-claim is far narrower than it looks. Verified by
// neutering it and re-running - three passes, no failure. The guard stays
// because the window is real, but nothing here proves it.
func TestSweeper_ClaimEnforcesTheAttemptBudgetUnderRace(t *testing.T) {
	truncate(t)
	svc, _ := newWebhookStack(t)
	ctx := context.Background()

	const maxAttempts = 3
	now := time.Now().UTC().Truncate(time.Second)

	hooks := repo.NewWebhookRepo(pool, 5*time.Minute)
	record := &domain.ProcessedWebhook{
		EventID: "evt_Budget000001", EventType: "customer.subscription.updated",
		StripeCreatedAt: now,
		Payload:         json.RawMessage(failingSubscriptionEvent("evt_Budget000001", "sub_Budget000001", now)),
	}
	if _, err := hooks.TryClaimEvent(ctx, record); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if err := hooks.MarkEventFailed(ctx, "evt_Budget000001", context.DeadlineExceeded); err != nil {
		t.Fatalf("seed failure: %v", err)
	}
	// One attempt below the budget, and due.
	if _, err := pool.Exec(ctx,
		`UPDATE processed_webhooks SET attempts = $2 WHERE event_id = $1`,
		"evt_Budget000001", maxAttempts-1); err != nil {
		t.Fatalf("set attempts: %v", err)
	}
	backdate(t, "evt_Budget000001", time.Hour)

	cfg := worker.SweeperConfig{
		MaxAttempts: maxAttempts, BaseBackoff: time.Second, MaxBackoff: time.Minute,
	}
	done := make(chan struct{}, 6)
	for i := 0; i < 6; i++ {
		go func() { newSweeper(t, svc, cfg).Sweep(ctx); done <- struct{}{} }()
	}
	for i := 0; i < 6; i++ {
		<-done
	}

	if got := ledgerAttempts(t, "evt_Budget000001"); got > maxAttempts {
		t.Errorf("attempts = %d, want at most %d: concurrent sweeps took the event past its "+
			"retry budget, so the claim is not re-checking attempts", got, maxAttempts)
	}
}
