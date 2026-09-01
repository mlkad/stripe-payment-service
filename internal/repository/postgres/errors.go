// Package postgres implements the repository ports on top of pgxpool.
//
// Every exported method translates driver and constraint failures into the
// sentinel errors in internal/domain, so that no caller has to import pgx to
// understand what went wrong.
package postgres

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mlkad/stripe-payment-service/internal/domain"
)

// PostgreSQL SQLSTATE codes worth distinguishing.
const (
	codeUniqueViolation     = "23505"
	codeForeignKeyViolation = "23503"
	codeCheckViolation      = "23514"
	codeNotNullViolation    = "23502"
	codeInvalidTextRepr     = "22P02"
	codeStringDataTruncated = "22001"
)

// constraintMessages maps a constraint name to an explanation aimed at whoever
// has to fix the call site. Without this the caller sees only the constraint
// name, which says what failed but never why it matters.
var constraintMessages = map[string]string{
	"uq_users_email_active":                           "a live user already exists with this email",
	"uq_users_stripe_customer_id":                     "this Stripe customer is already linked to another user",
	"subscriptions_stripe_subscription_id_key":        "this Stripe subscription has already been recorded",
	"subscriptions_user_id_fkey":                      "the referenced user does not exist, or still has billing history",
	"processed_webhooks_pkey":                         "this Stripe event has already been recorded",
	"users_email_format_chk":                          "email is not a valid address",
	"users_stripe_customer_id_format_chk":             "stripe_customer_id must look like cus_...",
	"subscriptions_period_order_chk":                  "current_period_end must be after current_period_start",
	"subscriptions_quantity_positive_chk":             "quantity must be greater than zero",
	"subscriptions_canceled_at_chk":                   "canceled_at is required when status is canceled",
	"subscriptions_currency_format_chk":               "currency must be a three-letter lowercase ISO 4217 code",
	"processed_webhooks_event_id_format_chk":          "event_id must look like evt_...",
	"processed_webhooks_processed_at_chk":             "processed_at must be set for a settled event and unset otherwise",
	"processed_webhooks_last_error_chk":               "last_error is required when status is failed",
	"subscriptions_stripe_subscription_id_format_chk": "stripe_subscription_id must look like sub_...",
}

// mapError converts a driver error into a domain sentinel, preserving the
// original through the wrap chain for logging.
func mapError(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s: %w", op, domain.ErrNotFound)
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fmt.Errorf("%s: %w", op, err)
	}

	detail := constraintMessages[pgErr.ConstraintName]
	if detail == "" {
		detail = pgErr.Message
	}

	switch pgErr.Code {
	case codeUniqueViolation:
		return fmt.Errorf("%s: %s: %w", op, detail, domain.ErrConflict)
	case codeForeignKeyViolation:
		return fmt.Errorf("%s: %s: %w", op, detail, domain.ErrConflict)
	case codeCheckViolation, codeNotNullViolation, codeInvalidTextRepr, codeStringDataTruncated:
		return fmt.Errorf("%s: %s: %w", op, detail, domain.ErrValidation)
	default:
		return fmt.Errorf("%s: %w", op, err)
	}
}
