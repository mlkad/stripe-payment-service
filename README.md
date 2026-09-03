# Stripe Payment & Subscription Gateway

Production-grade Go microservice for Stripe Checkout, subscription lifecycle
tracking, Customer Portal, and idempotent webhook processing.

**Stack:** Go 1.25+ · PostgreSQL 16 (pgx/v5) · Goose · Docker · slog
**Architecture:** Clean / Hexagonal — see [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)

---

## Quick start

```bash
cp .env.example .env          # then fill in your Stripe test keys
make up                       # postgres → migrations → API
make migrate-status           # verify the schema
curl localhost:8080/healthz
```

Forward real Stripe test events at the local webhook endpoint:

```bash
make stripe                   # runs the Stripe CLI in the dev profile
```

`make help` lists every target.

---

## Schema

Three tables, applied in order by Goose:

| Migration | Table | Role |
|---|---|---|
| `00001` | `users` | Identity ↔ Stripe Customer, 1:1 |
| `00002` | `subscriptions` | Local read-model of Stripe Subscriptions |
| `00003` | `processed_webhooks` | Idempotency ledger — one row per `evt_` id, ever |

Design notes that matter in production:

- **`updated_at` is enforced by the database**, not the application. One shared
  `set_updated_at()` trigger function; each trigger carries
  `WHEN (OLD.* IS DISTINCT FROM NEW.*)` so no-op UPDATEs don't bump the timestamp.
- **Partial indices everywhere it pays.** Entitlement checks hit
  `idx_subscriptions_user_id_live`, which only indexes `trialing/active/past_due`
  rows — it stays small no matter how much churn accumulates.
- **Stripe id shapes are checked at the column level** (`cus_`, `sub_`, `price_`,
  `evt_`). A mis-wired handler fails at the write, not three days later in a report.
- **`ON DELETE RESTRICT`** from `subscriptions` → `users`: a hard user delete must
  never silently destroy billing history.
- **Out-of-order delivery is handled**, not assumed away — see
  `subscriptions.last_stripe_event_at`.

- **`updated_at` uses `clock_timestamp()`, not `now()`** — `now()` is frozen at
  transaction start and would make same-transaction updates invisible. See
  [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

Migrations are Goose format (single file, `-- +goose Up` / `-- +goose Down`).
Every `down` is written and tested — a full `goose reset` leaves zero orphaned
tables, enum types, functions, or extensions.

```bash
make verify-schema   # 22 assertions over constraints, triggers, idempotency
```

⚠️ **Repository contract:** lookups by email must include `AND deleted_at IS NULL`
or they fall off the partial index (0.037 ms → 20.96 ms at 50k rows).

---

## Layout

```
cmd/api            composition root + graceful shutdown
internal/config    env -> typed Config, validated once at boot
internal/logger    slog setup, request-id correlation, redaction
internal/database  pgxpool construction, health, query tracing
internal/domain    entities + ports, zero I/O
internal/service   use cases
internal/repository/postgres, internal/adapter/stripe   driven adapters
internal/handler/http                                   driving adapter
migrations         schema source of truth
```

### Health endpoints

| Endpoint | Checks | Used by |
|---|---|---|
| `GET /livez` | process only | container `HEALTHCHECK`, k8s liveness |
| `GET /healthz` | process **and** database | load balancer, k8s readiness |

Liveness deliberately ignores the database. Restarting this container cannot
repair a database outage, so wiring liveness to the database converts one DB blip
into a cluster-wide restart loop.

---

## Configuration

All configuration is environment-driven and validated once at boot; see
[.env.example](.env.example) for the full annotated list. There are no defaults
for secrets — the process refuses to start without `STRIPE_SECRET_KEY` and
`STRIPE_WEBHOOK_SECRET`.

---

## Roadmap

- [x] **Step 1** — infrastructure, Docker stack, database schema
- [x] **Step 2** — config, logger, pgx pool, server + graceful shutdown
- [x] **Step 3** — domain models, repositories, integration tests
- [x] **Step 4** — Stripe adapter, Checkout, webhook signature + processing
- [x] **Step 5** — chi router, middleware (request id, logging, recovery, timeout)
- [x] **Step 6** — React dashboard, Stripe Elements, entitlement read API
- [x] **Step 7** — JWT authentication, protected routes, secure context
- [x] **Step 8** — auth rate limiting, invoice events, dunning state
- [x] **Step 9** — Customer Portal, CI pipeline
- [x] **Step 10** — webhook sweeper: dead-letter recovery and alerting
- [x] **Step 11** — payload retention and PII minimisation
- [x] **Step 12** — unit test coverage, coverage gate in CI
- [x] **Step 13** — rotating refresh tokens with reuse detection
- [x] **Step 14** — Prometheus metrics, production Docker Compose + Caddy
- [ ] **Step 15** — plan changes and proration
