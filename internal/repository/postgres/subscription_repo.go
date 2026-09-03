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
	RecordInvoicePayment(ctx context.Context, in InvoicePaymentUpdate) (*domain.Subscription, error)

	// CountInDunning reports subscriptions with an outstanding payment failure.
	CountInDunning(ctx context.Context) (int64, error)
}

// InvoicePaymentUpdate carries one invoice.payment_succeeded or
// invoice.payment_failed event.
type InvoicePaymentUpdate struct {
	StripeSubscriptionID string

	// Succeeded selects which side of the dunning state is written: a success
	// clears the flag and the counter, a failure sets them.
	Succeeded bool

	// EventID and EventCreatedAt drive the staleness check against
	// last_invoice_event_at - the invoice stream's own cursor, not the
	// subscription stream's.
	EventID        string
	EventCreatedAt time.Time

	InvoiceID *string

	// FailureReason and NextAttemptAt are only meaningful on a failure.
	FailureReason *string
	NextAttemptAt *time.Time

	// CurrentPeriodStart/End arrive on a successful renewal invoice. Nil leaves
	// the stored period untouched.
	CurrentPeriodStart *time.Time
	CurrentPeriodEnd   *time.Time
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
	payment_failed_at, payment_failure_count, last_payment_error,
	next_payment_attempt_at, last_invoice_event_id, last_invoice_event_at,
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

// RecordInvoicePayment applies one invoice.payment_* event.
//
// It guards on last_invoice_event_at, not last_stripe_event_at. The invoice and
// subscription event streams interleave freely, and a shared cursor would let
// an invoice event created at T5 reject a customer.subscription.updated created
// at T4 - discarding the event that carries the authoritative status. Each
// stream advances only its own cursor.
//
// Status is deliberately not written here. Stripe decides what a failed payment
// means for the subscription (past_due when it was active, incomplete on a
// first invoice, canceled once retries are exhausted) and says so in a
// customer.subscription.updated event. Deriving it from the invoice would race
// that event and sometimes contradict it.
//
// The row is locked for the same reason UpdateSubscriptionStatus locks it: the
// read-decide-write of the staleness check is only atomic while it is held.
func (r *SubscriptionRepo) RecordInvoicePayment(ctx context.Context, in InvoicePaymentUpdate) (*domain.Subscription, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, mapError("record invoice payment: begin", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var appliedAt *time.Time
	err = tx.QueryRow(ctx,
		`SELECT last_invoice_event_at FROM subscriptions WHERE stripe_subscription_id = $1 FOR UPDATE`,
		in.StripeSubscriptionID,
	).Scan(&appliedAt)
	if err != nil {
		return nil, mapError("record invoice payment: lock row", err)
	}

	// Strictly-older events are rejected, equal timestamps applied - Stripe's
	// one-second resolution means distinct invoice events routinely collide.
	if appliedAt != nil && in.EventCreatedAt.Before(*appliedAt) {
		return nil, domain.ErrStaleEvent
	}

	// The two branches are written separately rather than through COALESCE
	// gymnastics: a success must clear the dunning columns outright, and a
	// failure must increment a counter it also reads.
	//
	// Note that last_payment_error and next_payment_attempt_at are assigned,
	// not COALESCEd, unlike latest_invoice_id. That is deliberate. Each failure
	// carries its own decline reason, and Stripe nulls next_payment_attempt
	// once retries are exhausted - so an absent value means "no retry
	// scheduled", which is information worth keeping rather than a gap to paper
	// over with the previous attempt's answer. latest_invoice_id is COALESCEd
	// because an event that does not name an invoice is not saying the
	// subscription has none.
	const successQuery = `
		UPDATE subscriptions SET
			latest_invoice_id       = COALESCE($2, latest_invoice_id),
			current_period_start    = COALESCE($3, current_period_start),
			current_period_end      = COALESCE($4, current_period_end),
			payment_failed_at       = NULL,
			payment_failure_count   = 0,
			last_payment_error      = NULL,
			next_payment_attempt_at = NULL,
			last_invoice_event_id   = $5,
			last_invoice_event_at   = $6
		WHERE stripe_subscription_id = $1
		RETURNING ` + subscriptionColumns

	const failureQuery = `
		UPDATE subscriptions SET
			latest_invoice_id       = COALESCE($2, latest_invoice_id),
			payment_failed_at       = $3,
			payment_failure_count   = subscriptions.payment_failure_count + 1,
			last_payment_error      = $4,
			next_payment_attempt_at = $5,
			last_invoice_event_id   = $6,
			last_invoice_event_at   = $7
		WHERE stripe_subscription_id = $1
		RETURNING ` + subscriptionColumns

	var sub *domain.Subscription
	if in.Succeeded {
		sub, err = scanSubscription(tx.QueryRow(ctx, successQuery,
			in.StripeSubscriptionID, in.InvoiceID,
			in.CurrentPeriodStart, in.CurrentPeriodEnd,
			nullIfEmpty(in.EventID), in.EventCreatedAt,
		))
	} else {
		sub, err = scanSubscription(tx.QueryRow(ctx, failureQuery,
			in.StripeSubscriptionID, in.InvoiceID,
			in.EventCreatedAt, truncate(derefOr(in.FailureReason, "payment failed")),
			in.NextAttemptAt,
			nullIfEmpty(in.EventID), in.EventCreatedAt,
		))
	}
	if err != nil {
		return nil, mapError("record invoice payment", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, mapError("record invoice payment: commit", err)
	}
	return sub, nil
}

func derefOr(s *string, fallback string) string {
	if s == nil || *s == "" {
		return fallback
	}
	return *s
}

// CountInDunning uses idx_subscriptions_dunning, which is partial on
// payment_failed_at IS NOT NULL - so this scans only the rows in dunning rather
// than the whole table.
func (r *SubscriptionRepo) CountInDunning(ctx context.Context) (int64, error) {
	const query = `SELECT count(*) FROM subscriptions WHERE payment_failed_at IS NOT NULL`

	var n int64
	if err := r.pool.QueryRow(ctx, query).Scan(&n); err != nil {
		return 0, mapError("count subscriptions in dunning", err)
	}
	return n, nil
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
		&s.PaymentFailedAt,
		&s.PaymentFailureCount,
		&s.LastPaymentError,
		&s.NextPaymentAttemptAt,
		&s.LastInvoiceEventID,
		&s.LastInvoiceEventAt,
		&s.Metadata,
		&s.CreatedAt,
		&s.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &s, nil
}
