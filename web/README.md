# Web UI

React 19 + TypeScript + Vite 8 + Tailwind 4, talking to the Go gateway.

## Run it

```bash
cp .env.example .env      # then fill in the values below
npm install
npm run dev               # http://localhost:5173
```

The backend must be running (`make run` from the repo root).

## Configuration

| Variable | Purpose |
|---|---|
| `VITE_STRIPE_PUBLISHABLE_KEY` | Stripe **publishable** key (`pk_...`) |
| `VITE_PRICE_*` | Stripe price ids, one per plan |
| `VITE_DEV_PROXY_TARGET` | Where the dev server forwards `/api` (default `http://localhost:8080`) |
| `VITE_API_BASE_URL` | Absolute backend URL. Leave empty to use the dev proxy |

Everything prefixed `VITE_` is compiled into the JavaScript served to the
browser. The Stripe **secret** key belongs to the Go service and must never
appear here — `src/lib/stripe.ts` throws in development if it finds one.

Each `VITE_PRICE_*` id must also appear in the backend's
`STRIPE_ALLOWED_PRICE_IDS`, or checkout is rejected with 422.

## Structure

```
src/
├── api/
│   ├── client.ts        typed fetch wrapper, ApiError with status + request id
│   └── types.ts         mirrors the Go read models
├── components/
│   ├── PricingTable.tsx plan cards; starts checkout
│   ├── CheckoutModal.tsx embedded Stripe Checkout in a dialog
│   ├── Dashboard.tsx    subscription status view
│   ├── StatusBadge.tsx  one descriptor per subscription status
│   └── ui/              Button, Card, Alert, Spinner, Skeleton
├── hooks/
│   └── useSubscription.ts  load + poll after checkout
└── lib/
    ├── stripe.ts        loadStripe once, at module scope
    └── format.ts        money and date formatting
```

## Authentication

`AuthProvider` holds the session; `AuthForm` handles register and sign-in. The
token goes into `localStorage` and is attached as `Authorization: Bearer` by the
API client, which is told about the token through an injected bridge rather than
importing the store — moving to an httpOnly cookie later changes the provider,
not the client.

A `401` on any authenticated call clears the session once, centrally, so no
caller has to handle expiry. A stored token is verified against `/auth/me` on
load, so the UI never renders a signed-in shell whose every request then fails.

`localStorage` is readable by any script on the origin: one XSS is a stolen
credential. What makes it acceptable rather than merely convenient is that the
token is short-lived and carries no privilege beyond the subject. The upgrade is
a refresh token in an httpOnly cookie with the access token in memory only.

## Why embedded Checkout and not a bare PaymentElement

`PaymentElement` confirms a single `PaymentIntent`. Subscribing with it means
creating the subscription server-side with `payment_behavior=default_incomplete`,
pulling the client secret off the first invoice, confirming it in the browser,
then handling SCA, 3DS redirects and trials by hand.

Embedded Checkout does all of that inside Stripe's iframe and still finishes as
`checkout.session.completed` — the webhook path the backend already implements
and tests. The trade is layout control. Swapping later means adding a server
route that returns the invoice's client secret; it does not change how
subscriptions are recorded.

## The redirect is not the confirmation

Stripe returns the browser the moment payment succeeds, but the local row is
written by the webhook, which usually lands within a second and occasionally
much later. `useSubscription` therefore polls with backoff when it sees a
`session_id` in the URL, rather than trusting the redirect. Without a webhook
listener running (`stripe listen --forward-to localhost:8080/webhook`), a
completed payment never reaches the database and the dashboard stays empty —
that is the system working as designed, not a bug in the UI.
