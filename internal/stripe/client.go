// Package stripe wraps the Stripe SDK behind a narrow, context-aware surface.
//
// Nothing outside this package imports stripe-go. That keeps the SDK's global
// state, its parameter builders and its error taxonomy in one place, and it
// means the service layer can be exercised against a fake.
//
// The SDK is pinned to one API version (stripe.APIVersion). Objects are
// deserialised according to that version, so an account or a webhook endpoint
// pinned to a different one will silently produce zero-valued fields. New
// refuses to hide that mismatch: it logs a warning at startup, and webhook
// verification rejects mismatched events unless explicitly configured not to.
package stripe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	stripesdk "github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"
)

// Errors returned by this package. Callers branch on these rather than on the
// SDK's own error values.
var (
	// ErrSignatureVerification means the payload did not carry a valid
	// signature for the configured secret, or its timestamp fell outside the
	// tolerance window. It is never a transient condition: do not retry.
	ErrSignatureVerification = errors.New("stripe webhook signature verification failed")

	// ErrInvalidRequest means Stripe rejected the call as malformed. Retrying
	// an identical request cannot succeed.
	ErrInvalidRequest = errors.New("stripe rejected the request")

	// ErrUpstream means Stripe was reachable but failed, or was not reachable
	// at all. Retrying is appropriate.
	ErrUpstream = errors.New("stripe upstream failure")
)

// Config is the subset of application configuration this client needs. Secrets
// arrive already revealed; the caller owns the decision to unwrap them.
type Config struct {
	SecretKey     string
	WebhookSecret string

	// APIVersion is the version the Stripe account and webhook endpoints are
	// pinned to. It is compared against the SDK's own pin at startup.
	APIVersion string

	MaxNetworkRetries int
	HTTPTimeout       time.Duration

	// WebhookTolerance bounds the age of a signature timestamp. It is the only
	// defence against replay of a genuine, correctly signed request, so it
	// should stay small - Stripe's own recommendation is five minutes.
	WebhookTolerance time.Duration

	// IgnoreAPIVersionMismatch disables the SDK's check that an event was
	// rendered by the same API release train the SDK expects. Enable it only
	// while migrating an endpoint between versions, and expect fields outside
	// the SDK's version to deserialise as zero values.
	IgnoreAPIVersionMismatch bool
}

const (
	defaultHTTPTimeout      = 20 * time.Second
	defaultWebhookTolerance = 5 * time.Minute
)

type Client struct {
	sdk *stripesdk.Client
	log *slog.Logger

	webhookSecret            string
	webhookTolerance         time.Duration
	ignoreAPIVersionMismatch bool
}

func New(cfg Config, log *slog.Logger) (*Client, error) {
	if strings.TrimSpace(cfg.SecretKey) == "" {
		return nil, errors.New("stripe: secret key is required")
	}
	if strings.TrimSpace(cfg.WebhookSecret) == "" {
		return nil, errors.New("stripe: webhook secret is required")
	}

	timeout := cfg.HTTPTimeout
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	tolerance := cfg.WebhookTolerance
	if tolerance <= 0 {
		tolerance = defaultWebhookTolerance
	}

	retries := int64(cfg.MaxNetworkRetries)
	if retries < 0 {
		retries = 0
	}

	backendCfg := &stripesdk.BackendConfig{
		HTTPClient:        &http.Client{Timeout: timeout},
		MaxNetworkRetries: stripesdk.Int64(retries),
	}
	backends := &stripesdk.Backends{
		API:     stripesdk.GetBackendWithConfig(stripesdk.APIBackend, backendCfg),
		Connect: stripesdk.GetBackendWithConfig(stripesdk.ConnectBackend, backendCfg),
		Uploads: stripesdk.GetBackendWithConfig(stripesdk.UploadsBackend, backendCfg),
	}

	c := &Client{
		sdk:                      stripesdk.NewClient(cfg.SecretKey, stripesdk.WithBackends(backends)),
		log:                      log,
		webhookSecret:            cfg.WebhookSecret,
		webhookTolerance:         tolerance,
		ignoreAPIVersionMismatch: cfg.IgnoreAPIVersionMismatch,
	}

	if v := strings.TrimSpace(cfg.APIVersion); v != "" && !sameReleaseTrain(v, stripesdk.APIVersion) {
		log.Warn("stripe api version mismatch",
			slog.String("configured", v),
			slog.String("sdk_expects", stripesdk.APIVersion),
			slog.String("impact", "webhook events from the configured version will be rejected unless IgnoreAPIVersionMismatch is set"),
		)
	}
	return c, nil
}

// APIVersion reports the version this SDK build deserialises against.
func (c *Client) APIVersion() string { return stripesdk.APIVersion }

