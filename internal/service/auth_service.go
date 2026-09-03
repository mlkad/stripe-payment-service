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
	"github.com/mlkad/stripe-payment-service/internal/metrics"
	repo "github.com/mlkad/stripe-payment-service/internal/repository/postgres"
)

// AuthResult is what a successful register or login returns. The token is the
// only credential; nothing else here is sensitive.
type AuthResult struct {
	Token     string
	ExpiresAt time.Time
	User      *UserView

	// RefreshToken is returned once, in plaintext, for the handler to put in an
	// httpOnly cookie. It is never stored in that form and never logged.
	RefreshToken          string
	RefreshTokenExpiresAt time.Time
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
	users      repo.UserRepository
	refresh    repo.RefreshTokenRepository
	hasher     *auth.Hasher
	tokens     *auth.TokenService
	log        *slog.Logger
	refreshTTL time.Duration
	metrics    *metrics.Registry
}

// WithMetrics attaches instrumentation. Optional, so the CLI and tests can
// build the service without a registry.
func (s *AuthService) WithMetrics(m *metrics.Registry) *AuthService {
	s.metrics = m
	return s
}

func (s *AuthService) countAuth(operation, outcome string) {
	if s.metrics != nil {
		s.metrics.IncAuth(operation, outcome)
	}
}

func (s *AuthService) countRefresh(outcome string) {
	if s.metrics != nil {
		s.metrics.IncTokenRefresh(outcome)
	}
}

func NewAuthService(
	users repo.UserRepository,
	refresh repo.RefreshTokenRepository,
	hasher *auth.Hasher,
	tokens *auth.TokenService,
	refreshTTL time.Duration,
	log *slog.Logger,
) *AuthService {
	if refreshTTL <= 0 {
		refreshTTL = 30 * 24 * time.Hour
	}
	return &AuthService{
		users: users, refresh: refresh, hasher: hasher,
		tokens: tokens, refreshTTL: refreshTTL, log: log,
	}
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
			s.countAuth("register", "rejected")
			return nil, err
		}
		s.countAuth("register", "error")
		return nil, fmt.Errorf("create user: %w", err)
	}
	s.countAuth("register", "success")

	s.log.InfoContext(ctx, "user registered", slog.String("user_id", user.ID.String()))
	return s.issue(ctx, user)
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
			// One label for both failure modes, matching the response: a metric
			// that separated "unknown account" from "wrong password" would be
			// the same enumeration oracle, just read off a dashboard.
			s.countAuth("login", "failed")
			return nil, auth.ErrCredentialsMismatch
		}
		s.countAuth("login", "error")
		return nil, fmt.Errorf("look up user: %w", err)
	}

	digest := ""
	if user.PasswordHash != nil {
		digest = *user.PasswordHash
	}
	if err := s.hasher.Verify(digest, password); err != nil {
		s.log.WarnContext(ctx, "failed login", slog.String("user_id", user.ID.String()))
		s.countAuth("login", "failed")
		return nil, auth.ErrCredentialsMismatch
	}
	s.countAuth("login", "success")

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

	return s.issue(ctx, user)
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

