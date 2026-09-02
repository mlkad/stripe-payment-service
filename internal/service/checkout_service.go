package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/mlkad/stripe-payment-service/internal/domain"
	repo "github.com/mlkad/stripe-payment-service/internal/repository/postgres"
	paystripe "github.com/mlkad/stripe-payment-service/internal/stripe"
)

// CheckoutConfig holds the redirect targets and the price allowlist.
type CheckoutConfig struct {
	SuccessURL string
	CancelURL  string

	// ReturnURL is where Stripe sends the browser after an embedded checkout.
	// Required only when the frontend asks for the embedded presentation.
	ReturnURL string

	// PortalReturnURL is where Stripe sends the browser back after the customer
	// finishes in the billing portal.
	PortalReturnURL string

	// AllowedPriceIDs restricts which prices a caller may check out against.
	// Without it the price id is caller-controlled, and anyone who can reach the
	// endpoint can subscribe themselves to the cheapest price in the account -
	// or to a price belonging to an entirely different product. Empty means
	// unrestricted, which is only acceptable in development.
	AllowedPriceIDs []string
}

func (c CheckoutConfig) validate() error {
	var problems []string
	for name, raw := range map[string]string{
		"success_url":       c.SuccessURL,
		"cancel_url":        c.CancelURL,
		"return_url":        c.ReturnURL,
		"portal_return_url": c.PortalReturnURL,
	} {
		if raw == "" {
			// Both return URLs are optional: each is needed only for one
			// presentation, and the method that needs it says so at the point
			// of use.
			if name == "return_url" || name == "portal_return_url" {
				continue
			}
			problems = append(problems, name+" is required")
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || !u.IsAbs() {
			problems = append(problems, name+" must be an absolute URL")
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("checkout config: %s", strings.Join(problems, "; "))
	}
	return nil
}

func (c CheckoutConfig) priceAllowed(id string) bool {
	if len(c.AllowedPriceIDs) == 0 {
		return true
	}
	for _, allowed := range c.AllowedPriceIDs {
		if allowed == id {
			return true
		}
	}
	return false
}

type CheckoutService struct {
	users  repo.UserRepository
	stripe *paystripe.Client
	cfg    CheckoutConfig
	log    *slog.Logger
}

func NewCheckoutService(users repo.UserRepository, stripe *paystripe.Client, cfg CheckoutConfig, log *slog.Logger) (*CheckoutService, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if len(cfg.AllowedPriceIDs) == 0 {
		log.Warn("checkout price allowlist is empty; any price id will be accepted")
	}
	return &CheckoutService{users: users, stripe: stripe, cfg: cfg, log: log}, nil
}

// CheckoutRequest is the use case input. UserID comes from the authenticated
// session, never from the request body.
type CheckoutRequest struct {
	UserID          uuid.UUID
	PriceID         string
	Quantity        int64
	TrialPeriodDays int64

	// Embedded selects the in-app Stripe Elements form instead of a redirect to
	// Stripe's hosted page.
	Embedded bool
}

type CheckoutResult struct {
	SessionID string

	// Exactly one of URL and ClientSecret is set, according to the presentation
	// requested: URL for the hosted redirect, ClientSecret for the embedded form.
	URL          string
	ClientSecret string
}

// CreateCheckoutSession opens a Stripe Checkout session for an existing user.
func (s *CheckoutService) CreateCheckoutSession(ctx context.Context, req CheckoutRequest) (*CheckoutResult, error) {
	if req.Quantity <= 0 {
		req.Quantity = 1
	}
	if !s.cfg.priceAllowed(req.PriceID) {
		return nil, fmt.Errorf("%w: price %q is not offered", domain.ErrValidation, req.PriceID)
	}

	user, err := s.users.GetUserByID(ctx, req.UserID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("load user: %w", err)
	}

	in := paystripe.CheckoutSessionInput{
		UIMode:   paystripe.UIModeHosted,
		PriceID:  req.PriceID,
		Quantity: req.Quantity,
		// Carried through Stripe and returned on checkout.session.completed;
		// this is what links the resulting customer back to a local user.
		ClientReferenceID: user.ID.String(),
		SuccessURL:        s.cfg.SuccessURL,
		CancelURL:         s.cfg.CancelURL,
		TrialPeriodDays:   req.TrialPeriodDays,
		Metadata:          map[string]string{"user_id": user.ID.String()},
	}
	if req.Embedded {
		if s.cfg.ReturnURL == "" {
			return nil, fmt.Errorf("%w: embedded checkout is not configured on this deployment", domain.ErrValidation)
		}
		in.UIMode = paystripe.UIModeEmbedded
		in.ReturnURL = s.cfg.ReturnURL
	}

	// Reusing the existing customer keeps one billing identity per user. Passing
	// the email instead would let Stripe mint a second customer for someone who
	// already has one.
	if user.StripeCustomerID != nil {
		in.CustomerID = *user.StripeCustomerID
	} else {
		in.CustomerEmail = user.Email
	}

	session, err := s.stripe.CreateCheckoutSession(ctx, in)
	if err != nil {
		return nil, err
	}

	s.log.InfoContext(ctx, "checkout session created",
		slog.String("user_id", user.ID.String()),
		slog.String("session_id", session.ID),
		slog.String("price_id", req.PriceID),
		slog.Int64("quantity", req.Quantity),
	)
	return &CheckoutResult{
		SessionID:    session.ID,
		URL:          session.URL,
		ClientSecret: session.ClientSecret,
	}, nil
}

// PortalResult is a link into Stripe's hosted billing portal.
type PortalResult struct {
	URL string
}

// CreatePortalSession opens the billing portal for a user.
//
// Requires a linked Stripe customer, which a user only has after their first
// checkout completes. Before that there is nothing to manage, and the caller
// gets domain.ErrNotFound rather than a portal showing an empty account.
//
// The customer id comes from the user row, never from the request. That is the
// whole security boundary here: the returned URL authenticates its bearer as
// that customer, so letting a caller name the customer would hand them
// someone else's billing account.
func (s *CheckoutService) CreatePortalSession(ctx context.Context, userID uuid.UUID) (*PortalResult, error) {
	if s.cfg.PortalReturnURL == "" {
		return nil, fmt.Errorf("%w: the billing portal is not configured on this deployment", domain.ErrValidation)
	}

	user, err := s.users.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("load user: %w", err)
	}
	if user.StripeCustomerID == nil {
		return nil, fmt.Errorf("%w: no billing account for this user", domain.ErrNotFound)
	}

	session, err := s.stripe.CreatePortalSession(ctx, *user.StripeCustomerID, s.cfg.PortalReturnURL)
	if err != nil {
		return nil, err
	}

	// The session id is logged; the URL is not. It is a bearer credential for
	// the customer's billing account for as long as it lives.
	s.log.InfoContext(ctx, "billing portal session created",
		slog.String("user_id", user.ID.String()),
		slog.String("session_id", session.ID))

	return &PortalResult{URL: session.URL}, nil
}
