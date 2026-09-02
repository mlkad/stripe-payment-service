package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mlkad/stripe-payment-service/internal/domain"
)

// SubscriptionRepository is the port the service layer depends on.
type SubscriptionRepository interface {
	CreateSubscription(ctx context.Context, s *domain.Subscription) error
	GetSubscriptionByStripeID(ctx context.Context, stripeSubscriptionID string) (*domain.Subscription, error)
	GetLatestSubscriptionByUserID(ctx context.Context, userID uuid.UUID) (*domain.Subscription, error)
	UpdateSubscriptionStatus(ctx context.Context, in SubscriptionStatusUpdate) (*domain.Subscription, error)
}

// SubscriptionStatusUpdate carries one Stripe subscription event.
//
// Every field below Status is optional: a nil pointer leaves the stored value
// untouched. Stripe events vary in which fields they populate, and a partial
// event must not blank columns it simply did not mention.
type SubscriptionStatusUpdate struct {
	StripeSubscriptionID string
	Status               domain.SubscriptionStatus

	// EventID and EventCreatedAt come from the Stripe event envelope and drive
	// the staleness check.
	EventID        string
	EventCreatedAt time.Time

	CurrentPeriodStart     *time.Time
	CurrentPeriodEnd       *time.Time
	CancelAtPeriodEnd      *bool
	CancelAt               *time.Time
	CanceledAt             *time.Time
	EndedAt                *time.Time
	TrialEnd               *time.Time
	StripePriceID          *string
	Quantity               *int32
	LatestInvoiceID        *string
	DefaultPaymentMethodID *string
}

type SubscriptionRepo struct {
	pool *pgxpool.Pool
}

func NewSubscriptionRepo(pool *pgxpool.Pool) *SubscriptionRepo {
	return &SubscriptionRepo{pool: pool}
}

var _ SubscriptionRepository = (*SubscriptionRepo)(nil)

const subscriptionColumns = `
	id, user_id, stripe_subscription_id, stripe_customer_id, stripe_price_id,
	stripe_product_id, status, quantity, currency, unit_amount,
	current_period_start, current_period_end, cancel_at_period_end, cancel_at,
	canceled_at, ended_at, trial_start, trial_end, latest_invoice_id,
	default_payment_method_id, last_stripe_event_id, last_stripe_event_at,
	metadata, created_at, updated_at`

func (r *SubscriptionRepo) CreateSubscription(ctx context.Context, s *domain.Subscription) error {
	if err := s.Validate(); err != nil {
		return err
	}

	const query = `
		INSERT INTO subscriptions (
			user_id, stripe_subscription_id, stripe_customer_id, stripe_price_id,
			stripe_product_id, status, quantity, currency, unit_amount,
			current_period_start, current_period_end, cancel_at_period_end,
			cancel_at, canceled_at, ended_at, trial_start, trial_end,
			latest_invoice_id, default_payment_method_id,
			last_stripe_event_id, last_stripe_event_at, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
		        $15, $16, $17, $18, $19, $20, $21, COALESCE($22, '{}'::jsonb))
		RETURNING id, created_at, updated_at`

	err := r.pool.QueryRow(ctx, query,
		s.UserID, s.StripeSubscriptionID, s.StripeCustomerID, s.StripePriceID,
		s.StripeProductID, s.Status, s.Quantity, s.Currency, s.UnitAmount,
		s.CurrentPeriodStart, s.CurrentPeriodEnd, s.CancelAtPeriodEnd,
		s.CancelAt, s.CanceledAt, s.EndedAt, s.TrialStart, s.TrialEnd,
		s.LatestInvoiceID, s.DefaultPaymentMethodID,
		s.LastStripeEventID, s.LastStripeEventAt, s.Metadata,
	).Scan(&s.ID, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return mapError("create subscription", err)
	}
	return nil
}

func (r *SubscriptionRepo) GetSubscriptionByStripeID(ctx context.Context, stripeSubscriptionID string) (*domain.Subscription, error) {
	const query = `SELECT ` + subscriptionColumns + `
		FROM subscriptions
		WHERE stripe_subscription_id = $1`

	s, err := scanSubscription(r.pool.QueryRow(ctx, query, stripeSubscriptionID))
	if err != nil {
		return nil, mapError("get subscription by stripe id", err)
	}
	return s, nil
}

