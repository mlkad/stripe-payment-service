import { useState } from "react";
import { ApiError, api } from "@/api/client";
import { Alert, Button } from "@/components/ui";

/**
 * Sends the customer to Stripe's hosted billing portal, where they can cancel,
 * switch plan, or update their card.
 *
 * None of that is implemented in this app on purpose: the portal is
 * Stripe-hosted, so card details never touch our origin, and the changes come
 * back through the customer.subscription.* webhooks the backend already
 * handles. The dashboard picks them up on its next refresh.
 */
export function ManageBillingButton() {
  const [isOpening, setIsOpening] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function openPortal(): Promise<void> {
    setIsOpening(true);
    setError(null);
    try {
      const { url } = await api.createPortalSession();
      // A full navigation, not an SPA route: the destination is Stripe's origin.
      window.location.assign(url);
    } catch (cause) {
      setError(
        cause instanceof ApiError
          ? cause.isNotFound
            ? "You don't have a billing account yet. Choose a plan first."
            : cause.message
          : "Could not open the billing portal.",
      );
      setIsOpening(false);
    }
    // Left pending on success: the browser is navigating away, and clearing it
    // would flash the idle button.
  }

  return (
    <div className="space-y-3">
      <Button variant="outline" loading={isOpening} onClick={() => void openPortal()}>
        {isOpening ? "Opening…" : "Manage billing"}
      </Button>
      {error && <Alert title={error} tone="error" />}
    </div>
  );
}
