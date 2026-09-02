-- =============================================================================
-- 00005_payload_retention
--
-- Data minimisation for processed_webhooks.payload.
--
-- The column holds the raw Stripe event, which carries personal data: customer
-- email, name, billing address, and the last four digits of a card. Keeping it
-- forever is both a growing table and a growing liability - under GDPR, storage
-- has to be limited to what the purpose actually requires.
--
-- The purpose is replay. The sweeper reconstructs a failed event from this
-- column, so it cannot simply be dropped on a fixed clock. Two different
-- windows follow from that:
--
--   * settled events (succeeded / skipped) have no replay value left, so their
--     payload goes early - 30 days by default, enough for an operator to
--     investigate something that went wrong last month.
--
--   * unsettled events (failed) keep theirs until they are resolved or until a
--     much longer outer bound. The outer bound is not optional: a privacy
--     obligation does not pause because a retry queue is stuck, and without it
--     one permanently dead-lettered event holds personal data indefinitely.
--
-- What replaces the payload is an allowlisted skeleton, not a redaction. A
-- denylist that strips known-sensitive fields would silently start leaking the
-- day Stripe adds a field nobody thought about, and it would look compliant
-- while doing it. An allowlist can only ever be too conservative.
-- =============================================================================

-- +goose Up

ALTER TABLE processed_webhooks
    -- Evidence that minimisation ran, and when. An auditor asking "prove you
    -- delete this" needs an answer that is not "trust the cron job".
    ADD COLUMN payload_purged_at TIMESTAMPTZ,

    -- A purged payload must be the skeleton or nothing, never a full event
    -- again. Cheap to state, and it makes a bug that re-populates the column
    -- fail loudly instead of quietly restoring personal data.
    ADD CONSTRAINT processed_webhooks_purged_payload_chk
        CHECK (
            payload_purged_at IS NULL
            OR payload IS NULL
            OR NOT (payload ? 'data' AND jsonb_typeof(payload -> 'data') = 'object'
                    AND (payload #> '{data,object}') ?| ARRAY['customer_details', 'billing_details', 'customer_email'])
        );

-- Purge candidates. Partial on `payload IS NOT NULL`, so the index covers only
-- rows still holding data and shrinks toward nothing as retention catches up -
-- the opposite of the table it indexes.
CREATE INDEX idx_processed_webhooks_payload_retention
    ON processed_webhooks (received_at)
    WHERE payload IS NOT NULL;

COMMENT ON COLUMN processed_webhooks.payload IS
    'Raw event JSON for replay/audit. Contains personal data until retention replaces it with an allowlisted skeleton; see migration 00005.';
COMMENT ON COLUMN processed_webhooks.payload_purged_at IS
    'When retention reduced this payload to its non-identifying skeleton. NULL means the full event is still stored.';

-- +goose Down

DROP INDEX IF EXISTS idx_processed_webhooks_payload_retention;

ALTER TABLE processed_webhooks
    DROP CONSTRAINT IF EXISTS processed_webhooks_purged_payload_chk,
    DROP COLUMN IF EXISTS payload_purged_at;

COMMENT ON COLUMN processed_webhooks.payload IS
    'Raw event JSON for replay/audit. May contain PII - subject to retention policy.';
