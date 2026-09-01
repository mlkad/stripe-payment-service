# Architecture

## The dependency rule

Dependencies point **inward only**. `domain` imports nothing from this project.
Everything else may import `domain`; nothing in `domain` may import anything else.

```
        handler/http ──┐
                       ├──> service ──> domain <── (ports defined here)
   repository/postgres ┘        ▲
        adapter/stripe ─────────┘
                (adapters implement domain ports)
```

Concretely:

| Layer | Package | Knows about | Never imports |
|---|---|---|---|
| Domain | `internal/domain/**` | Go stdlib only | pgx, stripe-go, net/http |
| Application | `internal/service/**` | domain (entities + port interfaces) | pgx, stripe-go, net/http |
| Adapters (driven) | `internal/repository/postgres`, `internal/adapter/stripe` | domain, pgx / stripe-go | handler, service internals |
| Adapters (driving) | `internal/handler/http` | domain, service | pgx, stripe-go |
| Composition | `cmd/api` | everything — the only place wiring happens | — |

**Ports live with the domain, not with the implementation.** `domain/subscription`
declares `Repository`; `repository/postgres` implements it. That inversion is what
lets the service layer be unit-tested with no database and no Stripe account.

## Directory structure

```
stripe-payment-service/
├── cmd/
│   └── api/                        # composition root: config → pool → services → router
│       └── main.go
│
├── internal/
│   ├── config/                     # env → typed Config, validated once at boot
│   │
│   ├── domain/                     # ── pure business core, zero I/O ──
│   │   ├── shared/                 #    sentinel errors, Clock, ID types
│   │   ├── user/                   #    User entity + Repository port
│   │   ├── subscription/           #    Subscription entity, Status VO,
│   │   │                           #    state-transition rules, Repository port
│   │   └── webhook/                #    Event value object, EventType,
│   │                               #    IdempotencyStore port
│   │
│   ├── service/                    # ── use cases; orchestrate domain + ports ──
│   │   ├── billing/                #    CreateCheckoutSession, CreatePortalSession
│   │   └── webhook/                #    ProcessEvent: claim → dispatch → settle
│   │
│   ├── repository/
│   │   └── postgres/               # pgx/v5 implementations of the domain ports
│   │
│   ├── adapter/
│   │   └── stripe/                 # stripe-go client behind a domain-owned interface
│   │
│   ├── handler/
│   │   └── http/                   # routing, decode/encode, status mapping
│   │       ├── dto/                #    wire types — never leak domain structs
│   │       └── middleware/         #    request id, structured logging, recovery,
│   │                               #    rate limit, signature verification
│   │
│   └── platform/                   # ── framework glue, no business logic ──
│       ├── postgres/               #    pgxpool construction, health, tx helper
│       ├── logger/                 #    slog setup, redaction of secrets/PII
│       ├── httpserver/             #    server + graceful shutdown
│       └── validator/              #    request validation
│
├── migrations/                     # Goose SQL migrations (source of truth for schema)
├── pkg/                            # genuinely reusable, import-safe by third parties
│   └── money/
├── deployments/docker/             # Dockerfile (multi-target)
├── scripts/                        # operational one-offs
├── test/
│   ├── integration/                # testcontainers: real Postgres, build-tagged
│   └── fixtures/                   # golden Stripe event payloads
└── docs/
```

`internal/` is enforced by the compiler: nothing outside this module can import it.
Only `pkg/` is part of the public surface.

## Webhook processing invariant

Stripe delivers **at least once, unordered**, retrying for up to 3 days. Two
mechanisms keep the local read-model correct:

1. **Dedupe** — `processed_webhooks.event_id` is the primary key. A handler claims
   the row (`INSERT ... ON CONFLICT`) inside the same transaction as the state
   change, so a duplicate delivery is a no-op even across pods.
2. **Ordering** — `subscriptions.last_stripe_event_at` holds the `event.created` of
   the newest event applied. An arriving event older than that is acknowledged and
   discarded, so a retry of a stale `customer.subscription.updated` cannot resurrect
   a canceled subscription.

Both the claim and the business write happen in one transaction. Commit means
"processed"; rollback means Stripe retries. There is no window where one happened
without the other.


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

## Why `clock_timestamp()` in the updated_at trigger

`now()` is frozen at transaction start. A row inserted and then updated in the
same transaction would report `updated_at = created_at`, and a row written by a
long transaction would carry a timestamp *older* than rows committed by shorter
transactions that started later — which silently breaks "everything changed since
T" incremental sync and makes the audit trail lie. `clock_timestamp()` reads the
wall clock at the moment the row is written.
