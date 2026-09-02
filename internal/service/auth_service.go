package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mlkad/stripe-payment-service/internal/auth"
	"github.com/mlkad/stripe-payment-service/internal/domain"
	repo "github.com/mlkad/stripe-payment-service/internal/repository/postgres"
)

// AuthResult is what a successful register or login returns. The token is the
// only credential; nothing else here is sensitive.
type AuthResult struct {
	Token     string
	ExpiresAt time.Time
	User      *UserView
}

// UserView is the read model for an identity. domain.User carries the password
// digest and the Stripe customer id, neither of which belongs in a response.
type UserView struct {
	ID       uuid.UUID `json:"id"`
	Email    string    `json:"email"`
	FullName *string   `json:"full_name,omitempty"`
}

func viewOf(u *domain.User) *UserView {
	return &UserView{ID: u.ID, Email: u.Email, FullName: u.FullName}
}

type AuthService struct {
	users  repo.UserRepository
	hasher *auth.Hasher
	tokens *auth.TokenService
	log    *slog.Logger
}

func NewAuthService(users repo.UserRepository, hasher *auth.Hasher, tokens *auth.TokenService, log *slog.Logger) *AuthService {
	return &AuthService{users: users, hasher: hasher, tokens: tokens, log: log}
}

// Register creates an identity and returns a token for it.
//
// A duplicate email surfaces as domain.ErrConflict. That does disclose whether
// an address is registered, which registration cannot avoid: the alternative is
// to accept the signup silently and email the existing owner, which needs mail
// delivery this service does not have. Login makes no such disclosure.
func (s *AuthService) Register(ctx context.Context, email, password string, fullName *string) (*AuthResult, error) {
	email = strings.TrimSpace(email)

	if err := auth.ValidatePassword(password); err != nil {
		return nil, fmt.Errorf("%w: %s", domain.ErrValidation, err)
	}

	digest, err := s.hasher.Hash(password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	user := &domain.User{Email: email, PasswordHash: &digest, FullName: fullName}
	if err := s.users.CreateUser(ctx, user); err != nil {
		// Validation and conflict pass through untouched; the handler maps them.
		if errors.Is(err, domain.ErrValidation) || errors.Is(err, domain.ErrConflict) {
			return nil, err
		}
		return nil, fmt.Errorf("create user: %w", err)
	}

	s.log.InfoContext(ctx, "user registered", slog.String("user_id", user.ID.String()))
	return s.issue(user)
}

// Login verifies credentials.
//
// Both "no such user" and "wrong password" return auth.ErrCredentialsMismatch,
// and both spend a full bcrypt comparison. Returning early on an unknown email
// would answer in microseconds where a known one takes ~250ms, and that gap is
// a reliable account enumeration oracle over the network.
func (s *AuthService) Login(ctx context.Context, email, password string) (*AuthResult, error) {
	user, err := s.users.GetUserByEmail(ctx, strings.TrimSpace(email))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			s.hasher.DummyCompare()
			return nil, auth.ErrCredentialsMismatch
		}
		return nil, fmt.Errorf("look up user: %w", err)
	}

	digest := ""
	if user.PasswordHash != nil {
		digest = *user.PasswordHash
	}
	if err := s.hasher.Verify(digest, password); err != nil {
		s.log.WarnContext(ctx, "failed login", slog.String("user_id", user.ID.String()))
		return nil, auth.ErrCredentialsMismatch
	}

	// Cost upgrades ride along on a successful login, which is the only moment
	// the plaintext is available. Failure is not fatal: the user is already
	// authenticated and the old digest still works.
	if s.hasher.NeedsRehash(digest) {
		if rehashed, err := s.hasher.Hash(password); err == nil {
			user.PasswordHash = &rehashed
			if err := s.users.UpdateUser(ctx, user); err != nil {
				s.log.WarnContext(ctx, "could not upgrade password hash",
					slog.String("user_id", user.ID.String()), slog.String("error", err.Error()))
			}
		}
	}

	return s.issue(user)
}

// Me returns the identity behind a token, for the frontend to bootstrap with.
func (s *AuthService) Me(ctx context.Context, userID uuid.UUID) (*UserView, error) {
	user, err := s.users.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("load user: %w", err)
	}
	return viewOf(user), nil
}

func (s *AuthService) issue(user *domain.User) (*AuthResult, error) {
	token, expiresAt, err := s.tokens.Issue(user.ID, user.Email)
	if err != nil {
		return nil, fmt.Errorf("issue token: %w", err)
	}
	return &AuthResult{Token: token, ExpiresAt: expiresAt, User: viewOf(user)}, nil
}
