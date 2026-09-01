// Package handler adapts HTTP to the service layer. It decodes, delegates and
// encodes; it holds no business logic and never talks to Stripe or the database
// directly.
package handler

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mlkad/stripe-payment-service/internal/domain"
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

	// maxAPIBytes caps ordinary JSON request bodies, which are small structs.
	maxAPIBytes int64 = 32 << 10

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

// Register mounts the Stripe surface. Route patterns live with the handler so
// the composition root does not have to know them.
func (h *StripeHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /webhook", h.HandleWebhook)
	mux.HandleFunc("POST /api/v1/checkout", h.HandleCheckout)
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
			slog.String("remote_addr", remoteIP(r)))
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
				slog.String("remote_addr", remoteIP(r)))
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
			slog.String("remote_addr", remoteIP(r)),
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

type checkoutRequest struct {
	// UserID is accepted from the body only because authentication is not yet
	// wired. Once it is, this field must come from the session: a caller who can
	// name any user id can open a checkout that bills someone else's card.
	UserID          string `json:"user_id"`
	PriceID         string `json:"price_id"`
	Quantity        int64  `json:"quantity,omitempty"`
	TrialPeriodDays int64  `json:"trial_period_days,omitempty"`
}

type checkoutResponse struct {
	SessionID string `json:"session_id"`
	URL       string `json:"url"`
}

func (h *StripeHandler) HandleCheckout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req checkoutRequest
	if err := decodeJSON(w, r, &req); err != nil {
		h.log.WarnContext(ctx, "checkout request rejected", slog.String("error", err.Error()))
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	userID, err := uuid.Parse(strings.TrimSpace(req.UserID))
	if err != nil {
		writeError(w, http.StatusBadRequest, "user_id must be a UUID")
		return
	}

	result, err := h.checkout.CreateCheckoutSession(ctx, service.CheckoutRequest{
		UserID:          userID,
		PriceID:         strings.TrimSpace(req.PriceID),
		Quantity:        req.Quantity,
		TrialPeriodDays: req.TrialPeriodDays,
	})
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}

	writeJSON(w, http.StatusCreated, checkoutResponse{SessionID: result.SessionID, URL: result.URL})
}

// writeServiceError maps a use case failure onto a status code without leaking
// upstream detail to the caller.
func (h *StripeHandler) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	ctx := r.Context()
	switch {
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, http.StatusNotFound, "user not found")
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

// --- transport helpers -------------------------------------------------------

// decodeJSON reads a size-limited body and rejects unknown fields, so a typo in
// a client's payload surfaces as an error instead of a silently ignored value.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		return errors.New("content-type must be application/json")
	}

	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAPIBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return errors.New("request body too large")
		}
		return errors.New("request body is not valid JSON")
	}
	// A second value in the stream means the client sent something other than
	// the single object this endpoint accepts.
	if dec.More() {
		return errors.New("request body must contain a single JSON object")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// remoteIP is used for abuse logging only. It reads RemoteAddr rather than
// X-Forwarded-For, which is caller-controlled unless a trusted proxy overwrites
// it; treating it as identity would let anyone forge the audit trail.
func remoteIP(r *http.Request) string {
	host, _, found := strings.Cut(r.RemoteAddr, ":")
	if !found {
		return r.RemoteAddr
	}
	return host
}
