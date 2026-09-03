import { useCallback, useEffect, useRef } from "react";
import { EmbeddedCheckout, EmbeddedCheckoutProvider } from "@stripe/react-stripe-js";
import { isStripeConfigured, stripePromise } from "@/lib/stripe";
import { Alert, Button } from "@/components/ui";

interface CheckoutModalProps {
  clientSecret: string;
  onClose: () => void;
}

/**
 * Stripe's embedded Checkout, mounted in a dialog.
 *
 * This uses EmbeddedCheckout rather than a bare PaymentElement, and the
 * difference matters for subscriptions. PaymentElement confirms a single
 * PaymentIntent: to subscribe with it you must first create the subscription
 * server-side with payment_behavior=default_incomplete, pull the client secret
 * off the first invoice, confirm it here, then handle SCA, 3DS redirects and
 * trials by hand. Embedded Checkout does all of that inside the iframe and
 * still finishes as checkout.session.completed, which is the webhook path this
 * backend already implements and tests.
 *
 * The trade is control over layout. If this ever needs to be a bespoke form,
 * the swap is PaymentElement plus a server route returning the invoice's
 * client secret - not a change to how subscriptions are recorded.
 */
export function CheckoutModal({ clientSecret, onClose }: CheckoutModalProps) {
  const dialogRef = useRef<HTMLDivElement>(null);
  const previouslyFocused = useRef<Element | null>(null);

  useEffect(() => {
    previouslyFocused.current = document.activeElement;
    dialogRef.current?.focus();

    const onKeyDown = (event: KeyboardEvent): void => {
      if (event.key === "Escape") onClose();
    };
    document.addEventListener("keydown", onKeyDown);

    // The page behind a modal must not scroll; the checkout iframe scrolls
    // within its own container instead.
    const { overflow } = document.body.style;
    document.body.style.overflow = "hidden";

    return () => {
      document.removeEventListener("keydown", onKeyDown);
      document.body.style.overflow = overflow;
      (previouslyFocused.current as HTMLElement | null)?.focus?.();
    };
  }, [onClose]);

  // EmbeddedCheckoutProvider re-mounts the iframe whenever options change
  // identity, so the secret is handed over through a stable callback.
  const fetchClientSecret = useCallback(() => Promise.resolve(clientSecret), [clientSecret]);

  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-canvas/85 p-4 backdrop-blur-md sm:p-8"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) onClose();
      }}
    >
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-label="Complete your subscription"
        tabIndex={-1}
        className="panel my-auto w-full max-w-2xl overflow-hidden outline-none"
      >
        <header className="flex items-center justify-between border-b border-white/[0.07] px-7 py-5">
          <div>
            <h2 className="font-display text-lg text-ink">Complete your subscription</h2>
            <p className="mt-1 text-xs text-faint">Payments are processed by Stripe.</p>
          </div>
          <Button variant="ghost" onClick={onClose} aria-label="Close checkout" className="px-2.5">
            <svg viewBox="0 0 20 20" fill="currentColor" aria-hidden="true" className="size-5">
              <path d="M6.3 6.3a1 1 0 0 1 1.4 0L10 8.6l2.3-2.3a1 1 0 1 1 1.4 1.4L11.4 10l2.3 2.3a1 1 0 0 1-1.4 1.4L10 11.4l-2.3 2.3a1 1 0 0 1-1.4-1.4L8.6 10 6.3 7.7a1 1 0 0 1 0-1.4Z" />
            </svg>
          </Button>
        </header>

        <div className="embedded-checkout p-2 sm:p-4">
          {isStripeConfigured ? (
            <EmbeddedCheckoutProvider stripe={stripePromise} options={{ fetchClientSecret }}>
              <EmbeddedCheckout className="w-full" />
            </EmbeddedCheckoutProvider>
          ) : (
            <div className="p-4">
              <Alert title="Stripe is not configured" tone="warn">
                Set <code className="font-mono">VITE_STRIPE_PUBLISHABLE_KEY</code> in{" "}
                <code className="font-mono">web/.env</code> and restart the dev server.
              </Alert>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
