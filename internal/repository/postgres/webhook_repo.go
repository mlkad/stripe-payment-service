package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

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
	MarkEventSkipped(ctx context.Context, eventID, reason string) error

	// ReclaimAbandonedEvents moves claims left in 'processing' by a crashed
	// worker into 'failed', where they become retryable.
	ReclaimAbandonedEvents(ctx context.Context, staleAfter time.Duration, limit int) ([]string, error)

	// ListRetryableEvents returns failed events whose backoff has elapsed and
	// which have not exhausted their attempts.
	ListRetryableEvents(ctx context.Context, in RetryQuery) ([]*domain.ProcessedWebhook, error)

	// ClaimEventForRetry atomically moves one failed event back to
	// 'processing'. False means another worker got there first.
	ClaimEventForRetry(ctx context.Context, eventID string, maxAttempts int32) (bool, error)

	// LedgerStats summarises the unsettled backlog.
	LedgerStats(ctx context.Context, maxAttempts int32) (LedgerStats, error)

	// PurgePayloads reduces stored event payloads to a non-identifying
	// skeleton once they are past their retention window.
	PurgePayloads(ctx context.Context, in PurgeQuery) (PurgeResult, error)

	// RetentionStats reports how much personal data is still held.
	RetentionStats(ctx context.Context, in PurgeQuery) (RetentionStats, error)
}

// PurgeQuery bounds one retention pass.
type PurgeQuery struct {
	// SettledAfter is how long a succeeded or skipped event keeps its payload.
	// These have no replay value left; the window exists only so an operator
	// can investigate something that went wrong recently.
	SettledAfter time.Duration

	// UnsettledAfter is the outer bound for a failed event. Not optional: a
	// privacy obligation does not pause because a retry queue is stuck, and
	// without it one permanently dead-lettered event holds personal data
	// forever. It must comfortably exceed the time the sweeper needs to
	// exhaust its retry budget, or recoverable events lose their payload.
	UnsettledAfter time.Duration

	Limit int
}

// PurgeResult is what one pass removed.
type PurgeResult struct {
	Settled   int
	Unsettled int
}

func (r PurgeResult) Total() int { return r.Settled + r.Unsettled }

// RetentionStats describes the personal data still held.
type RetentionStats struct {
	// WithPayload is rows still holding a full event.
	WithPayload int64

	// Purged is rows already reduced to their skeleton.
	Purged int64

	// DueNow is rows past their window and awaiting the next pass. A number
	// that does not fall means retention is not running.
	DueNow int64

	// OldestPayload is the age of the oldest full event still stored. This is
	// the number a data protection review asks for.
	OldestPayload *time.Time
}

// RetryQuery bounds a sweep for retryable events.
type RetryQuery struct {
	MaxAttempts int32

	// BaseBackoff is doubled per prior attempt, capped at MaxBackoff. Retrying
	// a failing event every tick would turn a downstream outage into a tight
	// loop against it.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration

	Limit int
}

