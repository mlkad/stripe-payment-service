// Package handler adapts HTTP to the service layer. It decodes, delegates and
// encodes; it holds no business logic and never talks to Stripe or the database
// directly.
package handler

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mlkad/stripe-payment-service/internal/domain"
	"github.com/mlkad/stripe-payment-service/internal/handler/middleware"
	"github.com/mlkad/stripe-payment-service/internal/service"
	paystripe "github.com/mlkad/stripe-payment-service/internal/stripe"
)

const (
	// maxWebhookBytes caps the webhook body. Signature verification needs the
	// whole payload in memory before a single byte can be trusted, so this is
	// the only thing standing between an anonymous POST and the heap. Stripe's
	// own events stay well under 100 KiB; the largest legitimate ones are
	// invoices with many line items.
	maxWebhookBytes int64 = 256 << 10

	signatureHeader = "Stripe-Signature"
)

type StripeHandler struct {
	webhooks *service.WebhookService
	checkout *service.CheckoutService
	log      *slog.Logger

	// maxWebhookBytes is overridable for tests; zero selects the default.
	maxWebhookBytes int64
}

func NewStripeHandler(webhooks *service.WebhookService, checkout *service.CheckoutService, log *slog.Logger) *StripeHandler {
	return &StripeHandler{webhooks: webhooks, checkout: checkout, log: log, maxWebhookBytes: maxWebhookBytes}
}

// --- webhook -----------------------------------------------------------------

// HandleWebhook receives one Stripe delivery.
//
// Two constraints shape this handler. The body must be read raw and entire,
// because the signature covers the exact bytes Stripe sent - decoding and
// re-encoding the JSON invalidates it. And the status code is a control signal,
// not a courtesy: a non-2xx tells Stripe to redeliver, for up to three days.
// Anything permanently unprocessable must therefore be acknowledged, or the
// endpoint accumulates an ever-growing retry backlog it can never drain.
func (h *StripeHandler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	signature := r.Header.Get(signatureHeader)
	if signature == "" {
		h.log.WarnContext(ctx, "webhook rejected: missing signature header",
			slog.String("remote_addr", middleware.ClientIP(r)))
		writeError(w, http.StatusBadRequest, "missing signature")
		return
	}

	limit := h.maxWebhookBytes
	if limit <= 0 {
		limit = maxWebhookBytes
	}
	// MaxBytesReader caps the read and, unlike io.LimitReader, makes the
	// overrun distinguishable from a body that simply ended.
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.log.WarnContext(ctx, "webhook rejected: payload too large",
				slog.Int64("limit_bytes", limit),
				slog.String("remote_addr", middleware.ClientIP(r)))
			writeError(w, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		h.log.WarnContext(ctx, "webhook body read failed", slog.String("error", err.Error()))
		writeError(w, http.StatusBadRequest, "could not read request body")
		return
	}

	start := time.Now()
	outcome, err := h.webhooks.ProcessEvent(ctx, payload, signature)

	if errors.Is(err, paystripe.ErrSignatureVerification) {
		// Never echo why verification failed. Distinguishing a bad signature
		// from a stale timestamp tells a forger which half of the check they
		// have already defeated.
		h.log.WarnContext(ctx, "webhook signature verification failed",
			slog.String("remote_addr", middleware.ClientIP(r)),
			slog.Int("payload_bytes", len(payload)),
			slog.String("error", err.Error()))
		writeError(w, http.StatusBadRequest, "signature verification failed")
		return
	}

	if err != nil {
		// Genuine processing failure. 500 asks Stripe to redeliver, which is the
		// correct response to a transient fault; the claim row is already marked
		// failed and is reclaimable on the next attempt.
		h.log.ErrorContext(ctx, "webhook processing failed",
			slog.String("error", err.Error()),
			slog.Duration("duration", time.Since(start)))
		writeError(w, http.StatusInternalServerError, "event processing failed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": string(outcome)})
}

