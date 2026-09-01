//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mlkad/stripe-payment-service/internal/domain"
	repo "github.com/mlkad/stripe-payment-service/internal/repository/postgres"
)

func webhookEvent(id string) *domain.ProcessedWebhook {
	return &domain.ProcessedWebhook{
		EventID:         id,
		EventType:       "customer.subscription.updated",
		APIVersion:      ptr("2024-06-20"),
		Livemode:        false,
		StripeCreatedAt: time.Now().UTC().Truncate(time.Second),
		Payload:         json.RawMessage(`{"id":"` + id + `","object":"event"}`),
	}
}

// The claim lifecycle: a first delivery is claimed, a redelivery while the
// first is in flight is not, and a redelivery after success is not either.
// Every "not claimed" outcome must still be acknowledged to Stripe with a 2xx.
func TestWebhookRepo_ClaimLifecycle(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	hooks := repo.NewWebhookRepo(pool, 5*time.Minute)

	w := webhookEvent("evt_Claim001")
	claimed, err := hooks.TryClaimEvent(ctx, w)
	if err != nil || !claimed {
		t.Fatalf("first delivery: claimed=%v err=%v", claimed, err)
	}
	if w.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", w.Attempts)
	}

	inFlight, err := hooks.TryClaimEvent(ctx, webhookEvent("evt_Claim001"))
	if err != nil || inFlight {
		t.Fatalf("redelivery while in flight: claimed=%v err=%v, want false", inFlight, err)
	}

	if err := hooks.MarkEventProcessed(ctx, "evt_Claim001"); err != nil {
		t.Fatalf("MarkEventProcessed: %v", err)
	}

	settled, err := hooks.TryClaimEvent(ctx, webhookEvent("evt_Claim001"))
	if err != nil || settled {
		t.Fatalf("redelivery after success: claimed=%v err=%v, want false", settled, err)
	}
}

// A worker whose claim was taken over must not be able to overwrite the outcome
// recorded by whoever took it. Settling an event you no longer hold is an error.
func TestWebhookRepo_SettlingUnheldEventIsRejected(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	hooks := repo.NewWebhookRepo(pool, 5*time.Minute)

	if _, err := hooks.TryClaimEvent(ctx, webhookEvent("evt_Settle01")); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := hooks.MarkEventProcessed(ctx, "evt_Settle01"); err != nil {
		t.Fatalf("first settle: %v", err)
	}

	err := hooks.MarkEventProcessed(ctx, "evt_Settle01")
	if !errors.Is(err, domain.ErrEventNotClaimed) {
		t.Errorf("second MarkEventProcessed err = %v, want ErrEventNotClaimed", err)
	}
	err = hooks.MarkEventFailed(ctx, "evt_Settle01", errors.New("late"))
	if !errors.Is(err, domain.ErrEventNotClaimed) {
		t.Errorf("MarkEventFailed after success err = %v, want ErrEventNotClaimed", err)
	}
	if err := hooks.MarkEventProcessed(ctx, "evt_NeverSeen"); !errors.Is(err, domain.ErrEventNotClaimed) {
		t.Errorf("settling an unknown event err = %v, want ErrEventNotClaimed", err)
	}
}

func TestWebhookRepo_FailedEventIsReclaimable(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	hooks := repo.NewWebhookRepo(pool, 5*time.Minute)

	if _, err := hooks.TryClaimEvent(ctx, webhookEvent("evt_Fail001")); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := hooks.MarkEventFailed(ctx, "evt_Fail001", errors.New("downstream timeout")); err != nil {
		t.Fatalf("MarkEventFailed: %v", err)
	}

	w := webhookEvent("evt_Fail001")
	reclaimed, err := hooks.TryClaimEvent(ctx, w)
	if err != nil || !reclaimed {
		t.Fatalf("reclaim after failure: claimed=%v err=%v", reclaimed, err)
	}
	if w.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", w.Attempts)
	}

	var payload string
	if err := pool.QueryRow(ctx,
		`SELECT payload::text FROM processed_webhooks WHERE event_id = 'evt_Fail001'`,
	).Scan(&payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if payload == "" || payload == "null" {
		t.Errorf("jsonb payload not persisted: %q", payload)
	}
}

// last_error is bounded before it reaches the column: driver errors can carry an
// entire query plus parameters, and an operator reads this field, not a machine.
func TestWebhookRepo_LastErrorIsTruncated(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	hooks := repo.NewWebhookRepo(pool, 5*time.Minute)

	if _, err := hooks.TryClaimEvent(ctx, webhookEvent("evt_Long001")); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := hooks.MarkEventFailed(ctx, "evt_Long001", errors.New(strings.Repeat("x", 10_000))); err != nil {
		t.Fatalf("MarkEventFailed: %v", err)
	}

	var stored string
	if err := pool.QueryRow(ctx,
		`SELECT last_error FROM processed_webhooks WHERE event_id = 'evt_Long001'`,
	).Scan(&stored); err != nil {
		t.Fatalf("read last_error: %v", err)
	}
	if len([]rune(stored)) > 2001 {
		t.Errorf("last_error length = %d runes, want <= 2001", len([]rune(stored)))
	}
}

// A claim left in 'processing' by a crashed worker must become reclaimable once
// it goes stale, or the event is stuck forever.
//
// The window is shortened rather than backdating updated_at, because
// trg_processed_webhooks_set_updated_at overwrites any value written to that
// column - which is itself the property the stale check relies on: updated_at
// always reflects the last real touch of the row, so it cannot be faked into
// looking fresh either.
func TestWebhookRepo_StaleClaimIsReclaimable(t *testing.T) {
	truncate(t)
	ctx := context.Background()

	const window = 300 * time.Millisecond
	hooks := repo.NewWebhookRepo(pool, window)

	if _, err := hooks.TryClaimEvent(ctx, webhookEvent("evt_Stale01")); err != nil {
		t.Fatalf("claim: %v", err)
	}
	fresh, err := hooks.TryClaimEvent(ctx, webhookEvent("evt_Stale01"))
	if err != nil || fresh {
		t.Fatalf("claim still fresh: claimed=%v err=%v, want false", fresh, err)
	}

	time.Sleep(2 * window)

	w := webhookEvent("evt_Stale01")
	reclaimed, err := hooks.TryClaimEvent(ctx, w)
	if err != nil || !reclaimed {
		t.Fatalf("stale claim: claimed=%v err=%v, want true", reclaimed, err)
	}
	if w.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", w.Attempts)
	}
}

// event_id is the primary key, so the unique index performs the mutual
// exclusion: no advisory lock, no application-side coordination. Exactly one
// caller may win, and the losers must not inflate the attempt counter.
func TestWebhookRepo_ConcurrentDeliveriesElectOneWinner(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	hooks := repo.NewWebhookRepo(pool, 5*time.Minute)

	const workers = 50
	var (
		wins  atomic.Int64
		wg    sync.WaitGroup
		mu    sync.Mutex
		errs  []error
		start = make(chan struct{})
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, err := hooks.TryClaimEvent(ctx, webhookEvent("evt_Race001"))
			switch {
			case err != nil:
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			case ok:
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d workers errored, first: %v", len(errs), errs[0])
	}
	if got := wins.Load(); got != 1 {
		t.Errorf("winners = %d, want exactly 1", got)
	}

	var attempts int32
	if err := pool.QueryRow(ctx,
		`SELECT attempts FROM processed_webhooks WHERE event_id = 'evt_Race001'`,
	).Scan(&attempts); err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1: losing workers inflated the counter", attempts)
	}
}
