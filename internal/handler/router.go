package handler

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mlkad/stripe-payment-service/internal/handler/middleware"
)

// RouterConfig carries the deadlines the routes are mounted with.
type RouterConfig struct {
	// APITimeout bounds ordinary API requests.
	APITimeout time.Duration

	// CORS lists the browser origins allowed to call the API. Zero origins
	// leaves the middleware out of the chain entirely.
	CORS middleware.CORSConfig

	// Tokens verifies bearer credentials on protected routes.
	Tokens middleware.TokenParser

	// WebhookTimeout bounds webhook processing, and is deliberately the longer
	// of the two: checkout.session.completed makes an outbound call to Stripe
	// before it writes anything, so it inherits that client's timeout plus the
	// database work on either side.
	WebhookTimeout time.Duration
}

const (
	defaultAPITimeout     = 10 * time.Second
	defaultWebhookTimeout = 25 * time.Second
)

func (c RouterConfig) withDefaults() RouterConfig {
	if c.APITimeout <= 0 {
		c.APITimeout = defaultAPITimeout
	}
	if c.WebhookTimeout <= 0 {
		c.WebhookTimeout = defaultWebhookTimeout
	}
	return c
}

// NewRouter assembles the handler tree. Dependencies arrive as arguments;
// nothing is read from package scope.
//
// The global chain is ordered outermost first, and two of the three positions
// are load-bearing:
//
//   - RequestID is outermost so that every record produced by anything below
//     it - including a panic report - carries the correlation id.
//   - AccessLog sits *outside* Recoverer. It reads the status after the inner
//     handler returns, so a panic unwinding through it would be logged as 200,
//     or not logged at all. With Recoverer inside, the recovered 500 is already
//     written by the time AccessLog looks.
//   - Recoverer is innermost of the three so a panic in a handler is caught
//     while the request context is still live.
//
// Timeout is applied per route group rather than globally: the health probes
// carry their own short deadline and must stay answerable when everything else
// is saturated.
func NewRouter(
	stripe *StripeHandler,
	subs *SubscriptionHandler,
	authHandler *AuthHandler,
	health *HealthHandler,
	cfg RouterConfig,
	log *slog.Logger,
) http.Handler {
	cfg = cfg.withDefaults()

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.AccessLog(log))
	r.Use(middleware.Recoverer(log))

	// CORS goes inside Recoverer but outside routing, so a preflight for an
	// unmatched path still gets its headers rather than a bare 404 the browser
	// reports as a CORS failure.
	if len(cfg.CORS.AllowedOrigins) > 0 {
		r.Use(middleware.CORS(cfg.CORS))
	}

	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	})

	r.Get("/livez", health.Live)
	r.Get("/healthz", health.Ready)

	// Stripe is the client here, not a browser, and it retries on non-2xx for up
	// to three days. The longer deadline keeps a slow-but-succeeding delivery
	// from being converted into a redelivery.
	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(cfg.WebhookTimeout, log))
		r.Post("/webhook", stripe.HandleWebhook)
	})

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(middleware.Timeout(cfg.APITimeout, log))

		// Public: obtaining a credential cannot itself require one.
		r.Post("/auth/register", authHandler.HandleRegister)
		r.Post("/auth/login", authHandler.HandleLogin)

		// Everything below requires a valid bearer token. The subject is read
		// from the token inside the handlers; no route here accepts a user id
		// from the client, and RequireAuth is applied to the group rather than
		// per-route so a new endpoint is protected by default rather than by
		// remembering.
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireAuth(cfg.Tokens, log))

			r.Get("/auth/me", authHandler.HandleMe)
			r.Post("/checkout", stripe.HandleCheckout)
			r.Get("/subscription", subs.HandleGetSubscription)
		})
	})

	return r
}
