import { createContext, useContext, useEffect, useState, useCallback, type ReactNode } from 'react'
import posthog from 'posthog-js'
import { getMe, switchOrg as switchOrgApi, type OrgInfo } from '../api/client'

interface AuthUser {
  id: string
  email: string
  orgId: string
  orgs?: OrgInfo[]
}

interface AuthContextValue {
  user: AuthUser | null
  loading: boolean
  error: string | null
  switchOrg: (orgId: string) => Promise<void>
  refreshUser: () => Promise<void>
}

const AuthContext = createContext<AuthContextValue>({
  user: null,
  loading: true,
  error: null,
  switchOrg: async () => {},
  refreshUser: async () => {},
})

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<AuthUser | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const refreshUser = useCallback(async () => {
    try {
      const me = await getMe()
      setUser(me)
      if (me?.id) {
        posthog.identify(me.id, { email: me.email, org_id: me.orgId })
      }
    } catch (err: unknown) {
      if (err instanceof Error && !err.message.includes('Unauthorized')) {
        setError(err.message)
      }
    }
  }, [])

  useEffect(() => {
    refreshUser().finally(() => setLoading(false))
  }, [refreshUser])

  const switchOrg = useCallback(async (orgId: string) => {
    await switchOrgApi(orgId)
    await refreshUser()
  }, [refreshUser])

  return (
    <AuthContext.Provider value={{ user, loading, error, switchOrg, refreshUser }}>
      {children}
    </AuthContext.Provider>
  )
}

export function useAuth() {
  return useContext(AuthContext)
}
