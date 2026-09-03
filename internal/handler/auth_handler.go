package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/mlkad/stripe-payment-service/internal/auth"
	"github.com/mlkad/stripe-payment-service/internal/domain"
	"github.com/mlkad/stripe-payment-service/internal/handler/middleware"
	"github.com/mlkad/stripe-payment-service/internal/service"
)

// CookieConfig controls the refresh cookie.
type CookieConfig struct {
	// Secure should be false only for plain-HTTP local development. A refresh
	// token sent over HTTP is readable by anything on the path.
	Secure bool

	// Domain is left empty for a host-only cookie, which is the safer default:
	// a domain cookie is sent to every subdomain, so one compromised subdomain
	// receives the token.
	Domain string
}

const (
	refreshCookieName = "sps_refresh"

	// refreshCookiePath scopes the cookie to the auth endpoints. It is not sent
	// with ordinary API calls, so an XSS that can read responses still never
	// sees it on the wire, and a request-smuggling bug elsewhere cannot pick it
	// up in passing.
	refreshCookiePath = "/api/v1/auth"
)

type AuthHandler struct {
	auth   *service.AuthService
	cookie CookieConfig
	log    *slog.Logger
}

func NewAuthHandler(authService *service.AuthService, cookie CookieConfig, log *slog.Logger) *AuthHandler {
	return &AuthHandler{auth: authService, cookie: cookie, log: log}
}

// setRefreshCookie writes the refresh token.
//
// httpOnly so script cannot read it, which is the whole point: the access token
// lives where an XSS can reach it, and this is what stops that XSS from
// becoming permanent access.
//
// SameSite=Strict is what stands in for CSRF protection here. The refresh and
// logout endpoints are state-changing and authenticated purely by this cookie,
// so a cross-site POST would otherwise carry it. Strict means the browser does
// not attach it to any cross-site request at all. The API's own endpoints are
// unaffected either way, since they authenticate with a Bearer header that a
// cross-site form cannot set.
func (h *AuthHandler) setRefreshCookie(w http.ResponseWriter, token string, expiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    token,
		Path:     refreshCookiePath,
		Domain:   h.cookie.Domain,
		Expires:  expiresAt,
		MaxAge:   int(time.Until(expiresAt).Seconds()),
		HttpOnly: true,
		Secure:   h.cookie.Secure,
		SameSite: http.SameSiteStrictMode,
	})
}

// clearRefreshCookie must mirror the attributes used to set it, or the browser
// treats it as a different cookie and leaves the original in place.
func (h *AuthHandler) clearRefreshCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    "",
		Path:     refreshCookiePath,
		Domain:   h.cookie.Domain,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.cookie.Secure,
		SameSite: http.SameSiteStrictMode,
	})
}

func refreshCookieValue(r *http.Request) string {
	c, err := r.Cookie(refreshCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

type registerRequest struct {
	Email    string  `json:"email"`
	Password string  `json:"password"`
	FullName *string `json:"full_name,omitempty"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type authResponse struct {
	Token     string            `json:"token"`
	ExpiresAt time.Time         `json:"expires_at"`
	User      *service.UserView `json:"user"`
}

func (h *AuthHandler) HandleRegister(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req registerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := h.auth.Register(ctx, req.Email, req.Password, req.FullName)
	switch {
	case err == nil:
		h.setRefreshCookie(w, result.RefreshToken, result.RefreshTokenExpiresAt)
		writeJSON(w, http.StatusCreated, authResponse{result.Token, result.ExpiresAt, result.User})

	case errors.Is(err, domain.ErrConflict):
		writeError(w, http.StatusConflict, "an account with that email already exists")

	case errors.Is(err, domain.ErrValidation):
		// The password policy is safe to echo: it is public by definition, and a
		// caller cannot fix a rejected password without being told the rule.
		writeError(w, http.StatusUnprocessableEntity, err.Error())

	default:
		h.log.ErrorContext(ctx, "registration failed", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "could not create the account")
	}
}

// HandleLogin answers a credential check.
//
// Every failure is 401 with one message. Separating "unknown email" from "wrong
// password" - by status, body, or response time - turns this endpoint into an
// account enumeration oracle; the service layer keeps the timing uniform and
// this keeps the response uniform.
func (h *AuthHandler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req loginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := h.auth.Login(ctx, req.Email, req.Password)
	switch {
	case err == nil:
		h.setRefreshCookie(w, result.RefreshToken, result.RefreshTokenExpiresAt)
		writeJSON(w, http.StatusOK, authResponse{result.Token, result.ExpiresAt, result.User})

	case errors.Is(err, auth.ErrCredentialsMismatch):
		writeError(w, http.StatusUnauthorized, "invalid email or password")

	default:
		h.log.ErrorContext(ctx, "login failed", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "could not sign you in")
	}
}

// HandleRefresh exchanges the refresh cookie for a new access token.
//
// Public by design: the caller has no valid access token, which is why they are
// here. The cookie is the credential.
//
// Every failure clears the cookie and answers 401 with one message. A client
// cannot act differently on "expired" than on "reused", and telling a thief
// which one they hold is free information - particularly for reuse, where the
// distinction would confirm the token was stolen rather than merely stale.
func (h *AuthHandler) HandleRefresh(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	result, err := h.auth.Refresh(ctx, refreshCookieValue(r))
	if err != nil {
		h.clearRefreshCookie(w)
		if errors.Is(err, domain.ErrTokenReused) {
			// The family is already revoked by the repository. Logged at warn
			// here because it is worth seeing on the request path too.
			h.log.WarnContext(ctx, "refresh rejected: token reuse",
				slog.String("remote_addr", middleware.ClientIP(r)))
		} else if !errors.Is(err, domain.ErrNotFound) {
			h.log.ErrorContext(ctx, "refresh failed", slog.String("error", err.Error()))
		}
		writeError(w, http.StatusUnauthorized, "session expired, please sign in again")
		return
	}

	h.setRefreshCookie(w, result.RefreshToken, result.RefreshTokenExpiresAt)
	writeJSON(w, http.StatusOK, authResponse{result.Token, result.ExpiresAt, result.User})
}

// HandleLogout ends the session.
//
// Idempotent and always 204: a client clearing a session it no longer holds is
// not an error, and reporting one would leave the UI unable to sign out.
func (h *AuthHandler) HandleLogout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if err := h.auth.Logout(ctx, refreshCookieValue(r)); err != nil {
		// The cookie is cleared regardless. Leaving the client holding a
		// credential because the server had a database problem is the worse
		// failure.
		h.log.ErrorContext(ctx, "logout could not revoke the session",
			slog.String("error", err.Error()))
	}
	h.clearRefreshCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// HandleMe returns the caller's identity. Mounted behind RequireAuth, so the
// subject comes from the verified token and never from the request.
func (h *AuthHandler) HandleMe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userID, err := middleware.UserIDFromContext(ctx)
	if err != nil {
		writeUnwiredRoute(w, h.log, ctx, err)
		return
	}

	view, err := h.auth.Me(ctx, userID)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, view)
	case errors.Is(err, domain.ErrNotFound):
		// A valid token for a deleted account. 401 rather than 404: the
		// credential is the thing that is no longer good.
		writeError(w, http.StatusUnauthorized, "account no longer exists")
	default:
		h.log.ErrorContext(ctx, "identity lookup failed", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}
