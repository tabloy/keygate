import { useQueryClient } from "@tanstack/react-query"
import { createContext, type ReactNode, useContext, useEffect, useRef, useState } from "react"
import { showToast } from "@/components/toast"
import { useI18n } from "@/i18n"
import { auth, ServiceUnavailableError } from "@/lib/api"

interface AuthUser {
  id: string
  email: string
  name: string
  avatar_url: string
  is_admin: boolean
  role: string // "owner" | "admin" | "user"
}

interface AuthContextType {
  user: AuthUser | null
  loading: boolean
  // The server could not be reached to confirm the session. Not a
  // sign-out: show a retry screen, never the login redirect.
  unavailable: boolean
  logout: () => Promise<void>
  refetch: () => Promise<void>
}

const AuthContext = createContext<AuthContextType>({
  user: null,
  loading: true,
  unavailable: false,
  logout: async () => {},
  refetch: async () => {},
})

export function AuthProvider({ children }: { children: ReactNode }) {
  const { t } = useI18n()
  const [user, setUser] = useState<AuthUser | null>(null)
  const [loading, setLoading] = useState(true)
  const [unavailable, setUnavailable] = useState(false)
  const retryTimer = useRef<number | undefined>(undefined)
  const attempt = useRef(0)

  // Everything cached was read as whoever was signed in. When that
  // changes — a logout, or someone else signing in in the same tab — it
  // is dropped, or the next person would be shown the last one's
  // licenses until each query happened to refetch.
  const queryClient = useQueryClient()
  const identity = useRef<string | null | undefined>(undefined)
  const settleIdentity = (id: string | null) => {
    if (identity.current !== undefined && identity.current !== id) {
      // Only what was read as a user; the public site config stays.
      queryClient.removeQueries({ predicate: (q) => q.queryKey[0] === "admin" || q.queryKey[0] === "portal" })
    }
    identity.current = id
  }

  const fetchUser = async () => {
    window.clearTimeout(retryTimer.current)
    try {
      const u = await auth.me()
      settleIdentity(u.id)
      setUser({ ...u, role: u.role || "user" })
      setUnavailable(false)
      attempt.current = 0
    } catch (e) {
      if (e instanceof ServiceUnavailableError) {
        // Keep whoever was signed in; try again with backoff
        // (2s, 4s, 8s, 16s, then every 30s) until the server answers.
        setUnavailable(true)
        const delay = Math.min(30_000, 2_000 * 2 ** attempt.current)
        attempt.current += 1
        retryTimer.current = window.setTimeout(fetchUser, delay)
      } else {
        settleIdentity(null)
        setUser(null)
        setUnavailable(false)
        attempt.current = 0
      }
    } finally {
      setLoading(false)
    }
  }

  // biome-ignore lint/correctness/useExhaustiveDependencies: intentionally run only on mount
  useEffect(() => {
    fetchUser()
    // Coming back online is the moment a retry is most likely to work.
    const onOnline = () => {
      if (attempt.current > 0) fetchUser()
    }
    window.addEventListener("online", onOnline)
    return () => {
      window.removeEventListener("online", onOnline)
      window.clearTimeout(retryTimer.current)
    }
  }, [])

  // The server refuses a logout it could not carry out (the refresh
  // token still stands), and the user stays signed in: tell them, or the
  // menu item looks broken.
  const logout = async () => {
    try {
      await auth.logout()
    } catch (e) {
      // Only a 5xx or a network failure means the refresh token may
      // still stand. A 401 that could not be renewed means the session
      // is already gone: that is the logout done (and the request layer
      // is already on its way to /login).
      if (e instanceof ServiceUnavailableError) {
        showToast(t("nav.logoutFailed"), "error")
        return
      }
    }
    settleIdentity(null)
    setUser(null)
  }

  return (
    <AuthContext.Provider value={{ user, loading, unavailable, logout, refetch: fetchUser }}>
      {children}
    </AuthContext.Provider>
  )
}

export function useAuth() {
  return useContext(AuthContext)
}
