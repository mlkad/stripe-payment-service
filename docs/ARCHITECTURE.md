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
│   ├── auth/                       # ── credential primitives, zero I/O ──
│   │   ├── password.go             #    bcrypt policy, hashing, decoy compare
│   │   └── token.go                #    JWT issue/parse, pinned algorithm
│   │
│   ├── stripe/                     # ── the only package importing stripe-go ──
│   │   └── client.go               #    checkout, subscription reads, signature
│   │                               #    verification, error classification
│   │
│   ├── service/                    # ── use cases over domain + ports ──
│   │   ├── webhook_service.go      #    verify → claim → dispatch → settle
│   │   └── checkout_service.go     #    price allowlist, customer reuse
│   │
│   ├── handler/                    # ── HTTP adapter: decode, delegate, encode ──
│   │   ├── router.go               #    chi routing + middleware chain
│   │   ├── stripe_handler.go       #    POST /webhook, POST /api/v1/checkout
│   │   ├── health.go               #    GET /livez, GET /healthz
│   │   ├── response.go             #    one error envelope, one decoder
│   │   └── middleware/             #    request id, access log, recovery, timeout
│   │
│   ├── config/                     # env → typed Config, validated once at boot
│   ├── logger/                     # slog setup, request-id correlation, redaction
│   └── database/                   # pgxpool construction, health, query tracing
│
├── web/                            # ── React 19 + Vite 8 + Tailwind 4 UI ──
│   └── src/
│       ├── api/                    #    typed client; mirrors the Go read models
│       ├── components/             #    PricingTable, CheckoutModal, Dashboard
│       ├── hooks/                  #    useSubscription: load + poll after checkout
│       └── lib/                    #    stripe.js loader, formatting
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


## Middleware ordering

The global chain is applied outermost first, and two positions are load-bearing
rather than stylistic:

```
RequestID  →  AccessLog  →  Recoverer  →  [ Timeout ]  →  handler
```

- **RequestID is outermost** so every record produced below it — including a
  panic report — carries the correlation id. An inbound `X-Request-Id` is
  honoured so a trace survives a proxy hop, but only after a length and
  character check: the value is echoed into a response header and into every log
  line for the request, so a caller-supplied newline would let it forge log
  records.

- **AccessLog sits outside Recoverer.** It reads the status *after* the inner
  handler returns. Reversed, a panic unwinds through AccessLog before anything
  has been written, and the request is recorded as a 200 — or not recorded at
  all, since the log call is never reached. With Recoverer inside, the recovered
  500 is already written by the time AccessLog looks.

  This is verified, not asserted. `TestAccessLogOutsideRecovererRecordsThePanicStatus`
  proves the correct order reports 500, and
  `TestRecovererOutsideAccessLogMisreportsThePanic` is its negative control: it
  fails if the reversed order ever starts reporting correctly, which would mean
  the constraint no longer holds and this section is stale.

- **Timeout is applied per route group, not globally.** The health probes carry
  their own short deadline and must stay answerable when everything else is
  saturated. The webhook deadline is the longer of the two, because
  `checkout.session.completed` makes an outbound call to Stripe before it writes
  anything.

`Timeout` cancels the request context rather than racing the handler from a
watchdog goroutine the way `http.TimeoutHandler` does. That type buffers the
entire response so it can discard it and substitute its own, which would defeat
`http.MaxBytesReader` on the webhook route and make every response allocate
twice. Cancelling the context instead reaches the places that actually block —
pgx queries and the Stripe HTTP client both honour it — and no two goroutines
ever hold the `ResponseWriter` at once. The 503 is therefore written only after
the handler returns, and only if it returned without writing.

That leaves one gap by construction: a handler that ignores its context cannot
be interrupted this way. `http.Server.WriteTimeout` is the backstop, which is
why config refuses to start when a request deadline is longer than it —
otherwise the connection is torn down before the middleware can answer.

## Authentication

Every route under `/api/v1` except `auth/register` and `auth/login` requires a
bearer token. `RequireAuth` is applied to a chi group rather than per route, so
a new endpoint is protected by default rather than by remembering.

**No handler accepts an identity from the client.** The subject comes from the
verified token via `middleware.UserIDFromContext`. Both fields that used to
carry it are gone: `checkoutRequest` has no `user_id`, and the subscription
route has no `user_id` parameter. Because the decoder sets
`DisallowUnknownFields`, a stale client still sending one now gets a `400`
rather than being silently ignored — the failure is loud on the client side and
harmless on the server side.

The context key is unexported, so nothing outside `middleware` can write an
authenticated subject. A handler on a route that was never wrapped therefore
gets an error rather than a zero uuid it might mistake for a user, and answers
**500, not 401**. A 401 there would be indistinguishable from a genuine missing
token, which would make an endpoint accidentally mounted outside `RequireAuth`
look protected while enforcing nothing.

### Login does not disclose whether an account exists

