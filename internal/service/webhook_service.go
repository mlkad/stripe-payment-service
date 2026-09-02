// Package service holds the use cases. It orchestrates the domain and the
// ports; it owns no transport concerns and no SQL.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	stripesdk "github.com/stripe/stripe-go/v86"

	"github.com/mlkad/stripe-payment-service/internal/domain"
	repo "github.com/mlkad/stripe-payment-service/internal/repository/postgres"
	paystripe "github.com/mlkad/stripe-payment-service/internal/stripe"
)

// Outcome is what happened to one delivery. Every value except OutcomeFailed
// means the event is settled and Stripe must be told to stop redelivering.
type Outcome string

const (
	// OutcomeProcessed - the event was applied.
	OutcomeProcessed Outcome = "processed"
	// OutcomeDuplicate - already settled, or another worker holds the claim.
	OutcomeDuplicate Outcome = "duplicate"
	// OutcomeSkipped - a type this service does not subscribe to.
	OutcomeSkipped Outcome = "skipped"
	// OutcomeStale - superseded by a newer event already applied.
	OutcomeStale Outcome = "stale"
	// OutcomeFailed - processing errored; Stripe should redeliver.
	OutcomeFailed Outcome = "failed"
)

// Acknowledge reports whether Stripe should be answered 2xx.
func (o Outcome) Acknowledge() bool { return o != OutcomeFailed }

// handledEventTypes is the service's subscription list. Anything outside it is
// claimed, marked skipped and acknowledged, so the ledger records that the
// event was seen rather than silently dropping it.
var handledEventTypes = map[stripesdk.EventType]bool{
	"checkout.session.completed":    true,
	"customer.subscription.updated": true,
	"customer.subscription.deleted": true,
	"customer.subscription.created": true,
	"customer.subscription.paused":  true,
	"customer.subscription.resumed": true,

	// Invoice events drive dunning. payment_failed is the most commercially
	// important event Stripe sends: it is where a renewal starts going wrong,
	// and the window in which a customer can still be saved.
	"invoice.payment_failed":    true,
	"invoice.payment_succeeded": true,
}

type WebhookService struct {
	users  repo.UserRepository
	subs   repo.SubscriptionRepository
	hooks  repo.WebhookRepository
	stripe *paystripe.Client
	log    *slog.Logger
}

func NewWebhookService(
	users repo.UserRepository,
	subs repo.SubscriptionRepository,
	hooks repo.WebhookRepository,
	stripe *paystripe.Client,
	log *slog.Logger,
) *WebhookService {
	return &WebhookService{users: users, subs: subs, hooks: hooks, stripe: stripe, log: log}
}