// Refresh exchanges a refresh token for a new access token and a successor.
//
// Rotation is unconditional: every renewal consumes the presented token, so a
// leaked one is useful only until the legitimate client next refreshes. If a
// consumed token comes back, the repository revokes the whole family and this
// returns domain.ErrTokenReused - the caller has to sign in again, and so does
// whoever stole it.
func (s *AuthService) Refresh(ctx context.Context, presented string) (*AuthResult, error) {
	if presented == "" {
		return nil, domain.ErrNotFound
	}

	successorToken, successorHash, err := auth.NewRefreshToken()
	if err != nil {
		return nil, fmt.Errorf("mint refresh token: %w", err)
	}

	stored, err := s.refresh.ConsumeRefreshToken(ctx, repo.ConsumeRefreshToken{
		PresentedHash:      auth.HashRefreshToken(presented),
		SuccessorHash:      successorHash,
		SuccessorExpiresAt: time.Now().Add(s.refreshTTL),
	})
	switch {
	case errors.Is(err, domain.ErrTokenReused):
		// The signal worth alerting on: a non-zero rate here means a token was
		// stolen, or a client is refreshing concurrently without deduping.
		s.countRefresh("reuse_detected")
		// Worth an error line: this is either a stolen credential or a client
		// bug that will keep ending sessions, and both need looking at.
		s.log.ErrorContext(ctx, "refresh token reuse detected; revoked the session family",
			slog.String("reason", "a token that was already exchanged was presented again"))
		return nil, err
	case err != nil:
		s.countRefresh("rejected")
		return nil, err
	}

	user, err := s.users.GetUserByID(ctx, stored.UserID)
	if err != nil {
		// A valid token whose account is gone. The credential is the thing that
		// is no longer good.
		return nil, domain.ErrNotFound
	}

	access, expiresAt, err := s.tokens.Issue(user.ID, user.Email)
	if err != nil {
		return nil, fmt.Errorf("issue access token: %w", err)
	}
	s.countRefresh("success")
	return &AuthResult{
		Token:                 access,
		ExpiresAt:             expiresAt,
		User:                  viewOf(user),
		RefreshToken:          successorToken,
		RefreshTokenExpiresAt: stored.ExpiresAt,
	}, nil
}

// Logout ends the session the presented token belongs to.
//
// Revokes the family rather than the single token, so a logout on one device
// cannot be undone by a successor the client already holds. Other devices keep
// their own families and stay signed in; RevokeAllSessions is the tool for
// ending those.
func (s *AuthService) Logout(ctx context.Context, presented string) error {
	if presented == "" {
		// Nothing to revoke. Logout is idempotent by design: a client clearing
		// a session it no longer has should not see an error.
		return nil
	}

	revoked, err := s.refresh.RevokeFamilyByToken(ctx,
		auth.HashRefreshToken(presented), "signed out")
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("revoke session: %w", err)
	}
	s.log.InfoContext(ctx, "session ended", slog.Int64("tokens_revoked", revoked))
	return nil
}

// RevokeAllSessions ends every session for a user. The tool for a password
// change or a compromised account.
func (s *AuthService) RevokeAllSessions(ctx context.Context, userID uuid.UUID, reason string) error {
	revoked, err := s.refresh.RevokeAllForUser(ctx, userID, reason)
	if err != nil {
		return fmt.Errorf("revoke sessions: %w", err)
	}
	s.log.InfoContext(ctx, "all sessions revoked",
		slog.String("user_id", userID.String()),
		slog.Int64("tokens_revoked", revoked),
		slog.String("reason", reason))
	return nil
}

func (s *AuthService) issue(ctx context.Context, user *domain.User) (*AuthResult, error) {
	access, expiresAt, err := s.tokens.Issue(user.ID, user.Email)
	if err != nil {
		return nil, fmt.Errorf("issue token: %w", err)
	}

	refreshToken, refreshHash, err := auth.NewRefreshToken()
	if err != nil {
		return nil, fmt.Errorf("mint refresh token: %w", err)
	}

	// A fresh login starts a new family. Sessions on other devices are
	// untouched, so signing in here does not sign out there.
	record := &domain.RefreshToken{
		UserID:    user.ID,
		TokenHash: refreshHash,
		FamilyID:  uuid.New(),
		ExpiresAt: time.Now().Add(s.refreshTTL),
	}
	if err := s.refresh.CreateRefreshToken(ctx, record); err != nil {
		return nil, fmt.Errorf("store refresh token: %w", err)
	}

	return &AuthResult{
		Token:                 access,
		ExpiresAt:             expiresAt,
		User:                  viewOf(user),
		RefreshToken:          refreshToken,
		RefreshTokenExpiresAt: record.ExpiresAt,
	}, nil
}
