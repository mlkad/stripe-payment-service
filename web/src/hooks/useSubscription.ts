import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError, api } from "@/api/client";
import type { Subscription } from "@/api/types";

type State =
  | { status: "loading" }
  | { status: "ready"; subscription: Subscription | null }
  | { status: "error"; error: ApiError };

interface UseSubscription {
  state: State;
  refresh: () => void;
  /** True while a background refresh runs over already-rendered data. */
  isRefreshing: boolean;
}

/**
 * Loads the user's subscription and can poll for it.
 *
 * Polling exists because checkout is asynchronous end to end: Stripe redirects
 * the browser back the moment payment succeeds, but the local row is not
 * written until checkout.session.completed arrives on the webhook. That is
 * usually under a second and occasionally much longer, so the dashboard polls
 * rather than showing a stale empty state.
 */
export function useSubscription(userId: string, pollUntilActive = false): UseSubscription {
  const [state, setState] = useState<State>({ status: "loading" });
  const [isRefreshing, setIsRefreshing] = useState(false);
  const [nonce, setNonce] = useState(0);
  const hasLoaded = useRef(false);

  const refresh = useCallback(() => setNonce((n) => n + 1), []);

  useEffect(() => {
    if (!userId) return;

    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    let attempt = 0;

    const load = async (): Promise<void> => {
      if (hasLoaded.current) setIsRefreshing(true);
      try {
        const subscription = await api.getSubscription(userId, controller.signal);
        hasLoaded.current = true;
        setState({ status: "ready", subscription });

        // Back off from 1s toward 8s, and give up after ~45s rather than
        // polling a tab forever.
        if (pollUntilActive && !subscription?.is_active && attempt < 10) {
          const delay = Math.min(1000 * 2 ** Math.floor(attempt / 2), 8000);
          attempt += 1;
          timer = setTimeout(() => void load(), delay);
        }
      } catch (error) {
        if (controller.signal.aborted) return;
        setState({
          status: "error",
          error:
            error instanceof ApiError
              ? error
              : new ApiError(0, "Something went wrong.", null),
        });
      } finally {
        setIsRefreshing(false);
      }
    };

    void load();
    return () => {
      controller.abort();
      if (timer) clearTimeout(timer);
    };
  }, [userId, nonce, pollUntilActive]);

  return { state, refresh, isRefreshing };
}
