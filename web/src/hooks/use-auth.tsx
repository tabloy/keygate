import { useQueryClient } from "@tanstack/react-query"
import { createContext, type ReactNode, useContext, useEffect, useRef, useState } from "react"
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
  const queryClient = useQueryClient()
  const [user, setUser] = useState<AuthUser | null>(null)
  const [loading, setLoading] = useState(true)
  const [unavailable, setUnavailable] = useState(false)
  const retryTimer = useRef<number | undefined>(undefined)
  const attempt = useRef(0)
  const generation = useRef(0)
  const readSequence = useRef(0)
  const signingOut = useRef(false)
  const identity = useRef<string | null | undefined>(undefined)

  const changeIdentity = (next: string | null) => {
    if (identity.current === next) return
    const filters = {
      predicate: (query: { queryKey: readonly unknown[] }) =>
        query.queryKey[0] === "portal" || query.queryKey[0] === "admin",
    }
    void queryClient.cancelQueries(filters)
    queryClient.removeQueries(filters)
    identity.current = next
  }

  const fetchUser = async () => {
    if (signingOut.current) return
    const session = generation.current
    const sequence = ++readSequence.current
    const current = () => session === generation.current && sequence === readSequence.current
    window.clearTimeout(retryTimer.current)
    try {
      const u = await auth.me()
      if (!current()) return
      changeIdentity(`${u.id}:${u.role || "user"}:${u.is_admin}`)
      setUser({ ...u, role: u.role || "user" })
      setUnavailable(false)
      attempt.current = 0
    } catch (e) {
      if (!current()) return
      if (e instanceof ServiceUnavailableError) {
        // Keep whoever was signed in; try again with backoff
        // (2s, 4s, 8s, 16s, then every 30s) until the server answers.
        setUnavailable(true)
        const delay = Math.min(30_000, 2_000 * 2 ** attempt.current)
        attempt.current += 1
        retryTimer.current = window.setTimeout(() => {
          if (current()) void fetchUser()
        }, delay)
      } else {
        changeIdentity(null)
        setUser(null)
        setUnavailable(false)
        attempt.current = 0
      }
    } finally {
      if (current()) setLoading(false)
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
      generation.current += 1
      window.removeEventListener("online", onOnline)
      window.clearTimeout(retryTimer.current)
    }
  }, [])

  const logout = async () => {
    if (signingOut.current) return
    signingOut.current = true
    generation.current += 1
    window.clearTimeout(retryTimer.current)
    attempt.current = 0
    try {
      await auth.logout()
      changeIdentity(null)
      setUser(null)
      setUnavailable(false)
    } finally {
      signingOut.current = false
    }
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
