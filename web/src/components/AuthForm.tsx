import { useState, type FormEvent } from "react";
import { ApiError } from "@/api/client";
import { useAuth } from "@/hooks/useAuth";
import { Alert, Button, Card } from "@/components/ui";

type Mode = "login" | "register";

/** Mirrors auth.MinPasswordBytes in the Go service. */
const MIN_PASSWORD_LENGTH = 12;

export function AuthForm() {
  const { login, register } = useAuth();
  const [mode, setMode] = useState<Mode>("login");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
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
      setError(
        cause instanceof ApiError ? cause.message : "Something went wrong. Please try again.",
      );
      setIsSubmitting(false);
    }
    // On success the provider swaps this component out, so there is no state
    // left to reset.
  }

  function switchMode(): void {
    setMode(isRegister ? "login" : "register");
    setError(null);
    setPassword("");
  }

  return (
    <div className="mx-auto flex min-h-[70dvh] w-full max-w-md items-center">
      <Card className="w-full p-8">
        <h1 className="text-xl font-semibold tracking-tight">
          {isRegister ? "Create an account" : "Sign in"}
        </h1>
        <p className="mt-1.5 text-sm text-muted">
          {isRegister ? "Start a subscription in under a minute." : "Welcome back."}
        </p>

        <form onSubmit={onSubmit} className="mt-7 space-y-4" noValidate>
          <Field
            id="email"
            label="Email"
            type="email"
            value={email}
            onChange={setEmail}
            autoComplete="email"
            required
          />
          <Field
            id="password"
            label="Password"
            type="password"
            value={password}
            onChange={setPassword}
            // Tells a password manager whether to offer a saved credential or
            // to generate a new one.
            autoComplete={isRegister ? "new-password" : "current-password"}
            required
            {...(isRegister ? { hint: `At least ${MIN_PASSWORD_LENGTH} characters.` } : {})}
          />

          {error && <Alert title={error} tone="error" />}

          <Button type="submit" full loading={isSubmitting} className="!mt-6">
            {isRegister ? "Create account" : "Sign in"}
          </Button>
        </form>

        <p className="mt-6 text-center text-sm text-muted">
          {isRegister ? "Already have an account?" : "No account yet?"}{" "}
          <button
            type="button"
            onClick={switchMode}
            className="font-medium text-brand underline-offset-4 hover:underline"
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
  value: string;
  onChange: (value: string) => void;
  autoComplete: string;
  required?: boolean;
  hint?: string;
}

function Field({ id, label, type, value, onChange, autoComplete, required, hint }: FieldProps) {
  return (
    <div>
      <label htmlFor={id} className="block text-sm font-medium text-muted">
        {label}
      </label>
      <input
        id={id}
        type={type}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        autoComplete={autoComplete}
        required={required}
        aria-describedby={hint ? `${id}-hint` : undefined}
        className="mt-1.5 w-full rounded-xl border border-line bg-raised px-3.5 py-2.5 text-sm
                   text-ink placeholder:text-faint transition-colors
                   focus:border-brand focus:outline-none"
      />
      {hint && (
        <p id={`${id}-hint`} className="mt-1.5 text-xs text-faint">
          {hint}
        </p>
      )}
    </div>
  );
}
