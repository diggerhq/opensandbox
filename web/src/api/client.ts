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
export const getMe = () => apiFetch<MeResponse>('/me')

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

export const getCheckpoints = (page = 1, perPage = 20) =>
  apiFetch<CheckpointsResponse>(`/checkpoints?page=${page}&per_page=${perPage}`)

export const deleteCheckpointDashboard = (id: string) =>
  apiFetch<void>(`/checkpoints/${id}`, { method: 'DELETE' })

export const getImages = (all = false) =>
  apiFetch<ImageCacheItem[]>(`/images${all ? '?all=true' : ''}`)

export const deleteImage = (id: string) =>
  apiFetch<void>(`/images/${id}`, { method: 'DELETE' })

export const getSessionDetail = (sandboxId: string) =>
  apiFetch<SessionDetail>(`/sessions/${sandboxId}`)

export const getSessionStats = (sandboxId: string) =>
  apiFetch<SandboxStats>(`/sessions/${sandboxId}/stats`)

// Soft restart: guest kernel reboots, QEMU process + workspace stay.
export const rebootSession = (sandboxId: string) =>
  apiFetch<void>(`/sessions/${sandboxId}/reboot`, { method: 'POST' })

// Hard restart: QEMU process recreated, workspace data preserved.
export const powerCycleSession = (sandboxId: string) =>
  apiFetch<void>(`/sessions/${sandboxId}/power-cycle`, { method: 'POST' })

export const getOrg = () => apiFetch<Org>('/org')

export const updateOrg = (name: string) =>
  apiFetch<Org>('/org', { method: 'PUT', body: JSON.stringify({ name }) })

export const setCustomDomain = (domain: string) =>
  apiFetch<Org>('/org/custom-domain', { method: 'PUT', body: JSON.stringify({ domain }) })

export const deleteCustomDomain = () =>
  apiFetch<Org>('/org/custom-domain', { method: 'DELETE' })

export const refreshCustomDomain = () =>
  apiFetch<Org>('/org/custom-domain/refresh', { method: 'POST' })

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

export interface PreviewURL {
  id: string
  sandboxId: string
  orgId: string
  hostname: string
  customHostname?: string
  port: number
  cfHostnameId?: string
  sslStatus: string
  authConfig: Record<string, unknown>
  createdAt: string
}

export interface SessionDetail {
  id: string
  sandboxId: string
  template: string
  status: string
  startedAt: string
  stoppedAt?: string
  errorMsg?: string
  config?: {
    timeout?: number
    cpuCount?: number
    memoryMB?: number
    networkEnabled?: boolean
    envs?: Record<string, string>
  }
  checkpoint?: {
    checkpointKey: string
    sizeBytes: number
    hibernatedAt: string
  }
  previewUrls?: PreviewURL[]
}

export interface SandboxStats {
  cpuPercent: number
  memUsage: number
  memLimit: number
  netInput: number
  netOutput: number
  pids: number
}

export interface CheckpointItem {
  id: string
  sandboxId: string
  orgId: string
  name: string
  status: string
  sizeBytes: number
  activeForks: number
  totalForks: number
  createdAt: string
}

export interface CheckpointsResponse {
  checkpoints: CheckpointItem[]
  total: number
  page: number
  perPage: number
}

export interface ImageCacheItem {
  id: string
  orgId: string
  contentHash: string
  checkpointId?: string
  name?: string
  manifest: Record<string, unknown>
  status: string
  createdAt: string
  lastUsedAt: string
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
  customDomain?: string
  cfHostnameId?: string
  domainVerificationStatus: string
  domainSslStatus: string
  verificationTxtName?: string
  verificationTxtValue?: string
  sslTxtName?: string
  sslTxtValue?: string
  workosOrgId?: string
  isPersonal: boolean
  creditBalanceCents: number
}

export interface MeResponse {
  id: string
  email: string
  orgId: string
  orgs?: OrgInfo[]
}

export interface OrgInfo {
  id: string
  name: string
  isPersonal: boolean
  isActive: boolean
}

export interface OrgMember {
  membershipId?: string
  workosUserId?: string
  id?: string
  email: string
  name: string
  role: string
  status?: string
}

export interface OrgInvitation {
  id: string
  email: string
  state: string
  role?: string
  expiresAt: string
  createdAt: string
}

export interface Credits {
  balanceCents: number
  isPersonal: boolean
}

// Organization members
export const getOrgMembers = () => apiFetch<OrgMember[]>('/org/members')

export const removeMember = (membershipId: string) =>
  apiFetch<void>(`/org/members/${membershipId}`, { method: 'DELETE' })

// Invitations
export const sendInvitation = (email: string, role = 'member') =>
  apiFetch<OrgInvitation>('/org/invitations', {
    method: 'POST',
    body: JSON.stringify({ email, role }),
  })

