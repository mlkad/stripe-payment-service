// Package worker holds the background jobs that run alongside the HTTP server.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	repo "github.com/mlkad/stripe-payment-service/internal/repository/postgres"
	"github.com/mlkad/stripe-payment-service/internal/service"
)

// SweeperConfig tunes one sweep.
type SweeperConfig struct {
	// Interval between sweeps.
	Interval time.Duration

	// StaleClaimAfter is how long a claim may sit in 'processing' before it is
	// treated as abandoned. It must exceed the longest realistic processing
	// time, or a slow-but-healthy handler gets its work stolen.
	StaleClaimAfter time.Duration

	// MaxAttempts is the retry budget. Past it an event is dead-lettered and
	// left alone: something is wrong that another attempt will not fix.
	MaxAttempts int32

	// BaseBackoff is doubled per prior attempt, capped at MaxBackoff.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration

	// BatchSize bounds one sweep, so a large backlog is worked through over
	// several ticks rather than in one long transaction.
	BatchSize int

	// AlertAfter is how old the oldest unsettled event may get before the
	// sweep logs at error level. This is the number worth alerting on: a
	// backlog that is getting older is a different problem from one that is
	// merely large.
	AlertAfter time.Duration
}

const (
	defaultInterval    = time.Minute
	defaultMaxAttempts = 6
	defaultBaseBackoff = 30 * time.Second
	defaultMaxBackoff  = 30 * time.Minute
	defaultBatchSize   = 100
	defaultAlertAfter  = time.Hour
)

func (c SweeperConfig) withDefaults() SweeperConfig {
	if c.Interval <= 0 {
		c.Interval = defaultInterval
	}
	if c.StaleClaimAfter <= 0 {
		c.StaleClaimAfter = 5 * time.Minute
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = defaultMaxAttempts
	}
	if c.BaseBackoff <= 0 {
		c.BaseBackoff = defaultBaseBackoff
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = defaultMaxBackoff
	}
	if c.BatchSize <= 0 {
		c.BatchSize = defaultBatchSize
	}
	if c.AlertAfter <= 0 {
		c.AlertAfter = defaultAlertAfter
	}
	return c
}

// WebhookSweeper turns the failed-event ledger from a black hole into
// something that recovers on its own and complains when it cannot.
//
// Three jobs per tick:
//
//  1. Reclaim claims abandoned by a crashed worker.
//  2. Replay failed events whose backoff has elapsed. Stripe retries for three
//     days, but a bug fixed on day four leaves every event from days one to
//     three permanently unprocessed - this is what recovers them, from the
//     payload stored in the ledger.
//  3. Report. Silence when the ledger is clean, a warning when there is a
//     backlog, an error when it is going stale or anything is dead-lettered.
//
// Safe to run on every replica: each row is taken with an atomic claim, and
// the abandoned-claim scan uses SKIP LOCKED, so instances divide the work
// rather than block on each other.
type WebhookSweeper struct {
	hooks   repo.WebhookRepository
	webhook *service.WebhookService
	cfg     SweeperConfig
	log     *slog.Logger
}

func NewWebhookSweeper(
	hooks repo.WebhookRepository,
	webhook *service.WebhookService,
	cfg SweeperConfig,
	log *slog.Logger,
) *WebhookSweeper {
	return &WebhookSweeper{
		hooks:   hooks,
		webhook: webhook,
		cfg:     cfg.withDefaults(),
		log:     log.With(slog.String("component", "webhook_sweeper")),
	}
}

// Run sweeps until ctx is cancelled. It returns only on cancellation.
func (s *WebhookSweeper) Run(ctx context.Context) {
	s.log.InfoContext(ctx, "webhook sweeper started",
		slog.String("interval", s.cfg.Interval.String()),
		slog.Int("max_attempts", int(s.cfg.MaxAttempts)))

	// Every replica would otherwise wake on the same schedule and contend for
	// the same rows on every tick.
	jitter := time.Duration(rand.Int64N(int64(s.cfg.Interval / 4)))
	timer := time.NewTimer(jitter)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			s.log.Info("webhook sweeper stopped")
			return
		case <-timer.C:
			s.Sweep(ctx)
			timer.Reset(s.cfg.Interval)
		}
	}
}

// SweepResult is what one pass did.
type SweepResult struct {
	Reclaimed int
	Retried   int
	Recovered int
	Stats     repo.LedgerStats
}

// Sweep runs one pass. Exported so a test, or an operator through the CLI, can
// trigger it without waiting for a tick.
func (s *WebhookSweeper) Sweep(ctx context.Context) SweepResult {
	var result SweepResult

	result.Reclaimed = s.reclaimAbandoned(ctx)
	result.Retried, result.Recovered = s.retryFailed(ctx)
	result.Stats = s.report(ctx)
	return result
}

