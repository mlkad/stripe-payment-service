-- =============================================================================
-- verify-schema.sql - executable specification of the database contract.
--
-- Asserts every constraint, index guarantee and trigger behaviour the service
-- depends on, including the webhook idempotency claim. Runs entirely inside a
-- transaction that is rolled back, so it is safe against any migrated database.
--
--   make verify-schema
-- =============================================================================

\set ON_ERROR_STOP on
BEGIN;
SET LOCAL client_min_messages = notice;

-- helper: assert that a statement violates a named constraint
CREATE FUNCTION assert_rejects(stmt text, label text) RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
    BEGIN
        EXECUTE stmt;
    EXCEPTION WHEN others THEN
        RAISE NOTICE 'PASS  rejected: %  (%)', label, SQLERRM;
        RETURN;
    END;
    RAISE EXCEPTION 'FAIL  accepted but should have been rejected: %', label;
END;
$$;

-- ============================================================ users
INSERT INTO users (email, full_name) VALUES ('Ada@Example.com', 'Ada Lovelace');

DO $$
DECLARE u users;
BEGIN
    SELECT * INTO u FROM users WHERE email = 'ada@EXAMPLE.com';   -- citext: case-insensitive
    IF u.id IS NULL THEN RAISE EXCEPTION 'FAIL  citext lookup missed'; END IF;
    IF u.metadata <> '{}'::jsonb THEN RAISE EXCEPTION 'FAIL  metadata default'; END IF;
    IF u.created_at IS NULL OR u.updated_at IS NULL THEN RAISE EXCEPTION 'FAIL  timestamp defaults'; END IF;
    RAISE NOTICE 'PASS  citext lookup + defaults';
END $$;

SELECT assert_rejects($$INSERT INTO users (email) VALUES ('ADA@example.com')$$, 'duplicate email (case-insensitive)');
SELECT assert_rejects($$INSERT INTO users (email) VALUES ('not-an-email')$$,     'malformed email');
SELECT assert_rejects($$UPDATE users SET stripe_customer_id = 'sub_123'$$,       'wrong Stripe id prefix in stripe_customer_id');
SELECT assert_rejects($$UPDATE users SET metadata = '[]'::jsonb$$,               'non-object metadata');

-- trigger: updated_at bumps on real change...
DO $$
DECLARE before_ts timestamptz; after_ts timestamptz;
BEGIN
    SELECT updated_at INTO before_ts FROM users WHERE email='ada@example.com';
    PERFORM pg_sleep(0.01);
    UPDATE users SET stripe_customer_id='cus_TestAda001' WHERE email='ada@example.com';
    SELECT updated_at INTO after_ts FROM users WHERE email='ada@example.com';
    IF after_ts <= before_ts THEN RAISE EXCEPTION 'FAIL  updated_at did not advance'; END IF;
    RAISE NOTICE 'PASS  updated_at trigger fires on change';

    -- ...but not on a no-op UPDATE (the WHEN clause)
    before_ts := after_ts;
    PERFORM pg_sleep(0.01);
    UPDATE users SET stripe_customer_id='cus_TestAda001' WHERE email='ada@example.com';
    SELECT updated_at INTO after_ts FROM users WHERE email='ada@example.com';
    IF after_ts <> before_ts THEN RAISE EXCEPTION 'FAIL  updated_at bumped on a no-op UPDATE'; END IF;
    RAISE NOTICE 'PASS  updated_at trigger skips no-op UPDATE';
END $$;

-- soft delete frees the email for reuse; stripe_customer_id stays globally unique
UPDATE users SET deleted_at = now() WHERE email='ada@example.com';
INSERT INTO users (email, full_name) VALUES ('ada@example.com', 'Ada Reborn');
SELECT assert_rejects($$UPDATE users SET stripe_customer_id='cus_TestAda001' WHERE full_name='Ada Reborn'$$,
                      'stripe_customer_id reuse across soft-deleted user');
DO $$ BEGIN RAISE NOTICE 'PASS  soft-deleted email is reusable'; END $$;

-- ============================================================ subscriptions
INSERT INTO users (email, stripe_customer_id) VALUES ('grace@example.com', 'cus_Grace001');

INSERT INTO subscriptions (
    user_id, stripe_subscription_id, stripe_customer_id, stripe_price_id,
    status, currency, unit_amount, current_period_start, current_period_end)
SELECT id, 'sub_Grace001', 'cus_Grace001', 'price_Pro001',
       'active', 'usd', 2900, now(), now() + interval '30 days'
FROM users WHERE email='grace@example.com';

SELECT assert_rejects($$
    INSERT INTO subscriptions (user_id, stripe_subscription_id, stripe_customer_id, stripe_price_id,
                               status, current_period_start, current_period_end)
    SELECT id,'sub_Grace001','cus_Grace001','price_Pro001','active',now(),now()+interval '30 days'
    FROM users WHERE email='grace@example.com'$$, 'duplicate stripe_subscription_id');

