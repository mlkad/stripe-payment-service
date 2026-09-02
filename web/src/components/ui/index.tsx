import type { ButtonHTMLAttributes, HTMLAttributes, ReactNode } from "react";

function cx(...parts: Array<string | false | null | undefined>): string {
  return parts.filter(Boolean).join(" ");
}

/* --- Button ---------------------------------------------------------------- */

type Variant = "primary" | "secondary" | "ghost" | "danger";

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant;
  loading?: boolean;
  full?: boolean;
}

const VARIANTS: Record<Variant, string> = {
  primary:
    "bg-brand text-white shadow-lg shadow-brand/25 hover:brightness-110 active:brightness-95",
  secondary: "bg-raised text-ink border border-line hover:border-faint hover:bg-raised/70",
  ghost: "text-muted hover:text-ink hover:bg-raised/60",
  danger: "bg-bad-soft text-bad border border-bad/30 hover:bg-bad/20",
};

export function Button({
  variant = "primary",
  loading = false,
  full = false,
  className,
  children,
  disabled,
  ...rest
}: ButtonProps) {
  return (
    <button
      {...rest}
      disabled={disabled ?? loading}
      // aria-busy rather than swapping the label: replacing the text would move
      // focus for a screen reader mid-action.
      aria-busy={loading}
      className={cx(
        "relative inline-flex items-center justify-center gap-2 rounded-xl px-4 py-2.5",
        "text-sm font-medium transition-all duration-150",
        "disabled:cursor-not-allowed disabled:opacity-55",
        full && "w-full",
        VARIANTS[variant],
        className,
      )}
    >
      {loading && <Spinner className="size-4" />}
      {children}
    </button>
  );
}

/* --- Spinner --------------------------------------------------------------- */

export function Spinner({ className }: { className?: string }) {
  return (
    <svg className={cx("animate-spin", className)} viewBox="0 0 24 24" fill="none" aria-hidden="true">
      <circle cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="3" className="opacity-25" />
      <path
        d="M22 12a10 10 0 0 1-10 10"
        stroke="currentColor"
        strokeWidth="3"
        strokeLinecap="round"
      />
    </svg>
  );
}

/* --- Card ------------------------------------------------------------------ */

export function Card({ className, children, ...rest }: HTMLAttributes<HTMLDivElement>) {
  return (
    <div {...rest} className={cx("card", className)}>
      {children}
    </div>
  );
}

/* --- Alert ----------------------------------------------------------------- */

interface AlertProps {
  tone?: "error" | "warn" | "info";
  title: string;
  children?: ReactNode;
  action?: ReactNode;
}

const TONES = {
  error: "border-bad/30 bg-bad-soft/40 text-bad",
  warn: "border-warn/30 bg-warn-soft/40 text-warn",
  info: "border-line bg-raised/60 text-muted",
} as const;

export function Alert({ tone = "error", title, children, action }: AlertProps) {
  return (
    <div
      role={tone === "error" ? "alert" : "status"}
      className={cx("flex flex-wrap items-start gap-3 rounded-xl border p-4", TONES[tone])}
    >
      <div className="min-w-0 flex-1">
        <p className="text-sm font-semibold">{title}</p>
        {children && <div className="mt-1 text-sm opacity-80">{children}</div>}
      </div>
      {action}
    </div>
  );
}

/* --- Skeleton -------------------------------------------------------------- */

export function Skeleton({ className }: { className?: string }) {
  return <div className={cx("animate-pulse rounded-lg bg-raised", className)} aria-hidden="true" />;
}
