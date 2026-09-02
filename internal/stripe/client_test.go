package stripe

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	stripesdk "github.com/stripe/stripe-go/v86"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testClient(t *testing.T) *Client {
	t.Helper()
	c, err := New(Config{
		SecretKey:     "sk_test_abc",
		WebhookSecret: "whsec_abc",
		APIVersion:    stripesdk.APIVersion,
	}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestNewRejectsMissingCredentials(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"no secret key", Config{WebhookSecret: "whsec_abc"}},
		{"no webhook secret", Config{SecretKey: "sk_test_abc"}},
		{"blank secret key", Config{SecretKey: "   ", WebhookSecret: "whsec_abc"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.cfg, discardLogger()); err == nil {
				t.Error("client was built without complete credentials")
			}
		})
	}
}

/* --- error classification ------------------------------------------------- */

// The whole point of classify: a webhook handler decides between a 5xx that
// makes Stripe redeliver and a 2xx that stops it, and a checkout handler
// decides between 502 and 422. Getting this wrong either loses events or
// retries something that can never succeed.
func TestClassifyDecidesRetryability(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantErr   error
		wantRetry bool
	}{
		{
			name:      "network failure never reached Stripe",
			err:       errors.New("dial tcp: i/o timeout"),
			wantErr:   ErrUpstream,
			wantRetry: true,
		},
		{
			name:      "api_error is Stripe's fault",
			err:       &stripesdk.Error{Type: stripesdk.ErrorTypeAPI, Msg: "internal"},
			wantErr:   ErrUpstream,
			wantRetry: true,
		},
		{
			name:      "rate limit is worth retrying",
			err:       &stripesdk.Error{Type: stripesdk.ErrorTypeRateLimit, Msg: "slow down"},
			wantErr:   ErrUpstream,
			wantRetry: true,
		},
		{
			name:      "invalid_request cannot succeed on retry",
			err:       &stripesdk.Error{Type: stripesdk.ErrorTypeInvalidRequest, Msg: "no such price"},
			wantErr:   ErrInvalidRequest,
			wantRetry: false,
		},
		{
			name:      "card_error is the customer's card, not a fault to retry",
			err:       &stripesdk.Error{Type: stripesdk.ErrorTypeCard, Msg: "declined"},
			wantErr:   ErrInvalidRequest,
			wantRetry: false,
		},
		{
			name:      "5xx without a recognised type is still upstream",
			err:       &stripesdk.Error{HTTPStatusCode: http.StatusBadGateway, Msg: "bad gateway"},
			wantErr:   ErrUpstream,
			wantRetry: true,
		},
		{
			name:      "429 without a recognised type is still upstream",
			err:       &stripesdk.Error{HTTPStatusCode: http.StatusTooManyRequests, Msg: "too many"},
			wantErr:   ErrUpstream,
			wantRetry: true,
		},
		{
			name:      "4xx without a recognised type is permanent",
			err:       &stripesdk.Error{HTTPStatusCode: http.StatusBadRequest, Msg: "bad request"},
			wantErr:   ErrInvalidRequest,
			wantRetry: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classify("test op", tt.err)
			if !errors.Is(got, tt.wantErr) {
				t.Errorf("classify = %v, want %v", got, tt.wantErr)
			}
			if IsRetryable(got) != tt.wantRetry {
				t.Errorf("IsRetryable = %v, want %v", IsRetryable(got), tt.wantRetry)
			}
			if !strings.Contains(got.Error(), "test op") {
				t.Errorf("error lost the operation name: %v", got)
			}
		})
	}

	if classify("op", nil) != nil {
		t.Error("classify turned a nil error into a failure")
	}
}

// Stripe echoes a request id on every response. It is the only thing their
// support asks for, and it identifies nothing about the customer.
func TestClassifyKeepsTheStripeRequestID(t *testing.T) {
	err := classify("create checkout session", &stripesdk.Error{
		Type: stripesdk.ErrorTypeInvalidRequest, Msg: "No such price", RequestID: "req_abc123",
	})
	if !strings.Contains(err.Error(), "req_abc123") {
		t.Errorf("error lost the request id: %v", err)
	}
}

/* --- webhook verification ------------------------------------------------- */

// Verification failures must not be distinguishable in the returned error's
// sentinel: a caller has to answer 400 either way, and telling a forger which
// half they defeated is free information.
func TestVerifyWebhookRejectsBadInput(t *testing.T) {
	c := testClient(t)
	payload := []byte(`{"id":"evt_1","object":"event","type":"x","created":1}`)

	tests := []struct{ name, header string }{
		{"empty header", ""},
		{"garbage", "not-a-signature"},
		{"missing v1", "t=1"},
		{"wrong signature", "t=1,v1=" + strings.Repeat("a", 64)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := c.VerifyWebhook(payload, tt.header); !errors.Is(err, ErrSignatureVerification) {
				t.Errorf("err = %v, want ErrSignatureVerification", err)
			}
		})
	}
}

