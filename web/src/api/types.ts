/** Mirrors domain.SubscriptionStatus in the Go service and the
 *  subscription_status enum in migration 00002. The three must not drift. */
export type SubscriptionStatus =
  | "incomplete"
  | "incomplete_expired"
  | "trialing"
  | "active"
  | "past_due"
  | "canceled"
  | "unpaid"
  | "paused";

/** Mirrors service.SubscriptionView. */
export interface Subscription {
  status: SubscriptionStatus;
  is_active: boolean;
  price_id: string;
  quantity: number;
  currency?: string;
  unit_amount?: number;
  current_period_end: string;
  cancel_at_period_end: boolean;
  canceled_at?: string;
  trial_end?: string;
}

export type UIMode = "hosted" | "embedded";

/** No user_id: the caller is the bearer token's subject. */
export interface CheckoutRequest {
  price_id: string;
  quantity?: number;
  trial_period_days?: number;
  ui_mode?: UIMode;
}

export interface PortalSession {
  url: string;
}

/** Exactly one of url and client_secret is present, matching the ui_mode asked
 *  for: url for the hosted redirect, client_secret for the embedded form. */
export interface CheckoutSession {
  session_id: string;
  url?: string;
  client_secret?: string;
}

export interface Plan {
  id: string;
  /** Stripe price id. Must also appear in the backend's STRIPE_ALLOWED_PRICE_IDS. */
  priceId: string;
  name: string;
  tagline: string;
  amount: number;
  currency: string;
  interval: "month" | "year";
  features: string[];
  featured?: boolean;
}

/* --- auth ----------------------------------------------------------------- */

export interface User {
  id: string;
  email: string;
  full_name?: string;
}

export interface AuthResponse {
  token: string;
  expires_at: string;
  user: User;
}

export interface RegisterRequest {
  email: string;
  password: string;
  full_name?: string;
}

export interface LoginRequest {
  email: string;
  password: string;
}
