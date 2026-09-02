//go:build integration

package integration

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	stripesdk "github.com/stripe/stripe-go/v86"

	"github.com/mlkad/stripe-payment-service/internal/handler"
	"github.com/mlkad/stripe-payment-service/internal/handler/middleware"
	repo "github.com/mlkad/stripe-payment-service/internal/repository/postgres"
	"github.com/mlkad/stripe-payment-service/internal/service"
	paystripe "github.com/mlkad/stripe-payment-service/internal/stripe"
)

// throttledStack builds a router with a deliberately tiny auth rate limit. The
// shared newWebhookStack leaves the limiter off, because httptest gives every
// request the same RemoteAddr and the auth suite would throttle itself.
func throttledStack(t *testing.T, burst int) http.Handler {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := paystripe.New(paystripe.Config{
		SecretKey:        "sk_test_notusedhere",
		WebhookSecret:    testWebhookSecret,
		APIVersion:       stripesdk.APIVersion,
		WebhookTolerance: 5 * time.Minute,
	}, log)
	if err != nil {
		t.Fatalf("stripe client: %v", err)
	}

	userRepo := repo.NewUserRepo(pool)
	checkout, err := service.NewCheckoutService(userRepo, client, service.CheckoutConfig{
		SuccessURL: "https://example.test/ok", CancelURL: "https://example.test/cancel",
	}, log)
	if err != nil {
		t.Fatalf("checkout service: %v", err)
	}

	limiter := middleware.NewRateLimiter(middleware.RateLimitConfig{
		Rate: 0.01, Burst: burst,
	}, log)
	t.Cleanup(limiter.Close)

	return handler.NewRouter(
		handler.NewStripeHandler(
			service.NewWebhookService(userRepo, repo.NewSubscriptionRepo(pool),
				repo.NewWebhookRepo(pool, time.Minute), client, log),
			checkout, log),
		handler.NewSubscriptionHandler(
			service.NewSubscriptionService(repo.NewSubscriptionRepo(pool), log), log),
		handler.NewAuthHandler(
			service.NewAuthService(userRepo, testHasher(t), testTokens(t), log), log),
		handler.NewHealthHandler(stubProbe{}, log, "test"),
		handler.RouterConfig{AuthRateLimit: limiter, Tokens: testTokens(t)},
		log,
	)
}

func postLogin(t *testing.T, h http.Handler, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"email":"nobody@example.com","password":"a-sufficiently-long-password"}`))
	r.Header.Set("Content-Type", "application/json")
	if remoteAddr != "" {
		r.RemoteAddr = remoteAddr
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// Unlimited login attempts mean unlimited password guesses, and each one costs
// a bcrypt comparison - so the endpoint is also a cheap way to burn CPU.
func TestRateLimit_LoginIsThrottled(t *testing.T) {
	truncate(t)
	h := throttledStack(t, 3)

	for i := 1; i <= 3; i++ {
		if rec := postLogin(t, h, "192.0.2.50:1000"); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d was throttled inside the burst", i)
		}
	}

	rec := postLogin(t, h, "192.0.2.50:1000")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Error("no Retry-After header")
	} else if n, err := strconv.Atoi(ra); err != nil || n < 1 {
		t.Errorf("Retry-After = %q, want a positive integer", ra)
	}
	if !strings.Contains(rec.Body.String(), "error") {
		t.Errorf("429 body is not the standard envelope: %s", rec.Body)
	}
}

func TestRateLimit_RegisterIsThrottled(t *testing.T) {
	truncate(t)
	h := throttledStack(t, 2)

	post := func(i int) int {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register",
			strings.NewReader(fmt.Sprintf(
				`{"email":"rl%d@example.com","password":"a-sufficiently-long-password"}`, i)))
		r.Header.Set("Content-Type", "application/json")
		r.RemoteAddr = "192.0.2.51:1000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}

	for i := 0; i < 2; i++ {
		if code := post(i); code == http.StatusTooManyRequests {
			t.Fatalf("request %d was throttled inside the burst", i)
		}
	}
	if code := post(99); code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", code)
	}
}

// Only the credential endpoints are limited. Throttling the webhook route would
// make Stripe redeliver, and throttling the API would punish a legitimate
// authenticated client for someone else's traffic.
func TestRateLimit_AppliesOnlyToCredentialEndpoints(t *testing.T) {
	truncate(t)
	h := throttledStack(t, 1)

	// Exhaust the auth bucket.
	postLogin(t, h, "192.0.2.52:1000")
	if rec := postLogin(t, h, "192.0.2.52:1000"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("auth bucket was not exhausted: status = %d", rec.Code)
	}

	// Same client, other routes, still served.
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/livez"},
		{http.MethodGet, "/healthz"},
	} {
		r := httptest.NewRequest(route.method, route.path, nil)
		r.RemoteAddr = "192.0.2.52:1000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code == http.StatusTooManyRequests {
			t.Errorf("%s was rate limited", route.path)
		}
	}

	// The webhook route must never be throttled: a 429 tells Stripe to retry,
	// and a throttled endpoint would turn into an ever-growing retry backlog.
	r := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader("{}"))
	r.RemoteAddr = "192.0.2.52:1000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code == http.StatusTooManyRequests {
		t.Error("the webhook route is rate limited; Stripe would build a retry backlog")
	}
}

// One noisy client must not lock everyone else out.
func TestRateLimit_IsPerClient(t *testing.T) {
	truncate(t)
	h := throttledStack(t, 1)

	postLogin(t, h, "192.0.2.53:1000")
	if rec := postLogin(t, h, "192.0.2.53:1000"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("noisy client not throttled: status = %d", rec.Code)
	}
	if rec := postLogin(t, h, "198.51.100.99:1000"); rec.Code == http.StatusTooManyRequests {
		t.Error("a second client was throttled by the first client's traffic")
	}
}
