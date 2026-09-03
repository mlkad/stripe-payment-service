# Production deployment

A single self-hosted VPS: Caddy for TLS, the API, Postgres, and Prometheus.

## Shape

Caddy is the only container that publishes ports. Everything else listens on
the compose networks, so the API cannot be reached except through the proxy,
Postgres cannot be reached from the internet at all, and `/metrics` is not
exposed outside the stack.

That distinction matters more on a VPS than it sounds. Docker writes its own
iptables rules and a published port **bypasses ufw**, so "the firewall is on"
is not a defence against a stray `ports:` entry. The defence is not publishing
it.

```
internet ──▶ caddy :80 :443
                │
          edge  │           (not internal: the API must reach api.stripe.com)
                ▼
               api :8080  :9091
                │
        backend │           (internal: true — no egress)
                ▼
          postgres :5432    prometheus :9090
```

`edge` is deliberately **not** `internal: true`. That flag blocks outbound
traffic as well as inbound, and an API that cannot call Stripe fails in a way
that looks like a Stripe outage. Being on a network with egress does not make a
service reachable from outside — publishing a port does, and only Caddy does.

## First deploy

```bash
git clone <repo> && cd stripe-payment-service
cp deployments/production/.env.production.example .env.production
chmod 600 .env.production
$EDITOR .env.production          # every CHANGE_ME, and DOMAIN
```

Point an A record at the VPS **before** starting: Caddy obtains a certificate
on boot, and Let's Encrypt rate-limits failed issuance per domain per week.

```bash
docker compose -f deployments/production/docker-compose.yml \
  --env-file .env.production up -d --build
```

Migrations run as a one-shot container before the API starts, so a schema
change ships with the deploy that needs it.

Then add the webhook endpoint in the Stripe dashboard, pointing at
`https://<DOMAIN>/webhook`, pinned to the API version in `.env.production`.
Put the signing secret it gives you in `STRIPE_WEBHOOK_SECRET` and restart the
API.

## Checks after a deploy

```bash
docker compose -f deployments/production/docker-compose.yml ps
curl -s https://$DOMAIN/healthz | jq

# metrics are not public; reach them from inside
docker compose -f deployments/production/docker-compose.yml \
  exec api /app/api -webhook-report
```

`/healthz` checks the database and answers 503 when it is unreachable.
`/livez` does not, on purpose: restarting the API cannot repair a database
outage, and a liveness probe that depends on it turns a blip into a restart
loop.

## Operating

```bash
# what needs a human
docker compose ... exec api /app/api -webhook-report     # exits 2 on dead letters
docker compose ... exec api /app/api -retention-run      # exits 2 if data is overdue

# database
docker compose ... exec postgres psql -U payments payments

# Prometheus UI, over a tunnel rather than a published port
ssh -L 9090:localhost:9090 vps
docker compose ... exec prometheus wget -qO- localhost:9090/-/healthy
```

## Backups

`postgres_data` is the only volume that cannot be rebuilt. Nothing in this
compose file backs it up — that is deliberate rather than forgotten, because a
backup that lives on the same disk as the thing it backs up is not a backup.

```bash
docker compose ... exec -T postgres \
  pg_dump -U payments payments | gzip > backup-$(date +%F).sql.gz
```

Send it somewhere else. Restore is `gunzip -c … | psql`.

`caddy_data` holds the TLS certificates. Losing it re-issues them, which is fine
until you hit the weekly rate limit — worth including in the backup.

## Updating

```bash
git pull
docker compose ... up -d --build
```

Compose recreates the API only if the image changed. Migrations run first, so
forward-compatible schema changes deploy cleanly. A destructive migration needs
the usual two-step: deploy the code that tolerates both shapes, then the
migration.

## What this does not do

- **No horizontal scaling.** The rate limiter is per-instance, so N replicas
  means N times the configured limit. The sweeper and retention worker are safe
  to run on every replica; the rate limiter would need shared state.
- **No log shipping.** Logs are JSON on stdout with rotation at 10 MB × 5 files.
  Point a collector at the Docker socket if you want them off the box.
- **No alerting.** Prometheus scrapes but has no Alertmanager. The four rules
  worth having are written down in `prometheus.yml`.
- **No secret manager.** `.env.production` is a file on disk, `chmod 600`. Fine
  for one machine, not for several.
