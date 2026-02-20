const API_BASE = '/api/dashboard'

export async function apiFetch<T>(path: string, options: RequestInit = {}): Promise<T> {
  const res = await fetch(`${API_BASE}${path}`, {
    ...options,
    credentials: 'include',
    headers: {
      'Content-Type': 'application/json',
      ...options.headers,
    },
  })

  if (res.status === 401) {
    // Don't auto-redirect — let ProtectedRoute handle auth flow.
    // This prevents a redirect loop on the login page.
    throw new Error('Unauthorized')
  }

  if (!res.ok) {
    const body = await res.json().catch(() => ({}))
    throw new Error(body.error || `Request failed: ${res.status}`)
  }

  if (res.status === 204) {
    return undefined as T
  }

  return res.json()
}

// Logout: clears server session, then navigates to login
export async function logout(): Promise<void> {
  await fetch('/auth/logout', { method: 'POST', credentials: 'include' }).catch(() => {})
  // Navigate to login page — use replace to prevent back-button loop
  window.location.replace('/login')
}

// API functions
export const getMe = () => apiFetch<{ id: string; email: string; orgId: string }>('/me')

export const getSessions = (status?: string) =>
  apiFetch<Session[]>(`/sessions${status ? `?status=${status}` : ''}`)

export const getAPIKeys = () => apiFetch<APIKey[]>('/api-keys')

export const createAPIKey = (name: string) =>
  apiFetch<{ id: string; name: string; key: string; keyPrefix: string; createdAt: string }>(
    '/api-keys',
    { method: 'POST', body: JSON.stringify({ name }) },
  )

export const deleteAPIKey = (keyId: string) =>
  apiFetch<void>(`/api-keys/${keyId}`, { method: 'DELETE' })

export const getTemplates = () => apiFetch<Template[]>('/templates')

export const buildTemplate = (name: string, dockerfile: string) =>
  apiFetch<Template>('/templates', {
    method: 'POST',
    body: JSON.stringify({ name, dockerfile }),
  })

export const deleteTemplate = (id: string) =>
  apiFetch<void>(`/templates/${id}`, { method: 'DELETE' })

export const getOrg = () => apiFetch<Org>('/org')

export const updateOrg = (name: string) =>
  apiFetch<Org>('/org', { method: 'PUT', body: JSON.stringify({ name }) })

// Types
export interface Session {
  id: string
  sandboxId: string
  orgId: string
  template: string
  region: string
  workerId: string
  status: string
  startedAt: string
  stoppedAt?: string
  errorMsg?: string
}

export interface APIKey {
  id: string
  orgId: string
  keyPrefix: string
  name: string
  scopes: string[]
  lastUsed?: string
  expiresAt?: string
  createdAt: string
}

export interface Template {
  id: string
  orgId?: string
  name: string
  tag: string
  imageRef: string
  dockerfile?: string
  isPublic: boolean
  createdAt: string
}

export interface Org {
  id: string
  name: string
  slug: string
  plan: string
  maxConcurrentSandboxes: number
  maxSandboxTimeoutSec: number
  createdAt: string
  updatedAt: string
}
