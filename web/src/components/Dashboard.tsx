import type { Subscription } from "@/api/types";
import { ApiError } from "@/api/client";
import { daysUntil, formatDate, formatMoney } from "@/lib/format";
import { StatusBadge, describeStatus } from "@/components/StatusBadge";
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
          <span className="mt-1 block font-mono text-xs opacity-70">
            Reference: {state.error.requestId}
          </span>
        )}
      </Alert>
    );
  }

  const { subscription } = state;
  if (!subscription) return <EmptyState onChoosePlan={onChoosePlan} />;

  const { help } = describeStatus(subscription.status);
  const remaining = daysUntil(subscription.current_period_end);
  const endsNotRenews = subscription.cancel_at_period_end || !subscription.is_active;

  return (
    <Card className="overflow-hidden">
      <div className="flex flex-wrap items-start justify-between gap-4 border-b border-line p-6">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-3">
            <h2 className="text-lg font-semibold">Your subscription</h2>
            <StatusBadge status={subscription.status} />
            {isRefreshing && <Spinner className="size-3.5 text-faint" />}
          </div>
          {help && <p className="mt-2 max-w-prose text-sm text-muted">{help}</p>}
        </div>
        <Button variant="ghost" onClick={onRefresh} disabled={isRefreshing}>
          Refresh
        </Button>
      </div>

      <dl className="grid gap-px bg-line sm:grid-cols-2 lg:grid-cols-4">
        <Field label="Plan">
          <span className="font-mono text-sm">{subscription.price_id}</span>
        </Field>

        <Field label="Amount">
          {subscription.unit_amount != null && subscription.currency ? (
            <>
              {formatMoney(subscription.unit_amount, subscription.currency)}
              {subscription.quantity > 1 && (
                <span className="text-faint"> × {subscription.quantity}</span>
              )}
            </>
          ) : (
            <span className="text-faint">—</span>
          )}
        </Field>

        <Field label={endsNotRenews ? "Access ends" : "Renews"}>
          {formatDate(subscription.current_period_end)}
          {remaining !== null && remaining >= 0 && (
            <span className="text-faint"> · {remaining}d</span>
          )}
        </Field>

        <Field label="Trial ends">
          {subscription.trial_end ? (
            formatDate(subscription.trial_end)
          ) : (
            <span className="text-faint">—</span>
          )}
        </Field>
      </dl>

      {subscription.cancel_at_period_end && subscription.is_active && (
        <div className="p-6 pt-5">
          <Alert tone="warn" title="Set to cancel">
            Your plan stays active until {formatDate(subscription.current_period_end)} and will not
            renew.
          </Alert>
        </div>
      )}

      {!subscription.is_active && (
        <div className="p-6 pt-5">
          <Button onClick={onChoosePlan}>Choose a new plan</Button>
        </div>
      )}
    </Card>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="bg-surface px-6 py-5">
      <dt className="text-xs font-medium uppercase tracking-wider text-faint">{label}</dt>
      <dd className="mt-1.5 truncate text-sm font-medium">{children}</dd>
    </div>
  );
}

function EmptyState({ onChoosePlan }: { onChoosePlan: () => void }) {
  return (
    <Card className="p-10 text-center">
      <div className="mx-auto flex size-12 items-center justify-center rounded-full bg-brand-soft">
        <svg viewBox="0 0 24 24" fill="none" aria-hidden="true" className="size-6 text-brand">
          <path
            d="M12 6v12m6-6H6"
            stroke="currentColor"
            strokeWidth="2"
            strokeLinecap="round"
          />
        </svg>
      </div>
      <h2 className="mt-4 text-lg font-semibold">No active subscription</h2>
      <p className="mx-auto mt-2 max-w-sm text-sm text-muted">
        Pick a plan to get started. You can change or cancel it at any time.
      </p>
      <Button className="mt-6" onClick={onChoosePlan}>
        View plans
      </Button>
    </Card>
  );
}

function DashboardSkeleton() {
  return (
    <Card className="overflow-hidden" aria-busy="true">
      <div className="flex items-center justify-between border-b border-line p-6">
        <div className="space-y-2.5">
          <Skeleton className="h-5 w-44" />
          <Skeleton className="h-4 w-64" />
        </div>
        <Skeleton className="h-9 w-20" />
      </div>
      <div className="grid gap-px bg-line sm:grid-cols-2 lg:grid-cols-4">
        {Array.from({ length: 4 }, (_, i) => (
          <div key={i} className="space-y-2 bg-surface px-6 py-5">
            <Skeleton className="h-3 w-16" />
            <Skeleton className="h-4 w-24" />
          </div>
        ))}
      </div>
    </Card>
  );
}
