import { useCallback, useEffect, useState } from "react";
import type { Plan } from "@/api/types";
import { AuthProvider, useAuth } from "@/hooks/useAuth";
import { useSubscription } from "@/hooks/useSubscription";
import { AuthForm } from "@/components/AuthForm";
import { Dashboard } from "@/components/Dashboard";
import { PricingTable } from "@/components/PricingTable";
import { CheckoutModal } from "@/components/CheckoutModal";
import { Alert, Button, Spinner } from "@/components/ui";

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

export default function App() {
  return (
    <AuthProvider>
      <Shell />
    </AuthProvider>
  );
}

function Shell() {
  const { user, isAuthenticated, isBootstrapping, logout } = useAuth();

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

          {isAuthenticated && user && (
            <div className="flex items-center gap-4">
              <span className="hidden text-sm text-muted sm:inline">{user.email}</span>
              <Button variant="ghost" onClick={logout}>
                Sign out
              </Button>
            </div>
          )}
        </div>
      </header>

      <main className="mx-auto max-w-5xl px-6 py-10 sm:py-14">
        {isBootstrapping ? (
          <div className="flex justify-center py-24" aria-busy="true">
            <Spinner className="size-6 text-faint" />
          </div>
        ) : isAuthenticated ? (
          <Billing />
        ) : (
          <AuthForm />
        )}
      </main>
    </div>
  );
}

function Billing() {
  const [checkoutSecret, setCheckoutSecret] = useState<string | null>(null);
  const [showPricing, setShowPricing] = useState(false);

  // Stripe returns the browser here the moment payment succeeds, but the local
  // row is written by the webhook. Arriving with a session_id means "poll until
  // the webhook lands", not "you are subscribed".
  const [returnedFromCheckout] = useState(
    () => new URLSearchParams(window.location.search).has("session_id"),
  );

  const { state, refresh, isRefreshing } = useSubscription(returnedFromCheckout);

  useEffect(() => {
    if (!returnedFromCheckout) return;
    // Drop session_id from the URL so a reload does not restart polling.
    window.history.replaceState({}, "", window.location.pathname);
  }, [returnedFromCheckout]);

  const closeCheckout = useCallback(() => {
    setCheckoutSecret(null);
    // The webhook may have landed while the modal was open.
    refresh();
  }, [refresh]);

  const hasSubscription = state.status === "ready" && state.subscription !== null;
  const pricingVisible = showPricing || !hasSubscription;

  return (
    <div className="space-y-10">
      {returnedFromCheckout && !hasSubscription && (
        <Alert title="Finishing up" tone="info">
          Payment received. Waiting for Stripe to confirm the subscription — this usually takes a
          second.
        </Alert>
      )}

      <Dashboard
        state={state}
        isRefreshing={isRefreshing}
        onRefresh={refresh}
        onChoosePlan={() => setShowPricing(true)}
      />

      {hasSubscription && (
        <div className="flex justify-end">
          <Button variant="ghost" onClick={() => setShowPricing((v) => !v)}>
            {showPricing ? "Hide plans" : "Change plan"}
          </Button>
        </div>
      )}

      {pricingVisible && (
        <PricingTable
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

      {checkoutSecret && <CheckoutModal clientSecret={checkoutSecret} onClose={closeCheckout} />}
    </div>
  );
}
