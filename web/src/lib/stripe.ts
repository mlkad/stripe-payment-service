import { loadStripe, type Stripe } from "@stripe/stripe-js";

const publishableKey = import.meta.env.VITE_STRIPE_PUBLISHABLE_KEY;

/**
 * loadStripe injects a script tag, so it is called once at module scope and the
 * promise is shared. Calling it per render would add a script tag per render.
 *
 * Resolves to null when the key is missing, which lets the UI show a
 * configuration error instead of throwing inside a provider on first paint.
 */
export const stripePromise: Promise<Stripe | null> = publishableKey
  ? loadStripe(publishableKey)
  : Promise.resolve(null);

export const isStripeConfigured = Boolean(publishableKey);

if (import.meta.env.DEV && publishableKey?.startsWith("sk_")) {
  // A secret key in a bundle is an incident, not a typo: everything under VITE_
  // is compiled into the JavaScript served to the browser.
  throw new Error(
    "VITE_STRIPE_PUBLISHABLE_KEY holds a secret key (sk_). Use the publishable key (pk_) and rotate the leaked one.",
  );
}