// GetLatestSubscriptionByUserID returns the user's most recent subscription
// whatever its status, so a dashboard can render "canceled" rather than an
// empty state. The ORDER BY matches idx_subscriptions_user_id_created_at.
//
// Deliberately not filtered to live statuses: that would use the smaller
// partial index but make a lapsed subscriber indistinguishable from someone who
// never subscribed, and those need different messaging.
func (r *SubscriptionRepo) GetLatestSubscriptionByUserID(ctx context.Context, userID uuid.UUID) (*domain.Subscription, error) {
	const query = `SELECT ` + subscriptionColumns + `
		FROM subscriptions
		WHERE user_id = $1
		ORDER BY created_at DESC
		LIMIT 1`

	s, err := scanSubscription(r.pool.QueryRow(ctx, query, userID))
	if err != nil {
		return nil, mapError("get latest subscription by user id", err)
	}
	return s, nil
}

// UpdateSubscriptionStatus applies one Stripe event, rejecting events older
// than the one already recorded on the row.
//
// The row is locked with SELECT ... FOR UPDATE and the staleness comparison
// happens between the lock and the write. Doing the comparison inside the
// UPDATE's WHERE clause instead would be wrong in a subtler way than it looks:
// two workers handling different events for the same subscription both read the
// old last_stripe_event_at under READ COMMITTED, and whichever commits second
// wins regardless of event order.
//
// Returns domain.ErrNotFound if no such subscription exists, and
// domain.ErrStaleEvent if the event is older than the applied one. A stale
// event is a normal consequence of at-least-once unordered delivery - the
// caller should acknowledge it to Stripe, not retry it.
func (r *SubscriptionRepo) UpdateSubscriptionStatus(ctx context.Context, in SubscriptionStatusUpdate) (*domain.Subscription, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, mapError("update subscription status: begin", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var appliedAt *time.Time
	err = tx.QueryRow(ctx,
		`SELECT last_stripe_event_at FROM subscriptions WHERE stripe_subscription_id = $1 FOR UPDATE`,
		in.StripeSubscriptionID,
	).Scan(&appliedAt)
	if err != nil {
		return nil, mapError("update subscription status: lock row", err)
	}

	// Strictly-older events are rejected; equal timestamps are applied. Stripe
	// reports event.created at one-second resolution, so two genuinely distinct
	// events for the same subscription routinely share a timestamp. Rejecting
	// equality would silently drop the second one.
	if appliedAt != nil && in.EventCreatedAt.Before(*appliedAt) {
		return nil, domain.ErrStaleEvent
	}

	const query = `
		UPDATE subscriptions SET
			status                    = $2,
			current_period_start      = COALESCE($3,  current_period_start),
			current_period_end        = COALESCE($4,  current_period_end),
			cancel_at_period_end      = COALESCE($5,  cancel_at_period_end),
			cancel_at                 = COALESCE($6,  cancel_at),
			canceled_at               = COALESCE($7,  canceled_at),
			ended_at                  = COALESCE($8,  ended_at),
			trial_end                 = COALESCE($9,  trial_end),
			stripe_price_id           = COALESCE($10, stripe_price_id),
			quantity                  = COALESCE($11, quantity),
			latest_invoice_id         = COALESCE($12, latest_invoice_id),
			default_payment_method_id = COALESCE($13, default_payment_method_id),
			last_stripe_event_id      = $14,
			last_stripe_event_at      = $15
		WHERE stripe_subscription_id = $1
		RETURNING ` + subscriptionColumns

	sub, err := scanSubscription(tx.QueryRow(ctx, query,
		in.StripeSubscriptionID, in.Status,
		in.CurrentPeriodStart, in.CurrentPeriodEnd, in.CancelAtPeriodEnd,
		in.CancelAt, in.CanceledAt, in.EndedAt, in.TrialEnd,
		in.StripePriceID, in.Quantity, in.LatestInvoiceID, in.DefaultPaymentMethodID,
		nullIfEmpty(in.EventID), in.EventCreatedAt,
	))
	if err != nil {
		return nil, mapError("update subscription status", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, mapError("update subscription status: commit", err)
	}
	return sub, nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func scanSubscription(r row) (*domain.Subscription, error) {
	var s domain.Subscription
	err := r.Scan(
		&s.ID,
		&s.UserID,
		&s.StripeSubscriptionID,
		&s.StripeCustomerID,
		&s.StripePriceID,
		&s.StripeProductID,
		&s.Status,
		&s.Quantity,
		&s.Currency,
		&s.UnitAmount,
		&s.CurrentPeriodStart,
		&s.CurrentPeriodEnd,
		&s.CancelAtPeriodEnd,
		&s.CancelAt,
		&s.CanceledAt,
		&s.EndedAt,
		&s.TrialStart,
		&s.TrialEnd,
		&s.LatestInvoiceID,
		&s.DefaultPaymentMethodID,
		&s.LastStripeEventID,
		&s.LastStripeEventAt,
		&s.Metadata,
		&s.CreatedAt,
		&s.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &s, nil
}
