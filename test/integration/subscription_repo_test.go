//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mlkad/stripe-payment-service/internal/domain"
	repo "github.com/mlkad/stripe-payment-service/internal/repository/postgres"
)

// seedSubscription creates a user and one active subscription, returning the
// subscription and the period start it was anchored to.
func seedSubscription(t *testing.T, subID string) (*domain.Subscription, time.Time) {
	t.Helper()
	ctx := context.Background()

	u := &domain.User{Email: fmt.Sprintf("%s@example.com", subID)}
	if err := repo.NewUserRepo(pool).CreateUser(ctx, u); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	// The *_format_chk constraints accept only `^(sub|cus)_[A-Za-z0-9]+$`, so the
	// customer id is derived from the suffix rather than the whole subscription id.
	base := time.Now().UTC().Truncate(time.Second)
	s := &domain.Subscription{
		UserID:               u.ID,
		StripeSubscriptionID: subID,
		StripeCustomerID:     "cus_" + strings.TrimPrefix(subID, "sub_"),
		StripePriceID:        "price_Pro001",
		Status:               domain.SubscriptionActive,
		Quantity:             1,
		Currency:             ptr("usd"),
		UnitAmount:           ptr(int64(2900)),
		CurrentPeriodStart:   base,
		CurrentPeriodEnd:     base.Add(30 * 24 * time.Hour),
		Metadata:             map[string]string{"seat": "1"},
	}
	if err := repo.NewSubscriptionRepo(pool).CreateSubscription(ctx, s); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	return s, base
}

func TestSubscriptionRepo_CreateAndRead(t *testing.T) {
	truncate(t)
	s, _ := seedSubscription(t, "sub_Read001")
	if s.ID == uuid.Nil {
		t.Fatal("CreateSubscription did not populate ID")
	}

	got, err := repo.NewSubscriptionRepo(pool).GetSubscriptionByStripeID(context.Background(), "sub_Read001")
	if err != nil {
		t.Fatalf("GetSubscriptionByStripeID: %v", err)
	}
	if got.Status != domain.SubscriptionActive {
		t.Errorf("status = %q, want active", got.Status)
	}
	if !got.IsLive() {
		t.Error("IsLive() = false for an active subscription")
	}
}