// ProcessEvent authenticates, claims and applies one webhook delivery.
//
// The order of the first two steps is a security property, not a preference.
// Verification must complete before the payload is used for anything at all,
// including the claim: event_id, event_type and created all come from the body,
// and until the signature is checked that body is unauthenticated input from an
// anonymous caller. Claiming first would let anyone POST {"id":"evt_..."} for an
// event id they guessed or observed, insert a settled row, and cause the genuine
// delivery from Stripe to be discarded as a duplicate - a silent, unauthenticated
// denial of service against billing state.
//
// After verification the flow is claim -> dispatch -> settle. The claim is
// committed on its own so that a crashed worker leaves a reclaimable row behind
// rather than vanishing without trace.
func (s *WebhookService) ProcessEvent(ctx context.Context, payload []byte, signatureHeader string) (Outcome, error) {
	event, err := s.stripe.VerifyWebhook(payload, signatureHeader)
	if err != nil {
		return OutcomeFailed, err
	}

	log := s.log.With(
		slog.String("event_id", event.ID),
		slog.String("event_type", string(event.Type)),
		slog.Bool("livemode", event.Livemode),
		slog.Int("payload_bytes", len(payload)),
	)

	record := &domain.ProcessedWebhook{
		EventID:         event.ID,
		EventType:       string(event.Type),
		Livemode:        event.Livemode,
		StripeCreatedAt: time.Unix(event.Created, 0).UTC(),
		Payload:         json.RawMessage(payload),
	}
	if event.APIVersion != "" {
		record.APIVersion = &event.APIVersion
	}
	if event.Request != nil && event.Request.ID != "" {
		record.RequestID = &event.Request.ID
	}

	claimed, err := s.hooks.TryClaimEvent(ctx, record)
	if err != nil {
		log.ErrorContext(ctx, "claim failed", slog.String("error", err.Error()))
		return OutcomeFailed, fmt.Errorf("claim event: %w", err)
	}
	if !claimed {
		log.InfoContext(ctx, "event already settled or in flight")
		return OutcomeDuplicate, nil
	}
	log = log.With(slog.Int("attempt", int(record.Attempts)))

	if !handledEventTypes[event.Type] {
		return s.settleSkipped(ctx, log, event.ID, "event type not subscribed")
	}

	outcome, err := s.dispatch(ctx, log, event)
	if err != nil {
		// The settle is best-effort: the handler already failed, and losing the
		// reason to a second failure would leave the row stuck in processing
		// until the stale-claim window expires.
		if markErr := s.hooks.MarkEventFailed(ctx, event.ID, err); markErr != nil {
			log.ErrorContext(ctx, "could not record failure",
				slog.String("error", markErr.Error()))
		}
		log.ErrorContext(ctx, "event processing failed",
			slog.String("error", err.Error()),
			slog.Bool("retryable", paystripe.IsRetryable(err)))
		return OutcomeFailed, err
	}

	if outcome == OutcomeSkipped {
		return s.settleSkipped(ctx, log, event.ID, "no action required for this event")
	}
	if err := s.hooks.MarkEventProcessed(ctx, event.ID); err != nil {
		// The work is done and committed. Failing to stamp the ledger is a
		// bookkeeping fault: reporting 5xx here would make Stripe redeliver an
		// event whose effects already landed.
		log.ErrorContext(ctx, "processing succeeded but ledger not settled",
			slog.String("error", err.Error()))
	}
	log.InfoContext(ctx, "event processed", slog.String("outcome", string(outcome)))
	return outcome, nil
}

func (s *WebhookService) settleSkipped(ctx context.Context, log *slog.Logger, eventID, reason string) (Outcome, error) {
	if err := s.hooks.MarkEventSkipped(ctx, eventID, reason); err != nil {
		log.ErrorContext(ctx, "could not mark event skipped", slog.String("error", err.Error()))
	}
	log.InfoContext(ctx, "event skipped", slog.String("reason", reason))
	return OutcomeSkipped, nil
}

func (s *WebhookService) dispatch(ctx context.Context, log *slog.Logger, event stripesdk.Event) (Outcome, error) {
	switch event.Type {
	case "checkout.session.completed":
		return s.handleCheckoutCompleted(ctx, log, event)
	case "customer.subscription.deleted":
		return s.handleSubscriptionChanged(ctx, log, event, true)
	case "invoice.payment_succeeded":
		return s.handleInvoicePayment(ctx, log, event, true)
	case "invoice.payment_failed":
		return s.handleInvoicePayment(ctx, log, event, false)
	default:
		return s.handleSubscriptionChanged(ctx, log, event, false)
	}
}

// --- checkout.session.completed ----------------------------------------------

// handleCheckoutCompleted links the Stripe customer to a local user and records
// the subscription the session created.
//
// The session's subscription field is an id, not an object, so the subscription
// is fetched from the API: the webhook payload has no line items and therefore
// no billing period, which our schema requires.
func (s *WebhookService) handleCheckoutCompleted(ctx context.Context, log *slog.Logger, event stripesdk.Event) (Outcome, error) {
	var session stripesdk.CheckoutSession
	if err := json.Unmarshal(event.Data.Raw, &session); err != nil {
		return OutcomeFailed, fmt.Errorf("decode checkout session: %w", err)
	}

	if session.Mode != stripesdk.CheckoutSessionModeSubscription {
		log.InfoContext(ctx, "checkout session is not a subscription",
			slog.String("mode", string(session.Mode)))
		return OutcomeSkipped, nil
	}
	// An expired or unpaid session creates no subscription. Stripe still sends
	// the event, and acting on it would record a subscription that does not
	// exist.
	if session.Subscription == nil || session.Subscription.ID == "" {
		log.WarnContext(ctx, "checkout session completed without a subscription",
			slog.String("payment_status", string(session.PaymentStatus)))
		return OutcomeSkipped, nil
	}

	customerID := customerIDOf(session.Customer)
	user, err := s.resolveUser(ctx, session.ClientReferenceID, customerID, session.Metadata)
	if err != nil {
		return OutcomeFailed, err
	}
	if user == nil {
		log.WarnContext(ctx, "no local user for checkout session",
			slog.String("client_reference_id", session.ClientReferenceID))
		return OutcomeSkipped, nil
	}

	if err := s.linkCustomer(ctx, log, user, customerID); err != nil {
		return OutcomeFailed, err
	}

	sub, err := s.stripe.GetSubscription(ctx, session.Subscription.ID)
	if err != nil {
		return OutcomeFailed, err
	}
	if _, err := s.upsertSubscription(ctx, log, user.ID, sub, event); err != nil {
		return OutcomeFailed, err
	}
	return OutcomeProcessed, nil
}

