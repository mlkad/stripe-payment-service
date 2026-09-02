import type { AuthResponse, User } from "@/api/types";

const STORAGE_KEY = "sps.session";

export interface Session {
  token: string;
  expiresAt: string;
  user: User;
}

/**
 * Token storage.
 *
 * localStorage is readable by any script on the origin, so a single XSS is a
 * stolen credential. An httpOnly cookie would remove that, at the cost of
 * needing CSRF protection and a same-site deployment, and the backend issues a
 * bearer token for the Authorization header rather than setting a cookie.
 *
 * What makes this acceptable rather than merely convenient: the token is short
 * lived (one hour by default), carries no privileges beyond the subject, and
 * the API rejects anything else the token could be pointed at. It is not
 * acceptable indefinitely - the upgrade is a refresh token in an httpOnly
 * cookie with the access token held in memory only.
 */
export const session = {
  load(): Session | null {
    let raw: string | null;
    try {
      raw = localStorage.getItem(STORAGE_KEY);
    } catch {
      // Private mode, or storage disabled entirely.
      return null;
    }
    if (!raw) return null;

    try {
      const parsed = JSON.parse(raw) as Session;
      if (!parsed.token || !parsed.user?.id) return null;
      // Drop a token that has already expired rather than sending it and
      // taking a 401 on first paint.
      if (Date.parse(parsed.expiresAt) <= Date.now()) {
        session.clear();
        return null;
      }
      return parsed;
    } catch {
      session.clear();
      return null;
    }
  },

  save(response: AuthResponse): Session {
    const value: Session = {
      token: response.token,
      expiresAt: response.expires_at,
      user: response.user,
    };
    try {
      localStorage.setItem(STORAGE_KEY, JSON.stringify(value));
    } catch {
      // Storage failures must not block sign-in; the session simply does not
      // survive a reload.
    }
    return value;
  },

  clear(): void {
    try {
      localStorage.removeItem(STORAGE_KEY);
    } catch {
      /* nothing to do */
    }
  },
};
