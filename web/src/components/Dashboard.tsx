import type { Subscription } from "@/api/types";
import { ApiError } from "@/api/client";
import { daysUntil, formatDate, formatMoney } from "@/lib/format";
import { StatusBadge, describeStatus } from "@/components/StatusBadge";
import { HeroCard } from "@/components/HeroCard";
import { ManageBillingButton } from "@/components/ManageBillingButton";
import { Alert, Button, Card, Skeleton, Spinner } from "@/components/ui";

type State =
  | { status: "loading" }
  | { status: "ready"; subscription: Subscription | null }
  | { status: "error"; error: ApiError };

interface DashboardProps {
  state: State;
  isRefreshing: boolean;
  onRefresh: () => void;
  onChoosePlan: () => void;
}

export function Dashboard({ state, isRefreshing, onRefresh, onChoosePlan }: DashboardProps) {
  if (state.status === "loading") return <DashboardSkeleton />;

  if (state.status === "error") {
    return (
      <Alert
        title="Couldn't load your subscription"
        tone={state.error.isRetryable ? "warn" : "error"}
        action={
          state.error.isRetryable ? (
            <Button variant="secondary" onClick={onRefresh}>
              Try again
            </Button>
          ) : undefined
        }
      >
        {state.error.message}
        {state.error.requestId && (
          <span className="mt-1 block font-mono text-xs opacity-70">Reference: {state.error.requestId}</span>
        )}
      </Alert>
    );
  }

  const { subscription } = state;

  // No subscription is the ordinary first visit, not an error state - so it
  // gets the same hero treatment as an active plan rather than an empty box.
  if (!subscription) {
    return (
      <HeroCard
        eyebrow="Welcome back"
        title="No active subscription"
        lines={["Pick a plan to get started.", "You can change or cancel it at any time."]}
        action={
          <Button onClick={onChoosePlan} className="px-7 py-3.5 text-[15px]">
            View plans
            <ArrowIcon />
          </Button>
        }
      />
    );
  }

  const { help } = describeStatus(subscription.status);
  const remaining = daysUntil(subscription.current_period_end);
  const endsNotRenews = subscription.cancel_at_period_end || !subscription.is_active;

  return (
    <div className="space-y-6">
      <HeroCard
        eyebrow="Your plan"
        title={<>Subscription <span className="swash italic">active</span></>}
        lines={[help]}
        action={
          <div className="flex flex-wrap items-center gap-3">
            <ManageBillingButton />
            {!subscription.is_active && (
              <Button onClick={onChoosePlan}>Choose a new plan</Button>
            )}
          </div>
        }
      />

      <Card className="overflow-hidden">
        <div className="flex flex-wrap items-center justify-between gap-4 border-b border-white/[0.07] px-7 py-5">
          <div className="flex items-center gap-3">
            <h2 className="font-display text-lg text-ink">Details</h2>
            <StatusBadge status={subscription.status} />
            {isRefreshing && <Spinner className="size-3.5 text-faint" />}
          </div>
          <Button variant="ghost" onClick={onRefresh} disabled={isRefreshing} className="px-4 py-2">
            Refresh
          </Button>
        </div>

        <dl className="grid gap-px bg-white/[0.06] sm:grid-cols-2 lg:grid-cols-4">
          <Field label="Plan">
            <span className="font-mono text-[13px]">{subscription.price_id}</span>
          </Field>

          <Field label="Amount">
            {subscription.unit_amount != null && subscription.currency ? (
              <>
                {formatMoney(subscription.unit_amount, subscription.currency)}
                {subscription.quantity > 1 && <span className="text-faint"> × {subscription.quantity}</span>}
              </>
            ) : (
              <span className="text-faint">—</span>
            )}
          </Field>

          <Field label={endsNotRenews ? "Access ends" : "Renews"}>
            {formatDate(subscription.current_period_end)}
            {remaining !== null && remaining >= 0 && <span className="text-faint"> · {remaining}d</span>}
          </Field>

          <Field label="Trial ends">
            {subscription.trial_end ? formatDate(subscription.trial_end) : <span className="text-faint">—</span>}
          </Field>
        </dl>

        {subscription.cancel_at_period_end && subscription.is_active && (
          <div className="p-7 pt-6">
            <Alert tone="warn" title="Set to cancel">
              Your plan stays active until {formatDate(subscription.current_period_end)} and will not renew.
            </Alert>
          </div>
        )}
      </Card>
    </div>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="bg-surface/60 px-7 py-6 backdrop-blur-xl">
      <dt className="eyebrow text-[10px]">{label}</dt>
      <dd className="mt-2 truncate text-[15px] font-medium text-ink">{children}</dd>
    </div>
  );
}

function DashboardSkeleton() {
  return (
    <Card className="overflow-hidden px-10 py-12" aria-busy="true">
      <div className="grid items-center gap-10 lg:grid-cols-2">
        <Skeleton className="h-56 w-full max-w-sm rounded-2xl" />
        <div className="space-y-4">
          <Skeleton className="h-6 w-32 rounded-full" />
          <Skeleton className="h-12 w-4/5" />
          <Skeleton className="h-4 w-3/5" />
          <Skeleton className="h-4 w-2/5" />
          <Skeleton className="mt-4 h-12 w-40 rounded-full" />
        </div>
      </div>
    </Card>
  );
}

function ArrowIcon() {
  return (
    <svg viewBox="0 0 24 24" fill="none" className="size-4" aria-hidden="true">
      <path d="M5 12h13m0 0-5-5m5 5-5 5" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}
