# Architecture

## The dependency rule

Dependencies point **inward only**. `domain` imports nothing from this project.
Everything else may import `domain`; nothing in `domain` may import anything else.

```
        handler/http ──┐
                       ├──> service ──> domain
   repository/postgres ┘        ▲
        adapter/stripe ─────────┘
        (service depends on port interfaces; adapters satisfy them)
```

Concretely:

| Layer | Package | Knows about | Never imports |
|---|---|---|---|
| Domain | `internal/domain/**` | Go stdlib only | pgx, stripe-go, net/http |
| Application | `internal/service/**` | domain (entities + port interfaces) | pgx, stripe-go, net/http |
| Adapters (driven) | `internal/repository/postgres`, `internal/adapter/stripe` | domain, pgx / stripe-go | handler, service internals |
| Infrastructure | `internal/config`, `internal/logger`, `internal/database` | stdlib, pgx | domain, service, handler |
| Adapters (driving) | `internal/handler/http` | domain, service | pgx, stripe-go |
| Composition | `cmd/api` | everything — the only place wiring happens | — |

**Ports are declared as interfaces, and the service layer depends only on those.**
`repository/postgres` declares `UserRepository`, `SubscriptionRepository` and
`WebhookRepository` alongside the concrete types that satisfy them, each pinned
with a `var _ Port = (*Impl)(nil)` assertion. The service layer accepts the
interface, which is what lets it be unit-tested with no database and no Stripe
account.

Textbook hexagonal would put those interfaces in `internal/domain`. They sit with
the adapter here for one concrete reason: `SubscriptionStatusUpdate` is a Stripe
event-shaped parameter struct — every field below `Status` is an optional pointer
because Stripe events vary in which fields they populate — and moving it into the
domain would drag Stripe's wire semantics into the layer that is supposed to be
free of them. The dependency rule is unaffected: `domain` still imports nothing
from `repository`, and the arrow still points inward.

## Directory structure

```
stripe-payment-service/
├── cmd/
│   └── api/                        # composition root: config → pool → services → router
│       └── main.go
│
├── internal/
│   ├── domain/                     # ── pure business core, zero I/O ──
│   │   └── models.go               #    User, Subscription, ProcessedWebhook,
│   │                               #    status value objects, sentinel errors
│   │
│   ├── repository/
│   │   └── postgres/               # ── pgx/v5 adapters + the ports they satisfy ──
│   │       ├── errors.go           #    SQLSTATE → domain sentinel translation
│   │       ├── user_repo.go
│   │       ├── subscription_repo.go
│   │       └── webhook_repo.go     #    claim / settle idempotency
│   │
│   ├── stripe/                     # ── the only package importing stripe-go ──
│   │   └── client.go               #    checkout, subscription reads, signature
│   │                               #    verification, error classification
│   │
│   ├── service/                    # ── use cases over domain + ports ──
│   │   ├── webhook_service.go      #    verify → claim → dispatch → settle
│   │   └── checkout_service.go     #    price allowlist, customer reuse
│   │
│   ├── handler/
│   │   └── stripe_handler.go       # POST /webhook, POST /api/v1/checkout
│   │
│   ├── config/                     # env → typed Config, validated once at boot
│   ├── logger/                     # slog setup, request-id correlation, redaction
│   └── database/                   # pgxpool construction, health, query tracing
│
├── migrations/                     # Goose SQL migrations (source of truth for schema)
├── pkg/                            # genuinely reusable, import-safe by third parties
│   └── money/
├── deployments/docker/             # Dockerfile (multi-target)
├── scripts/                        # operational one-offs
├── test/
│   ├── integration/                # real Postgres, `integration` build tag
│   └── fixtures/                   # golden Stripe event payloads
└── docs/
```

`internal/` is enforced by the compiler: nothing outside this module can import it.
Only `pkg/` is part of the public surface.

## Webhook processing invariant

Stripe delivers **at least once, unordered**, retrying for up to 3 days. Two
mechanisms keep the local read-model correct:

1. **Dedupe** — `processed_webhooks.event_id` is the primary key, so the unique
   index itself performs the mutual exclusion: exactly one caller can create the
   row. `TryClaimEvent` is an `INSERT ... ON CONFLICT DO UPDATE ... WHERE`, and
   the `WHERE` decides who may retry — a `failed` row is reclaimable, a
   `processing` row only once its claim has gone stale, and a `succeeded` row
   never. No advisory locks, no application-side coordination.
2. **Ordering** — `subscriptions.last_stripe_event_at` holds the `event.created` of
   the newest event applied. An arriving event older than that is acknowledged and
   discarded, so a retry of a stale `customer.subscription.updated` cannot resurrect
   a canceled subscription.

**The protocol is claim → dispatch → settle, not one transaction.** Holding a
single transaction across the business write would remove the need for the
`processing` status, `attempts`, and the stale-claim window — but it would also
mean a worker that dies mid-event leaves nothing behind to diagnose or retry
distinctly from a first delivery. Instead: `TryClaimEvent` commits the claim,
the handler does its work, and `MarkEventProcessed` / `MarkEventFailed` settle
it. Both settle calls carry `AND status = 'processing'`, so a worker that
resumes after its claim was reclaimed gets `ErrEventNotClaimed` rather than
overwriting the outcome recorded by whoever took over.

