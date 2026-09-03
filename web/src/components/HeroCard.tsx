import type { ReactNode } from "react";
import { Card, Sparkle } from "@/components/ui";

interface HeroCardProps {
  eyebrow: string;
  title: ReactNode;
  lines: string[];
  action?: ReactNode;
}

/**
 * The banner at the top of the dashboard: a floating card render on the left,
 * the account's headline state on the right.
 *
 * The card and its orbit are drawn in CSS rather than shipped as an image, so
 * they stay sharp at any density and cost nothing to download.
 */
export function HeroCard({ eyebrow, title, lines, action }: HeroCardProps) {
  return (
    <Card className="relative overflow-hidden px-6 py-10 sm:px-10 sm:py-12">
      <Sparkle className="absolute right-[14%] top-[18%] size-4 text-gold/70" />
      <Sparkle className="absolute right-[9%] top-[30%] size-2 text-gold/45" />
      <Sparkle className="absolute right-[19%] top-[44%] size-1.5 text-gold/35" />

      {/* A soft wave along the lower edge, echoing the mock's backdrop. */}
      <svg
        className="pointer-events-none absolute inset-x-0 bottom-0 h-24 w-full text-white/[0.04]"
        viewBox="0 0 1200 120" preserveAspectRatio="none" aria-hidden="true"
      >
        <path d="M0 80C260 20 520 110 780 70s340-70 420-52V120H0Z" fill="currentColor" />
      </svg>

      <div className="relative grid items-center gap-10 lg:grid-cols-[minmax(0,0.9fr)_minmax(0,1.1fr)]">
        <FloatingCard />

        <div>
          <span className="eyebrow inline-flex rounded-full border border-white/10 bg-white/[0.04] px-4 py-1.5">
            {eyebrow}
          </span>

          <h1 className="mt-5 font-display text-[2.6rem] font-normal leading-[1.08] tracking-tight text-ink sm:text-5xl">
            {title}
          </h1>

          <div className="mt-4 space-y-1 text-[15px] leading-relaxed text-muted">
            {lines.map((line) => (
              <p key={line}>{line}</p>
            ))}
          </div>

          {action && <div className="mt-8">{action}</div>}
        </div>
      </div>
    </Card>
  );
}

/** The tilted glass card with its orbital rings and corner bloom. */
function FloatingCard() {
  return (
    <div className="relative mx-auto grid h-56 w-full max-w-sm place-items-center sm:h-64">
      {/* Warm bloom behind the card. */}
      <div
        className="absolute left-[8%] top-[58%] size-40 rounded-full blur-3xl"
        style={{ background: "radial-gradient(circle, color-mix(in oklch, var(--color-gold) 55%, transparent), transparent 70%)" }}
        aria-hidden="true"
      />

      {/* Two ellipses, counter-rotated, reading as an orbit around the card. */}
      <svg className="absolute inset-0 h-full w-full" viewBox="0 0 400 260" fill="none" aria-hidden="true">
        <ellipse
          cx="200" cy="130" rx="185" ry="86" transform="rotate(-19 200 130)"
          stroke="url(#orbit)" strokeWidth="1.4"
        />
        <ellipse
          cx="200" cy="130" rx="160" ry="66" transform="rotate(14 200 130)"
          stroke="url(#orbit)" strokeWidth="1" opacity="0.55"
        />
        <defs>
          <linearGradient id="orbit" x1="0" y1="0" x2="400" y2="260" gradientUnits="userSpaceOnUse">
            <stop stopColor="var(--color-gold-bright)" stopOpacity="0.9" />
            <stop offset="0.5" stopColor="var(--color-gold)" stopOpacity="0.25" />
            <stop offset="1" stopColor="var(--color-gold)" stopOpacity="0" />
          </linearGradient>
        </defs>
      </svg>

      <div
        className="relative h-36 w-56 rounded-2xl border border-white/15 p-4 sm:h-40 sm:w-64"
        style={{
          transform: "rotate(-11deg)",
          background:
            "linear-gradient(145deg, color-mix(in oklch, white 13%, transparent), color-mix(in oklch, white 3%, transparent) 55%, transparent)",
          backdropFilter: "blur(14px)",
          boxShadow:
            "0 1px 0 0 rgb(255 255 255 / 0.22) inset, 0 22px 45px -18px rgb(0 0 0 / 0.85), 0 0 30px -10px color-mix(in oklch, var(--color-gold) 45%, transparent)",
        }}
      >
        <div className="flex items-start justify-between">
          <span className="text-[11px] font-medium tracking-wide text-ink/80">Stripe Gateway</span>
          <span className="text-[11px] text-ink/50">✳</span>
        </div>

        {/* The chip. */}
        <div className="mt-5 grid h-6 w-8 grid-cols-2 grid-rows-2 gap-px overflow-hidden rounded-[3px] bg-white/25">
          {Array.from({ length: 4 }, (_, i) => (
            <span key={i} className="bg-white/40" />
          ))}
        </div>

        <span
          className="absolute bottom-3 right-4 font-display text-2xl italic"
          style={{ color: "color-mix(in oklch, var(--color-gold) 85%, white)" }}
        >
          S
        </span>
      </div>
    </div>
  );
}