Both "no such user" and "wrong password" return the same status and the same
body, and both spend a full bcrypt comparison — the unknown-account path
compares against a decoy digest rather than returning early.

The decoy is generated at the configured cost, not hardcoded. That detail is
load-bearing: an earlier version used a fixed cost-12 digest, and against
cost-10 stored hashes it produced **294ms for an unknown account versus 65ms for
a wrong password** — a louder oracle than having no decoy at all. Measured live,
not reasoned about. After the fix both paths sit at ~65ms.

### Password policy

`MaxPasswordBytes` is 72 because bcrypt silently truncates there. Accepting
longer input would mean two passwords sharing a 72-byte prefix authenticate each
other, with no way for the user to know their tail was discarded. The limit is
measured in bytes, not runes: 40 accented characters exceed it.

Cost upgrades ride along on successful login, which is the only moment the
plaintext is available.

### Token handling

HS256, with the issuer and audience verified on every parse so a token minted
for staging cannot be replayed against production. `WithValidMethods` pins the
algorithm as defence in depth — jwt/v5 refuses `alg:none` on its own, and the
keyfunc always returns `[]byte`, so an RS256 header fails on key type before any
signature check. The allowlist is what keeps that true if the keyfunc ever
returns more than one key type.

Every parse failure except expiry collapses to one error. A caller has no
legitimate use for the distinction between a bad signature, a wrong audience and
a malformed segment, and reporting it tells a forger which part to fix next.

**Tokens are stateless and cannot be revoked before they expire.** That is the
reason the TTL is short rather than a reason to be relaxed about it, and config
refuses a TTL over 24h in production. Adding revocation means a store keyed on
the `jti` claim, which is already minted for that purpose.

### What this step does not do

- No refresh tokens. The frontend holds the access token in `localStorage`,
  where any script on the origin can read it; a single XSS is a stolen
  credential. The upgrade is a refresh token in an httpOnly cookie with the
  access token held in memory only.
- No rate limiting on `login` or `register`. bcrypt at cost 12 makes online
  guessing slow, but it makes the endpoint a cheap way to burn server CPU.
- No password reset, email verification, or account lockout.

## Dunning and the second event cursor

`invoice.payment_failed` is where a renewal starts going wrong and the window in
which a customer can still be saved. `invoice.payment_succeeded` closes it.
Migration 00004 adds the state those two maintain: `payment_failed_at` (the
flag), `payment_failure_count`, `last_payment_error`, `next_payment_attempt_at`.

**Neither handler writes `status`.** Stripe decides what a failed payment means
for the subscription — `past_due` when it was active, `incomplete` on a first
invoice, `canceled` once retries run out — and reports it in a separate
`customer.subscription.updated`. Deriving status from the invoice would race
that event and sometimes contradict it.

### Why invoice events need their own cursor

`subscriptions.last_stripe_event_at` guards the `customer.subscription.*`
stream. Invoice events must not share it. They are a different Stripe object
with its own event stream, and the two interleave freely: an
`invoice.payment_failed` created at T+5 would advance a shared cursor past a
`customer.subscription.updated` created at T+4, and that subscription event —
the one carrying the authoritative status — would be rejected as stale and
silently dropped.

Migration 00004 therefore adds `last_invoice_event_at` as a second, independent
cursor. `TestInvoice_DoesNotStarveTheSubscriptionEventStream` is the negative
control: pointing the invoice path at the shared cursor makes the status stay
`active` when it should be `past_due`.

Idempotency needs nothing new. The event ledger already makes a redelivery a
no-op, which is what keeps `payment_failure_count` from double-counting.

## Customer Portal

`POST /api/v1/portal` returns a link into Stripe's hosted billing portal, where
a customer cancels, switches plan, or updates their card.

None of that is implemented here on purpose. The portal is Stripe-hosted, so
card details never reach this service and PCI scope stays with Stripe, and the
resulting changes arrive back through the `customer.subscription.*` webhooks
already handled.

**The request carries no customer id, and there is no field it could.** The
returned URL authenticates its bearer as that Stripe customer for its lifetime,
so the account is derived from the token subject only. It is also never logged —
the session id is, the URL is not.

What the portal permits is configured in the Stripe dashboard rather than here.
That is deliberate on Stripe's part: the permissions live with the account, so a
bug in this service cannot widen them.

A user with no linked customer gets 404, not an error. They have not completed a
checkout, there is nothing to manage, and the UI should offer a plan instead.

## Continuous integration

`.github/workflows/ci.yml` runs four jobs on every push to `main` and every pull
request: build and vet, unit tests, integration tests against a real
PostgreSQL 16 service container, and the frontend typecheck and build.

Details worth keeping:

- **`go vet` runs under both tag sets.** The integration files only compile with
  `-tags=integration`, so vetting once would leave them unchecked.
- **`go mod tidy` must be a no-op.** A stale `go.mod` fails the build rather
  than being silently corrected on someone's machine.
