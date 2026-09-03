import { useCallback, useEffect, useState, type ReactNode } from "react";
import type { Plan } from "@/api/types";
import { AuthProvider, useAuth } from "@/hooks/useAuth";
import { useSubscription } from "@/hooks/useSubscription";
import { AuthForm } from "@/components/AuthForm";
import { Dashboard } from "@/components/Dashboard";
import { PricingTable } from "@/components/PricingTable";
import { CheckoutModal } from "@/components/CheckoutModal";
import { Alert, Button, Card, Spinner } from "@/components/ui";

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
    <div className="mx-auto flex min-h-dvh w-full max-w-6xl flex-col gap-8 px-4 py-6 sm:px-6 sm:py-8">
      <Header
        {...(isAuthenticated && user
          ? { email: user.email, onSignOut: () => void logout() }
          : {})}
      />

      <main className="flex-1">
        {isBootstrapping ? (
          <div className="flex justify-center py-32" aria-busy="true">
            <Spinner className="size-6 text-gold/70" />
          </div>
        ) : isAuthenticated ? (
          <Billing />
        ) : (
          <AuthForm />
        )}
      </main>

      {isAuthenticated && <FeatureRow />}
    </div>
  );
}

interface HeaderProps {
  email?: string;
  onSignOut?: () => void;
}

/**
 * The account controls appear only when signed in. The mock shows them on the
 * sign-in screen too, which would be a state bug rather than a style choice -
 * there is nothing to sign out of.
 */
function Header({ email, onSignOut }: HeaderProps) {
  return (
    <Card className="flex items-center justify-between gap-4 px-5 py-4">
      <div className="flex items-center gap-3">
        <span className="btn-gold grid size-9 place-items-center rounded-xl font-display text-lg font-semibold">
          S
        </span>
        <span className="font-semibold tracking-tight text-ink">Stripe Gateway</span>
      </div>

      {email && onSignOut && (
        <div className="flex items-center gap-4">
          <span className="hidden text-sm text-muted sm:inline">{email}</span>
          <Button variant="secondary" onClick={onSignOut} className="px-5 py-2.5 text-[13px]">
            <ArrowIcon />
            Sign out
          </Button>
        </div>
      )}
    </Card>
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
    <div className="space-y-16">
      {returnedFromCheckout && !hasSubscription && (
        <Alert title="Finishing up" tone="info">
          Payment received. Waiting for Stripe to confirm the subscription — this usually takes a second.
        </Alert>
      )}

      <Dashboard
        state={state}
        isRefreshing={isRefreshing}
        onRefresh={refresh}
        onChoosePlan={() => setShowPricing(true)}
      />

      {hasSubscription && (
        <div className="flex justify-center">
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

const FEATURES: Array<{ icon: ReactNode; title: string; body: string }> = [
  { icon: <LockGlyph />, title: "Built for developers", body: "Powerful APIs and SDKs" },
  { icon: <GlobeGlyph />, title: "Secure by default", body: "Enterprise-grade security" },
  { icon: <ChartGlyph />, title: "Reliable infrastructure", body: "99.95% uptime SLA" },
  { icon: <HeartGlyph />, title: "Here to help", body: "Support when you need it" },
];

function FeatureRow() {
  return (
    <Card className="grid gap-7 px-7 py-7 sm:grid-cols-2 lg:grid-cols-4">
      {FEATURES.map((feature) => (
        <div key={feature.title} className="flex items-center gap-4">
          <span className="chip size-11 shrink-0 text-gold">{feature.icon}</span>
          <div className="min-w-0">
            <p className="text-sm font-semibold text-ink">{feature.title}</p>
            <p className="mt-0.5 text-[13px] text-muted">{feature.body}</p>
          </div>
        </div>
      ))}
    </Card>
  );
}

/* --- glyphs ---------------------------------------------------------------- */

function LockGlyph() {
  return (
    <svg viewBox="0 0 24 24" fill="none" className="size-[18px]" aria-hidden="true">
      <rect x="4" y="10" width="16" height="11" rx="3" stroke="currentColor" strokeWidth="1.7" />
      <path d="M8 10V7a4 4 0 1 1 8 0v3" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" />
    </svg>
  );
}

function GlobeGlyph() {
  return (
    <svg viewBox="0 0 24 24" fill="none" className="size-[18px]" aria-hidden="true">
      <circle cx="12" cy="12" r="9" stroke="currentColor" strokeWidth="1.7" />
      <path d="M3 12h18M12 3a15 15 0 0 1 0 18M12 3a15 15 0 0 0 0 18" stroke="currentColor" strokeWidth="1.7" />
    </svg>
  );
}

function ChartGlyph() {
  return (
    <svg viewBox="0 0 24 24" fill="none" className="size-[18px]" aria-hidden="true">
      <path d="M5 20V11M12 20V5m7 15v-6" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" />
    </svg>
  );
}

function HeartGlyph() {
  return (
    <svg viewBox="0 0 24 24" fill="none" className="size-[18px]" aria-hidden="true">
      <path
        d="M12 20.2 4.3 12.7a4.6 4.6 0 0 1 6.5-6.5l1.2 1.2 1.2-1.2a4.6 4.6 0 1 1 6.5 6.5L12 20.2Z"
        stroke="currentColor" strokeWidth="1.7" strokeLinejoin="round"
      />
    </svg>
  );
}

function ArrowIcon() {
  return (
    <svg viewBox="0 0 24 24" fill="none" className="size-4" aria-hidden="true">
      <path d="M5 12h13m0 0-5-5m5 5-5 5" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}
