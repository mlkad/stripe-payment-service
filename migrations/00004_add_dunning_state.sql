-- =============================================================================
-- 00004_add_dunning_state
--
-- Dunning state for invoice.payment_failed / invoice.payment_succeeded.
--
-- Two things are being added, and they solve different problems.
--
-- 1. Dunning columns. When a renewal charge fails, Stripe retries on a schedule
--    for days or weeks before giving up. During that window the subscription is
--    still live (past_due grants access on purpose - see SubscriptionStatus.
--    IsLive) but the customer needs to be told, and support needs to see who is
--    at risk. None of that is derivable from `status` alone: past_due says
--    "a payment failed" but not when, how many times, why, or when Stripe will
--    try again.
--
-- 2. A SECOND ordering cursor. subscriptions.last_stripe_event_at guards
--    customer.subscription.* against out-of-order delivery. Invoice events must
--    NOT share it. They are a separate Stripe object with its own event stream,
--    and the two interleave freely: an invoice.payment_failed created at T5
--    would advance the shared cursor past a customer.subscription.updated
--    created at T4, and that subscription event - which carries the
--    authoritative status - would then be rejected as stale and silently
--    dropped. Each stream gets its own cursor so neither can starve the other.
-- =============================================================================

-- +goose Up

ALTER TABLE subscriptions
    -- NULL means "no failure outstanding". Set on invoice.payment_failed and
    -- cleared on the next success, so it doubles as the dunning flag.
    ADD COLUMN payment_failed_at       TIMESTAMPTZ,

    -- Consecutive failures. Reset to 0 on success, so dunning escalation can
    -- key off it ("third failure -> warn the customer harder").
    ADD COLUMN payment_failure_count   INTEGER     NOT NULL DEFAULT 0,

    -- Stripe's decline reason, shown to the customer. Bounded by the repository
    -- before it lands here.
    ADD COLUMN last_payment_error      TEXT,

    -- invoice.next_payment_attempt. Lets the UI say "we will retry on the 14th"
    -- instead of "payment failed", which is the difference between a customer
    -- who waits and one who churns.
    ADD COLUMN next_payment_attempt_at TIMESTAMPTZ,

    -- The invoice stream's own ordering cursor. See the header.
    ADD COLUMN last_invoice_event_id   TEXT,
    ADD COLUMN last_invoice_event_at   TIMESTAMPTZ,

    ADD CONSTRAINT subscriptions_payment_failure_count_chk
        CHECK (payment_failure_count >= 0),

    -- A failure count without a timestamp (or the reverse) means a handler
    -- updated one and not the other. Cheap to enforce, painful to debug later.
    ADD CONSTRAINT subscriptions_dunning_consistency_chk
        CHECK (
            (payment_failed_at IS NULL     AND payment_failure_count = 0)
            OR
            (payment_failed_at IS NOT NULL AND payment_failure_count > 0)
        );

-- The dunning worklist: every subscription with an outstanding failure. Partial
-- because healthy rows are the overwhelming majority and are never scanned by
-- this query.
CREATE INDEX idx_subscriptions_dunning
    ON subscriptions (payment_failed_at)
    WHERE payment_failed_at IS NOT NULL;

-- Sweeping for customers whose retry is due. Same reasoning: only rows in
-- dunning ever appear.
CREATE INDEX idx_subscriptions_next_payment_attempt
    ON subscriptions (next_payment_attempt_at)
    WHERE next_payment_attempt_at IS NOT NULL;

COMMENT ON COLUMN subscriptions.payment_failed_at       IS 'When the most recent invoice payment failed. NULL means no outstanding failure.';
COMMENT ON COLUMN subscriptions.payment_failure_count   IS 'Consecutive failed payment attempts; reset to 0 on success.';
COMMENT ON COLUMN subscriptions.next_payment_attempt_at IS 'Stripe invoice.next_payment_attempt - when the dunning retry is scheduled.';
COMMENT ON COLUMN subscriptions.last_invoice_event_at   IS 'Ordering cursor for the invoice.* event stream, kept separate from last_stripe_event_at so the two streams cannot reject each other.';

-- +goose Down

DROP INDEX IF EXISTS idx_subscriptions_next_payment_attempt;
DROP INDEX IF EXISTS idx_subscriptions_dunning;

ALTER TABLE subscriptions
    DROP CONSTRAINT IF EXISTS subscriptions_dunning_consistency_chk,
    DROP CONSTRAINT IF EXISTS subscriptions_payment_failure_count_chk,
    DROP COLUMN IF EXISTS last_invoice_event_at,
    DROP COLUMN IF EXISTS last_invoice_event_id,
    DROP COLUMN IF EXISTS next_payment_attempt_at,
    DROP COLUMN IF EXISTS last_payment_error,
    DROP COLUMN IF EXISTS payment_failure_count,
    DROP COLUMN IF EXISTS payment_failed_at;
