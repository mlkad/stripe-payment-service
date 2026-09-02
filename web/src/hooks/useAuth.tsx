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
  /** True until the stored session has been checked against the server. */
  isBootstrapping: boolean;
  login: (body: LoginRequest) => Promise<void>;
  register: (body: RegisterRequest) => Promise<void>;
  logout: () => void;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [current, setCurrent] = useState<Session | null>(() => session.load());
  const [isBootstrapping, setIsBootstrapping] = useState(current !== null);

  // Read through a ref so the bridge installed below never captures a stale
  // token; it is registered once and must see the latest value.
  const sessionRef = useRef(current);
  sessionRef.current = current;

  const logout = useCallback(() => {
    session.clear();
    sessionRef.current = null;
    setCurrent(null);
  }, []);

  // Installed before the first request so a restored token is attached to it.
  useEffect(() => {
    setAuthBridge({
      getToken: () => sessionRef.current?.token ?? null,
      onUnauthorized: logout,
    });
  }, [logout]);

  // A stored token can be expired, revoked, or issued by a server that has
  // since rotated its secret. Confirming it once on load means the UI never
  // renders a signed-in shell that every request then fails.
  useEffect(() => {
    if (!current) {
      setIsBootstrapping(false);
      return;
    }
    const controller = new AbortController();

    api
      .me(controller.signal)
      .then((user) => setCurrent((prev) => (prev ? { ...prev, user } : prev)))
      .catch((error: unknown) => {
        if (error instanceof ApiError && error.isUnauthorized) logout();
        // Any other failure is the network or the server, not the token:
        // signing the user out over a blip would be worse than proceeding.
      })
      .finally(() => setIsBootstrapping(false));

    return () => controller.abort();
    // Runs once: re-running on every session change would re-verify a token
    // that was just issued by login.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const login = useCallback(async (body: LoginRequest) => {
    setCurrent(session.save(await api.login(body)));
  }, []);

  const register = useCallback(async (body: RegisterRequest) => {
    setCurrent(session.save(await api.register(body)));
  }, []);

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