func (s *WebhookSweeper) reclaimAbandoned(ctx context.Context) int {
	ids, err := s.hooks.ReclaimAbandonedEvents(ctx, s.cfg.StaleClaimAfter, s.cfg.BatchSize)
	if err != nil {
		s.log.ErrorContext(ctx, "could not reclaim abandoned claims", slog.String("error", err.Error()))
		return 0
	}
	if len(ids) > 0 {
		// Worth an explicit line: this only happens when a worker died holding
		// a claim, which usually means a crash or an OOM kill worth chasing.
		s.log.WarnContext(ctx, "reclaimed abandoned webhook claims",
			slog.Int("count", len(ids)),
			slog.Any("event_ids", ids))
	}
	return len(ids)
}

func (s *WebhookSweeper) retryFailed(ctx context.Context) (attempted, recovered int) {
	events, err := s.hooks.ListRetryableEvents(ctx, repo.RetryQuery{
		MaxAttempts: s.cfg.MaxAttempts,
		BaseBackoff: s.cfg.BaseBackoff,
		MaxBackoff:  s.cfg.MaxBackoff,
		Limit:       s.cfg.BatchSize,
	})
	if err != nil {
		s.log.ErrorContext(ctx, "could not list retryable events", slog.String("error", err.Error()))
		return 0, 0
	}

	for _, event := range events {
		// Stop promptly on shutdown rather than working through the batch.
		if ctx.Err() != nil {
			return attempted, recovered
		}

		claimed, err := s.hooks.ClaimEventForRetry(ctx, event.EventID, s.cfg.MaxAttempts)
		if err != nil {
			s.log.ErrorContext(ctx, "could not claim event for retry",
				slog.String("event_id", event.EventID), slog.String("error", err.Error()))
			continue
		}
		if !claimed {
			// Another sweeper, or a Stripe redelivery, got there first.
			continue
		}
		attempted++

		// The claim incremented attempts; reflect that in the log line.
		event.Attempts++
		outcome, err := s.webhook.ReplayEvent(ctx, event)
		switch {
		case err != nil:
			// Already recorded as failed by ReplayEvent, and already logged.
		case outcome == service.OutcomeFailed:
		default:
			recovered++
		}
	}
	return attempted, recovered
}

// report emits the one log line an operator should alert on.
//
// The level is the alert: info while the backlog is empty or shrinking within
// its budget, error once anything is dead-lettered or the oldest unsettled
// event has aged past AlertAfter. A dead letter means retries are exhausted and
// nothing further will happen without a human.
func (s *WebhookSweeper) report(ctx context.Context) repo.LedgerStats {
	stats, err := s.hooks.LedgerStats(ctx, s.cfg.MaxAttempts)
	if err != nil {
		s.log.ErrorContext(ctx, "could not read ledger stats", slog.String("error", err.Error()))
		return stats
	}
	if stats.Unsettled() == 0 {
		s.log.DebugContext(ctx, "webhook ledger is clean")
		return stats
	}

	attrs := []any{
		slog.Int64("processing", stats.Processing),
		slog.Int64("retryable", stats.Retryable),
		slog.Int64("dead_lettered", stats.DeadLettered),
	}
	stale := false
	if stats.OldestUnsettled != nil {
		age := time.Since(*stats.OldestUnsettled)
		attrs = append(attrs, slog.Duration("oldest_unsettled_age", age))
		stale = age > s.cfg.AlertAfter
	}

	switch {
	case stats.DeadLettered > 0:
		attrs = append(attrs, slog.String("action",
			"retries exhausted; inspect processed_webhooks.last_error and replay after fixing"))
		s.log.ErrorContext(ctx, "webhook events are dead-lettered", attrs...)
	case stale:
		attrs = append(attrs, slog.String("action",
			"backlog is not draining; check downstream dependencies"))
		s.log.ErrorContext(ctx, "webhook backlog is going stale", attrs...)
	default:
		s.log.InfoContext(ctx, "webhook backlog draining", attrs...)
	}
	return stats
}

// ErrDeadLettered is returned by a health check when the ledger holds events
// nothing will retry.
var ErrDeadLettered = errors.New("webhook events are dead-lettered")

// HealthCheck reports the ledger as unhealthy once events are dead-lettered.
//
// Deliberately not wired into /healthz: a dead-lettered webhook does not mean
// this instance should stop receiving traffic, and taking it out of the load
// balancer would make the backlog worse. It is here for a diagnostic endpoint
// or a monitoring probe that treats "needs a human" separately from "cannot
// serve requests".
func (s *WebhookSweeper) HealthCheck(ctx context.Context) (repo.LedgerStats, error) {
	stats, err := s.hooks.LedgerStats(ctx, s.cfg.MaxAttempts)
	if err != nil {
		return stats, err
	}
	if stats.DeadLettered > 0 {
		return stats, fmt.Errorf("%w: %d event(s)", ErrDeadLettered, stats.DeadLettered)
	}
	return stats, nil
}
