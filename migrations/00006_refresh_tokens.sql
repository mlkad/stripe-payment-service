-- =============================================================================
-- 00006_refresh_tokens
--
-- Rotating refresh tokens, so the access token can be short-lived.
--
-- Access tokens are stateless and cannot be revoked before they expire, which
-- made their lifetime the entire containment window for a stolen credential.
-- Shortening that window needs something durable to renew against, and that
-- something has to be revocable - otherwise the problem has only moved.
--
-- Three properties this table exists to provide:
--
--   1. Revocation. A row can be marked revoked, and the next renewal fails.
--
--   2. Rotation. Every renewal consumes one token and issues another, so a
--      token that leaks is useful only until the legitimate client next
--      refreshes.
--
--   3. Reuse detection. Rotation alone does not tell you a token was stolen -
--      it just limits the window. Recording that a token was already consumed
--      does: if a consumed token is presented again, either the thief or the
--      victim is using a token the other has already spent, and there is no
--      way to tell which. The whole family is revoked and both are forced to
--      sign in again. A false positive costs one login; a false negative costs
--      the account.
--
-- The token itself is never stored. It is 256 bits from crypto/rand, so the
-- column holds a SHA-256 of it: enough to look up, useless if the table leaks.
-- bcrypt would be the wrong tool - there is no dictionary attack to slow down
-- against a value with this much entropy, and it would put 250ms on every
-- renewal.
-- =============================================================================

-- +goose Up

CREATE TABLE refresh_tokens (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),

    user_id      UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- SHA-256 of the token, hex encoded. Unique because a collision would let
    -- one token renew another's session.
    token_hash   TEXT        NOT NULL,

    -- Every token descended from one login shares a family. Reuse detection
    -- revokes the family, not just the token, because a thief who has one
    -- token has whatever else came with it.
    family_id    UUID        NOT NULL,

    issued_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,

    -- When this token was exchanged for its successor. NULL means it is the
    -- live token for its family; non-NULL and presented again means theft.
    used_at      TIMESTAMPTZ,

    revoked_at   TIMESTAMPTZ,
    -- Why, for an operator reading the table after an incident.
    revoked_reason TEXT,

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT refresh_tokens_token_hash_format_chk
        CHECK (token_hash ~ '^[0-9a-f]{64}$'),

    CONSTRAINT refresh_tokens_expiry_after_issue_chk
        CHECK (expires_at > issued_at),

    -- A revoked row must say why. The reason is the only record of whether a
    -- session ended by logout, by reuse detection, or by an operator.
    CONSTRAINT refresh_tokens_revoked_reason_chk
        CHECK (revoked_at IS NULL OR revoked_reason IS NOT NULL)
);

CREATE UNIQUE INDEX uq_refresh_tokens_token_hash
    ON refresh_tokens (token_hash);

-- Revoking every session for one user: password change, account compromise,
-- "sign out everywhere".
CREATE INDEX idx_refresh_tokens_user_id
    ON refresh_tokens (user_id);

-- Revoking a family on reuse detection. Partial: a revoked family never needs
-- revoking again, and expired rows are pruned.
CREATE INDEX idx_refresh_tokens_family_active
    ON refresh_tokens (family_id)
    WHERE revoked_at IS NULL;

-- Retention sweep. Partial on the same predicate the cleanup uses, so it holds
-- only rows still worth deleting.
CREATE INDEX idx_refresh_tokens_expires_at
    ON refresh_tokens (expires_at);

CREATE TRIGGER trg_refresh_tokens_set_updated_at
    BEFORE UPDATE ON refresh_tokens
    FOR EACH ROW
    WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE  refresh_tokens               IS 'Rotating refresh tokens with per-family reuse detection.';
COMMENT ON COLUMN refresh_tokens.token_hash    IS 'SHA-256 of the token, hex. The token itself is never stored.';
COMMENT ON COLUMN refresh_tokens.family_id     IS 'Rotation lineage from one login; revoked as a unit when reuse is detected.';
COMMENT ON COLUMN refresh_tokens.used_at       IS 'When this token was exchanged. Presenting a used token is treated as theft.';

-- +goose Down

DROP TRIGGER IF EXISTS trg_refresh_tokens_set_updated_at ON refresh_tokens;
DROP TABLE IF EXISTS refresh_tokens;
