import type { SubscriptionStatus } from "@/api/types";

interface Descriptor {
  label: string;
  className: string;
  dot: string;
  /** Shown on the dashboard beneath the status. */
  help: string;
}

/**
 * Every status the backend can report gets an explicit entry. `past_due` is
 * deliberately a warning rather than an error: Stripe is still retrying the
 * card, access is still granted, and telling the user their plan is dead while
 * dunning is in progress churns customers who would have paid.
 */
const DESCRIPTORS: Record<SubscriptionStatus, Descriptor> = {
  active: {
    label: "Active",
    className: "border-ok/30 bg-ok-soft/40 text-ok",
    dot: "bg-ok",
    help: "Your subscription is active and renews automatically.",
  },
  trialing: {
    label: "Trialing",
    className: "border-gold/35 bg-gold-soft/40 text-gold",
    dot: "bg-gold",
    help: "You're on a free trial. Billing starts when it ends.",
  },
  past_due: {
    label: "Past due",
    className: "border-warn/30 bg-warn-soft/40 text-warn",
    dot: "bg-warn",
    help: "The last payment failed. We're retrying — access continues meanwhile.",
  },
  unpaid: {
    label: "Unpaid",
    className: "border-bad/30 bg-bad-soft/40 text-bad",
    dot: "bg-bad",
    help: "Payment retries were exhausted. Update your card to restore access.",
  },
  canceled: {
    label: "Canceled",
    className: "border-white/10 bg-white/[0.05] text-faint",
    dot: "bg-faint",
    help: "This subscription has ended.",
  },
  paused: {
    label: "Paused",
    className: "border-white/10 bg-white/[0.05] text-muted",
    dot: "bg-muted",
    help: "Collection is paused. No charges are being made.",
  },
  incomplete: {
    label: "Incomplete",
    className: "border-warn/30 bg-warn-soft/40 text-warn",
    dot: "bg-warn",
    help: "The first payment hasn't finished. It may need extra authentication.",
  },
  incomplete_expired: {
    label: "Expired",
    className: "border-white/10 bg-white/[0.05] text-faint",
    dot: "bg-faint",
    help: "The first payment was never completed, so the subscription expired.",
  },
};

export function describeStatus(status: SubscriptionStatus): Descriptor {
  return (
    DESCRIPTORS[status] ?? {
      label: status,
      className: "border-white/10 bg-white/[0.05] text-muted",
      dot: "bg-muted",
      help: "",
    }
  );
}

export function StatusBadge({ status }: { status: SubscriptionStatus }) {
  const { label, className, dot } = describeStatus(status);
  return (
    <span
      className={`inline-flex items-center gap-2 rounded-full border px-3 py-1 text-[11px] font-semibold tracking-wide ${className}`}
    >
      <span className={`size-1.5 rounded-full ${dot}`} aria-hidden="true" />
      {label}
    </span>
  );
}