- **The Go version comes from `go.mod`**, not a duplicated string, so a
  toolchain bump cannot leave CI behind.
- **Migrations are rolled all the way back** at the end of the integration job,
  and a surviving table fails it. That is what keeps the down migrations honest.
- **`npm ci`, not `npm install`** — it fails on a lockfile that has drifted from
  `package.json` instead of quietly resolving something else.

## The webhook sweeper

Failed events used to land in the ledger and stay there. `internal/worker`
turns that into something that recovers on its own and complains when it
cannot.

Three jobs per tick:

1. **Reclaim abandoned claims.** A worker that dies mid-event leaves a row in
   `processing` forever. `TryClaimEvent` would reclaim it once the stale window
   passes, but only if Stripe redelivers — and Stripe stops after three days.
   The sweeper moves it to `failed`, which puts it back in reach of the retry
   path, which does not depend on Stripe at all.

2. **Replay failed events** whose backoff has elapsed, from the payload stored
   in the ledger. This is the case that matters: Stripe retries for three days,
   so a bug fixed on day four leaves every event from days one to three
   permanently unprocessed. Backoff doubles per attempt so a downstream outage
   does not become a tight loop against it.

3. **Report.** Debug when the ledger is clean, info while a backlog drains,
   **error** once anything is dead-lettered or the oldest unsettled event ages
   past `WEBHOOK_SWEEPER_ALERT_AFTER`. That error line is the thing to alert on.

Past `MaxAttempts` an event is dead-lettered and left alone. Something is wrong
that another attempt will not fix, and continuing to retry would bury the signal
in noise.

### ReplayEvent skips signature verification, and why that is safe

`WebhookService.ReplayEvent` does not verify a signature, because there is none
to verify: the `Stripe-Signature` header is not stored, and the payload was
authenticated when the event first arrived.

That is safe **only** because its argument comes from the ledger, and a row
reaches the ledger only through `ProcessEvent`, which verifies before it claims.
It must never be reachable from a request handler, and no caller may build a
`ProcessedWebhook` from user input and pass it in — doing so would reintroduce
exactly the forgery `ProcessEvent` exists to prevent. Its only caller is the
sweeper.

### Running on every replica is safe

Each row is taken with an atomic claim, and the abandoned-claim scan uses
`FOR UPDATE SKIP LOCKED`, so instances divide the work instead of blocking on
each other. Each sweeper also starts with a random offset within its interval,
so replicas do not all wake together and contend on every tick.

`WEBHOOK_STALE_CLAIM_AFTER` must exceed `HTTP_WEBHOOK_TIMEOUT`, and config
refuses to start otherwise: a shorter window lets the sweeper reclaim a claim
that a handler is still working on, and then two workers process one event.

### Operator surface

`api -webhook-report` prints the unsettled ledger and exits **2** when anything
is dead-lettered, so a cron entry or a monitoring check can act on it without
parsing output. `api -webhook-sweep` runs a single pass and exits.

A CLI rather than an HTTP route on purpose: an admin endpoint needs an
authorisation model this service does not have, and inventing one to expose
diagnostics would be a larger security surface than the diagnostics are worth.

## Rate limiting

Applied to `POST /api/v1/auth/login` and `/register` only. Those are the only
routes where an anonymous caller can make the server do expensive work — one
bcrypt comparison at cost 12 is ~250ms of CPU — and the only ones where
unlimited attempts mean unlimited password guesses.

The webhook route is deliberately **not** limited. A 429 tells Stripe to
redeliver, so throttling it builds a retry backlog rather than shedding load.

Three details that are easy to get wrong:

**A rejected request must not consume a token.** `rate.Limiter.ReserveN` takes
one whether or not the caller proceeds, so a denial has to cancel its
reservation. Without the cancel, every blocked attempt pushes the client's
recovery further out and sustained hammering becomes an indefinite lockout —
measured at 195ms of deferred recovery after twenty denials that should have
cost nothing.

**IPv6 is limited per /64, not per address.** A single customer allocation is a
/64 or larger, so limiting per address lets one attacker cycle through billions
of them — defeating the limit and filling the bucket map at the same time.

**`X-Forwarded-For` is only read when `TRUSTED_PROXIES` says how many hops to
skip.** The header is appended to by each hop, so with N trusted proxies the
client is the Nth entry from the right; everything further left is
caller-supplied. Reading the leftmost entry — the common shortcut — lets an
attacker send a fresh header per request and get an unlimited allowance. The
default is 0, meaning `RemoteAddr` only.

The bucket map is bounded and self-pruning; at capacity it resets wholesale
rather than evicting a victim, because a targeted eviction would let an attacker
who can spray addresses clear a specific client's limit on demand.

The limiter is in-memory and therefore per-instance: behind N replicas the
effective limit is N times the configured one. That is the right trade at this
scale — a shared counter adds a network round trip and a new failure mode to the
login path — but it is a ceiling, not a floor, and a horizontally scaled
deployment should move the state out.

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