func TestSubscriptionRepo_ConstraintsMapToDomainErrors(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	subs := repo.NewSubscriptionRepo(pool)
	seeded, base := seedSubscription(t, "sub_Dup001")

	t.Run("duplicate stripe id", func(t *testing.T) {
		err := subs.CreateSubscription(ctx, &domain.Subscription{
			UserID: seeded.UserID, StripeSubscriptionID: "sub_Dup001", StripeCustomerID: "cus_x",
			StripePriceID: "price_x", Status: domain.SubscriptionActive, Quantity: 1,
			CurrentPeriodStart: base, CurrentPeriodEnd: base.Add(time.Hour),
		})
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
	})

	t.Run("orphan user_id violates FK", func(t *testing.T) {
		err := subs.CreateSubscription(ctx, &domain.Subscription{
			UserID: uuid.New(), StripeSubscriptionID: "sub_Orphan1", StripeCustomerID: "cus_x",
			StripePriceID: "price_x", Status: domain.SubscriptionActive, Quantity: 1,
			CurrentPeriodStart: base, CurrentPeriodEnd: base.Add(time.Hour),
		})
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
	})

	t.Run("unknown subscription", func(t *testing.T) {
		_, err := subs.UpdateSubscriptionStatus(ctx, repo.SubscriptionStatusUpdate{
			StripeSubscriptionID: "sub_DoesNotExist",
			Status:               domain.SubscriptionActive,
			EventID:              "evt_X",
			EventCreatedAt:       base,
		})
		if !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}

// Stripe delivers events out of order. An event older than the one already
// applied must be rejected and must leave the row untouched.
func TestSubscriptionRepo_RejectsStaleEvent(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	subs := repo.NewSubscriptionRepo(pool)
	_, base := seedSubscription(t, "sub_Order001")

	t2 := base.Add(2 * time.Hour)
	updated, err := subs.UpdateSubscriptionStatus(ctx, repo.SubscriptionStatusUpdate{
		StripeSubscriptionID: "sub_Order001",
		Status:               domain.SubscriptionPastDue,
		EventID:              "evt_T2",
		EventCreatedAt:       t2,
	})
	if err != nil || updated.Status != domain.SubscriptionPastDue {
		t.Fatalf("apply T2: status=%v err=%v", updated, err)
	}

	t1 := base.Add(1 * time.Hour)
	_, err = subs.UpdateSubscriptionStatus(ctx, repo.SubscriptionStatusUpdate{
		StripeSubscriptionID: "sub_Order001",
		Status:               domain.SubscriptionCanceled,
		CanceledAt:           &t1,
		EventID:              "evt_T1",
		EventCreatedAt:       t1,
	})
	if !errors.Is(err, domain.ErrStaleEvent) {
		t.Fatalf("err = %v, want ErrStaleEvent", err)
	}

	after, err := subs.GetSubscriptionByStripeID(ctx, "sub_Order001")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if after.Status != domain.SubscriptionPastDue {
		t.Errorf("stale event mutated the row: status = %q, want past_due", after.Status)
	}
	if after.CanceledAt != nil {
		t.Errorf("stale event wrote canceled_at = %v", after.CanceledAt)
	}
}

// event.created has one-second resolution, so two distinct events for the same
// subscription routinely share a timestamp. Rejecting equality would drop the
// second one on the floor.
func TestSubscriptionRepo_AppliesEventWithEqualTimestamp(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	subs := repo.NewSubscriptionRepo(pool)
	_, base := seedSubscription(t, "sub_SameSec1")

	at := base.Add(time.Hour)
	for _, ev := range []struct {
		id     string
		status domain.SubscriptionStatus
	}{
		{"evt_A", domain.SubscriptionPastDue},
		{"evt_B", domain.SubscriptionActive},
	} {
		got, err := subs.UpdateSubscriptionStatus(ctx, repo.SubscriptionStatusUpdate{
			StripeSubscriptionID: "sub_SameSec1",
			Status:               ev.status,
			EventID:              ev.id,
			EventCreatedAt:       at,
		})
		if err != nil {
			t.Fatalf("apply %s: %v", ev.id, err)
		}
		if got.Status != ev.status {
			t.Fatalf("after %s: status = %q, want %q", ev.id, got.Status, ev.status)
		}
	}
}

// Stripe events vary in which fields they populate. A nil pointer means "not
// mentioned", and COALESCE must preserve the stored value rather than blank it.
func TestSubscriptionRepo_PartialUpdatePreservesUnmentionedFields(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	subs := repo.NewSubscriptionRepo(pool)
	_, base := seedSubscription(t, "sub_Partial1")

	got, err := subs.UpdateSubscriptionStatus(ctx, repo.SubscriptionStatusUpdate{
		StripeSubscriptionID: "sub_Partial1",
		Status:               domain.SubscriptionPastDue,
		EventID:              "evt_P1",
		EventCreatedAt:       base.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("UpdateSubscriptionStatus: %v", err)
	}

	if got.Currency == nil || *got.Currency != "usd" {
		t.Errorf("currency = %v, want usd", got.Currency)
	}
	if got.UnitAmount == nil || *got.UnitAmount != 2900 {
		t.Errorf("unit_amount = %v, want 2900", got.UnitAmount)
	}
	if got.StripePriceID != "price_Pro001" {
		t.Errorf("stripe_price_id = %q, want price_Pro001", got.StripePriceID)
	}
	if got.LastStripeEventID == nil || *got.LastStripeEventID != "evt_P1" {
		t.Errorf("last_stripe_event_id = %v, want evt_P1", got.LastStripeEventID)
	}
}

// The staleness check reads last_stripe_event_at, decides, then writes. Under
// READ COMMITTED that read-decide-write is only atomic because the row is held
// with SELECT ... FOR UPDATE; without the lock, concurrent workers all observe
// the same pre-state and the last committer wins regardless of event order.
//
// Whatever the interleaving, the row must end up holding the newest event.
// Removing FOR UPDATE from the repository fails every round of this test.
func TestSubscriptionRepo_ConcurrentOutOfOrderEventsConverge(t *testing.T) {
	ctx := context.Background()
	subs := repo.NewSubscriptionRepo(pool)

	const rounds, workers = 8, 24
	statuses := []domain.SubscriptionStatus{
		domain.SubscriptionTrialing, domain.SubscriptionActive,
		domain.SubscriptionPastDue, domain.SubscriptionUnpaid,
	}

	for round := range rounds {
		truncate(t)
		subID := fmt.Sprintf("sub_Race%02d", round)
		_, base := seedSubscription(t, subID)

		type event struct {
			at     time.Time
			id     string
			status domain.SubscriptionStatus
		}
		events := make([]event, workers)
		for i := range events {
			events[i] = event{
				at:     base.Add(time.Duration(i+1) * time.Minute),
				id:     fmt.Sprintf("evt_R%02d", i),
				status: statuses[i%len(statuses)],
			}
		}
		newest := events[workers-1]
		rand.Shuffle(len(events), func(i, j int) { events[i], events[j] = events[j], events[i] })

		var (
			wg    sync.WaitGroup
			mu    sync.Mutex
			errs  []error
			start = make(chan struct{})
		)
		for _, e := range events {
			wg.Add(1)
			go func(e event) {
				defer wg.Done()
				<-start
				_, err := subs.UpdateSubscriptionStatus(ctx, repo.SubscriptionStatusUpdate{
					StripeSubscriptionID: subID,
					Status:               e.status,
					EventID:              e.id,
					EventCreatedAt:       e.at,
				})
				// Losing to a newer event is the expected outcome, not a failure.
				if err != nil && !errors.Is(err, domain.ErrStaleEvent) {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			}(e)
		}
		close(start)
		wg.Wait()

		if len(errs) > 0 {
			t.Fatalf("round %d: %d unexpected errors, first: %v", round, len(errs), errs[0])
		}

		final, err := subs.GetSubscriptionByStripeID(ctx, subID)
		if err != nil {
			t.Fatalf("round %d: read back: %v", round, err)
		}
		if final.LastStripeEventID == nil || *final.LastStripeEventID != newest.id {
			t.Errorf("round %d: last_stripe_event_id = %v, want %s",
				round, derefString(final.LastStripeEventID), newest.id)
		}
		if final.LastStripeEventAt == nil || !final.LastStripeEventAt.Equal(newest.at) {
			t.Errorf("round %d: last_stripe_event_at = %v, want %v",
				round, final.LastStripeEventAt, newest.at)
		}
		if final.Status != newest.status {
			t.Errorf("round %d: status = %q, want %q", round, final.Status, newest.status)
		}
	}
}

func derefString(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
