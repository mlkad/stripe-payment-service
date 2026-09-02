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

type AuthHandler struct {
	auth *service.AuthService
	log  *slog.Logger
}

func NewAuthHandler(authService *service.AuthService, log *slog.Logger) *AuthHandler {
	return &AuthHandler{auth: authService, log: log}
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
		writeJSON(w, http.StatusOK, authResponse{result.Token, result.ExpiresAt, result.User})

	case errors.Is(err, auth.ErrCredentialsMismatch):
		writeError(w, http.StatusUnauthorized, "invalid email or password")

	default:
		h.log.ErrorContext(ctx, "login failed", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "could not sign you in")
	}
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