A `false` from `TryClaimEvent` is not an error — the event is finished or in
flight elsewhere, and it must still be acknowledged to Stripe with a 2xx.
Returning non-2xx there would make Stripe redeliver an event that needs no work.

**Signature verification runs before the claim, and this ordering is a security
property rather than a preference.** `event_id`, `event_type` and `created` all
come from the request body, and until the signature is checked that body is
unauthenticated input from an anonymous caller. Claiming first would let anyone
POST `{"id":"evt_..."}` for an event id they guessed or observed, insert a
settled row, and cause Stripe's genuine delivery to be discarded as a duplicate —
a silent, unauthenticated denial of service against billing state.

This is verified, not asserted: `TestWebhook_ForgedPayloadNeverReachesLedger`
fails when the ordering is reversed, and in that configuration a forged request
is answered `200` because the row it planted made the next delivery look like a
duplicate.

The webhook body is read raw and whole — the signature covers the exact bytes
Stripe sent, so decoding and re-encoding the JSON invalidates it. That means the
payload is fully in memory before it can be trusted, and `http.MaxBytesReader`
(256 KiB) is the only thing between an anonymous POST and the heap.

The out-of-order guard in `UpdateSubscriptionStatus` reads
`last_stripe_event_at`, decides, then writes. Under READ COMMITTED that sequence
is atomic only because the row is held with `SELECT ... FOR UPDATE`; performing
the comparison inside the `UPDATE`'s `WHERE` instead lets two workers both
observe the same pre-state, and the last committer wins regardless of event
order. `TestSubscriptionRepo_ConcurrentOutOfOrderEventsConverge` races 24
shuffled events over 8 rounds and fails every round when the lock is removed.

Equal timestamps are **applied**, not rejected: Stripe reports `event.created` at
one-second resolution, so two genuinely distinct events for one subscription
routinely share a timestamp, and rejecting equality would silently drop the
second.


## Query contracts the repository layer must honour

Partial indices are the reason the hot paths stay fast at scale, but two of them
only work if the query carries the predicate. Measured against 50k users /
50k subscriptions on PostgreSQL 16:

| Query | Required predicate | With | Without |
|---|---|---|---|
| user by email | `AND deleted_at IS NULL` | 0.037 ms (index scan) | 20.96 ms (seq scan) |
| user by `stripe_customer_id` | none — planner proves `= 'x'` ⇒ `IS NOT NULL` | 0.016 ms | n/a |
| live subscription for user | `AND status IN ('trialing','active','past_due')` | index-only scan | seq scan |

The live-subscription partial index is **56 kB** where the equivalent full index
is **2.4 MB** — a 44x difference that grows with every churned customer, because
canceled rows are never indexed at all.

`scripts/verify-schema.sql` (`make verify-schema`) is the executable form of this
contract: 22 assertions covering every constraint, both trigger behaviours, and
the webhook idempotency claim. It runs inside a transaction that rolls back, so it
is safe against any migrated database.

`test/integration` (`make test-integration`) is the executable form of the
repository and webhook contracts: constraint-to-sentinel translation, trigger
side effects, the stale-claim window, the two concurrency guarantees above, and
the full HTTP webhook path — run under `-race` against a live PostgreSQL 16.

## Why `clock_timestamp()` in the updated_at trigger

`now()` is frozen at transaction start. A row inserted and then updated in the
same transaction would report `updated_at = created_at`, and a row written by a
long transaction would carry a timestamp *older* than rows committed by shorter
transactions that started later — which silently breaks "everything changed since
T" incremental sync and makes the audit trail lie. `clock_timestamp()` reads the
wall clock at the moment the row is written.


## Startup and shutdown

Construction happens once, in `run()`, and every dependency is passed as an
argument. `slog.SetDefault` is deliberately not called: a package that logs
through the default logger should show up as a missing wire, not be silently
absorbed.

Shutdown order is load-bearing:

```
SIGTERM/SIGINT
  -> stop trapping signals   (a second Ctrl-C now kills immediately)
  -> srv.Shutdown(timeout)   (stop accepting; drain in-flight requests)
  -> db.Close()              (blocks until every connection is returned)
```

Closing the pool first would tear connections out from under requests that are
still finishing, potentially mid-transaction. The signal context is **not** used
as the server's `BaseContext` for the same reason — that would cancel every
in-flight request the instant the signal arrives and defeat the drain entirely.

## Production safety checks at boot

`config.Validate` refuses to start on any of these, reporting all violations at
once rather than one per restart:

- a **live** Stripe key when `APP_ENV` is not production — a live key on a
  developer's laptop moves real money against real customers
- a **test** Stripe key when `APP_ENV` is production — silently accepts payments
  that never settle
- `sslmode=disable` in production
- `DB_MIN_CONNS` above `DB_MAX_CONNS`, an out-of-range port, an unknown log level

Secrets use the `config.Secret` type, which redacts itself through `fmt`, `slog`
and `encoding/json`. The boot log prints the whole effective configuration with
every secret replaced, so operators can confirm what loaded without the
configuration becoming the leak.
