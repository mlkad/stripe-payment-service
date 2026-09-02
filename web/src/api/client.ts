import type { CheckoutRequest, CheckoutSession, Subscription } from "./types";

/** Empty by default so the Vite dev proxy keeps the browser on one origin. */
const BASE_URL = (import.meta.env.VITE_API_BASE_URL ?? "").replace(/\/$/, "");

/** The Go service answers every failure with {"error": "..."}. */
interface ErrorEnvelope {
  error?: string;
}

/**
 * A failed request, carrying the status so callers can branch without parsing
 * strings. `requestId` is the backend's correlation id, echoed on every
 * response, and is the one thing worth showing a user in an error state.
 */
export class ApiError extends Error {
  readonly status: number;
  readonly requestId: string | null;

  constructor(status: number, message: string, requestId: string | null) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.requestId = requestId;
  }

  /** 404 on the subscription route means "never subscribed", not a fault. */
  get isNotFound(): boolean {
    return this.status === 404;
  }

  /** 5xx and 502 are worth a retry button; 4xx are not. */
  get isRetryable(): boolean {
    return this.status >= 500 || this.status === 0;
  }
}

interface RequestOptions {
  method?: "GET" | "POST";
  body?: unknown;
  signal?: AbortSignal;
}

async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { method = "GET", body, signal } = options;

  // Built up rather than declared inline: exactOptionalPropertyTypes rejects
  // an explicit `undefined` where RequestInit expects the key to be absent.
  const init: RequestInit = { method };
  if (signal) init.signal = signal;
  if (body !== undefined) {
    init.headers = { "Content-Type": "application/json" };
    init.body = JSON.stringify(body);
  }

  let response: Response;
  try {
    response = await fetch(`${BASE_URL}${path}`, init);
  } catch (cause) {
    // fetch rejects only on network failure; an aborted request is the caller
    // unmounting, not an error worth surfacing.
    if (cause instanceof DOMException && cause.name === "AbortError") throw cause;
    throw new ApiError(0, "Could not reach the server. Check your connection.", null);
  }

  const requestId = response.headers.get("X-Request-Id");

  if (!response.ok) {
    let message = `Request failed (${response.status})`;
    try {
      const envelope = (await response.json()) as ErrorEnvelope;
      if (envelope.error) message = envelope.error;
    } catch {
      // A non-JSON body means a proxy or gateway answered, not our service.
    }
    throw new ApiError(response.status, message, requestId);
  }

  if (response.status === 204) return undefined as T;
  return (await response.json()) as T;
}

export const api = {
  /** Resolves to null when the user has never subscribed. */
  async getSubscription(userId: string, signal?: AbortSignal): Promise<Subscription | null> {
    try {
      return await request<Subscription>(
        `/api/v1/subscription?user_id=${encodeURIComponent(userId)}`,
        signal ? { signal } : {},
      );
    } catch (error) {
      if (error instanceof ApiError && error.isNotFound) return null;
      throw error;
    }
  },

  createCheckoutSession(body: CheckoutRequest, signal?: AbortSignal): Promise<CheckoutSession> {
    return request<CheckoutSession>("/api/v1/checkout", {
      method: "POST",
      body,
      ...(signal ? { signal } : {}),
    });
  },

  health(signal?: AbortSignal): Promise<{ status: string; version: string }> {
    return request("/healthz", signal ? { signal } : {});
  },
};
