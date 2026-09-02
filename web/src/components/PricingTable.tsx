import { useState } from "react";
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

export function PricingTable({
  plans,
  currentPriceId,
  mode,
  onEmbeddedSession,
}: PricingTableProps) {
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
      setError(
        cause instanceof ApiError ? cause : new ApiError(0, "Checkout could not be started.", null),
      );
      setPendingPriceId(null);
    }
    // pendingPriceId is deliberately left set on the hosted success path: the
    // browser is navigating away, and clearing it would flash the idle button.
  }

  return (
    <section aria-labelledby="pricing-heading" className="space-y-6">
      <header className="text-center">
        <h2 id="pricing-heading" className="text-2xl font-semibold tracking-tight">
          Choose a plan
        </h2>
        <p className="mt-2 text-sm text-muted">
          Cancel any time. Prices in {plans[0]?.currency.toUpperCase() ?? "USD"}.
        </p>
      </header>

      {error && (
        <Alert title="Couldn't start checkout" tone={error.isRetryable ? "warn" : "error"}>
          {error.message}
          {error.requestId && (
            <span className="mt-1 block font-mono text-xs opacity-70">
              Reference: {error.requestId}
            </span>
          )}
        </Alert>
      )}

      <div className="grid gap-5 sm:grid-cols-2 lg:grid-cols-3">
        {plans.map((plan) => {
          const isCurrent = plan.priceId === currentPriceId;
          const isPending = pendingPriceId === plan.priceId;

          return (
            <Card
              key={plan.id}
              className={`relative flex flex-col p-6 transition-transform duration-200 ${
                plan.featured ? "ring-1 ring-brand/40 sm:-translate-y-2" : "hover:-translate-y-1"
              }`}
            >
              {plan.featured && (
                <span className="absolute -top-3 left-6 rounded-full bg-brand px-3 py-1 text-xs font-semibold text-white shadow-lg shadow-brand/30">
                  Most popular
                </span>
              )}

              <h3 className="text-lg font-semibold">{plan.name}</h3>
              <p className="mt-1 text-sm text-muted">{plan.tagline}</p>

              <p className="mt-5 flex items-baseline gap-1">
                <span className="text-4xl font-bold tracking-tight">
                  {formatMoney(plan.amount, plan.currency)}
                </span>
                <span className="text-sm text-faint">/{plan.interval}</span>
              </p>

              <ul className="mt-6 flex-1 space-y-3 text-sm">
                {plan.features.map((feature) => (
                  <li key={feature} className="flex items-start gap-2.5 text-muted">
                    <CheckIcon />
                    <span>{feature}</span>
                  </li>
                ))}
              </ul>

              <Button
                full
                className="mt-7"
                variant={plan.featured ? "primary" : "secondary"}
                loading={isPending}
                disabled={isCurrent || pendingPriceId !== null}
                onClick={() => void startCheckout(plan)}
              >
                {isCurrent ? "Current plan" : isPending ? "Starting…" : `Choose ${plan.name}`}
              </Button>
            </Card>
          );
        })}
      </div>
    </section>
  );
}

function CheckIcon() {
  return (
    <svg
      viewBox="0 0 20 20"
      fill="currentColor"
      aria-hidden="true"
      className="mt-0.5 size-4 shrink-0 text-ok"
    >
      <path
        fillRule="evenodd"
        d="M16.7 5.3a1 1 0 0 1 0 1.4l-7.5 7.5a1 1 0 0 1-1.4 0l-3.5-3.5a1 1 0 1 1 1.4-1.4l2.8 2.79 6.8-6.79a1 1 0 0 1 1.4 0Z"
        clipRule="evenodd"
      />
    </svg>
  );
}