SELECT assert_rejects($$
    INSERT INTO subscriptions (user_id, stripe_subscription_id, stripe_customer_id, stripe_price_id,
                               status, current_period_start, current_period_end)
    SELECT id,'sub_Bad001','cus_Grace001','price_Pro001','active',now(),now()-interval '1 day'
    FROM users WHERE email='grace@example.com'$$, 'current_period_end before start');

SELECT assert_rejects($$UPDATE subscriptions SET quantity = 0 WHERE stripe_subscription_id='sub_Grace001'$$,
                      'quantity <= 0');
SELECT assert_rejects($$UPDATE subscriptions SET currency = 'USD' WHERE stripe_subscription_id='sub_Grace001'$$,
                      'uppercase currency');
SELECT assert_rejects($$UPDATE subscriptions SET status='canceled' WHERE stripe_subscription_id='sub_Grace001'$$,
                      'canceled status without canceled_at');
SELECT assert_rejects($$DELETE FROM users WHERE email='grace@example.com'$$,
                      'hard-deleting a user that still has billing history');
SELECT assert_rejects($$INSERT INTO subscriptions (user_id, stripe_subscription_id, stripe_customer_id,
                          stripe_price_id, status, current_period_start, current_period_end)
                        VALUES (gen_random_uuid(),'sub_Orphan','cus_X1','price_X1','active',now(),now()+interval '1 day')$$,
                      'subscription referencing a non-existent user');

-- ============================================================ processed_webhooks
-- first delivery: claim succeeds
INSERT INTO processed_webhooks (event_id, event_type, api_version, livemode, stripe_created_at, payload)
VALUES ('evt_Test001','customer.subscription.updated','2024-06-20',false, now(), '{"id":"evt_Test001"}'::jsonb);

DO $$
DECLARE claimed text;
BEGIN
    -- duplicate delivery while the first is still in flight: must NOT be claimed
    INSERT INTO processed_webhooks (event_id, event_type, stripe_created_at, status, attempts)
    VALUES ('evt_Test001','customer.subscription.updated', now(), 'processing', 1)
    ON CONFLICT (event_id) DO UPDATE
        SET status='processing', attempts = processed_webhooks.attempts + 1, updated_at = now()
        WHERE processed_webhooks.status = 'failed'
           OR (processed_webhooks.status = 'processing' AND processed_webhooks.updated_at < now() - INTERVAL '5 minutes')
    RETURNING event_id INTO claimed;

    IF claimed IS NOT NULL THEN RAISE EXCEPTION 'FAIL  in-flight event was re-claimed'; END IF;
    RAISE NOTICE 'PASS  duplicate delivery of an in-flight event is not re-claimed';
END $$;

-- settle it
UPDATE processed_webhooks SET status='succeeded', processed_at=now() WHERE event_id='evt_Test001';

DO $$
DECLARE claimed text;
BEGIN
    INSERT INTO processed_webhooks (event_id, event_type, stripe_created_at, status, attempts)
    VALUES ('evt_Test001','customer.subscription.updated', now(), 'processing', 1)
    ON CONFLICT (event_id) DO UPDATE
        SET status='processing', attempts = processed_webhooks.attempts + 1
        WHERE processed_webhooks.status = 'failed'
    RETURNING event_id INTO claimed;
    IF claimed IS NOT NULL THEN RAISE EXCEPTION 'FAIL  succeeded event was re-claimed'; END IF;
    RAISE NOTICE 'PASS  Stripe retry of an already-succeeded event is a no-op';
END $$;

-- a failed event IS re-claimable
INSERT INTO processed_webhooks (event_id, event_type, stripe_created_at, status, last_error)
VALUES ('evt_Test002','invoice.payment_failed', now(), 'failed', 'downstream timeout');

DO $$
DECLARE claimed text; n int;
BEGIN
    INSERT INTO processed_webhooks (event_id, event_type, stripe_created_at, status, attempts)
    VALUES ('evt_Test002','invoice.payment_failed', now(), 'processing', 1)
    ON CONFLICT (event_id) DO UPDATE
        SET status='processing', attempts = processed_webhooks.attempts + 1, last_error = NULL
        WHERE processed_webhooks.status = 'failed'
    RETURNING event_id INTO claimed;
    IF claimed IS NULL THEN RAISE EXCEPTION 'FAIL  failed event was not re-claimable'; END IF;
    SELECT attempts INTO n FROM processed_webhooks WHERE event_id='evt_Test002';
    IF n <> 2 THEN RAISE EXCEPTION 'FAIL  attempts not incremented (got %)', n; END IF;
    RAISE NOTICE 'PASS  failed event is re-claimable and attempts increments';
END $$;

SELECT assert_rejects($$INSERT INTO processed_webhooks (event_id,event_type,stripe_created_at)
                        VALUES ('sub_NotAnEvent','x.y', now())$$, 'non-evt_ event id');
SELECT assert_rejects($$UPDATE processed_webhooks SET status='succeeded' WHERE event_id='evt_Test002'$$,
                      'succeeded without processed_at');
SELECT assert_rejects($$UPDATE processed_webhooks SET status='failed', last_error=NULL WHERE event_id='evt_Test002'$$,
                      'failed without last_error');

SELECT 'ALL SCHEMA ASSERTIONS PASSED' AS result;
ROLLBACK;