// resolveUser maps a Stripe customer back to a local user. client_reference_id
// is authoritative because we set it when opening the session; the customer id
// is the fallback for subscriptions created outside this service.
func (s *WebhookService) resolveUser(ctx context.Context, clientRef, customerID string, metadata map[string]string) (*domain.User, error) {
	candidate := clientRef
	if candidate == "" {
		candidate = metadata["user_id"]
	}
	if candidate != "" {
		id, err := uuid.Parse(candidate)
		if err == nil {
			user, err := s.users.GetUserByID(ctx, id)
			switch {
			case err == nil:
				return user, nil
			case !errors.Is(err, domain.ErrNotFound):
				return nil, fmt.Errorf("look up user %s: %w", id, err)
			}
		}
	}

	if customerID == "" {
		return nil, nil
	}
	user, err := s.users.GetUserByStripeCustomerID(ctx, customerID)
	switch {
	case err == nil:
		return user, nil
	case errors.Is(err, domain.ErrNotFound):
		return nil, nil
	default:
		return nil, fmt.Errorf("look up user by customer: %w", err)
	}
}

// linkCustomer records the Stripe customer id on the user the first time we see
// it. A different id on an existing user is not overwritten: that would move a
// live subscription onto the wrong billing account, and it always means a bug
// upstream rather than a legitimate change.
func (s *WebhookService) linkCustomer(ctx context.Context, log *slog.Logger, user *domain.User, customerID string) error {
	if customerID == "" {
		return nil
	}
	if user.StripeCustomerID != nil {
		if *user.StripeCustomerID != customerID {
			log.ErrorContext(ctx, "user already linked to a different stripe customer",
				slog.String("user_id", user.ID.String()))
		}
		return nil
	}

	user.StripeCustomerID = &customerID
	if err := s.users.UpdateUser(ctx, user); err != nil {
		// Another delivery of the same checkout may have linked it first.
		if errors.Is(err, domain.ErrConflict) {
			log.WarnContext(ctx, "stripe customer already linked to another user",
				slog.String("user_id", user.ID.String()))
			return nil
		}
		return fmt.Errorf("link stripe customer to user: %w", err)
	}
	log.InfoContext(ctx, "linked stripe customer to user", slog.String("user_id", user.ID.String()))
	return nil
}

// --- customer.subscription.* -------------------------------------------------

// handleSubscriptionChanged applies a subscription lifecycle event.
//
// Stripe delivers unordered, so an update can arrive before the
// checkout.session.completed that first created the row. ErrNotFound is
// therefore an expected branch, not a fault: the subscription is inserted from
// the event's own object rather than dropped.
func (s *WebhookService) handleSubscriptionChanged(ctx context.Context, log *slog.Logger, event stripesdk.Event, deleted bool) (Outcome, error) {
	var sub stripesdk.Subscription
	if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
		return OutcomeFailed, fmt.Errorf("decode subscription: %w", err)
	}
	if sub.ID == "" {
		return OutcomeFailed, errors.New("subscription event carried no subscription id")
	}

	update, err := statusUpdateFrom(&sub, event, deleted)
	if err != nil {
		return OutcomeFailed, err
	}

	updated, err := s.subs.UpdateSubscriptionStatus(ctx, update)
	switch {
	case err == nil:
		log.InfoContext(ctx, "subscription updated",
			slog.String("subscription_id", updated.StripeSubscriptionID),
			slog.String("status", string(updated.Status)))
		return OutcomeProcessed, nil

	case errors.Is(err, domain.ErrStaleEvent):
		log.InfoContext(ctx, "event superseded by a newer one",
			slog.String("subscription_id", sub.ID))
		return OutcomeStale, nil

	case errors.Is(err, domain.ErrNotFound):
		return s.createFromEvent(ctx, log, &sub, event)

	default:
		return OutcomeFailed, fmt.Errorf("update subscription %s: %w", sub.ID, err)
	}
}

