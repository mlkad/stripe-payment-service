# Stripe Payment & Subscription Gateway

A billing service in Go: checkout, subscription lifecycle, dunning, and the
Customer Portal, with a React dashboard on top.

I built it to get right the parts that usually go wrong — webhook idempotency,
out-of-order events, and what happens after a payment fails. Most of the work is
there rather than in the Stripe API calls.

**Go 1.25 · PostgreSQL 16 (pgx/v5) · chi · React 19 · Vite · Tailwind 4**

Modular monolith: one binary, one database, workers as goroutines. The package
boundaries are enforced, so the seams exist if it ever needs splitting. It does
not — the guarantees below depend on single-database transactions, and
distributing them would trade row locks for distributed locks to buy scale I do
not have.

---

## The parts worth reading

**Stripe delivers at least once, unordered, for three days.**

`processed_webhooks.event_id` is the primary key, so the unique index does the
mutual exclusion — no advisory locks. Claiming is `INSERT ... ON CONFLICT DO
UPDATE ... WHERE`, and the `WHERE` decides who may retry: failed is reclaimable,
processing only once the claim goes stale, succeeded never.

For ordering, `subscriptions.last_stripe_event_at` holds the newest applied
`event.created`; older events are acknowledged and discarded, so a stale retry
cannot resurrect a canceled subscription. That guard reads, decides, then
writes, which under READ COMMITTED is only atomic because the row is held with
`SELECT ... FOR UPDATE`. Remove the lock and 18 assertions fail — two workers see
the same pre-state and the last committer wins. Equal timestamps are applied,
not rejected: `event.created` has one-second resolution and distinct events
collide routinely.

**Signature verification runs before the claim.**

`event_id` and `created` come from the request body, which is unauthenticated
until the signature checks out. Claiming first would let someone POST a guessed
event id, plant a settled row, and make Stripe's real delivery get dropped as a
duplicate. I tested it rather than assuming: with the order reversed, a forged
request creates a ledger row and the next delivery is answered `200` and never
processed.

**Invoice events get their own ordering cursor.**

Invoice and subscription events are separate streams that interleave. An
`invoice.payment_failed` at T+5 would advance a shared cursor past a
`customer.subscription.updated` at T+4 — and that one carries the authoritative
status. Migration `00004` adds a second cursor so neither starves the other.
Neither invoice handler writes `status`; Stripe decides what a failed payment
means, and deriving it here would race the event that says so.

**Failed events do not sit there.**

A sweeper reclaims claims abandoned by crashed workers, replays failed events
from the stored payload, and logs at error level once anything is dead-lettered.
Stripe gives up after three days, so a bug fixed on day four would otherwise
leave three days of events permanently unprocessed.

**Payloads expire, because they hold personal data.**

Settled events lose their payload after 30 days; failed ones keep theirs for 90,
since the sweeper replays from it. Past that they go anyway — a privacy
obligation does not pause because a retry queue is stuck. What replaces the
payload is an allowlisted skeleton, not a redaction: a denylist would start
leaking the day Stripe adds a field nobody anticipated, and would look compliant
while doing it. The reduction runs in SQL, so the data never reaches the app.

**Sessions.**

15-minute access tokens in memory, 30-day refresh tokens in an httpOnly cookie
scoped to `/api/v1/auth`. Rotation is unconditional. If a spent token comes back,
either the thief or the victim is presenting one the other already used and
there is no telling which, so the whole family is revoked. A false positive costs
one login.

---

## Running it

```bash
cp .env.example .env      # Stripe test keys, and a JWT_SECRET
make up                   # postgres, migrations, api
```

Webhooks need a listener, or checkout completes at Stripe and the database never
hears about it:

```bash
stripe listen --forward-to localhost:8080/webhook
```

Put the `whsec_` it prints into `.env` and restart. Then:

```bash
cd web && cp .env.example .env && npm install && npm run dev
```

`make help` lists the rest.

---

## Tests

```bash
make test                 # unit
make test-integration     # against a real PostgreSQL
make verify-schema        # constraints, triggers, idempotency
```

219 tests, 75% combined coverage, gated in CI.

Where a property matters, the test was checked against a deliberately broken
implementation — removing `FOR UPDATE`, reversing signature-then-claim, dropping
the rate limiter's reservation cancel, pointing the invoice cursor at the
subscription cursor. Each produces a failure, and each is noted where it lives.

Two checks turned out not to have teeth and say so: the `alg:none` token test
passes with or without `WithValidMethods`, because jwt/v5 refuses it
independently, and the sweeper's claim-level attempts guard survives being
neutered. Both guards stay; neither is claimed as proven.

---

## Layout

```
cmd/api                       composition root, shutdown, operator CLI
internal/domain               entities and invariants. No I/O.
internal/service              use cases
internal/repository/postgres  pgx adapters and the ports they satisfy
internal/stripe               the only package importing stripe-go
internal/auth                 bcrypt policy, JWT, refresh tokens
internal/handler              routing, middleware, HTTP surface
internal/worker               webhook sweeper, payload retention
internal/config               env to typed config, validated at boot
migrations                    schema, source of truth
web                           React dashboard
```

Four tables, six migrations, every `down` written and tested — a full reset
leaves nothing behind, and CI checks that. Design notes in
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

---

## Operating

```
GET  /livez /healthz
POST /webhook
POST /api/v1/auth/register|login|refresh|logout
GET  /api/v1/auth/me
POST /api/v1/checkout  /api/v1/portal
GET  /api/v1/subscription
```

Liveness ignores the database on purpose: restarting the container cannot repair
a database outage, and wiring liveness to it turns one blip into a restart loop.
`/metrics` is on a separate listener, since it publishes request rates and
business volume.

Two subcommands exit non-zero when a human is needed, so cron can use them:

```bash
api -webhook-report     # unsettled ledger; exits 2 on dead letters
api -retention-run      # one retention pass; exits 2 if data is overdue
```

`deployments/production` has a compose stack for a single VPS — Caddy with
automatic TLS, the API, Postgres, Prometheus. Caddy is the only container that
publishes ports.

---

## Not done

- Plan changes and proration. A customer can subscribe and cancel, not upgrade.
- Password reset and email verification, both of which need mail delivery.
- Horizontal scaling. The rate limiter is per-instance, so N replicas means N
  times the limit. The workers replicate safely; the limiter does not.
- Alerting. Prometheus scrapes but there is no Alertmanager. The four rules
  worth having are in `deployments/production/prometheus.yml`.
