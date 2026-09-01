-- =============================================================================
-- 00002_create_subscriptions_table
--
-- Local projection of Stripe Subscription objects. Stripe stays the source of
-- truth; this table exists so that entitlement checks never hit the Stripe API
-- on the request path.
-- =============================================================================

-- +goose Up

-- Mirrors Stripe's subscription.status enum exactly (2024+ API, incl. 'paused').
CREATE TYPE subscription_status AS ENUM (
    'incomplete',
    'incomplete_expired',
    'trialing',
    'active',
    'past_due',
    'canceled',
    'unpaid',
    'paused'
);

CREATE TABLE subscriptions (
    id                        UUID                PRIMARY KEY DEFAULT gen_random_uuid(),

    -- RESTRICT, not CASCADE: a hard user delete must not silently destroy billing
    -- history. Erasure flows have to settle subscriptions explicitly first.
    user_id                   UUID                NOT NULL
        REFERENCES users (id) ON UPDATE CASCADE ON DELETE RESTRICT,

    stripe_subscription_id    TEXT                NOT NULL,
    -- Denormalized from users: webhooks arrive keyed by customer, and this lets a
    -- handler resolve the subscription without a join.
    stripe_customer_id        TEXT                NOT NULL,
    stripe_price_id           TEXT                NOT NULL,
    stripe_product_id         TEXT,

    status                    subscription_status NOT NULL,
    quantity                  INTEGER             NOT NULL DEFAULT 1,
    currency                  TEXT,                       -- ISO 4217, lowercase (Stripe's form)
    unit_amount               BIGINT,                     -- minor units; NULL for metered prices

    current_period_start      TIMESTAMPTZ         NOT NULL,
    current_period_end        TIMESTAMPTZ         NOT NULL,

    cancel_at_period_end      BOOLEAN             NOT NULL DEFAULT FALSE,
    cancel_at                 TIMESTAMPTZ,
    canceled_at               TIMESTAMPTZ,
    ended_at                  TIMESTAMPTZ,

    trial_start               TIMESTAMPTZ,
    trial_end                 TIMESTAMPTZ,

    latest_invoice_id         TEXT,
    default_payment_method_id TEXT,

    -- Out-of-order webhook guard. Stripe delivers at-least-once and unordered;
    -- the service layer applies an update only when the incoming event's
    -- `created` is >= this value, so a retried old event cannot resurrect
    -- stale state. See 00003_create_processed_webhooks_table.
    last_stripe_event_id      TEXT,
    last_stripe_event_at      TIMESTAMPTZ,

    metadata                  JSONB               NOT NULL DEFAULT '{}'::jsonb,

    created_at                TIMESTAMPTZ         NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ         NOT NULL DEFAULT now(),

    CONSTRAINT subscriptions_stripe_subscription_id_key
        UNIQUE (stripe_subscription_id),

    CONSTRAINT subscriptions_stripe_subscription_id_format_chk
        CHECK (stripe_subscription_id ~ '^sub_[A-Za-z0-9]+$'),

    CONSTRAINT subscriptions_stripe_customer_id_format_chk
        CHECK (stripe_customer_id ~ '^cus_[A-Za-z0-9]+$'),

    CONSTRAINT subscriptions_stripe_price_id_format_chk
        CHECK (stripe_price_id ~ '^price_[A-Za-z0-9]+$'),

    CONSTRAINT subscriptions_quantity_positive_chk
        CHECK (quantity > 0),

    CONSTRAINT subscriptions_currency_format_chk
        CHECK (currency IS NULL OR currency ~ '^[a-z]{3}$'),

    CONSTRAINT subscriptions_unit_amount_non_negative_chk
        CHECK (unit_amount IS NULL OR unit_amount >= 0),

    CONSTRAINT subscriptions_period_order_chk
        CHECK (current_period_end > current_period_start),

    CONSTRAINT subscriptions_trial_order_chk
        CHECK (trial_start IS NULL OR trial_end IS NULL OR trial_end > trial_start),

    -- A canceled/ended subscription must carry the timestamp that explains it.
    CONSTRAINT subscriptions_canceled_at_chk
        CHECK (status <> 'canceled' OR canceled_at IS NOT NULL),

    CONSTRAINT subscriptions_metadata_is_object_chk
        CHECK (jsonb_typeof(metadata) = 'object')
);

-- --- Indices ------------------------------------------------------------------

-- "All subscriptions for this user", newest first (dashboard / account page).
CREATE INDEX idx_subscriptions_user_id_created_at
    ON subscriptions (user_id, created_at DESC);

-- Hot path: entitlement check - "does this user have a live subscription?"
-- Partial index keeps it tiny regardless of churned-customer volume.
CREATE INDEX idx_subscriptions_user_id_live
    ON subscriptions (user_id)
    WHERE status IN ('trialing', 'active', 'past_due');

-- Webhook fan-out: customer.subscription.* and invoice.* arrive keyed by customer.
CREATE INDEX idx_subscriptions_stripe_customer_id
    ON subscriptions (stripe_customer_id);

-- Reporting / plan-migration sweeps.
CREATE INDEX idx_subscriptions_stripe_price_id
    ON subscriptions (stripe_price_id);

CREATE INDEX idx_subscriptions_status
    ON subscriptions (status);

-- Renewal + dunning sweeps: "what expires in the next N hours?"
CREATE INDEX idx_subscriptions_current_period_end_live
    ON subscriptions (current_period_end)
    WHERE status IN ('trialing', 'active', 'past_due');

-- Trial-ending reminder job.
CREATE INDEX idx_subscriptions_trial_end
    ON subscriptions (trial_end)
    WHERE trial_end IS NOT NULL AND status = 'trialing';

-- Optional business rule. Enable only if the product sells exactly one seat/plan
-- per account; it will reject legitimate multi-product customers otherwise.
--
-- CREATE UNIQUE INDEX uq_subscriptions_one_live_per_user
--     ON subscriptions (user_id)
--     WHERE status IN ('trialing', 'active', 'past_due');

CREATE TRIGGER trg_subscriptions_set_updated_at
    BEFORE UPDATE ON subscriptions
    FOR EACH ROW
    WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE  subscriptions                      IS 'Local read-model of Stripe Subscriptions, kept current by webhook processing.';
COMMENT ON COLUMN subscriptions.stripe_customer_id   IS 'Denormalized from users.stripe_customer_id for join-free webhook routing.';
COMMENT ON COLUMN subscriptions.last_stripe_event_at IS 'Stripe `event.created` of the newest event applied to this row; guards against out-of-order delivery.';
COMMENT ON COLUMN subscriptions.unit_amount          IS 'Price in the currency minor unit (e.g. cents). NULL for metered/usage prices.';

-- +goose Down

DROP TRIGGER IF EXISTS trg_subscriptions_set_updated_at ON subscriptions;
DROP TABLE IF EXISTS subscriptions;
DROP TYPE IF EXISTS subscription_status;
