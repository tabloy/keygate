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
  loggingOut: boolean
  logout: () => Promise<void>
  refetch: () => Promise<void>
}

const AuthContext = createContext<AuthContextType>({
  user: null,
  loading: true,
  unavailable: false,
  loggingOut: false,
  logout: async () => {},
  refetch: async () => {},
})

export function AuthProvider({ children }: { children: ReactNode }) {
  const { t } = useI18n()
  const logoutPending = useRef(false)
  const [loggingOut, setLoggingOut] = useState(false)
  const [user, setUser] = useState<AuthUser | null>(null)
  const [loading, setLoading] = useState(true)
  const [unavailable, setUnavailable] = useState(false)
  const retryTimer = useRef<number | undefined>(undefined)
  const attempt = useRef(0)

  const fetchUser = async () => {
    window.clearTimeout(retryTimer.current)
    try {
      const u = await auth.me()
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

  const logout = async () => {
    if (logoutPending.current) return
    logoutPending.current = true
    setLoggingOut(true)
    try {
      await auth.logout()
      setUser(null)
    } catch {
      showToast(t("auth.logoutFailed"), "error")
    } finally {
      logoutPending.current = false
      setLoggingOut(false)
    }
  }

  return (
    <AuthContext.Provider value={{ user, loading, unavailable, loggingOut, logout, refetch: fetchUser }}>
      {children}
    </AuthContext.Provider>
  )
}

export function useAuth() {
  return useContext(AuthContext)
}