// --- checkout ----------------------------------------------------------------

// checkoutRequest carries no user id by design. The caller is the verified
// token subject; a body field would let anyone open a checkout that bills
// someone else's saved card, and DisallowUnknownFields means a stale client
// still sending one gets a 400 rather than being quietly ignored.
type checkoutRequest struct {
	PriceID         string `json:"price_id"`
	Quantity        int64  `json:"quantity,omitempty"`
	TrialPeriodDays int64  `json:"trial_period_days,omitempty"`

	// UIMode is "hosted" (default) or "embedded".
	UIMode string `json:"ui_mode,omitempty"`
}

type checkoutResponse struct {
	SessionID string `json:"session_id"`

	// Exactly one of these is populated, matching the requested ui_mode.
	URL          string `json:"url,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
}

func (h *StripeHandler) HandleCheckout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userID, err := middleware.UserIDFromContext(ctx)
	if err != nil {
		writeUnwiredRoute(w, h.log, ctx, err)
		return
	}

	var req checkoutRequest
	if err := decodeJSON(w, r, &req); err != nil {
		h.log.WarnContext(ctx, "checkout request rejected", slog.String("error", err.Error()))
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	mode := strings.TrimSpace(req.UIMode)
	if mode != "" && mode != "hosted" && mode != "embedded" {
		writeError(w, http.StatusBadRequest, `ui_mode must be "hosted" or "embedded"`)
		return
	}

	result, err := h.checkout.CreateCheckoutSession(ctx, service.CheckoutRequest{
		UserID:          userID,
		PriceID:         strings.TrimSpace(req.PriceID),
		Quantity:        req.Quantity,
		TrialPeriodDays: req.TrialPeriodDays,
		Embedded:        mode == "embedded",
	})
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	writeJSON(w, http.StatusCreated, checkoutResponse{
		SessionID:    result.SessionID,
		URL:          result.URL,
		ClientSecret: result.ClientSecret,
	})
}

type portalResponse struct {
	URL string `json:"url"`
}

// HandlePortal opens Stripe's billing portal for the authenticated caller.
//
// This is where a customer cancels, switches plan, or updates their card. None
// of that is implemented here on purpose: the portal is Stripe-hosted, so PCI
// scope stays with them, and the resulting changes arrive back through the
// customer.subscription.* webhooks this service already handles.
//
// There is no customer id in the request. The returned URL authenticates its
// bearer as that customer, so it is derived from the token subject only.
func (h *StripeHandler) HandlePortal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userID, err := middleware.UserIDFromContext(ctx)
	if err != nil {
		writeUnwiredRoute(w, h.log, ctx, err)
		return
	}

	result, err := h.checkout.CreatePortalSession(ctx, userID)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, portalResponse{URL: result.URL})
}

// writeServiceError maps a use case failure onto a status code without leaking
// upstream detail to the caller.
func (h *StripeHandler) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	ctx := r.Context()
	switch {
	case errors.Is(err, domain.ErrNotFound):
		// Covers both "no such user" and "no billing account yet". The caller
		// is authenticated, so the second is the realistic case: they have not
		// completed a checkout, and the UI should offer one.
		writeError(w, http.StatusNotFound, "no billing account for this user")
	case errors.Is(err, domain.ErrValidation), errors.Is(err, paystripe.ErrInvalidRequest):
		h.log.WarnContext(ctx, "checkout rejected", slog.String("error", err.Error()))
		writeError(w, http.StatusUnprocessableEntity, "request could not be fulfilled")
	case errors.Is(err, paystripe.ErrUpstream):
		// Stripe is down or rate limiting. 502 is honest about whose fault it is
		// and tells the caller a retry may succeed.
		h.log.ErrorContext(ctx, "stripe upstream failure", slog.String("error", err.Error()))
		writeError(w, http.StatusBadGateway, "payment provider unavailable")
	default:
		h.log.ErrorContext(ctx, "checkout failed", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}