func TestAPIVersionIsReported(t *testing.T) {
	if got := testClient(t).APIVersion(); got != stripesdk.APIVersion {
		t.Errorf("APIVersion = %q, want %q", got, stripesdk.APIVersion)
	}
}

// A token minted against a different API release train deserialises fields the
// SDK does not know about, so the trains have to match.
func TestSameReleaseTrain(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"2026-08-26.dahlia", "2026-08-26.dahlia", true},
		{"2026-08-26.dahlia", "2026-01-01.dahlia", true},
		{"2026-08-26.dahlia", "2025-01-01.acacia", false},
		// Versions predating release trains carry no label and can never match.
		{"2024-06-20", "2026-08-26.dahlia", false},
		{"2026-08-26.dahlia", "2024-06-20", false},
		{"", "2026-08-26.dahlia", false},
	}
	for _, tt := range tests {
		if got := sameReleaseTrain(tt.a, tt.b); got != tt.want {
			t.Errorf("sameReleaseTrain(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

/* --- input validation ----------------------------------------------------- */

// These are checked before any network call, so a malformed request fails fast
// rather than costing a round trip and a Stripe error.
func TestCheckoutSessionInputValidation(t *testing.T) {
	valid := CheckoutSessionInput{
		PriceID: "price_abc", Quantity: 1,
		SuccessURL: "https://x.test/ok", CancelURL: "https://x.test/no",
	}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*CheckoutSessionInput)
		wantIn string
	}{
		{"bad price prefix", func(i *CheckoutSessionInput) { i.PriceID = "prod_abc" }, "price_"},
		{"zero quantity", func(i *CheckoutSessionInput) { i.Quantity = 0 }, "quantity"},
		{"bad customer prefix", func(i *CheckoutSessionInput) { i.CustomerID = "sub_abc" }, "customer_id"},
		{"customer and email together", func(i *CheckoutSessionInput) {
			i.CustomerID, i.CustomerEmail = "cus_abc", "a@b.com"
		}, "mutually exclusive"},
		{"no success url", func(i *CheckoutSessionInput) { i.SuccessURL = "" }, "success_url"},
		{"no cancel url", func(i *CheckoutSessionInput) { i.CancelURL = "" }, "cancel_url"},
		{"negative trial", func(i *CheckoutSessionInput) { i.TrialPeriodDays = -1 }, "trial_period_days"},
		{"unknown ui mode", func(i *CheckoutSessionInput) { i.UIMode = "popup" }, "ui_mode"},
		{"embedded without return url", func(i *CheckoutSessionInput) {
			i.UIMode, i.SuccessURL, i.CancelURL = UIModeEmbedded, "", ""
		}, "return_url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := valid
			tt.mutate(&in)
			err := in.validate()
			if err == nil {
				t.Fatal("invalid input accepted")
			}
			if !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("err = %v, want ErrInvalidRequest", err)
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("err = %q, want it to mention %q", err, tt.wantIn)
			}
		})
	}

	// Embedded checkout supplies a return URL instead of success/cancel, and
	// Stripe rejects a session carrying both.
	t.Run("embedded with a return url", func(t *testing.T) {
		in := CheckoutSessionInput{
			PriceID: "price_abc", Quantity: 1,
			UIMode: UIModeEmbedded, ReturnURL: "https://x.test/back",
		}
		if err := in.validate(); err != nil {
			t.Errorf("valid embedded input rejected: %v", err)
		}
	})
}

func TestUIModeValidity(t *testing.T) {
	for _, m := range []UIMode{UIModeHosted, UIModeEmbedded, ""} {
		if !m.valid() {
			t.Errorf("%q is not valid", m)
		}
	}
	if UIMode("popup").valid() {
		t.Error("an unknown ui mode is valid")
	}
}

// The portal URL authenticates its bearer as that Stripe customer, so a
// malformed customer id must fail before it reaches the API.
func TestCreatePortalSessionRejectsBadInput(t *testing.T) {
	c := testClient(t)
	ctx := t.Context()

	if _, err := c.CreatePortalSession(ctx, "sub_wrong", "https://x.test/"); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("err = %v, want ErrInvalidRequest for a bad customer id", err)
	}
	if _, err := c.CreatePortalSession(ctx, "cus_abc", ""); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("err = %v, want ErrInvalidRequest for a missing return url", err)
	}
}

func TestGetSubscriptionRejectsBadID(t *testing.T) {
	if _, err := testClient(t).GetSubscription(t.Context(), "cus_wrong"); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	c, err := New(Config{SecretKey: "sk_test_abc", WebhookSecret: "whsec_abc"}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.webhookTolerance != defaultWebhookTolerance {
		t.Errorf("tolerance = %s, want the %s default", c.webhookTolerance, defaultWebhookTolerance)
	}

	// A negative retry count must not reach the SDK.
	if _, err := New(Config{
		SecretKey: "sk_test_abc", WebhookSecret: "whsec_abc",
		MaxNetworkRetries: -5, HTTPTimeout: time.Second,
	}, discardLogger()); err != nil {
		t.Errorf("negative retries were not clamped: %v", err)
	}
}
