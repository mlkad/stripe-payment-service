// Package domain holds the core entities and the errors that describe what can
// go wrong with them. It imports nothing from the rest of the project and
// performs no I/O.
package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sentinel errors. Repositories translate driver and constraint failures into
// these so that callers can branch on errors.Is without importing pgx.
var (
	ErrNotFound   = errors.New("resource not found")
	ErrConflict   = errors.New("resource conflict")
	ErrValidation = errors.New("validation failed")

	// ErrStaleEvent means the subscription exists but the incoming Stripe event
	// is older than the one already applied to it. Stripe delivers unordered, so
	// this is an expected outcome, not a fault: acknowledge and discard.
	ErrStaleEvent = errors.New("stale stripe event")

	// ErrEventNotClaimed means a settle call found no row in the processing
	// state - the event was already settled, or another worker holds the claim.
	ErrEventNotClaimed = errors.New("webhook event is not claimed for processing")
)

// FieldError attributes a validation failure to a specific field.
type FieldError struct {
	Field  string
	Detail string
}

func (e FieldError) Error() string { return e.Field + ": " + e.Detail }
func (e FieldError) Unwrap() error { return ErrValidation }

// FieldErrors aggregates every validation failure on one entity so a caller
// fixes all of them in one pass.
type FieldErrors []FieldError

func (e FieldErrors) Error() string {
	parts := make([]string, len(e))
	for i, fe := range e {
		parts[i] = fe.Error()
	}
	return strings.Join(parts, "; ")
}

func (e FieldErrors) Unwrap() error { return ErrValidation }

type validator struct{ errs FieldErrors }

func (v *validator) add(field, detail string) {
	v.errs = append(v.errs, FieldError{Field: field, Detail: detail})
}

func (v *validator) result() error {
	if len(v.errs) == 0 {
		return nil
	}
	return v.errs
}

// SubscriptionStatus mirrors Stripe's subscription.status and the
// subscription_status enum in migration 00002. The two must not drift.
type SubscriptionStatus string

const (
	SubscriptionIncomplete        SubscriptionStatus = "incomplete"
	SubscriptionIncompleteExpired SubscriptionStatus = "incomplete_expired"
	SubscriptionTrialing          SubscriptionStatus = "trialing"
	SubscriptionActive            SubscriptionStatus = "active"
	SubscriptionPastDue           SubscriptionStatus = "past_due"
	SubscriptionCanceled          SubscriptionStatus = "canceled"
	SubscriptionUnpaid            SubscriptionStatus = "unpaid"
	SubscriptionPaused            SubscriptionStatus = "paused"
)

func (s SubscriptionStatus) Valid() bool {
	switch s {
	case SubscriptionIncomplete, SubscriptionIncompleteExpired, SubscriptionTrialing,
		SubscriptionActive, SubscriptionPastDue, SubscriptionCanceled,
		SubscriptionUnpaid, SubscriptionPaused:
		return true
	}
	return false
}

// IsLive reports whether the subscription should grant access. The three
// statuses here are exactly the predicate of idx_subscriptions_user_id_live and
// idx_subscriptions_current_period_end_live; changing this set without changing
// those indices silently drops entitlement queries onto a sequential scan.
//
// past_due is included deliberately: Stripe is still retrying payment, and
// revoking access before dunning completes churns customers who would have paid.
func (s SubscriptionStatus) IsLive() bool {
	switch s {
	case SubscriptionTrialing, SubscriptionActive, SubscriptionPastDue:
		return true
	}
	return false
}

// IsTerminal reports whether Stripe will send no further events for this
// subscription.
func (s SubscriptionStatus) IsTerminal() bool {
	switch s {
	case SubscriptionCanceled, SubscriptionIncompleteExpired:
		return true
	}
	return false
}

// WebhookStatus mirrors the webhook_status enum in migration 00003.
type WebhookStatus string

const (
	WebhookProcessing WebhookStatus = "processing"
	WebhookSucceeded  WebhookStatus = "succeeded"
	WebhookFailed     WebhookStatus = "failed"
	WebhookSkipped    WebhookStatus = "skipped"
)

func (s WebhookStatus) Valid() bool {
	switch s {
	case WebhookProcessing, WebhookSucceeded, WebhookFailed, WebhookSkipped:
		return true
	}
	return false
}

