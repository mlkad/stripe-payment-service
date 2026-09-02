package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/mlkad/stripe-payment-service/internal/auth"
)

// ctxKey is unexported so no other package can write the authenticated subject
// into a context. A handler reading UserIDFromContext is therefore reading
// something only this middleware could have put there.
type ctxKey int

const userIDKey ctxKey = iota

// ErrNoUserInContext means a handler asked for the caller's identity on a route
// that was never wrapped in RequireAuth. It is a wiring mistake, not a client
// error, and must never be reported as 401 - that would make an unprotected
// route look protected.
var ErrNoUserInContext = errors.New("no authenticated user in context")

// UserIDFromContext returns the authenticated caller.
func UserIDFromContext(ctx context.Context) (uuid.UUID, error) {
	id, ok := ctx.Value(userIDKey).(uuid.UUID)
	if !ok || id == uuid.Nil {
		return uuid.Nil, ErrNoUserInContext
	}
	return id, nil
}

// WithUserID injects an authenticated subject. Exported only for tests that
// exercise a handler without a live token.
func WithUserID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, userIDKey, id)
}

// TokenParser is the slice of auth.TokenService this middleware needs.
type TokenParser interface {
	Parse(token string) (uuid.UUID, *auth.Claims, error)
}

// RequireAuth rejects any request without a valid bearer token.
//
// The response says only that authentication failed. Distinguishing "expired"
// from "malformed" from "wrong signature" in the body tells a forger which part
// to fix next; the distinction goes to the log instead. The one exception is
// the WWW-Authenticate header, where `error="invalid_token"` is what RFC 6750
// defines and what clients use to decide whether to refresh.
func RequireAuth(tokens TokenParser, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if !ok {
				unauthorized(w, "missing bearer token")
				return
			}

			userID, claims, err := tokens.Parse(token)
			if err != nil {
				// Debug, not warn: an expired token is the normal state of any
				// long-lived tab, and logging it at warn trains operators to
				// ignore the level that matters.
				log.DebugContext(r.Context(), "token rejected",
					slog.String("reason", err.Error()),
					slog.Bool("expired", errors.Is(err, auth.ErrTokenExpired)),
					slog.String("remote_addr", ClientIP(r)))
				unauthorized(w, "invalid or expired token")
				return
			}

			ctx := WithUserID(r.Context(), userID)
			if claims != nil && claims.ID != "" {
				// Correlates every log line for this request with the specific
				// token that authorised it, which is what an audit needs.
				ctx = context.WithValue(ctx, tokenIDKey, claims.ID)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

const tokenIDKey ctxKey = iota + 1

// bearerToken extracts the credential from an Authorization header.
//
// The scheme is compared case-insensitively because RFC 7235 defines it that
// way and real clients send "bearer". The value is not trimmed beyond the
// single delimiting space: a token with surrounding whitespace is malformed,
// and silently repairing it hides a broken client.
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "bearer") || token == "" {
		return "", false
	}
	return token, true
}

func unauthorized(w http.ResponseWriter, message string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="api", error="invalid_token"`)
	writeJSONError(w, http.StatusUnauthorized, message)
}
