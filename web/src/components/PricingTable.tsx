import { useState, type ReactNode } from "react";
import { ApiError, api } from "@/api/client";
import type { Plan, UIMode } from "@/api/types";
import { formatMoney } from "@/lib/format";
import { Alert, Button, Card } from "@/components/ui";

interface PricingTableProps {
  plans: Plan[];
  currentPriceId?: string | undefined;
  /** "hosted" redirects to Stripe; "embedded" opens the in-app form. */
  mode: UIMode;
  onEmbeddedSession: (clientSecret: string) => void;
}

export function PricingTable({ plans, currentPriceId, mode, onEmbeddedSession }: PricingTableProps) {
  const [pendingPriceId, setPendingPriceId] = useState<string | null>(null);
  const [error, setError] = useState<ApiError | null>(null);

  async function startCheckout(plan: Plan): Promise<void> {
    setPendingPriceId(plan.priceId);
    setError(null);
    try {
      const session = await api.createCheckoutSession({
        price_id: plan.priceId,
        quantity: 1,
        ui_mode: mode,
      });

      if (mode === "embedded") {
        if (!session.client_secret) {
          throw new ApiError(502, "The server did not return a checkout secret.", null);
        }
        onEmbeddedSession(session.client_secret);
        return;
      }

      if (!session.url) {
        throw new ApiError(502, "The server did not return a checkout URL.", null);
      }
      // A full navigation, not an SPA route: the destination is Stripe's origin.
      window.location.assign(session.url);
    } catch (cause) {
      setError(cause instanceof ApiError ? cause : new ApiError(0, "Checkout could not be started.", null));
      setPendingPriceId(null);
    }
    // pendingPriceId is left set on the hosted success path: the browser is
    // navigating away, and clearing it would flash the idle button.
  }

  return (
    <section aria-labelledby="pricing-heading" className="space-y-12">
      <header className="text-center">
        <h2
          id="pricing-heading"
          className="font-display text-[2.75rem] font-normal leading-tight tracking-tight text-ink sm:text-5xl"
        >
          Choose a <span className="swash italic">plan</span>
        </h2>
        <p className="mt-5 text-[15px] text-muted">
          Cancel any time. Prices in {plans[0]?.currency.toUpperCase() ?? "USD"}.
        </p>
      </header>

      {error && (
        <Alert title="Couldn't start checkout" tone={error.isRetryable ? "warn" : "error"}>
          {error.message}
          {error.requestId && (
            <span className="mt-1 block font-mono text-xs opacity-70">Reference: {error.requestId}</span>
          )}
        </Alert>
      )}

      {/* The featured card sits a little proud of its neighbours, so items-start
          keeps the other two from stretching to match its height. */}
      <div className="grid items-start gap-6 lg:grid-cols-3">
        {plans.map((plan) => (
          <PlanCard
            key={plan.id}
            plan={plan}
            isCurrent={plan.priceId === currentPriceId}
            isPending={pendingPriceId === plan.priceId}
            disabled={pendingPriceId !== null}
            onChoose={() => void startCheckout(plan)}
          />
        ))}
      </div>
    </section>
  );
}

interface PlanCardProps {
  plan: Plan;
  isCurrent: boolean;
  isPending: boolean;
  disabled: boolean;
  onChoose: () => void;
}

function PlanCard({ plan, isCurrent, isPending, disabled, onChoose }: PlanCardProps) {
  const featured = Boolean(plan.featured);

  return (
    <div className={featured ? "relative lg:-mt-4" : "relative"}>
      {featured && (
        <span
          className="btn-gold absolute -top-3.5 left-1/2 z-10 flex -translate-x-1/2 items-center gap-1.5
                     whitespace-nowrap rounded-full px-4 py-1.5 text-xs font-semibold"
        >
          <CrownIcon />
          Most popular
        </span>
      )}

      <Card featured={featured} className="flex h-full flex-col p-7 sm:p-8">
        <div className="flex items-start gap-4">
          <span className={`chip size-12 shrink-0 ${featured ? "text-gold" : "text-muted"}`}>
            <PlanIcon id={plan.id} />
          </span>
          <div className="min-w-0">
            <h3 className="font-display text-xl font-semibold text-ink">{plan.name}</h3>
            <p className="mt-1 text-sm leading-snug text-muted">{plan.tagline}</p>
          </div>
        </div>

        <p className="mt-7 flex items-baseline gap-1.5">
          <span className="font-display text-2xl text-muted">$</span>
          <span className="font-display text-[3.25rem] font-medium leading-none tracking-tight text-ink">
            {Math.round(plan.amount / 100)}
          </span>
          <span className="text-sm text-faint">/{plan.interval}</span>
        </p>
        <span className="sr-only">{formatMoney(plan.amount, plan.currency)} per {plan.interval}</span>

        <ul className="mt-8 flex-1 space-y-3.5">
          {plan.features.map((feature) => (
            <li key={feature} className="flex items-start gap-3 text-[15px] text-muted">
              <CheckIcon />
              <span>{feature}</span>
            </li>
          ))}
        </ul>

        <Button
          full
          className="mt-9"
          variant={featured ? "primary" : "outline"}
          loading={isPending}
          disabled={isCurrent || disabled}
          onClick={onChoose}
        >
          {isCurrent ? "Current plan" : isPending ? "Starting…" : `Choose ${plan.name}`}
          {featured && !isPending && !isCurrent && <ArrowIcon />}
        </Button>
      </Card>
    </div>
  );
}

/* --- icons ----------------------------------------------------------------- */

function PlanIcon({ id }: { id: string }): ReactNode {
  if (id === "pro") {
    return <span className="font-display text-xl font-semibold text-gold">S</span>;
  }
  if (id === "scale") {
    return (
      <svg viewBox="0 0 24 24" fill="none" className="size-5 text-indigo-300" aria-hidden="true">
        <path d="M12 3 3 7.5l9 4.5 9-4.5L12 3Z" stroke="currentColor" strokeWidth="1.7" strokeLinejoin="round" />
        <path d="m3 12.5 9 4.5 9-4.5M3 17l9 4.5 9-4.5" stroke="currentColor" strokeWidth="1.7" strokeLinejoin="round" />
      </svg>
    );
  }
  return (
    <svg viewBox="0 0 24 24" fill="none" className="size-5" aria-hidden="true">
      <path d="M21 3 10.5 13.5M21 3l-6.8 18-3.7-7.5L3 9.8 21 3Z" stroke="currentColor" strokeWidth="1.7" strokeLinejoin="round" />
    </svg>
  );
}

function CheckIcon() {
  return (
    <svg viewBox="0 0 20 20" fill="none" aria-hidden="true" className="mt-0.5 size-4 shrink-0 text-gold">
      <path d="m4 10.5 4 4 8-9" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

function CrownIcon() {
  return (
    <svg viewBox="0 0 24 24" fill="currentColor" className="size-3.5" aria-hidden="true">
      <path d="M3 8.5 6.5 12 12 5l5.5 7L21 8.5 19.5 19h-15L3 8.5Z" />
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
