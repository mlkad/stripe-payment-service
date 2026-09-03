import type { AuthResponse, User } from "@/api/types";

export interface Session {
  token: string;
  expiresAt: string;
  user: User;
}

/**
 * Access token storage — in memory only.
 *
 * It is never written to localStorage, so a script that runs on this origin
 * cannot read it out of storage, and it does not survive a reload. The refresh
 * cookie restores the session instead.
 *
 * The refresh token is not here at all: it lives in an httpOnly cookie the
 * browser attaches to /api/v1/auth only, so script cannot read it even during
 * an XSS. That is the point of the split — the token script can reach expires
 * in minutes, and the one that lasts a month is unreachable.
 *
 * The cost is a flash of the signed-out shell on reload while the refresh call
 * completes. `hadSession` trades a single non-sensitive bit of localStorage for
 * avoiding that: it says a session probably exists, so the UI can wait rather
 * than render the login form and then replace it.
 */
const HINT_KEY = "sps.session_hint";

let current: Session | null = null;

export const session = {
  get(): Session | null {
    return current;
  },

  set(response: AuthResponse): Session {
    current = {
      token: response.token,
      expiresAt: response.expires_at,
      user: response.user,
    };
    try {
      localStorage.setItem(HINT_KEY, "1");
    } catch {
      // Private mode, or storage disabled. Only costs the reload flash.
    }
    return current;
  },

  clear(): void {
    current = null;
    try {
      localStorage.removeItem(HINT_KEY);
    } catch {
      /* nothing to do */
    }
  },

  /** Whether a session probably exists, for deciding what to render on load. */
  hadSession(): boolean {
    try {
      return localStorage.getItem(HINT_KEY) === "1";
    } catch {
      return false;
    }
  },

  /** True when the access token is within `skewMs` of expiring. */
  isExpiring(skewMs = 30_000): boolean {
    if (!current) return true;
    return Date.parse(current.expiresAt) - Date.now() <= skewMs;
  },
};
