package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mlkad/stripe-payment-service/internal/domain"
	repo "github.com/mlkad/stripe-payment-service/internal/repository/postgres"
)

// SubscriptionView is the read model the dashboard renders. It is deliberately
// not domain.Subscription: internal ids, the Stripe customer id and the event
// bookkeeping columns have no business reaching a browser.
type SubscriptionView struct {
	Status            domain.SubscriptionStatus `json:"status"`
	IsActive          bool                      `json:"is_active"`
	PriceID           string                    `json:"price_id"`
	Quantity          int32                     `json:"quantity"`
	Currency          *string                   `json:"currency,omitempty"`
	UnitAmount        *int64                    `json:"unit_amount,omitempty"`
	CurrentPeriodEnd  time.Time                 `json:"current_period_end"`
	CancelAtPeriodEnd bool                      `json:"cancel_at_period_end"`
	CanceledAt        *time.Time                `json:"canceled_at,omitempty"`
	TrialEnd          *time.Time                `json:"trial_end,omitempty"`
}

type SubscriptionService struct {
	subs repo.SubscriptionRepository
	log  *slog.Logger
}

func NewSubscriptionService(subs repo.SubscriptionRepository, log *slog.Logger) *SubscriptionService {
	return &SubscriptionService{subs: subs, log: log}
}

// GetForUser returns the user's most recent subscription.
//
// A user who never subscribed yields domain.ErrNotFound, which the handler maps
// to 404. That is distinct from a user whose subscription lapsed: the latter
// returns a view with a terminal status, so the UI can say "your plan ended"
// rather than "you have no plan".
func (s *SubscriptionService) GetForUser(ctx context.Context, userID uuid.UUID) (*SubscriptionView, error) {
	sub, err := s.subs.GetLatestSubscriptionByUserID(ctx, userID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("load subscription: %w", err)
	}

	return &SubscriptionView{
		Status:            sub.Status,
		IsActive:          sub.IsLive(),
		PriceID:           sub.StripePriceID,
		Quantity:          sub.Quantity,
		Currency:          sub.Currency,
		UnitAmount:        sub.UnitAmount,
		CurrentPeriodEnd:  sub.CurrentPeriodEnd,
		CancelAtPeriodEnd: sub.CancelAtPeriodEnd,
		CanceledAt:        sub.CanceledAt,
		TrialEnd:          sub.TrialEnd,
	}, nil
}