// sameReleaseTrain compares the trailing label of two Stripe API versions
// ("2026-08-26.dahlia" -> "dahlia"). Versions predating release trains carry no
// label and are never compatible with a version that has one.
func sameReleaseTrain(a, b string) bool {
	ai, bi := strings.LastIndex(a, "."), strings.LastIndex(b, ".")
	if ai < 0 || bi < 0 {
		return false
	}
	return a[ai+1:] == b[bi+1:]
}

// --- webhook verification ----------------------------------------------------

// VerifyWebhook authenticates a raw request body against the Stripe-Signature
// header and returns the parsed event.
//
// The payload must be the exact bytes received. The signature covers the raw
// body, so decoding and re-encoding the JSON - or reading it through anything
// that normalises whitespace - invalidates it.
//
// This must run before any other use of the payload. Until it returns, the body
// is unauthenticated input from an anonymous caller.
func (c *Client) VerifyWebhook(payload []byte, signatureHeader string) (stripesdk.Event, error) {
	event, err := webhook.ConstructEventWithOptions(payload, signatureHeader, c.webhookSecret,
		webhook.ConstructEventOptions{
			Tolerance:                c.webhookTolerance,
			IgnoreAPIVersionMismatch: c.ignoreAPIVersionMismatch,
		})
	if err != nil {
		// The SDK's message is safe to surface internally but must not reach the
		// client: distinguishing "bad signature" from "stale timestamp" tells an
		// attacker which half of the check they defeated.
		return stripesdk.Event{}, fmt.Errorf("%w: %s", ErrSignatureVerification, err)
	}
	return event, nil
}

// --- checkout ----------------------------------------------------------------

// UIMode selects how the checkout is presented.
//
// Hosted redirects the browser to Stripe's own page; Embedded returns a client
// secret the frontend mounts inside the app with Stripe Elements. Embedded
// requires a ReturnURL and forbids SuccessURL/CancelURL - Stripe rejects the
// call if they are mixed.
type UIMode string

const (
	UIModeHosted   UIMode = "hosted"
	UIModeEmbedded UIMode = "embedded"
)

func (m UIMode) valid() bool { return m == UIModeHosted || m == UIModeEmbedded || m == "" }

// CheckoutSessionInput describes a subscription checkout to open.
type CheckoutSessionInput struct {
	// UIMode defaults to UIModeHosted when empty.
	UIMode UIMode

	// ReturnURL is where Stripe sends the browser after an embedded checkout
	// completes. Required for UIModeEmbedded, ignored otherwise.
	ReturnURL string

	PriceID  string
	Quantity int64

	// CustomerID reuses an existing Stripe customer. When empty, CustomerEmail
	// seeds a new one. Supplying both is an error: Stripe would ignore the
	// email, and the caller clearly believes otherwise.
	CustomerID    string
	CustomerEmail string

	// ClientReferenceID carries our own user id through Stripe and back on
	// checkout.session.completed. It is the primary link between a Stripe
	// customer and a local user, and it survives even when the customer record
	// is created by Stripe rather than by us.
	ClientReferenceID string

	SuccessURL string
	CancelURL  string

	TrialPeriodDays     int64
	AllowPromotionCodes bool

	// Metadata is copied onto both the session and the resulting subscription.
	// Session metadata is not inherited automatically, and only the
	// subscription's copy is visible on later customer.subscription.* events.
	Metadata map[string]string
}

func (in CheckoutSessionInput) validate() error {
	var problems []string
	if !strings.HasPrefix(in.PriceID, "price_") {
		problems = append(problems, "price_id must begin with price_")
	}
	if in.Quantity <= 0 {
		problems = append(problems, "quantity must be greater than zero")
	}
	if in.CustomerID != "" && !strings.HasPrefix(in.CustomerID, "cus_") {
		problems = append(problems, "customer_id must begin with cus_")
	}
	if in.CustomerID != "" && in.CustomerEmail != "" {
		problems = append(problems, "customer_id and customer_email are mutually exclusive")
	}
	if in.UIMode != UIModeEmbedded {
		if in.SuccessURL == "" {
			problems = append(problems, "success_url is required")
		}
		if in.CancelURL == "" {
			problems = append(problems, "cancel_url is required")
		}
	}
	if in.TrialPeriodDays < 0 {
		problems = append(problems, "trial_period_days must not be negative")
	}
	if !in.UIMode.valid() {
		problems = append(problems, "ui_mode must be hosted or embedded")
	}
	if in.UIMode == UIModeEmbedded && in.ReturnURL == "" {
		problems = append(problems, "return_url is required for embedded checkout")
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidRequest, strings.Join(problems, "; "))
	}
	return nil
}

// CheckoutSession is the subset of the created session the caller needs. The
// full SDK object is deliberately not exposed.
type CheckoutSession struct {
	ID  string
	URL string

	// ClientSecret is populated only for embedded checkout. It is a
	// session-scoped, short-lived token that the frontend needs in order to
	// mount the form, not an API credential.
	ClientSecret string
	ExpiresAt    time.Time
}

