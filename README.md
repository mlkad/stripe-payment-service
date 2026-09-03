# Stripe Payment & Subscription Gateway

Billing service in Go. Checkout, subscriptions, dunning, Customer Portal, React
dashboard.

Calling the Stripe API is the easy part. This is about the rest: webhooks
arriving twice, arriving out of order, arriving three days late, and what
happens when a payment fails at 3am.

**Go 1.25 · PostgreSQL 16 · pgx/v5 · chi · React 19 · Vite · Tailwind 4**

Modular monolith. One binary, one database, workers as goroutines. The
guarantees below are row locks in single transactions — splitting this into
services would trade them for distributed locks to buy scale I don't have.

---

## Six things that would break, and don't

Each one has a test. I broke the code to check the test noticed.

### Remove the row lock, lose a subscription state

`SELECT ... FOR UPDATE` guards the out-of-order check. Under READ COMMITTED,
read-decide-write is only atomic while the row is held.

```
--- FAIL: TestSubscriptionRepo_ConcurrentOutOfOrderEventsConverge
    round 0: last_stripe_event_id = evt_R08, want evt_R23
    round 0: status = "trialing", want "unpaid"
    ... every round fails
```

24 shuffled events per round, 8 rounds. Two workers read the same pre-state, and
the last one to commit wins regardless of which event is newer.

### Claim before verifying, and anyone can drop your webhooks

`event_id` comes from the request body. Unauthenticated until the signature
checks out. Claim first and someone can POST a guessed id, plant a settled row,
and your real delivery gets discarded as a duplicate.

```
--- FAIL: TestWebhook_ForgedPayloadNeverReachesLedger/garbage_signature
    forged payload created 1 ledger row(s); the claim ran before verification

--- FAIL: .../valid_shape,_wrong_secret
    status = 200, want 400
```

That second line is the attack landing. The row planted by the previous forged
request made the next delivery look like a duplicate — answered `200`, never
processed.

### Share one cursor between two event streams, silently lose the status

Invoice and subscription events interleave. An `invoice.payment_failed` at T+5
advances a shared cursor past a `customer.subscription.updated` at T+4 — and
that one carries the real status.

```
--- FAIL: TestInvoice_DoesNotStarveTheSubscriptionEventStream
    status = "active", want past_due: the invoice event advanced the
    subscription cursor and starved the event carrying the real status
```

The customer's card failed. Your database says active. Migration `00004` gives
each stream its own cursor.

### Forget to cancel a rate-limit reservation, lock out the victim

`rate.Limiter.ReserveN` takes a token whether you proceed or not. A denial has
to give it back.

```
--- FAIL: TestRateLimiterRejectionDoesNotDeferRecovery
    still throttled after a refill window; denials consumed tokens (delay=195ms)
```

Twenty denials pushed recovery from 10ms to 195ms. Under a brute-force attempt
that compounds until the real user can never log in — the limiter punishing the
person being attacked.

### Rotate refresh tokens without detecting reuse, and theft is invisible

Rotation limits the window. Recording that a token was *spent* is what catches
the thief.

```
--- FAIL: TestRefresh_ReuseRevokesTheEntireFamily
    the live token still works after reuse detection: status = 200
```

When a spent token comes back, either the thief or the victim is holding it and
there's no telling which. Whole family revoked. False positive costs one login.

### Hardcode a decoy hash, build the oracle you were preventing

The unknown-account login path compares against a decoy so it costs the same as
a real check. I hardcoded cost-12 against cost-10 stored hashes:

```
unknown account : 401  0.294s
wrong password  : 401  0.065s
```

4.5x apart, and backwards. Louder than having no decoy at all. Found it by
measuring, not reasoning. The decoy is now generated at the configured cost:

```
unknown 0.068  wrong-pw 0.065
unknown 0.065  wrong-pw 0.065
unknown 0.065  wrong-pw 0.064
```

---

## Two more, without the drama

**Failed events don't sit there.** A sweeper reclaims claims from crashed
workers and replays failures from the stored payload. Stripe gives up after
three days — fix a bug on day four and those three days are gone otherwise.

**Payloads expire, because they hold customer email and address.** Settled: 30
days. Failed: 90, since the sweeper replays from them. Past that they go anyway;
a privacy obligation doesn't pause because a queue is stuck. What's left is an
allowlisted skeleton — a denylist starts leaking the day Stripe adds a field
nobody anticipated, and looks compliant while doing it.

---

## Run it

```bash
cp .env.example .env      # Stripe test keys, and a JWT_SECRET
make up                   # postgres, migrations, api
```

Webhooks need a listener or nothing reaches the database:

```bash
stripe listen --forward-to localhost:8080/webhook
```

Paste the `whsec_` into `.env`, restart. Then:

```bash
cd web && cp .env.example .env && npm install && npm run dev
```

## Test it

```bash
make test                 # unit
make test-integration     # real PostgreSQL
make verify-schema        # constraints, triggers, idempotency
```

219 tests, 75% combined coverage, gated in CI.

Two guards turned out **not** to be load-bearing, and the code says so: the
`alg:none` test passes with or without `WithValidMethods` — jwt/v5 refuses it
independently — and the sweeper's attempts guard survives being neutered. Both
stay. Neither is claimed as proven.

---

## Layout

```
cmd/api                       composition root, shutdown, operator CLI
internal/domain               entities and invariants. No I/O.
internal/service              use cases
internal/repository/postgres  pgx adapters and their ports
internal/stripe               the only package importing stripe-go
internal/auth                 bcrypt, JWT, refresh tokens
internal/handler              routing, middleware, HTTP
internal/worker               sweeper, retention
internal/config               env to typed config, validated at boot
migrations                    schema, source of truth
web                           React dashboard
```

Four tables, six migrations. Every `down` is written and tested — a full reset
leaves nothing behind and CI checks it. Design notes in
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Operate it

```
GET  /livez /healthz
POST /webhook
POST /api/v1/auth/register|login|refresh|logout
GET  /api/v1/auth/me
POST /api/v1/checkout  /api/v1/portal
GET  /api/v1/subscription
```

Liveness ignores the database. Restarting the container can't fix a database
outage — wire liveness to it and one blip becomes a restart loop. `/metrics`
sits on a separate listener; it publishes request rates and business volume.

Two subcommands exit non-zero when a human is needed, so cron can call them:

```bash
api -webhook-report     # exits 2 on dead letters
api -retention-run      # exits 2 if data is overdue
```

`deployments/production` has a single-VPS compose stack — Caddy with automatic
TLS, API, Postgres, Prometheus. Caddy is the only container publishing ports.

---

## Not done

- **Plan changes and proration.** Subscribe and cancel work. Upgrade doesn't.
- **Password reset, email verification.** Both need mail delivery.
- **Horizontal scaling.** The rate limiter is per-instance: N replicas, N times
  the limit. Workers replicate fine; the limiter doesn't.
- **Alerting.** Prometheus scrapes, no Alertmanager. The four rules worth having
  are written down in `deployments/production/prometheus.yml`.
