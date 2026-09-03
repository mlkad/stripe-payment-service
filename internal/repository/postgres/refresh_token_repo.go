package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mlkad/stripe-payment-service/internal/domain"
)

// RefreshTokenRepository is the port the auth service depends on.
type RefreshTokenRepository interface {
	CreateRefreshToken(ctx context.Context, t *domain.RefreshToken) error

	// ConsumeRefreshToken exchanges a token for its successor in one
	// transaction, reporting reuse rather than silently issuing again.
	ConsumeRefreshToken(ctx context.Context, in ConsumeRefreshToken) (*domain.RefreshToken, error)

	RevokeFamily(ctx context.Context, familyID uuid.UUID, reason string) (int64, error)

	// RevokeFamilyByToken ends the session a token belongs to, without the
	// caller needing to know its family.
	RevokeFamilyByToken(ctx context.Context, tokenHash, reason string) (int64, error)
	RevokeAllForUser(ctx context.Context, userID uuid.UUID, reason string) (int64, error)

	// DeleteExpiredRefreshTokens prunes rows nothing can use again.
	DeleteExpiredRefreshTokens(ctx context.Context, gracePeriod time.Duration, limit int) (int64, error)
}

// ConsumeRefreshToken describes one rotation.
type ConsumeRefreshToken struct {
	// PresentedHash is the SHA-256 of the token the client sent.
	PresentedHash string

	// SuccessorHash and SuccessorExpiresAt describe the replacement issued in
	// the same transaction as the exchange.
	SuccessorHash      string
	SuccessorExpiresAt time.Time
}

type RefreshTokenRepo struct {
	pool *pgxpool.Pool
}

func NewRefreshTokenRepo(pool *pgxpool.Pool) *RefreshTokenRepo {
	return &RefreshTokenRepo{pool: pool}
}

var _ RefreshTokenRepository = (*RefreshTokenRepo)(nil)

const refreshTokenColumns = `
	id, user_id, token_hash, family_id, issued_at, expires_at,
	used_at, revoked_at, revoked_reason, created_at, updated_at`

func (r *RefreshTokenRepo) CreateRefreshToken(ctx context.Context, t *domain.RefreshToken) error {
	const query = `
		INSERT INTO refresh_tokens (user_id, token_hash, family_id, expires_at)
		VALUES ($1, $2, $3, $4)
		RETURNING id, issued_at, created_at, updated_at`

	err := r.pool.QueryRow(ctx, query, t.UserID, t.TokenHash, t.FamilyID, t.ExpiresAt).
		Scan(&t.ID, &t.IssuedAt, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return mapError("create refresh token", err)
	}
	return nil
}

