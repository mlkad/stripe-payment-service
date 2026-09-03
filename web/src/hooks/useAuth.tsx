import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { ApiError, api, setAuthBridge } from "@/api/client";
import type { LoginRequest, RegisterRequest, User } from "@/api/types";
import { session, type Session } from "@/lib/session";

interface AuthContextValue {
  user: User | null;
  isAuthenticated: boolean;
  /** True until the stored session has been restored or ruled out. */
  isBootstrapping: boolean;
  login: (body: LoginRequest) => Promise<void>;
  register: (body: RegisterRequest) => Promise<void>;
  logout: () => Promise<void>;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [current, setCurrent] = useState<Session | null>(() => session.get());
  // A hint that a session probably exists lets the shell wait rather than
  // render the login form and immediately replace it.
  const [isBootstrapping, setIsBootstrapping] = useState(() => session.hadSession());

  const sessionRef = useRef(current);
  sessionRef.current = current;

  const clearSession = useCallback(() => {
    session.clear();
    sessionRef.current = null;
    setCurrent(null);
  }, []);

  // Concurrent 401s must produce one renewal, not one per request. Without
  // this, five parallel calls would each rotate the refresh token and four
  // would present an already-consumed one - which the server correctly reads
  // as theft and ends the session.
  const inFlight = useRef<Promise<boolean> | null>(null);

  const renew = useCallback(async (): Promise<boolean> => {
    if (inFlight.current) return inFlight.current;

    const attempt = (async () => {
      try {
        const next = session.set(await api.refresh());
        sessionRef.current = next;
        setCurrent(next);
        return true;
      } catch {
        clearSession();
        return false;
      } finally {
        inFlight.current = null;
      }
    })();

    inFlight.current = attempt;
    return attempt;
  }, [clearSession]);

  // Installed before the first request so a renewal is available immediately.
  useEffect(() => {
    setAuthBridge({
      getToken: () => sessionRef.current?.token ?? null,
      refresh: renew,
      onUnauthorized: clearSession,
    });
  }, [renew, clearSession]);

  // The access token lives in memory, so a reload always starts without one.
  // The refresh cookie is what restores the session.
  useEffect(() => {
    if (!session.hadSession()) {
      setIsBootstrapping(false);
      return;
    }
    void renew().finally(() => setIsBootstrapping(false));
    // Runs once on mount: re-running would renew a token login just issued.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const login = useCallback(async (body: LoginRequest) => {
    setCurrent(session.set(await api.login(body)));
  }, []);

  const register = useCallback(async (body: RegisterRequest) => {
    setCurrent(session.set(await api.register(body)));
  }, []);

  const logout = useCallback(async () => {
    try {
      // Server-side first: this is what revokes the refresh family. Clearing
      // only the client would leave a live token in the cookie jar.
      await api.logout();
    } catch (error) {
      // A network failure must not trap the user in a signed-in shell.
      if (!(error instanceof ApiError)) throw error;
    } finally {
      clearSession();
    }
  }, [clearSession]);

  const value = useMemo<AuthContextValue>(
    () => ({
      user: current?.user ?? null,
      isAuthenticated: current !== null,
      isBootstrapping,
      login,
      register,
      logout,
    }),
    [current, isBootstrapping, login, register, logout],
  );

  return <AuthContext value={value}>{children}</AuthContext>;
}

export function useAuth(): AuthContextValue {
  const context = useContext(AuthContext);
  if (!context) throw new Error("useAuth must be used inside <AuthProvider>");
  return context;
}
