package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mlkad/stripe-payment-service/internal/domain"
)

// UserRepository is the port the service layer depends on.
type UserRepository interface {
	CreateUser(ctx context.Context, u *domain.User) error
	GetUserByID(ctx context.Context, id uuid.UUID) (*domain.User, error)
	GetUserByStripeCustomerID(ctx context.Context, customerID string) (*domain.User, error)
	UpdateUser(ctx context.Context, u *domain.User) error
}

type UserRepo struct {
	pool *pgxpool.Pool
}

func NewUserRepo(pool *pgxpool.Pool) *UserRepo {
	return &UserRepo{pool: pool}
}

var _ UserRepository = (*UserRepo)(nil)

const userColumns = `
	id, email, email_verified_at, password_hash, full_name,
	stripe_customer_id, metadata, created_at, updated_at, deleted_at`

func (r *UserRepo) CreateUser(ctx context.Context, u *domain.User) error {
	if err := u.Validate(); err != nil {
		return err
	}

	const query = `
		INSERT INTO users (email, email_verified_at, password_hash, full_name, stripe_customer_id, metadata)
		VALUES ($1, $2, $3, $4, $5, COALESCE($6, '{}'::jsonb))
		RETURNING id, created_at, updated_at`

	err := r.pool.QueryRow(ctx, query,
		u.Email, u.EmailVerifiedAt, u.PasswordHash, u.FullName, u.StripeCustomerID, u.Metadata,
	).Scan(&u.ID, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return mapError("create user", err)
	}
	return nil
}

func (r *UserRepo) GetUserByID(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	const query = `SELECT ` + userColumns + `
		FROM users
		WHERE id = $1 AND deleted_at IS NULL`

	u, err := scanUser(r.pool.QueryRow(ctx, query, id))
	if err != nil {
		return nil, mapError("get user by id", err)
	}
	return u, nil
}

// GetUserByStripeCustomerID resolves the customer on every inbound webhook, so
// it is the hottest read in the service. The predicate matches
// uq_users_stripe_customer_id; the planner proves `= $1` implies IS NOT NULL and
// uses that partial index without further help.
func (r *UserRepo) GetUserByStripeCustomerID(ctx context.Context, customerID string) (*domain.User, error) {
	const query = `SELECT ` + userColumns + `
		FROM users
		WHERE stripe_customer_id = $1 AND deleted_at IS NULL`

	u, err := scanUser(r.pool.QueryRow(ctx, query, customerID))
	if err != nil {
		return nil, mapError("get user by stripe customer id", err)
	}
	return u, nil
}

// UpdateUser writes the mutable fields. updated_at is not set here: the
// trg_users_set_updated_at trigger owns it, and its WHEN clause means a no-op
// UPDATE leaves the timestamp alone.
func (r *UserRepo) UpdateUser(ctx context.Context, u *domain.User) error {
	if err := u.Validate(); err != nil {
		return err
	}
	if u.ID == uuid.Nil {
		return domain.FieldError{Field: "id", Detail: "is required for update"}
	}

	const query = `
		UPDATE users
		SET email = $2,
		    email_verified_at = $3,
		    password_hash = $4,
		    full_name = $5,
		    stripe_customer_id = $6,
		    metadata = COALESCE($7, '{}'::jsonb)
		WHERE id = $1 AND deleted_at IS NULL
		RETURNING updated_at`

	err := r.pool.QueryRow(ctx, query,
		u.ID, u.Email, u.EmailVerifiedAt, u.PasswordHash, u.FullName, u.StripeCustomerID, u.Metadata,
	).Scan(&u.UpdatedAt)
	if err != nil {
		return mapError("update user", err)
	}
	return nil
}

// row is satisfied by both pgx.Row and pgx.Rows, so scanners work for single
// reads and future list queries alike.
type row interface {
	Scan(dest ...any) error
}

func scanUser(r row) (*domain.User, error) {
	var u domain.User
	err := r.Scan(
		&u.ID,
		&u.Email,
		&u.EmailVerifiedAt,
		&u.PasswordHash,
		&u.FullName,
		&u.StripeCustomerID,
		&u.Metadata,
		&u.CreatedAt,
		&u.UpdatedAt,
		&u.DeletedAt,
	)
	if err != nil {
		return nil, err
	}
	return &u, nil
}