// createFromEvent inserts a subscription seen for the first time on an update
// event, resolving its owner through the Stripe customer id.
func (s *WebhookService) createFromEvent(ctx context.Context, log *slog.Logger, sub *stripesdk.Subscription, event stripesdk.Event) (Outcome, error) {
	customerID := customerIDOf(sub.Customer)
	user, err := s.resolveUser(ctx, "", customerID, sub.Metadata)
	if err != nil {
		return OutcomeFailed, err
	}
	if user == nil {
		// A subscription for a customer this service has never seen. Skipping
		// keeps an unrelated Stripe account's traffic from failing the endpoint
		// forever; the ledger retains the payload for reconciliation.
		log.WarnContext(ctx, "subscription for unknown customer",
			slog.String("subscription_id", sub.ID))
		return OutcomeSkipped, nil
	}

	if _, err := s.upsertSubscription(ctx, log, user.ID, sub, event); err != nil {
		return OutcomeFailed, err
	}
	return OutcomeProcessed, nil
}

// upsertSubscription creates the row, falling back to an update when a
// concurrent delivery won the insert.
func (s *WebhookService) upsertSubscription(
	ctx context.Context, log *slog.Logger, userID uuid.UUID,
	sub *stripesdk.Subscription, event stripesdk.Event,
) (Outcome, error) {
	record, err := subscriptionFrom(userID, sub, event)
	if err != nil {
		return OutcomeFailed, err
	}

	err = s.subs.CreateSubscription(ctx, record)
	switch {
	case err == nil:
		log.InfoContext(ctx, "subscription created",
			slog.String("subscription_id", record.StripeSubscriptionID),
			slog.String("status", string(record.Status)))
		return OutcomeProcessed, nil

	case errors.Is(err, domain.ErrConflict):
		update, uerr := statusUpdateFrom(sub, event, false)
		if uerr != nil {
			return OutcomeFailed, uerr
		}
		if _, uerr := s.subs.UpdateSubscriptionStatus(ctx, update); uerr != nil {
			if errors.Is(uerr, domain.ErrStaleEvent) {
				return OutcomeStale, nil
			}
			return OutcomeFailed, fmt.Errorf("update existing subscription %s: %w", sub.ID, uerr)
		}
		return OutcomeProcessed, nil

	default:
		return OutcomeFailed, fmt.Errorf("create subscription %s: %w", sub.ID, err)
	}
}

// --- invoice.payment_* --------------------------------------------------------

