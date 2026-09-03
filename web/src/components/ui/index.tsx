import type { ButtonHTMLAttributes, HTMLAttributes, ReactNode } from "react";

function cx(...parts: Array<string | false | null | undefined>): string {
  return parts.filter(Boolean).join(" ");
}

/* --- Button ---------------------------------------------------------------- */

type Variant = "primary" | "secondary" | "ghost" | "outline";

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant;
  loading?: boolean;
  full?: boolean;
}

const VARIANTS: Record<Variant, string> = {
  primary: "btn-gold font-semibold",
  secondary:
    "bg-white/[0.04] text-ink border border-white/10 hover:bg-white/[0.08] hover:border-white/20",
  outline:
    "bg-transparent text-ink border border-white/12 hover:border-gold/50 hover:bg-white/[0.03]",
  ghost: "text-muted hover:text-ink hover:bg-white/[0.05]",
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
        "inline-flex items-center justify-center gap-2 rounded-full px-6 py-3",
        "text-sm tracking-tight transition-all duration-200",
        "disabled:cursor-not-allowed disabled:opacity-50",
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
      <path d="M22 12a10 10 0 0 1-10 10" stroke="currentColor" strokeWidth="3" strokeLinecap="round" />
    </svg>
  );
}

/* --- Card ------------------------------------------------------------------ */

interface CardProps extends HTMLAttributes<HTMLDivElement> {
  /** Applies the gold rim and outer bloom used for the featured plan. */
  featured?: boolean;
}

export function Card({ featured = false, className, children, ...rest }: CardProps) {
  return (
    <div {...rest} className={cx("panel", featured && "panel-gold", className)}>
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
  error: "border-bad/30 bg-bad-soft/30 text-bad",
  warn: "border-warn/30 bg-warn-soft/30 text-warn",
  info: "border-white/10 bg-white/[0.04] text-muted",
} as const;

export function Alert({ tone = "error", title, children, action }: AlertProps) {
  return (
    <div
      role={tone === "error" ? "alert" : "status"}
      className={cx(
        "flex flex-wrap items-start gap-3 rounded-2xl border px-5 py-4 backdrop-blur-xl",
        TONES[tone],
      )}
    >
      <div className="min-w-0 flex-1">
        <p className="text-sm font-medium">{title}</p>
        {children && <div className="mt-1 text-sm opacity-80">{children}</div>}
      </div>
      {action}
    </div>
  );
}

/* --- Skeleton -------------------------------------------------------------- */

export function Skeleton({ className }: { className?: string }) {
  return <div className={cx("animate-pulse rounded-xl bg-white/[0.06]", className)} aria-hidden="true" />;
}

/* --- Decorative ------------------------------------------------------------ */

/** Four-point sparkle, as scattered around the hero and the sign-in card. */
export function Sparkle({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 24 24" className={cx("pointer-events-none", className)} aria-hidden="true">
      <path
        d="M12 0c.6 6.4 5 10.8 12 12-7 1.2-11.4 5.6-12 12-.6-6.4-5-10.8-12-12C7 10.8 11.4 6.4 12 0Z"
        fill="currentColor"
      />
    </svg>
  );
}
