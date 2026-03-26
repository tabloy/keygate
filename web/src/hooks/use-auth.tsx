import { createContext, type ReactNode, useContext, useEffect, useState } from "react"
import { auth } from "@/lib/api"

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
  logout: () => Promise<void>
  refetch: () => Promise<void>
}

const AuthContext = createContext<AuthContextType>({
  user: null,
  loading: true,
  logout: async () => {},
  refetch: async () => {},
})

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<AuthUser | null>(null)
  const [loading, setLoading] = useState(true)

  const fetchUser = async () => {
    try {
      const u = await auth.me()
      setUser({ ...u, role: u.role || "user" })
    } catch {
      setUser(null)
    } finally {
      setLoading(false)
    }
  }

  // biome-ignore lint/correctness/useExhaustiveDependencies: intentionally run only on mount
  useEffect(() => {
    fetchUser()
  }, [])

  const logout = async () => {
    await auth.logout()
    setUser(null)
  }

  return <AuthContext.Provider value={{ user, loading, logout, refetch: fetchUser }}>{children}</AuthContext.Provider>
}

export function useAuth() {
  return useContext(AuthContext)
}