// handleInvoicePayment records a renewal outcome and maintains dunning state.
//
// It does not touch subscription status. Stripe decides what a failed payment
// means - past_due when the subscription was active, incomplete on a first
// invoice, canceled once retries run out - and reports it in a separate
// customer.subscription.updated event. Deriving status here would race that
// event and sometimes contradict it.
func (s *WebhookService) handleInvoicePayment(ctx context.Context, log *slog.Logger, event stripesdk.Event, succeeded bool) (Outcome, error) {
	var invoice stripesdk.Invoice
	if err := json.Unmarshal(event.Data.Raw, &invoice); err != nil {
		return OutcomeFailed, fmt.Errorf("decode invoice: %w", err)
	}

	subscriptionID := subscriptionIDOf(&invoice)
	if subscriptionID == "" {
		// One-off invoices and quote-generated invoices have no subscription.
		// Nothing to record, and nothing wrong.
		log.InfoContext(ctx, "invoice is not tied to a subscription",
			slog.String("invoice_id", invoice.ID),
			slog.String("billing_reason", string(invoice.BillingReason)))
		return OutcomeSkipped, nil
	}

	update := repo.InvoicePaymentUpdate{
		StripeSubscriptionID: subscriptionID,
		Succeeded:            succeeded,
		EventID:              event.ID,
		EventCreatedAt:       time.Unix(event.Created, 0).UTC(),
		NextAttemptAt:        unixPtr(invoice.NextPaymentAttempt),
	}
	if invoice.ID != "" {
		update.InvoiceID = &invoice.ID
	}
	if succeeded {
		// The invoice's period is the period actually paid for, which is what
		// the subscription should now report.
		update.CurrentPeriodStart = unixPtr(invoice.PeriodStart)
		update.CurrentPeriodEnd = unixPtr(invoice.PeriodEnd)
	} else if reason := declineReason(&invoice); reason != "" {
		update.FailureReason = &reason
	}

	updated, err := s.subs.RecordInvoicePayment(ctx, update)
	switch {
	case err == nil:
		if succeeded {
			log.InfoContext(ctx, "invoice paid",
				slog.String("subscription_id", subscriptionID),
				slog.String("invoice_id", invoice.ID))
		} else {
			log.WarnContext(ctx, "invoice payment failed",
				slog.String("subscription_id", subscriptionID),
				slog.String("invoice_id", invoice.ID),
				slog.Int("consecutive_failures", int(updated.PaymentFailureCount)),
				slog.Int64("stripe_attempt_count", invoice.AttemptCount),
				slog.Any("next_attempt_at", updated.NextPaymentAttemptAt))
		}
		return OutcomeProcessed, nil

	case errors.Is(err, domain.ErrStaleEvent):
		log.InfoContext(ctx, "invoice event superseded by a newer one",
			slog.String("subscription_id", subscriptionID))
		return OutcomeStale, nil

	case errors.Is(err, domain.ErrNotFound):
		// Unordered delivery again: the invoice can land before the
		// subscription row exists. The invoice payload carries no items, so
		// there is nothing to create the row from - acknowledge and let the
		// subscription event do it.
		log.WarnContext(ctx, "invoice for a subscription not yet recorded",
			slog.String("subscription_id", subscriptionID))
		return OutcomeSkipped, nil

	default:
		return OutcomeFailed, fmt.Errorf("record invoice payment for %s: %w", subscriptionID, err)
	}
}

// subscriptionIDOf digs the subscription out of an invoice.
//
// Since the 2025 API versions it hangs off invoice.parent.subscription_details
// rather than invoice.subscription, and the line items are the fallback for
// invoices rendered before that move.
func subscriptionIDOf(invoice *stripesdk.Invoice) string {
	if p := invoice.Parent; p != nil && p.SubscriptionDetails != nil && p.SubscriptionDetails.Subscription != nil {
		if id := p.SubscriptionDetails.Subscription.ID; id != "" {
			return id
		}
	}
	if invoice.Lines != nil {
		for _, line := range invoice.Lines.Data {
			if line != nil && line.Subscription != nil && line.Subscription.ID != "" {
				return line.Subscription.ID
			}
		}
	}
	return ""
}

// declineReason renders something an operator - and eventually a customer - can
// act on. Stripe's decline_code is the useful part; the generic message is the
// fallback.
func declineReason(invoice *stripesdk.Invoice) string {
	err := invoice.LastFinalizationError
	if err == nil {
		return ""
	}
	switch {
	case err.DeclineCode != "":
		return fmt.Sprintf("%s: %s", err.DeclineCode, err.Msg)
	case err.Code != "":
		return fmt.Sprintf("%s: %s", err.Code, err.Msg)
	default:
		return err.Msg
	}
}

// --- mapping -----------------------------------------------------------------

// primaryItem returns the subscription item that carries the price and the
// billing period. Since the 2025 API versions those fields live on the item,
// not on the subscription. Multi-item subscriptions are not modelled: our
// schema stores a single price, so the first item wins and the rest are lost.
func primaryItem(sub *stripesdk.Subscription) (*stripesdk.SubscriptionItem, error) {
	if sub.Items == nil || len(sub.Items.Data) == 0 {
		return nil, fmt.Errorf("subscription %s carries no items", sub.ID)
	}
	return sub.Items.Data[0], nil
}