export const getInvitations = () => apiFetch<OrgInvitation[]>('/org/invitations')

export const revokeInvitation = (id: string) =>
  apiFetch<void>(`/org/invitations/${id}`, { method: 'DELETE' })

// Org switching
export const listOrgs = () => apiFetch<OrgInfo[]>('/orgs')

export const switchOrg = (orgId: string) =>
  apiFetch<Org>('/org/switch', { method: 'POST', body: JSON.stringify({ orgId }) })

// Credits
export const getCredits = () => apiFetch<Credits>('/org/credits')

// Billing types
export interface BillingState {
  plan: string
  stripeCreditCents: number
  maxConcurrentSandboxes: number
  hasPaymentMethod: boolean
  freeCreditsRemainingCents: number
}

export interface StripeInvoice {
  id: string
  number: string
  status: string
  amountDue: number
  amountPaid: number
  currency: string
  created: number
  hostedUrl: string
  pdfUrl: string
}

// Billing API
export const getBilling = () => apiFetch<BillingState>('/billing')

export const billingSetup = () =>
  apiFetch<{ url: string }>('/billing/setup', { method: 'POST' })

export const billingPortal = () =>
  apiFetch<{ url: string }>('/billing/portal', { method: 'POST' })

export const getBillingInvoices = (limit = 10) =>
  apiFetch<{ invoices: StripeInvoice[] }>(`/billing/invoices?limit=${limit}`)

export const redeemPromoCode = (code: string) =>
  apiFetch<{ creditAppliedCents: number }>('/billing/redeem', {
    method: 'POST',
    body: JSON.stringify({ code }),
  })

// ── Agents (proxied to sessions-api at /api/dashboard/agents/*) ──

export interface Agent {
  id: string
  display_name: string
  core: string | null
  model: string | null
  channels: Array<{ name: string; bot_username?: string | null; connected_at?: string }>
  packages: Array<{ name: string; installed_at?: string }>
  secret_store: string | null
  config: Record<string, unknown>
  created_at: string
  updated_at: string
}

export interface AgentDetail extends Agent {
  status: 'ready' | 'starting' | 'degraded' | 'error' | 'unknown'
  instance_id: string | null
  instance_status: string | null
  core_status: { status: string; reason?: string; message?: string; updated_at?: string } | null
  channel_status: Record<string, { status: string; phase?: string; message?: string }>
  package_status: Record<string, { status: string; phase?: string; message?: string }>
  conditions: Array<{ type: string; status: string; reason?: string; message?: string }>
  current_operation: AgentOperation | null
  last_error: { phase: string; message: string; at: string } | null
}

export interface AgentOperation {
  id: string
  agent_id: string
  kind: string
  target_type?: string | null
  target_key?: string | null
  phase: string
  state: 'queued' | 'running' | 'success' | 'error' | 'canceled'
  message?: string | null
  created_at: string
  updated_at: string
}

export const listAgents = () =>
  apiFetch<{ agents: Agent[] }>('/agents')

export const getAgent = (id: string) =>
  apiFetch<AgentDetail>(`/agents/${encodeURIComponent(id)}`)

export const createAgent = (input: {
  id: string
  display_name?: string
  core?: string
  model?: string
  config?: Record<string, unknown>
  secrets?: Record<string, string>
}) =>
  apiFetch<AgentDetail>('/agents', {
    method: 'POST',
    body: JSON.stringify(input),
  })

export const deleteAgent = (id: string) =>
  apiFetch<{ id: string; deleted: boolean }>(`/agents/${encodeURIComponent(id)}`, {
    method: 'DELETE',
  })

export const installGbrain = (agentId: string) =>
  apiFetch<{ agent_id: string; package: string; status: string; operation: AgentOperation }>(
    `/agents/${encodeURIComponent(agentId)}/packages/gbrain`,
    { method: 'POST' },
  )

export const uninstallGbrain = (agentId: string) =>
  apiFetch<{ agent_id: string; package: string; status: string }>(
    `/agents/${encodeURIComponent(agentId)}/packages/gbrain`,
    { method: 'DELETE' },
  )

export const connectTelegram = (agentId: string, botToken: string) =>
  apiFetch<{ agent_id: string; channel: string; status: string; operation: AgentOperation }>(
    `/agents/${encodeURIComponent(agentId)}/channels/telegram`,
    {
      method: 'POST',
      body: JSON.stringify({ bot_token: botToken }),
    },
  )

export const disconnectTelegram = (agentId: string) =>
  apiFetch<{ agent_id: string; channel: string; status: string }>(
    `/agents/${encodeURIComponent(agentId)}/channels/telegram`,
    { method: 'DELETE' },
  )
