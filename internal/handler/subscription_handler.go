package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/mlkad/stripe-payment-service/internal/domain"
	"github.com/mlkad/stripe-payment-service/internal/handler/middleware"
	"github.com/mlkad/stripe-payment-service/internal/service"
)

type SubscriptionHandler struct {
	subs *service.SubscriptionService
	log  *slog.Logger
}

func NewSubscriptionHandler(subs *service.SubscriptionService, log *slog.Logger) *SubscriptionHandler {
	return &SubscriptionHandler{subs: subs, log: log}
}

// HandleGetSubscription answers the dashboard's "what am I paying for" query
// for the authenticated caller.
//
// There is no user_id parameter. The subject comes from the verified token, so
// no request field points at anyone else's billing state; a stale client still
// sending ?user_id= has it ignored rather than honoured.
func (h *SubscriptionHandler) HandleGetSubscription(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userID, err := middleware.UserIDFromContext(ctx)
	if err != nil {
		writeUnwiredRoute(w, h.log, ctx, err)
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