func (c *Client) CreateCheckoutSession(ctx context.Context, in CheckoutSessionInput) (*CheckoutSession, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}

	params := &stripesdk.CheckoutSessionCreateParams{
		Mode: stripesdk.String(string(stripesdk.CheckoutSessionModeSubscription)),
		LineItems: []*stripesdk.CheckoutSessionCreateLineItemParams{{
			Price:    stripesdk.String(in.PriceID),
			Quantity: stripesdk.Int64(in.Quantity),
		}},
		SubscriptionData: &stripesdk.CheckoutSessionCreateSubscriptionDataParams{
			Metadata: in.Metadata,
		},
		Metadata: in.Metadata,
	}
	params.Context = ctx

	// Stripe rejects a session that carries both a return_url and the
	// success/cancel pair, so the two presentations are mutually exclusive here
	// rather than merely different.
	if in.UIMode == UIModeEmbedded {
		params.UIMode = stripesdk.String(string(stripesdk.CheckoutSessionUIModeEmbeddedPage))
		params.ReturnURL = stripesdk.String(in.ReturnURL)
	} else {
		params.SuccessURL = stripesdk.String(in.SuccessURL)
		params.CancelURL = stripesdk.String(in.CancelURL)
	}

	if in.ClientReferenceID != "" {
		params.ClientReferenceID = stripesdk.String(in.ClientReferenceID)
	}
	switch {
	case in.CustomerID != "":
		params.Customer = stripesdk.String(in.CustomerID)
	case in.CustomerEmail != "":
		params.CustomerEmail = stripesdk.String(in.CustomerEmail)
	}
	if in.TrialPeriodDays > 0 {
		params.SubscriptionData.TrialPeriodDays = stripesdk.Int64(in.TrialPeriodDays)
	}
	if in.AllowPromotionCodes {
		params.AllowPromotionCodes = stripesdk.Bool(true)
	}

	session, err := c.sdk.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		return nil, classify("create checkout session", err)
	}

	out := &CheckoutSession{ID: session.ID, URL: session.URL, ClientSecret: session.ClientSecret}
	if session.ExpiresAt > 0 {
		out.ExpiresAt = time.Unix(session.ExpiresAt, 0).UTC()
	}
	return out, nil
}

// --- subscriptions -----------------------------------------------------------

// GetSubscription fetches a subscription with its items expanded.
//
// The items matter: since the 2025 API versions, current_period_start and
// current_period_end live on the subscription item, not on the subscription.
// A subscription fetched without them cannot be mapped to our schema, whose
// period columns are NOT NULL.
func (c *Client) GetSubscription(ctx context.Context, id string) (*stripesdk.Subscription, error) {
	if !strings.HasPrefix(id, "sub_") {
		return nil, fmt.Errorf("%w: subscription id %q must begin with sub_", ErrInvalidRequest, id)
	}

	params := &stripesdk.SubscriptionRetrieveParams{}
	params.Context = ctx
	params.AddExpand("items.data.price.product")

	sub, err := c.sdk.V1Subscriptions.Retrieve(ctx, id, params)
	if err != nil {
		return nil, classify("retrieve subscription", err)
	}
	return sub, nil
}

// --- error classification ----------------------------------------------------

// classify maps an SDK error onto this package's sentinels so callers can
// decide whether a retry is worth attempting without importing stripe-go.
func classify(op string, err error) error {
	if err == nil {
		return nil
	}

	var serr *stripesdk.Error
	if !errors.As(err, &serr) {
		// No API error means the request never completed: DNS, TLS, timeout or a
		// cancelled context. All are worth retrying.
		return fmt.Errorf("%s: %w: %s", op, ErrUpstream, err)
	}

	// Stripe echoes the request id on every response; it is the only thing
	// their support will ask for, and it identifies nothing about the customer.
	detail := serr.Msg
	if serr.RequestID != "" {
		detail = fmt.Sprintf("%s (request_id=%s)", detail, serr.RequestID)
	}

	switch serr.Type {
	case stripesdk.ErrorTypeAPI, stripesdk.ErrorTypeRateLimit:
		return fmt.Errorf("%s: %w: %s", op, ErrUpstream, detail)
	case stripesdk.ErrorTypeInvalidRequest, stripesdk.ErrorTypeCard:
		return fmt.Errorf("%s: %w: %s", op, ErrInvalidRequest, detail)
	}

	if serr.HTTPStatusCode >= http.StatusInternalServerError || serr.HTTPStatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("%s: %w: %s", op, ErrUpstream, detail)
	}
	return fmt.Errorf("%s: %w: %s", op, ErrInvalidRequest, detail)
}

// IsRetryable reports whether err is worth retrying. A webhook handler uses it
// to decide between a 5xx (Stripe redelivers) and a 2xx (permanent, do not).
func IsRetryable(err error) bool { return errors.Is(err, ErrUpstream) }