func subscriptionFrom(userID uuid.UUID, sub *stripesdk.Subscription, event stripesdk.Event) (*domain.Subscription, error) {
	item, err := primaryItem(sub)
	if err != nil {
		return nil, err
	}
	if item.Price == nil {
		return nil, fmt.Errorf("subscription %s item carries no price", sub.ID)
	}

	eventID := event.ID
	eventAt := time.Unix(event.Created, 0).UTC()

	record := &domain.Subscription{
		UserID:               userID,
		StripeSubscriptionID: sub.ID,
		StripeCustomerID:     customerIDOf(sub.Customer),
		StripePriceID:        item.Price.ID,
		Status:               domain.SubscriptionStatus(sub.Status),
		Quantity:             int32(item.Quantity),
		CurrentPeriodStart:   time.Unix(item.CurrentPeriodStart, 0).UTC(),
		CurrentPeriodEnd:     time.Unix(item.CurrentPeriodEnd, 0).UTC(),
		CancelAtPeriodEnd:    sub.CancelAtPeriodEnd,
		CancelAt:             unixPtr(sub.CancelAt),
		CanceledAt:           unixPtr(sub.CanceledAt),
		EndedAt:              unixPtr(sub.EndedAt),
		TrialStart:           unixPtr(sub.TrialStart),
		TrialEnd:             unixPtr(sub.TrialEnd),
		LastStripeEventID:    &eventID,
		LastStripeEventAt:    &eventAt,
		Metadata:             sub.Metadata,
	}
	if record.Quantity <= 0 {
		record.Quantity = 1
	}
	if c := string(item.Price.Currency); c != "" {
		record.Currency = &c
	}
	if item.Price.UnitAmount > 0 {
		amount := item.Price.UnitAmount
		record.UnitAmount = &amount
	}
	if item.Price.Product != nil && item.Price.Product.ID != "" {
		record.StripeProductID = &item.Price.Product.ID
	}

	ensureCanceledAt(record.Status, &record.CanceledAt, eventAt)
	return record, nil
}

func statusUpdateFrom(sub *stripesdk.Subscription, event stripesdk.Event, deleted bool) (repo.SubscriptionStatusUpdate, error) {
	item, err := primaryItem(sub)
	if err != nil {
		return repo.SubscriptionStatusUpdate{}, err
	}

	status := domain.SubscriptionStatus(sub.Status)
	// customer.subscription.deleted is terminal even if the object still reports
	// an earlier status, which happens when a subscription is deleted mid-cycle.
	if deleted {
		status = domain.SubscriptionCanceled
	}
	if !status.Valid() {
		return repo.SubscriptionStatusUpdate{}, fmt.Errorf("subscription %s has unknown status %q", sub.ID, sub.Status)
	}

	eventAt := time.Unix(event.Created, 0).UTC()
	update := repo.SubscriptionStatusUpdate{
		StripeSubscriptionID: sub.ID,
		Status:               status,
		EventID:              event.ID,
		EventCreatedAt:       eventAt,
		CurrentPeriodStart:   unixPtr(item.CurrentPeriodStart),
		CurrentPeriodEnd:     unixPtr(item.CurrentPeriodEnd),
		CancelAtPeriodEnd:    &sub.CancelAtPeriodEnd,
		CancelAt:             unixPtr(sub.CancelAt),
		CanceledAt:           unixPtr(sub.CanceledAt),
		EndedAt:              unixPtr(sub.EndedAt),
		TrialEnd:             unixPtr(sub.TrialEnd),
	}
	if item.Price != nil && item.Price.ID != "" {
		update.StripePriceID = &item.Price.ID
	}
	if item.Quantity > 0 {
		q := int32(item.Quantity)
		update.Quantity = &q
	}
	if sub.LatestInvoice != nil && sub.LatestInvoice.ID != "" {
		update.LatestInvoiceID = &sub.LatestInvoice.ID
	}
	if sub.DefaultPaymentMethod != nil && sub.DefaultPaymentMethod.ID != "" {
		update.DefaultPaymentMethodID = &sub.DefaultPaymentMethod.ID
	}

	ensureCanceledAt(status, &update.CanceledAt, eventAt)
	return update, nil
}

// ensureCanceledAt satisfies subscriptions_canceled_at_chk, which requires a
// timestamp whenever the status is canceled. Stripe usually supplies one, but
// not on a subscription deleted before its first invoice, and the write would
// otherwise be rejected by the database.
func ensureCanceledAt(status domain.SubscriptionStatus, at **time.Time, fallback time.Time) {
	if status == domain.SubscriptionCanceled && *at == nil {
		*at = &fallback
	}
}

func customerIDOf(c *stripesdk.Customer) string {
	if c == nil {
		return ""
	}
	return c.ID
}

func unixPtr(sec int64) *time.Time {
	if sec <= 0 {
		return nil
	}
	t := time.Unix(sec, 0).UTC()
	return &t
}