// User is an application identity and its 1:1 mapping to a Stripe Customer.
type User struct {
	ID              uuid.UUID  `json:"id"`
	Email           string     `json:"email" validate:"required,email,max=320"`
	EmailVerifiedAt *time.Time `json:"email_verified_at,omitempty"`

	// PasswordHash is never serialised. The json:"-" tag is the last line of
	// defence if a handler ever returns the entity instead of a DTO.
	PasswordHash *string `json:"-"`

	FullName         *string `json:"full_name,omitempty" validate:"omitempty,max=255"`
	StripeCustomerID *string `json:"stripe_customer_id,omitempty" validate:"omitempty,startswith=cus_"`

	Metadata  map[string]string `json:"metadata,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	DeletedAt *time.Time        `json:"-"`
}

// Validate enforces the same invariants as the CHECK constraints on the users
// table, so a bad entity fails before it costs a database round trip. The
// database remains the authority; this is a fast path, not a replacement.
func (u *User) Validate() error {
	var v validator

	email := strings.TrimSpace(u.Email)
	switch {
	case email == "":
		v.add("email", "is required")
	case len(email) < 3 || len(email) > 320:
		v.add("email", "must be between 3 and 320 characters")
	case !strings.Contains(email, "@"), strings.ContainsAny(email, " \t\n"):
		v.add("email", "is not a valid address")
	}

	if u.StripeCustomerID != nil && !strings.HasPrefix(*u.StripeCustomerID, "cus_") {
		v.add("stripe_customer_id", "must begin with cus_")
	}
	for k := range u.Metadata {
		if strings.TrimSpace(k) == "" {
			v.add("metadata", "keys must not be blank")
			break
		}
	}
	return v.result()
}

// IsDeleted reports whether the user has been soft-deleted.
func (u *User) IsDeleted() bool { return u.DeletedAt != nil }

// Subscription is the local projection of a Stripe Subscription.
type Subscription struct {
	ID     uuid.UUID `json:"id"`
	UserID uuid.UUID `json:"user_id" validate:"required"`

	StripeSubscriptionID string  `json:"stripe_subscription_id" validate:"required,startswith=sub_"`
	StripeCustomerID     string  `json:"stripe_customer_id" validate:"required,startswith=cus_"`
	StripePriceID        string  `json:"stripe_price_id" validate:"required,startswith=price_"`
	StripeProductID      *string `json:"stripe_product_id,omitempty"`

	Status     SubscriptionStatus `json:"status" validate:"required"`
	Quantity   int32              `json:"quantity" validate:"required,gt=0"`
	Currency   *string            `json:"currency,omitempty" validate:"omitempty,len=3,lowercase"`
	UnitAmount *int64             `json:"unit_amount,omitempty" validate:"omitempty,gte=0"`

	CurrentPeriodStart time.Time `json:"current_period_start" validate:"required"`
	CurrentPeriodEnd   time.Time `json:"current_period_end" validate:"required,gtfield=CurrentPeriodStart"`

	CancelAtPeriodEnd bool       `json:"cancel_at_period_end"`
	CancelAt          *time.Time `json:"cancel_at,omitempty"`
	CanceledAt        *time.Time `json:"canceled_at,omitempty"`
	EndedAt           *time.Time `json:"ended_at,omitempty"`

	TrialStart *time.Time `json:"trial_start,omitempty"`
	TrialEnd   *time.Time `json:"trial_end,omitempty"`

	LatestInvoiceID        *string `json:"latest_invoice_id,omitempty"`
	DefaultPaymentMethodID *string `json:"default_payment_method_id,omitempty"`

	// Dunning state, maintained by the invoice.* handlers. PaymentFailedAt is
	// the flag: nil means no outstanding failure.
	PaymentFailedAt      *time.Time `json:"payment_failed_at,omitempty"`
	PaymentFailureCount  int32      `json:"payment_failure_count"`
	LastPaymentError     *string    `json:"last_payment_error,omitempty"`
	NextPaymentAttemptAt *time.Time `json:"next_payment_attempt_at,omitempty"`

	// LastInvoiceEventID and LastInvoiceEventAt are the invoice stream's own
	// ordering cursor, deliberately separate from LastStripeEventAt. The two
	// streams interleave, and sharing one cursor would let an invoice event
	// reject the customer.subscription.* event that carries the real status.
	LastInvoiceEventID *string    `json:"-"`
	LastInvoiceEventAt *time.Time `json:"-"`

	// LastStripeEventID and LastStripeEventAt record the newest event applied to
	// this row. See UpdateSubscriptionStatus for how they gate out-of-order
	// delivery.
	LastStripeEventID *string    `json:"-"`
	LastStripeEventAt *time.Time `json:"-"`

	Metadata  map[string]string `json:"metadata,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

func (s *Subscription) Validate() error {
	var v validator

	if s.UserID == uuid.Nil {
		v.add("user_id", "is required")
	}
	if !strings.HasPrefix(s.StripeSubscriptionID, "sub_") {
		v.add("stripe_subscription_id", "must begin with sub_")
	}
	if !strings.HasPrefix(s.StripeCustomerID, "cus_") {
		v.add("stripe_customer_id", "must begin with cus_")
	}
	if !strings.HasPrefix(s.StripePriceID, "price_") {
		v.add("stripe_price_id", "must begin with price_")
	}
	if !s.Status.Valid() {
		v.add("status", fmt.Sprintf("%q is not a known subscription status", s.Status))
	}
	if s.Quantity <= 0 {
		v.add("quantity", "must be greater than zero")
	}
	if s.Currency != nil {
		c := *s.Currency
		if len(c) != 3 || c != strings.ToLower(c) {
			v.add("currency", "must be a three-letter lowercase ISO 4217 code")
		}
	}
	if s.UnitAmount != nil && *s.UnitAmount < 0 {
		v.add("unit_amount", "must not be negative")
	}
	if !s.CurrentPeriodEnd.After(s.CurrentPeriodStart) {
		v.add("current_period_end", "must be after current_period_start")
	}
	if s.TrialStart != nil && s.TrialEnd != nil && !s.TrialEnd.After(*s.TrialStart) {
		v.add("trial_end", "must be after trial_start")
	}
	// Mirrors subscriptions_canceled_at_chk.
	if s.Status == SubscriptionCanceled && s.CanceledAt == nil {
		v.add("canceled_at", "is required when status is canceled")
	}
	// Mirrors subscriptions_dunning_consistency_chk: the flag and the counter
	// must agree, or a handler updated one and forgot the other.
	if s.PaymentFailureCount < 0 {
		v.add("payment_failure_count", "must not be negative")
	}
	if (s.PaymentFailedAt == nil) != (s.PaymentFailureCount == 0) {
		v.add("payment_failed_at", "must be set exactly when payment_failure_count is greater than zero")
	}
	return v.result()
}

// IsLive reports whether this subscription currently grants access.
func (s *Subscription) IsLive() bool { return s.Status.IsLive() }

// InDunning reports whether a payment has failed and not yet been recovered.
//
// Independent of IsLive: a past_due subscription is both live and in dunning,
// which is the whole point - access continues while Stripe retries, and the
// customer is told rather than locked out.
func (s *Subscription) InDunning() bool { return s.PaymentFailedAt != nil }

// ProcessedWebhook is one row of the idempotency ledger.
type ProcessedWebhook struct {
	EventID    string  `json:"event_id" validate:"required,startswith=evt_"`
	EventType  string  `json:"event_type" validate:"required"`
	APIVersion *string `json:"api_version,omitempty"`
	Livemode   bool    `json:"livemode"`
	RequestID  *string `json:"request_id,omitempty"`

	Status    WebhookStatus `json:"status"`
	Attempts  int32         `json:"attempts"`
	LastError *string       `json:"last_error,omitempty"`

	// Payload is the raw event body, retained for replay and audit. It may
	// contain PII and is subject to the retention policy in migration 00003.
	Payload json.RawMessage `json:"-"`

	// StripeCreatedAt is event.created, the ordering key for staleness checks.
	// Stripe reports it at one-second granularity.
	StripeCreatedAt time.Time  `json:"stripe_created_at"`
	ReceivedAt      time.Time  `json:"received_at"`
	ProcessedAt     *time.Time `json:"processed_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (w *ProcessedWebhook) Validate() error {
	var v validator

	if !strings.HasPrefix(w.EventID, "evt_") {
		v.add("event_id", "must begin with evt_")
	}
	if strings.TrimSpace(w.EventType) == "" {
		v.add("event_type", "is required")
	}
	if w.StripeCreatedAt.IsZero() {
		v.add("stripe_created_at", "is required")
	}
	if w.Status != "" && !w.Status.Valid() {
		v.add("status", fmt.Sprintf("%q is not a known webhook status", w.Status))
	}
	if len(w.Payload) > 0 && !json.Valid(w.Payload) {
		v.add("payload", "must be valid JSON")
	}
	return v.result()
}

// IsSettled reports whether the event reached a terminal state.
func (w *ProcessedWebhook) IsSettled() bool {
	return w.Status == WebhookSucceeded || w.Status == WebhookSkipped
}
