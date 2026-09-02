package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/mlkad/stripe-payment-service/internal/domain"
	"github.com/mlkad/stripe-payment-service/internal/service"
)

type SubscriptionHandler struct {
	subs *service.SubscriptionService
	log  *slog.Logger
}

func NewSubscriptionHandler(subs *service.SubscriptionService, log *slog.Logger) *SubscriptionHandler {
	return &SubscriptionHandler{subs: subs, log: log}
}

// HandleGetSubscription answers the dashboard's "what am I paying for" query.
//
// user_id arrives as a query parameter only because authentication is not yet
// wired; it must come from the session once it is. As written, any caller can
// read any user's billing state.
func (h *SubscriptionHandler) HandleGetSubscription(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userID, err := uuid.Parse(strings.TrimSpace(r.URL.Query().Get("user_id")))
	if err != nil {
		writeError(w, http.StatusBadRequest, "user_id must be a UUID")
		return
	}

	view, err := h.subs.GetForUser(ctx, userID)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, view)
	case errors.Is(err, domain.ErrNotFound):
		// Not an error condition: a user who has never subscribed is the normal
		// first-visit state, and the UI renders pricing rather than a failure.
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "no subscription"})
	default:
		h.log.ErrorContext(ctx, "subscription lookup failed", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}
