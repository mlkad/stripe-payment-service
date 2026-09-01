package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mlkad/stripe-payment-service/internal/domain"
)

// defaultStaleClaimAfter is how long a claim may sit in the processing state
// before another worker may take it over. It must exceed the longest realistic
// processing time, or two workers will handle the same event concurrently.
const defaultStaleClaimAfter = 5 * time.Minute

// maxLastErrorLen bounds what is written to last_error. Driver errors can carry
// an entire query plus parameters, and this column is read by operators, not
// machines.
const maxLastErrorLen = 2000

// WebhookRepository is the idempotency port the webhook service depends on.
type WebhookRepository interface {
	// TryClaimEvent atomically claims an event for processing. It reports false
	// when the event needs no work: already settled, or in flight elsewhere.
	TryClaimEvent(ctx context.Context, w *domain.ProcessedWebhook) (bool, error)
	MarkEventProcessed(ctx context.Context, eventID string) error
	MarkEventFailed(ctx context.Context, eventID string, cause error) error
}

type WebhookRepo struct {
	pool            *pgxpool.Pool
	staleClaimAfter time.Duration
}

// NewWebhookRepo builds the repository. A zero staleClaimAfter selects
// defaultStaleClaimAfter.
func NewWebhookRepo(pool *pgxpool.Pool, staleClaimAfter time.Duration) *WebhookRepo {
	if staleClaimAfter <= 0 {
		staleClaimAfter = defaultStaleClaimAfter
	}
	return &WebhookRepo{pool: pool, staleClaimAfter: staleClaimAfter}
}

var _ WebhookRepository = (*WebhookRepo)(nil)

// TryClaimEvent is the concurrency control for the whole webhook pipeline.
//
// The INSERT is the claim: event_id is the primary key, so exactly one caller
// can create the row, and the unique index does the mutual exclusion without an
// advisory lock. The ON CONFLICT branch handles redelivery, and its WHERE
// decides who may retry:
//
//   - status 'failed'      -> reclaim; the previous attempt errored
//   - status 'processing'  -> reclaim only once the claim has gone stale, which
//     covers a worker that crashed mid-event
//   - 'succeeded'/'skipped' -> no row returned; the work is already done
//
// When no row comes back the event must still be acknowledged to Stripe with a
// 2xx. Returning an error there would make Stripe redeliver an event that is
// either finished or actively being handled.
func (r *WebhookRepo) TryClaimEvent(ctx context.Context, w *domain.ProcessedWebhook) (bool, error) {
	if err := w.Validate(); err != nil {
		return false, err
	}

	const query = `
		INSERT INTO processed_webhooks (
			event_id, event_type, api_version, livemode, request_id,
			stripe_created_at, payload, status, attempts)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, 'processing', 1)
		ON CONFLICT (event_id) DO UPDATE
			SET status     = 'processing',
			    attempts   = processed_webhooks.attempts + 1,
			    last_error = NULL
			WHERE processed_webhooks.status = 'failed'
			   OR (processed_webhooks.status = 'processing'
			       AND processed_webhooks.updated_at < now() - make_interval(secs => $8))
		RETURNING attempts, status, received_at, created_at, updated_at`

	err := r.pool.QueryRow(ctx, query,
		w.EventID, w.EventType, w.APIVersion, w.Livemode, w.RequestID,
		w.StripeCreatedAt, payloadOrNil(w.Payload), r.staleClaimAfter.Seconds(),
	).Scan(&w.Attempts, &w.Status, &w.ReceivedAt, &w.CreatedAt, &w.UpdatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, mapError("claim webhook event", err)
	}
	return true, nil
}

// MarkEventProcessed settles a claimed event as succeeded.
//
// The status = 'processing' predicate makes this safe against a worker that
// resumes after its claim was reclaimed: it can only settle an event it still
// holds, and otherwise gets ErrEventNotClaimed rather than overwriting the
// outcome recorded by whoever took over.
func (r *WebhookRepo) MarkEventProcessed(ctx context.Context, eventID string) error {
	const query = `
		UPDATE processed_webhooks
		SET status = 'succeeded', processed_at = now(), last_error = NULL
		WHERE event_id = $1 AND status = 'processing'`

	tag, err := r.pool.Exec(ctx, query, eventID)
	if err != nil {
		return mapError("mark webhook processed", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("mark webhook processed %s: %w", eventID, domain.ErrEventNotClaimed)
	}
	return nil
}

// MarkEventFailed records a failed attempt and leaves the event reclaimable.
// processed_at stays NULL, which processed_webhooks_processed_at_chk requires
// for a non-terminal status.
func (r *WebhookRepo) MarkEventFailed(ctx context.Context, eventID string, cause error) error {
	const query = `
		UPDATE processed_webhooks
		SET status = 'failed', last_error = $2, processed_at = NULL
		WHERE event_id = $1 AND status = 'processing'`

	tag, err := r.pool.Exec(ctx, query, eventID, failureReason(cause))
	if err != nil {
		return mapError("mark webhook failed", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("mark webhook failed %s: %w", eventID, domain.ErrEventNotClaimed)
	}
	return nil
}

// failureReason renders cause for last_error, which the CHECK constraint
// requires to be non-null whenever status is 'failed'.
func failureReason(cause error) string {
	msg := "unspecified failure"
	if cause != nil && cause.Error() != "" {
		msg = cause.Error()
	}
	if len(msg) > maxLastErrorLen {
		msg = msg[:maxLastErrorLen] + "…"
	}
	return msg
}

func payloadOrNil(p []byte) any {
	if len(p) == 0 {
		return nil
	}
	return p
}
