import { useCallback, useEffect, useState } from "react";
import type { Plan } from "@/api/types";
import { useSubscription } from "@/hooks/useSubscription";
import { Dashboard } from "@/components/Dashboard";
import { PricingTable } from "@/components/PricingTable";
import { CheckoutModal } from "@/components/CheckoutModal";
import { Alert, Button } from "@/components/ui";

/**
 * Plan copy lives in the frontend; prices live in Stripe. Only priceId crosses
 * the boundary, and the backend rejects any id outside STRIPE_ALLOWED_PRICE_IDS,
 * so editing this array cannot subscribe anyone to an unintended price.
 */
const PLANS: Plan[] = [
  {
    id: "starter",
    priceId: import.meta.env.VITE_PRICE_STARTER ?? "price_starter_placeholder",
    name: "Starter",
    tagline: "For side projects finding their footing.",
    amount: 900,
    currency: "usd",
    interval: "month",
    features: ["10,000 API calls / month", "Community support", "1 project", "7-day log retention"],
  },
  {
    id: "pro",
    priceId: import.meta.env.VITE_PRICE_PRO ?? "price_pro_placeholder",
    name: "Pro",
    tagline: "For teams shipping to real customers.",
    amount: 2900,
    currency: "usd",
    interval: "month",
    featured: true,
    features: [
      "500,000 API calls / month",
      "Priority email support",
      "Unlimited projects",
      "90-day log retention",
      "Webhook replay",
    ],
  },
  {
    id: "scale",
    priceId: import.meta.env.VITE_PRICE_SCALE ?? "price_scale_placeholder",
    name: "Scale",
    tagline: "For products where billing is the product.",
    amount: 9900,
    currency: "usd",
    interval: "month",
    features: [
      "Unlimited API calls",
      "Dedicated support channel",
      "SSO & audit logs",
      "1-year log retention",
      "99.95% uptime SLA",
    ],
  },
];

/** Stand-in for a session. Replace with the authenticated user's id. */
const DEMO_USER_ID = import.meta.env.VITE_DEMO_USER_ID ?? "";

export default function App() {
  const [checkoutSecret, setCheckoutSecret] = useState<string | null>(null);
  const [showPricing, setShowPricing] = useState(false);

  // Stripe sends the browser back here after checkout. The local row is written
  // by the webhook, not the redirect, so arriving with a session_id means "poll
  // until the webhook lands" rather than "you are subscribed".
  const [returnedFromCheckout] = useState(
    () => new URLSearchParams(window.location.search).has("session_id"),
  );

  const { state, refresh, isRefreshing } = useSubscription(DEMO_USER_ID, returnedFromCheckout);

  useEffect(() => {
    if (!returnedFromCheckout) return;
    // Drop session_id from the URL so a reload does not restart polling.
    window.history.replaceState({}, "", window.location.pathname);
  }, [returnedFromCheckout]);

  const closeCheckout = useCallback(() => {
    setCheckoutSecret(null);
    // The webhook may already have landed while the modal was open.
    refresh();
  }, [refresh]);

  const hasSubscription = state.status === "ready" && state.subscription !== null;
  const pricingVisible = showPricing || !hasSubscription;

  return (
    <div className="min-h-dvh">
      <header className="border-b border-line/60">
        <div className="mx-auto flex max-w-5xl items-center justify-between px-6 py-5">
          <div className="flex items-center gap-2.5">
            <div className="flex size-8 items-center justify-center rounded-lg bg-brand text-sm font-bold text-white">
              S
            </div>
            <span className="font-semibold tracking-tight">Stripe Gateway</span>
          </div>
          {hasSubscription && (
            <Button variant="ghost" onClick={() => setShowPricing((v) => !v)}>
              {showPricing ? "Hide plans" : "Change plan"}
            </Button>
          )}
        </div>
      </header>

      <main className="mx-auto max-w-5xl space-y-10 px-6 py-10 sm:py-14">
        {!DEMO_USER_ID && (
          <Alert title="No user configured" tone="warn">
            Set <code className="font-mono">VITE_DEMO_USER_ID</code> in{" "}
            <code className="font-mono">web/.env</code> to a UUID from the{" "}
            <code className="font-mono">users</code> table. Until authentication exists, the UI has
            no other way to identify the caller.
          </Alert>
        )}

        {returnedFromCheckout && !hasSubscription && (
          <Alert title="Finishing up" tone="info">
            Payment received. Waiting for Stripe to confirm the subscription — this usually takes a
            second.
          </Alert>
        )}

        {DEMO_USER_ID && (
          <Dashboard
            state={state}
            isRefreshing={isRefreshing}
            onRefresh={refresh}
            onChoosePlan={() => setShowPricing(true)}
          />
        )}

        {DEMO_USER_ID && pricingVisible && (
          <PricingTable
            userId={DEMO_USER_ID}
            plans={PLANS}
            currentPriceId={
              state.status === "ready" && state.subscription?.is_active
                ? state.subscription.price_id
                : undefined
            }
            mode="embedded"
            onEmbeddedSession={setCheckoutSecret}
          />
        )}
      </main>

      {checkoutSecret && (
        <CheckoutModal clientSecret={checkoutSecret} onClose={closeCheckout} />
      )}
    </div>
  );
}