// ConsumeRefreshToken performs the whole rotation in one transaction.
//
// The row is locked before it is examined, for the same reason
// UpdateSubscriptionStatus locks: the check and the write have to be one step.
// Two requests arriving with the same token would otherwise both find it
// unused, both mark it used, and both issue a successor - which is exactly the
// situation reuse detection exists to catch, missed because the detector
// itself raced.
//
// Returns domain.ErrNotFound for an unknown token, auth-level reuse as
// ErrRefreshTokenReused via the caller, and the newly issued token otherwise.
func (r *RefreshTokenRepo) ConsumeRefreshToken(ctx context.Context, in ConsumeRefreshToken) (*domain.RefreshToken, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, mapError("consume refresh token: begin", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const lockQuery = `SELECT ` + refreshTokenColumns + `
		FROM refresh_tokens WHERE token_hash = $1 FOR UPDATE`

	presented, err := scanRefreshToken(tx.QueryRow(ctx, lockQuery, in.PresentedHash))
	if err != nil {
		return nil, mapError("consume refresh token: lock", err)
	}

	// A token already exchanged is being presented a second time. Whether the
	// thief or the victim is holding it cannot be determined, so the family
	// goes and both sign in again.
	if presented.UsedAt != nil {
		const revokeQuery = `
			UPDATE refresh_tokens
			SET revoked_at = now(), revoked_reason = $2
			WHERE family_id = $1 AND revoked_at IS NULL`
		if _, err := tx.Exec(ctx, revokeQuery, presented.FamilyID,
			"refresh token reuse detected"); err != nil {
			return nil, mapError("consume refresh token: revoke family", err)
		}
		// Committed on purpose: the revocation must survive even though the
		// caller is about to be told this failed.
		if err := tx.Commit(ctx); err != nil {
			return nil, mapError("consume refresh token: commit revocation", err)
		}
		return nil, domain.ErrTokenReused
	}

	if presented.RevokedAt != nil || time.Now().After(presented.ExpiresAt) {
		return nil, domain.ErrNotFound
	}

	const markUsed = `UPDATE refresh_tokens SET used_at = now() WHERE id = $1`
	if _, err := tx.Exec(ctx, markUsed, presented.ID); err != nil {
		return nil, mapError("consume refresh token: mark used", err)
	}

	const issue = `
		INSERT INTO refresh_tokens (user_id, token_hash, family_id, expires_at)
		VALUES ($1, $2, $3, $4)
		RETURNING ` + refreshTokenColumns

	// The successor inherits the family, so the whole chain stays revocable as
	// a unit.
	successor, err := scanRefreshToken(tx.QueryRow(ctx, issue,
		presented.UserID, in.SuccessorHash, presented.FamilyID, in.SuccessorExpiresAt))
	if err != nil {
		return nil, mapError("consume refresh token: issue successor", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, mapError("consume refresh token: commit", err)
	}
	return successor, nil
}

func (r *RefreshTokenRepo) RevokeFamily(ctx context.Context, familyID uuid.UUID, reason string) (int64, error) {
	const query = `
		UPDATE refresh_tokens
		SET revoked_at = now(), revoked_reason = $2
		WHERE family_id = $1 AND revoked_at IS NULL`

	tag, err := r.pool.Exec(ctx, query, familyID, truncate(reason))
	if err != nil {
		return 0, mapError("revoke refresh token family", err)
	}
	return tag.RowsAffected(), nil
}

// RevokeFamilyByToken revokes the family the given token belongs to.
//
// One statement rather than a lookup followed by a revoke: the two-step version
// would report ErrNotFound for a token that another request revoked in between,
// turning a successful logout into an error.
func (r *RefreshTokenRepo) RevokeFamilyByToken(ctx context.Context, tokenHash, reason string) (int64, error) {
	const query = `
		UPDATE refresh_tokens
		SET revoked_at = now(), revoked_reason = $2
		WHERE revoked_at IS NULL
		  AND family_id = (SELECT family_id FROM refresh_tokens WHERE token_hash = $1)`

	tag, err := r.pool.Exec(ctx, query, tokenHash, truncate(reason))
	if err != nil {
		return 0, mapError("revoke refresh token family by token", err)
	}
	return tag.RowsAffected(), nil
}

// RevokeAllForUser ends every session. This is what a password change or a
// compromised account needs, and what "sign out everywhere" means.
func (r *RefreshTokenRepo) RevokeAllForUser(ctx context.Context, userID uuid.UUID, reason string) (int64, error) {
	const query = `
		UPDATE refresh_tokens
		SET revoked_at = now(), revoked_reason = $2
		WHERE user_id = $1 AND revoked_at IS NULL`

	tag, err := r.pool.Exec(ctx, query, userID, truncate(reason))
	if err != nil {
		return 0, mapError("revoke refresh tokens for user", err)
	}
	return tag.RowsAffected(), nil
}

// DeleteExpiredRefreshTokens removes rows past their expiry.
//
// Deleted rather than kept: unlike the webhook ledger, an expired refresh token
// is not an audit record, and its family_id and timing are a small amount of
// behavioural data with no remaining purpose. The grace period keeps recently
// expired rows briefly so reuse detection still fires on a token that expired
// between theft and use.
func (r *RefreshTokenRepo) DeleteExpiredRefreshTokens(ctx context.Context, gracePeriod time.Duration, limit int) (int64, error) {
	const query = `
		DELETE FROM refresh_tokens
		WHERE id IN (
			SELECT id FROM refresh_tokens
			WHERE expires_at < now() - make_interval(secs => $1)
			ORDER BY expires_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)`

	tag, err := r.pool.Exec(ctx, query, gracePeriod.Seconds(), limit)
	if err != nil {
		return 0, mapError("delete expired refresh tokens", err)
	}
	return tag.RowsAffected(), nil
}

func scanRefreshToken(r row) (*domain.RefreshToken, error) {
	var t domain.RefreshToken
	err := r.Scan(
		&t.ID, &t.UserID, &t.TokenHash, &t.FamilyID, &t.IssuedAt, &t.ExpiresAt,
		&t.UsedAt, &t.RevokedAt, &t.RevokedReason, &t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
