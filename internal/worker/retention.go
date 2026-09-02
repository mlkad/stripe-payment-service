package worker

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"

	repo "github.com/mlkad/stripe-payment-service/internal/repository/postgres"
)

// RetentionConfig tunes the payload retention pass.
type RetentionConfig struct {
	// Interval between passes. Retention is a daily concern, not a per-minute
	// one; running it often only adds churn.
	Interval time.Duration

	// SettledAfter is how long a succeeded or skipped event keeps its payload.
	SettledAfter time.Duration

	// UnsettledAfter is the outer bound for a failed event.
	UnsettledAfter time.Duration

	// BatchSize bounds one pass, so a large first run is spread over several
	// rather than holding one long transaction against the hot table.
	BatchSize int
}

const (
	defaultRetentionInterval = 6 * time.Hour
	defaultSettledAfter      = 30 * 24 * time.Hour
	defaultUnsettledAfter    = 90 * 24 * time.Hour
	defaultRetentionBatch    = 500
)

func (c RetentionConfig) withDefaults() RetentionConfig {
	if c.Interval <= 0 {
		c.Interval = defaultRetentionInterval
	}
	if c.SettledAfter <= 0 {
		c.SettledAfter = defaultSettledAfter
	}
	if c.UnsettledAfter <= 0 {
		c.UnsettledAfter = defaultUnsettledAfter
	}
	if c.BatchSize <= 0 {
		c.BatchSize = defaultRetentionBatch
	}
	return c
}

func (c RetentionConfig) query(limit int) repo.PurgeQuery {
	return repo.PurgeQuery{
		SettledAfter:   c.SettledAfter,
		UnsettledAfter: c.UnsettledAfter,
		Limit:          limit,
	}
}

// RetentionWorker enforces data minimisation on stored webhook payloads.
//
// processed_webhooks.payload holds the raw Stripe event, which carries customer
// email, name, billing address and card metadata. Keeping it indefinitely is
// both a growing table and a growing liability.
//
// It cannot simply be dropped on a clock, because the sweeper replays failed
// events from exactly this column. Two windows follow: settled events lose
// their payload early, since they have no replay value left, and failed events
// keep theirs until a much longer outer bound. The outer bound is the part that
// is easy to leave out and must not be - a privacy obligation does not pause
// because a retry queue is stuck.
//
// Separate from WebhookSweeper on purpose. The sweeper runs every minute
// because a stuck event is urgent; retention runs every few hours because a
// 30-day window does not care about minutes, and coupling them would mean
// either pointless churn or a sluggish sweeper.
type RetentionWorker struct {
	hooks repo.WebhookRepository
	cfg   RetentionConfig
	log   *slog.Logger
}

func NewRetentionWorker(hooks repo.WebhookRepository, cfg RetentionConfig, log *slog.Logger) *RetentionWorker {
	return &RetentionWorker{
		hooks: hooks,
		cfg:   cfg.withDefaults(),
		log:   log.With(slog.String("component", "payload_retention")),
	}
}

// Run enforces retention until ctx is cancelled.
func (w *RetentionWorker) Run(ctx context.Context) {
	w.log.InfoContext(ctx, "payload retention started",
		slog.String("interval", w.cfg.Interval.String()),
		slog.String("settled_after", w.cfg.SettledAfter.String()),
		slog.String("unsettled_after", w.cfg.UnsettledAfter.String()))

	// A first pass at startup rather than one interval later: after a deploy
	// that had retention disabled, waiting six hours to begin minimising is a
	// choice nobody would make deliberately.
	jitter := time.Duration(rand.Int64N(int64(time.Minute)))
	timer := time.NewTimer(jitter)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			w.log.Info("payload retention stopped")
			return
		case <-timer.C:
			w.RunOnce(ctx)
			timer.Reset(w.cfg.Interval)
		}
	}
}

// RunOnce performs one pass and reports what remains.
//
// It works in batches until nothing is due or the context ends, so a backlog
// from a deployment that ran without retention is cleared on the first pass
// rather than 500 rows at a time every six hours.
func (w *RetentionWorker) RunOnce(ctx context.Context) repo.PurgeResult {
	var total repo.PurgeResult

	for {
		if ctx.Err() != nil {
			break
		}
		batch, err := w.hooks.PurgePayloads(ctx, w.cfg.query(w.cfg.BatchSize))
		if err != nil {
			w.log.ErrorContext(ctx, "payload purge failed", slog.String("error", err.Error()))
			break
		}
		total.Settled += batch.Settled
		total.Unsettled += batch.Unsettled

		// A short batch means the queue is drained.
		if batch.Total() < w.cfg.BatchSize {
			break
		}
	}

	if total.Total() > 0 {
		w.log.InfoContext(ctx, "webhook payloads minimised",
			slog.Int("settled", total.Settled),
			slog.Int("unsettled", total.Unsettled))
	}
	if total.Unsettled > 0 {
		// Reaching the outer bound means these were dead-lettered and nobody
		// acted for months. The data had to go, but losing the ability to
		// replay them is a real consequence and should not be silent.
		w.log.WarnContext(ctx, "purged payloads of unresolved events past the retention bound; "+
			"these can no longer be replayed",
			slog.Int("count", total.Unsettled),
			slog.String("bound", w.cfg.UnsettledAfter.String()))
	}

	w.report(ctx)
	return total
}

func (w *RetentionWorker) report(ctx context.Context) {
	stats, err := w.hooks.RetentionStats(ctx, w.cfg.query(0))
	if err != nil {
		w.log.ErrorContext(ctx, "could not read retention stats", slog.String("error", err.Error()))
		return
	}

	attrs := []any{
		slog.Int64("with_payload", stats.WithPayload),
		slog.Int64("purged", stats.Purged),
		slog.Int64("due_now", stats.DueNow),
	}
	if stats.OldestPayload != nil {
		attrs = append(attrs, slog.Duration("oldest_payload_age", time.Since(*stats.OldestPayload)))
	}

	// Anything still due after a pass means purging is not keeping up, which
	// is a compliance gap rather than a performance one.
	if stats.DueNow > 0 {
		w.log.WarnContext(ctx, "webhook payloads remain past their retention window", attrs...)
		return
	}
	w.log.DebugContext(ctx, "webhook payload retention is current", attrs...)
}

// Stats exposes the current position for the CLI.
func (w *RetentionWorker) Stats(ctx context.Context) (repo.RetentionStats, error) {
	return w.hooks.RetentionStats(ctx, w.cfg.query(0))
}
