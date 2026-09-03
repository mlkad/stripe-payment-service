import { useState, type FormEvent } from "react";
import { ApiError } from "@/api/client";
import { useAuth } from "@/hooks/useAuth";
import { Alert, Button, Card, Sparkle } from "@/components/ui";

type Mode = "login" | "register";

/** Mirrors auth.MinPasswordBytes in the Go service. */
const MIN_PASSWORD_LENGTH = 12;

export function AuthForm() {
  const { login, register } = useAuth();
  const [mode, setMode] = useState<Mode>("login");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [showPassword, setShowPassword] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [isSubmitting, setIsSubmitting] = useState(false);

  const isRegister = mode === "register";

  async function onSubmit(event: FormEvent): Promise<void> {
    event.preventDefault();
    setError(null);

    // Checked here only to save a round trip; the server enforces the policy
    // and its answer is what counts.
    if (isRegister && password.length < MIN_PASSWORD_LENGTH) {
      setError(`Password must be at least ${MIN_PASSWORD_LENGTH} characters.`);
      return;
    }

    setIsSubmitting(true);
    try {
      await (isRegister ? register({ email, password }) : login({ email, password }));
    } catch (cause) {
      setError(cause instanceof ApiError ? cause.message : "Something went wrong. Please try again.");
      setIsSubmitting(false);
    }
    // On success the provider swaps this component out.
  }

  function switchMode(): void {
    setMode(isRegister ? "login" : "register");
    setError(null);
    setPassword("");
  }

  return (
    <div className="flex min-h-[72dvh] items-center justify-center px-2 py-10">
      <Card className="relative w-full max-w-md overflow-hidden px-8 py-10 sm:px-10 sm:py-12">
        <Sparkle className="absolute right-14 top-9 size-4 text-gold/70" />
        <Sparkle className="absolute right-8 top-16 size-2.5 text-gold/50" />
        <Sparkle className="absolute right-20 top-24 size-1.5 text-gold/40" />

        <h1 className="font-display text-4xl font-normal tracking-tight text-ink">
          {isRegister ? "Create account" : "Sign in"}
        </h1>
        <p className="mt-2 text-[15px] text-muted">
          {isRegister ? "Start a subscription in under a minute." : "Welcome back."}
        </p>

        <form onSubmit={onSubmit} className="mt-9 space-y-5" noValidate>
          <Field
            id="email"
            label="Email"
            type="email"
            placeholder="Enter your email"
            value={email}
            onChange={setEmail}
            autoComplete="email"
            icon={<MailIcon />}
            required
          />

          <Field
            id="password"
            label="Password"
            type={showPassword ? "text" : "password"}
            placeholder="Enter your password"
            value={password}
            onChange={setPassword}
            // Tells a password manager whether to offer a saved credential or
            // to generate a new one.
            autoComplete={isRegister ? "new-password" : "current-password"}
            icon={<LockIcon />}
            required
            trailing={
              <button
                type="button"
                onClick={() => setShowPassword((v) => !v)}
                aria-label={showPassword ? "Hide password" : "Show password"}
                className="text-faint transition-colors hover:text-gold"
              >
                <EyeIcon crossed={showPassword} />
              </button>
            }
            {...(isRegister ? { hint: `At least ${MIN_PASSWORD_LENGTH} characters.` } : {})}
          />

          {error && <Alert title={error} tone="error" />}

          <Button type="submit" full loading={isSubmitting} className="!mt-8 py-3.5 text-[15px]">
            {isRegister ? "Create account" : "Sign in"}
            {!isSubmitting && <ArrowIcon />}
          </Button>
        </form>

        <p className="mt-7 text-center text-sm text-muted">
          {isRegister ? "Already have an account?" : "No account yet?"}{" "}
          <button
            type="button"
            onClick={switchMode}
            className="font-semibold text-gold underline-offset-4 transition-colors hover:text-gold-bright hover:underline"
          >
            {isRegister ? "Sign in" : "Create one"}
          </button>
        </p>
      </Card>
    </div>
  );
}

interface FieldProps {
  id: string;
  label: string;
  type: string;
  placeholder: string;
  value: string;
  onChange: (value: string) => void;
  autoComplete: string;
  icon: React.ReactNode;
  required?: boolean;
  hint?: string;
  trailing?: React.ReactNode;
}

function Field({
  id, label, type, placeholder, value, onChange,
  autoComplete, icon, required, hint, trailing,
}: FieldProps) {
  return (
    <div>
      <label htmlFor={id} className="block text-sm font-medium text-ink">
        {label}
      </label>
      <div className="relative mt-2">
        <span className="pointer-events-none absolute left-5 top-1/2 -translate-y-1/2 text-gold/70">
          {icon}
        </span>
        <input
          id={id}
          type={type}
          value={value}
          placeholder={placeholder}
          onChange={(event) => onChange(event.target.value)}
          autoComplete={autoComplete}
          required={required}
          aria-describedby={hint ? `${id}-hint` : undefined}
          className="field py-3.5 pl-14 pr-12 text-[15px]"
        />
        {trailing && (
          <span className="absolute right-5 top-1/2 -translate-y-1/2">{trailing}</span>
        )}
      </div>
      {hint && (
        <p id={`${id}-hint`} className="mt-2 pl-1 text-xs text-faint">
          {hint}
        </p>
      )}
    </div>
  );
}

/* --- icons ----------------------------------------------------------------- */

function MailIcon() {
  return (
    <svg viewBox="0 0 24 24" fill="none" className="size-[18px]" aria-hidden="true">
      <rect x="2.5" y="4.5" width="19" height="15" rx="3" stroke="currentColor" strokeWidth="1.6" />
      <path d="m3.5 7 7.7 5.4a1.4 1.4 0 0 0 1.6 0L20.5 7" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
    </svg>
  );
}

function LockIcon() {
  return (
    <svg viewBox="0 0 24 24" fill="none" className="size-[18px]" aria-hidden="true">
      <rect x="4" y="10" width="16" height="11" rx="3" stroke="currentColor" strokeWidth="1.6" />
      <path d="M8 10V7a4 4 0 1 1 8 0v3" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
    </svg>
  );
}

function EyeIcon({ crossed }: { crossed: boolean }) {
  return (
    <svg viewBox="0 0 24 24" fill="none" className="size-[18px]" aria-hidden="true">
      <path
        d="M2.5 12S6 5.5 12 5.5 21.5 12 21.5 12 18 18.5 12 18.5 2.5 12 2.5 12Z"
        stroke="currentColor" strokeWidth="1.6"
      />
      <circle cx="12" cy="12" r="3" stroke="currentColor" strokeWidth="1.6" />
      {crossed && <path d="m4 20 16-16" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />}
    </svg>
  );
}

function ArrowIcon() {
  return (
    <svg viewBox="0 0 24 24" fill="none" className="size-4" aria-hidden="true">
      <path d="M5 12h13m0 0-5-5m5 5-5 5" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}
