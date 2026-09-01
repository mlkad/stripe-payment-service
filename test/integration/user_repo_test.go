//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mlkad/stripe-payment-service/internal/domain"
	repo "github.com/mlkad/stripe-payment-service/internal/repository/postgres"
)

func TestUserRepo_CreateAndRead(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	users := repo.NewUserRepo(pool)

	u := &domain.User{
		Email:    "Ada@Example.com",
		FullName: ptr("Ada Lovelace"),
		Metadata: map[string]string{"plan_intent": "pro"},
	}
	if err := users.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.ID == uuid.Nil {
		t.Error("CreateUser did not populate ID")
	}
	if u.CreatedAt.IsZero() || u.UpdatedAt.IsZero() {
		t.Error("CreateUser did not populate timestamps")
	}

	got, err := users.GetUserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if got.Email != "Ada@Example.com" {
		t.Errorf("email = %q, want %q", got.Email, "Ada@Example.com")
	}
	if got.Metadata["plan_intent"] != "pro" {
		t.Errorf("metadata did not round-trip: %v", got.Metadata)
	}
}

func TestUserRepo_GetMissingReturnsNotFound(t *testing.T) {
	truncate(t)
	users := repo.NewUserRepo(pool)

	_, err := users.GetUserByID(context.Background(), uuid.New())
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// uq_users_email_active is built on lower(email), so case differences must
// collide. This is the constraint that stops one human owning two accounts.
func TestUserRepo_DuplicateEmailIsCaseInsensitiveConflict(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	users := repo.NewUserRepo(pool)

	if err := users.CreateUser(ctx, &domain.User{Email: "ada@example.com"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := users.CreateUser(ctx, &domain.User{Email: "ADA@EXAMPLE.com"})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestUserRepo_ValidationRejectsBadInput(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	users := repo.NewUserRepo(pool)

	tests := []struct {
		name string
		user *domain.User
	}{
		{"malformed email", &domain.User{Email: "not-an-email"}},
		{"empty email", &domain.User{Email: ""}},
		{"wrong stripe id prefix", &domain.User{Email: "x@y.com", StripeCustomerID: ptr("sub_wrong")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := users.CreateUser(ctx, tt.user)
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation", err)
			}
		})
	}
}

// The trigger owns updated_at; the repository never sets it. clock_timestamp()
// rather than now() is what makes this advance within a single transaction.
func TestUserRepo_UpdateAdvancesUpdatedAtViaTrigger(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	users := repo.NewUserRepo(pool)

	u := &domain.User{Email: "ada@example.com"}
	if err := users.CreateUser(ctx, u); err != nil {
		t.Fatalf("seed: %v", err)
	}

	before := u.UpdatedAt
	time.Sleep(5 * time.Millisecond)
	u.StripeCustomerID = ptr("cus_Ada001")
	if err := users.UpdateUser(ctx, u); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if !u.UpdatedAt.After(before) {
		t.Errorf("updated_at did not advance: %v -> %v", before, u.UpdatedAt)
	}

	byCus, err := users.GetUserByStripeCustomerID(ctx, "cus_Ada001")
	if err != nil {
		t.Fatalf("GetUserByStripeCustomerID: %v", err)
	}
	if byCus.ID != u.ID {
		t.Errorf("resolved id = %v, want %v", byCus.ID, u.ID)
	}
}

// Every read path carries `deleted_at IS NULL` so it stays on the partial
// indices. The observable consequence is that a soft-deleted user disappears.
func TestUserRepo_SoftDeletedUserIsInvisible(t *testing.T) {
	truncate(t)
	ctx := context.Background()
	users := repo.NewUserRepo(pool)

	u := &domain.User{Email: "ada@example.com", StripeCustomerID: ptr("cus_Ada002")}
	if err := users.CreateUser(ctx, u); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET deleted_at = now() WHERE id = $1`, u.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	if _, err := users.GetUserByID(ctx, u.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetUserByID err = %v, want ErrNotFound", err)
	}
	if _, err := users.GetUserByStripeCustomerID(ctx, "cus_Ada002"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("GetUserByStripeCustomerID err = %v, want ErrNotFound", err)
	}
}