// LedgerStats is what an operator needs to know at a glance.
type LedgerStats struct {
	// Processing is claims currently in flight. A number that does not fall is
	// a stuck worker.
	Processing int64

	// Retryable is failed events still within their attempt budget.
	Retryable int64

	// DeadLettered is failed events past MaxAttempts. Nothing will retry these
	// again; they need a human.
	DeadLettered int64

	// OldestUnsettled is the age of the oldest unsettled row, nil when the
	// ledger is clean. It is the single most useful number here: a backlog that
	// is growing older is a different problem from one that is merely large.
	OldestUnsettled *time.Time
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

// MarkEventSkipped settles a claimed event the service does not subscribe to.
//
// Distinct from succeeded on purpose: the ledger should show that an event was
// seen and deliberately ignored, not that work was performed. Terminal, so
// processed_at is set to satisfy processed_webhooks_processed_at_chk.
//
// reason lands in last_error, which the CHECK constraint only *requires* for
// failed rows but does not reserve. An operator auditing the ledger needs to
// know why a row was skipped, and there is no other column for it.
func (r *WebhookRepo) MarkEventSkipped(ctx context.Context, eventID, reason string) error {
	const query = `
		UPDATE processed_webhooks
		SET status = 'skipped', processed_at = now(), last_error = $2
		WHERE event_id = $1 AND status = 'processing'`

	tag, err := r.pool.Exec(ctx, query, eventID, truncate(reason))
	if err != nil {
		return mapError("mark webhook skipped", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("mark webhook skipped %s: %w", eventID, domain.ErrEventNotClaimed)
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
	if cause == nil || cause.Error() == "" {
		return "unspecified failure"
	}
	return truncate(cause.Error())
}

// truncate bounds free text destined for last_error. Driver errors can carry an
// entire query plus parameters, and this column is read by operators.
func truncate(s string) string {
	if len(s) <= maxLastErrorLen {
		return s
	}
	// Cut on a rune boundary so the column never holds a broken sequence.
	cut := maxLastErrorLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// ReclaimAbandonedEvents rescues claims from a worker that died mid-event.
//
// Such a row sits in 'processing' forever otherwise: TryClaimEvent will reclaim
// it once the stale window passes, but only if Stripe redelivers, and Stripe
// stops after three days. Moving it to 'failed' puts it back in reach of the
// retry path, which does not depend on Stripe at all.
func (r *WebhookRepo) ReclaimAbandonedEvents(ctx context.Context, staleAfter time.Duration, limit int) ([]string, error) {
	const query = `
		UPDATE processed_webhooks
		SET status = 'failed', last_error = $3, processed_at = NULL
		WHERE event_id IN (
			SELECT event_id FROM processed_webhooks
			WHERE status = 'processing'
			  AND updated_at < now() - make_interval(secs => $1)
			ORDER BY updated_at
			LIMIT $2
			-- Another sweeper may be doing the same scan. Skipping locked rows
			-- keeps them from blocking on each other rather than dividing the
			-- work.
			FOR UPDATE SKIP LOCKED
		)
		RETURNING event_id`

	rows, err := r.pool.Query(ctx, query, staleAfter.Seconds(), limit,
		"claim abandoned by a worker that did not settle it")
	if err != nil {
		return nil, mapError("reclaim abandoned events", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, mapError("reclaim abandoned events: scan", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError("reclaim abandoned events: iterate", err)
	}
	return ids, nil
}

// ListRetryableEvents finds failed events due for another attempt.
//
// The backoff is computed in SQL so the decision and the scan are one
// operation: 2^(attempts-1) times the base, capped. The status predicate is
// mandatory - idx_processed_webhooks_unsettled is partial, and without it this
// becomes a sequential scan over every webhook the service has ever received.
func (r *WebhookRepo) ListRetryableEvents(ctx context.Context, in RetryQuery) ([]*domain.ProcessedWebhook, error) {
	const query = `SELECT ` + webhookColumns + `
		FROM processed_webhooks
		WHERE status = 'failed'
		  AND attempts < $1
		  AND updated_at < now() - make_interval(
		        secs => LEAST($2::float8 * POWER(2, attempts - 1), $3::float8))
		ORDER BY updated_at
		LIMIT $4`

	rows, err := r.pool.Query(ctx, query,
		in.MaxAttempts, in.BaseBackoff.Seconds(), in.MaxBackoff.Seconds(), in.Limit)
	if err != nil {
		return nil, mapError("list retryable events", err)
	}
	defer rows.Close()

	var out []*domain.ProcessedWebhook
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, mapError("list retryable events: scan", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError("list retryable events: iterate", err)
	}
	return out, nil
}

// ClaimEventForRetry takes ownership of one failed event.
//
// The attempts guard sits inside the UPDATE rather than being checked
// beforehand, so a stale listing cannot take an event past its budget: two
// sweepers scanning together both see an event at maxAttempts-1, and the second
// may still be holding that listing when the first has already failed the event
// back to 'failed'.
//
// Defence in depth rather than a load-bearing check - the window is narrow
// enough that a six-way concurrent sweep does not reproduce it. Cheap to keep,
// and the alternative is reasoning about that window on every future change.
func (r *WebhookRepo) ClaimEventForRetry(ctx context.Context, eventID string, maxAttempts int32) (bool, error) {
	const query = `
		UPDATE processed_webhooks
		SET status = 'processing', attempts = attempts + 1, last_error = NULL
		WHERE event_id = $1 AND status = 'failed' AND attempts < $2`

	tag, err := r.pool.Exec(ctx, query, eventID, maxAttempts)
	if err != nil {
		return false, mapError("claim event for retry", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r *WebhookRepo) LedgerStats(ctx context.Context, maxAttempts int32) (LedgerStats, error) {
	const query = `
		SELECT
			count(*) FILTER (WHERE status = 'processing'),
			count(*) FILTER (WHERE status = 'failed' AND attempts <  $1),
			count(*) FILTER (WHERE status = 'failed' AND attempts >= $1),
			min(updated_at)
		FROM processed_webhooks
		WHERE status IN ('processing', 'failed')`

	var stats LedgerStats
	err := r.pool.QueryRow(ctx, query, maxAttempts).Scan(
		&stats.Processing, &stats.Retryable, &stats.DeadLettered, &stats.OldestUnsettled)
	if err != nil {
		return LedgerStats{}, mapError("ledger stats", err)
	}
	return stats, nil
}

// Unsettled reports whether anything needs attention.
func (s LedgerStats) Unsettled() int64 { return s.Processing + s.Retryable + s.DeadLettered }

// payloadSkeleton is the allowlist that replaces a purged payload.
//
// Built in SQL on purpose: the personal data is reduced in place and never
// travels to the application, so a purge cannot leak through a log line, a
// crash dump, or a debugger on the way past.
//
// An allowlist rather than a redaction. Stripping known-sensitive fields would
// start leaking the day Stripe adds a field nobody anticipated, and it would
// look compliant while doing it. This keeps only what identifies the event
// itself - never anything describing the person it concerns.
const payloadSkeleton = `
	jsonb_strip_nulls(jsonb_build_object(
		'id',          payload -> 'id',
		'object',      payload -> 'object',
		'type',        payload -> 'type',
		'created',     payload -> 'created',
		'livemode',    payload -> 'livemode',
		'api_version', payload -> 'api_version',
		'data', jsonb_build_object('object', jsonb_strip_nulls(jsonb_build_object(
			'id',     payload #> '{data,object,id}',
			'object', payload #> '{data,object,object}'
		)))
	))`

// PurgePayloads applies the retention policy.
//
// Rows in 'processing' are never touched: the sweeper may be mid-replay, and
// taking the payload out from under it would turn a recoverable event into an
// unreplayable one. They become eligible once the sweeper reclaims them.
//
// The UPDATE moves updated_at, because trg_processed_webhooks_set_updated_at
// owns that column. That is harmless here - settled rows do not use it, and an
// unsettled row this old has long since exhausted its retry budget - but it is
// the reason UnsettledAfter must stay far larger than the sweeper's backoff.
func (r *WebhookRepo) PurgePayloads(ctx context.Context, in PurgeQuery) (PurgeResult, error) {
	const query = `
		UPDATE processed_webhooks
		SET payload = ` + payloadSkeleton + `,
		    payload_purged_at = now()
		WHERE event_id IN (
			SELECT event_id FROM processed_webhooks
			WHERE payload IS NOT NULL
			  AND payload_purged_at IS NULL
			  AND status <> 'processing'
			  AND (
			        (status IN ('succeeded', 'skipped')
			           AND received_at < now() - make_interval(secs => $1))
			     OR (status = 'failed'
			           AND received_at < now() - make_interval(secs => $2))
			  )
			ORDER BY received_at
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		RETURNING status::text`

	rows, err := r.pool.Query(ctx, query,
		in.SettledAfter.Seconds(), in.UnsettledAfter.Seconds(), in.Limit)
	if err != nil {
		return PurgeResult{}, mapError("purge payloads", err)
	}
	defer rows.Close()

	var result PurgeResult
	for rows.Next() {
		var status string
		if err := rows.Scan(&status); err != nil {
			return PurgeResult{}, mapError("purge payloads: scan", err)
		}
		if status == string(domain.WebhookFailed) {
			result.Unsettled++
		} else {
			result.Settled++
		}
	}
	if err := rows.Err(); err != nil {
		return PurgeResult{}, mapError("purge payloads: iterate", err)
	}
	return result, nil
}

func (r *WebhookRepo) RetentionStats(ctx context.Context, in PurgeQuery) (RetentionStats, error) {
	const query = `
		SELECT
			count(*) FILTER (WHERE payload IS NOT NULL AND payload_purged_at IS NULL),
			count(*) FILTER (WHERE payload_purged_at IS NOT NULL),
			count(*) FILTER (WHERE payload IS NOT NULL
			                   AND payload_purged_at IS NULL
			                   AND status <> 'processing'
			                   AND ((status IN ('succeeded','skipped')
			                           AND received_at < now() - make_interval(secs => $1))
			                     OR (status = 'failed'
			                           AND received_at < now() - make_interval(secs => $2)))),
			min(received_at) FILTER (WHERE payload IS NOT NULL AND payload_purged_at IS NULL)
		FROM processed_webhooks`

	var stats RetentionStats
	err := r.pool.QueryRow(ctx, query, in.SettledAfter.Seconds(), in.UnsettledAfter.Seconds()).
		Scan(&stats.WithPayload, &stats.Purged, &stats.DueNow, &stats.OldestPayload)
	if err != nil {
		return RetentionStats{}, mapError("retention stats", err)
	}
	return stats, nil
}

const webhookColumns = `
	event_id, event_type, api_version, livemode, request_id,
	status, attempts, last_error, payload,
	stripe_created_at, received_at, processed_at, created_at, updated_at`

func scanWebhook(r row) (*domain.ProcessedWebhook, error) {
	var w domain.ProcessedWebhook
	err := r.Scan(
		&w.EventID, &w.EventType, &w.APIVersion, &w.Livemode, &w.RequestID,
		&w.Status, &w.Attempts, &w.LastError, &w.Payload,
		&w.StripeCreatedAt, &w.ReceivedAt, &w.ProcessedAt, &w.CreatedAt, &w.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &w, nil
}

func payloadOrNil(p []byte) any {
	if len(p) == 0 {
		return nil
	}
	return p
}
