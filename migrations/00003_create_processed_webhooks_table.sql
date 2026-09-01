-- =============================================================================
-- 00003_create_processed_webhooks_table
--
-- Database-backed idempotency for Stripe webhook delivery.
--
-- Stripe guarantees at-least-once delivery, unordered, with retries for up to
-- 3 days. This table is the dedupe ledger: a handler *claims* an event row in
-- its own transaction before doing any work, so a duplicate delivery - even one
-- racing on another pod - can never apply the same event twice.
--
-- Claim (run inside the handler's transaction):
--
--   INSERT INTO processed_webhooks
--       (event_id, event_type, api_version, livemode, request_id,
--        stripe_created_at, payload, status, attempts)
--   VALUES ($1, $2, $3, $4, $5, $6, $7, 'processing', 1)
--   ON CONFLICT (event_id) DO UPDATE
--       SET status     = 'processing',
--           attempts   = processed_webhooks.attempts + 1,
--           last_error = NULL,
--           updated_at = now()
--       WHERE processed_webhooks.status = 'failed'
--          OR (processed_webhooks.status = 'processing'
--              AND processed_webhooks.updated_at < now() - INTERVAL '5 minutes')
--   RETURNING event_id;
--
-- Zero rows returned => already succeeded, or another worker holds the claim.
-- Reply 200 and drop it. On success mark 'succeeded'; on error mark 'failed'
-- and return 5xx so Stripe retries.
-- =============================================================================

-- +goose Up

CREATE TYPE webhook_status AS ENUM (
    'processing',   -- claimed by a worker, in flight
    'succeeded',    -- applied; terminal
    'failed',       -- handler errored; eligible for Stripe retry / manual replay
    'skipped'       -- event type this service does not subscribe to; terminal
);

CREATE TABLE processed_webhooks (
    -- Stripe's own event id is the natural idempotency key. No surrogate PK:
    -- the unique constraint IS the concurrency control.
    event_id          TEXT           PRIMARY KEY,

    event_type        TEXT           NOT NULL,   -- e.g. 'customer.subscription.updated'
    api_version       TEXT,                      -- Stripe API version that rendered the event
    livemode          BOOLEAN        NOT NULL DEFAULT FALSE,
    request_id        TEXT,                      -- event.request.id, for correlating with our own calls

    status            webhook_status NOT NULL DEFAULT 'processing',
    attempts          INTEGER        NOT NULL DEFAULT 1,
    last_error        TEXT,

    -- Raw event body. Enables deterministic replay after a bad deploy and gives
    -- auditors the exact bytes we acted on.
    -- NOTE: may contain PII (email, billing address). Covered by the retention
    -- policy below - prune or null this column, not the ledger row.
    payload           JSONB,

    stripe_created_at TIMESTAMPTZ    NOT NULL,   -- event.created; the ordering key
    received_at       TIMESTAMPTZ    NOT NULL DEFAULT now(),
    processed_at      TIMESTAMPTZ,

    created_at        TIMESTAMPTZ    NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ    NOT NULL DEFAULT now(),

    CONSTRAINT processed_webhooks_event_id_format_chk
        CHECK (event_id ~ '^evt_[A-Za-z0-9]+$'),

    CONSTRAINT processed_webhooks_event_type_not_blank_chk
        CHECK (char_length(btrim(event_type)) > 0),

    CONSTRAINT processed_webhooks_attempts_positive_chk
        CHECK (attempts >= 1),

    -- A terminal row must record when it settled; an in-flight one must not.
    CONSTRAINT processed_webhooks_processed_at_chk
        CHECK (
            (status IN ('succeeded', 'skipped') AND processed_at IS NOT NULL)
            OR (status IN ('processing', 'failed') AND processed_at IS NULL)
        ),

    CONSTRAINT processed_webhooks_last_error_chk
        CHECK (status <> 'failed' OR last_error IS NOT NULL),

    CONSTRAINT processed_webhooks_payload_is_object_chk
        CHECK (payload IS NULL OR jsonb_typeof(payload) = 'object')
);

-- --- Indices ------------------------------------------------------------------

-- Dead-letter queue view + the sweeper that reclaims rows abandoned by a crashed
-- pod. Partial: succeeded rows are the overwhelming majority and are never scanned.
CREATE INDEX idx_processed_webhooks_unsettled
    ON processed_webhooks (status, updated_at)
    WHERE status IN ('processing', 'failed');

-- Operational drill-down: "show me every invoice.payment_failed from yesterday".
CREATE INDEX idx_processed_webhooks_event_type_created
    ON processed_webhooks (event_type, stripe_created_at DESC);

-- Retention pruning scans this table in physical (append) order, so BRIN gives
-- the same pruning power as a btree at a fraction of the size.
CREATE INDEX idx_processed_webhooks_received_at_brin
    ON processed_webhooks USING BRIN (received_at)
    WITH (pages_per_range = 32);

CREATE TRIGGER trg_processed_webhooks_set_updated_at
    BEFORE UPDATE ON processed_webhooks
    FOR EACH ROW
    WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE  processed_webhooks                   IS 'Idempotency ledger for Stripe webhook events. One row per event id, ever.';
COMMENT ON COLUMN processed_webhooks.event_id          IS 'Stripe event id (evt_...). Primary key: the dedupe guarantee.';
COMMENT ON COLUMN processed_webhooks.stripe_created_at IS 'event.created. Compared against subscriptions.last_stripe_event_at to reject out-of-order updates.';
COMMENT ON COLUMN processed_webhooks.payload           IS 'Raw event JSON for replay/audit. May contain PII - subject to retention policy.';
COMMENT ON COLUMN processed_webhooks.attempts          IS 'Number of times this event has been claimed for processing.';

-- Retention: keep the ledger rows (they are the dedupe guarantee) far longer
-- than the payloads. Stripe retries for at most 3 days, so a 90-day ledger with
-- 30-day payloads is a safe default. Wire this into a scheduled job, not a
-- migration:
--
--   UPDATE processed_webhooks SET payload = NULL
--    WHERE payload IS NOT NULL AND received_at < now() - INTERVAL '30 days';
--
--   DELETE FROM processed_webhooks
--    WHERE status IN ('succeeded', 'skipped')
--      AND received_at < now() - INTERVAL '90 days';

-- +goose Down

DROP TRIGGER IF EXISTS trg_processed_webhooks_set_updated_at ON processed_webhooks;
DROP TABLE IF EXISTS processed_webhooks;
DROP TYPE IF EXISTS webhook_status;
