import posthog from 'posthog-js'

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
    // OC dashboard returns {error: "string"}; sessions-api returns
    // {error: {type, message}}. Handle both so we don't surface
    // "[object Object]" when the proxy talks to sessions-api.
    let msg: string
    if (typeof body.error === 'string') msg = body.error
    else if (body.error && typeof body.error.message === 'string') msg = body.error.message
    else if (typeof body.message === 'string') msg = body.message
    else msg = `Request failed: ${res.status}`
    throw new Error(msg)
  }

  if (res.status === 204) {
    return undefined as T
  }

  return res.json()
}

// Logout: clears server session, then navigates to login
export async function logout(): Promise<void> {
  await fetch('/auth/logout', { method: 'POST', credentials: 'include' }).catch(() => {})
  posthog.reset()
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

// Sandbox session logs: SSE stream of /var/log + exec stdout/stderr.
// The server proxies queries through to Axiom; the read token never
// reaches the browser. The returned EventSource emits one `message`
// event per log line (event.data = JSON-stringified LogEvent).
export interface LogEvent {
  _time: string
  source: 'var_log' | 'exec_stdout' | 'exec_stderr' | 'agent'
  line: string
  sandbox_id?: string
  path?: string
  exec_id?: string
  command?: string
  argv?: string[]
  exit_code?: number
}

export interface LogStreamOptions {
  tail?: boolean      // default true; if false, returns historical batch then closes
  q?: string          // free-text search (server applies "line contains")
  source?: string     // comma-separated subset of source values
  since?: string      // RFC3339; default = sandbox.startedAt
  limit?: number      // historical batch cap; default 1000, max 10000
}

export function streamSessionLogs(
  sandboxId: string,
  opts: LogStreamOptions = {},
): EventSource {
  const url = new URL(`${API_BASE}/sessions/${encodeURIComponent(sandboxId)}/logs`, window.location.origin)
  if (opts.tail !== undefined) url.searchParams.set('tail', String(opts.tail))
  if (opts.q) url.searchParams.set('q', opts.q)
  if (opts.source) url.searchParams.set('source', opts.source)
  if (opts.since) url.searchParams.set('since', opts.since)
  if (opts.limit !== undefined) url.searchParams.set('limit', String(opts.limit))
  return new EventSource(url.toString(), { withCredentials: true })
}

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
  sandbox_id: string | null
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

export interface AgentEvent {
  id: string
  agent_id: string
  type: 'info' | 'warning' | 'error'
  phase: string
  message: string
  at: string
}

export const getAgentEvents = (agentId: string, limit = 50) =>
  apiFetch<{ events: AgentEvent[]; next_before: string | null }>(
    `/agents/${encodeURIComponent(agentId)}/events?limit=${limit}`,
  )

export const getAgentOperations = (agentId: string, limit = 20) =>
  apiFetch<{ operations: AgentOperation[]; next_before: string | null }>(
    `/agents/${encodeURIComponent(agentId)}/operations?limit=${limit}`,
  )

export const restartAgent = (agentId: string) =>
  apiFetch<{ agent_id: string; status: string }>(
    `/agents/${encodeURIComponent(agentId)}/restart`,
    { method: 'POST' },
  )

export const getAgentLogs = (agentId: string, tail = 300) =>
  apiFetch<{ agent_id: string; sandbox_id: string; source: string; lines: number; content: string }>(
    `/agents/${encodeURIComponent(agentId)}/logs?tail=${tail}`,
  )

// ── Per-agent paywalled feature subscriptions (Telegram et al) ──

export type EntitlementReason = 'ungated' | 'subscription_required' | string

export interface AgentEntitlement {
  feature: string
  entitled: boolean
  reason?: EntitlementReason
  price_monthly_cents?: number
  status?: string
  current_period_end?: string
  cancel_at_period_end?: boolean
  stripe_subscription_id?: string
}

export const listAgentEntitlements = (agentId: string) =>
  apiFetch<{ agent_id: string; entitlements: AgentEntitlement[] }>(
    `/agents/${encodeURIComponent(agentId)}/entitlements`,
  )

export type SubscribeResult =
  | { status: 'active'; feature: string; agent_id: string; subscription_id: string; price_id: string }
  | { status: 'already_subscribed'; feature: string; agent_id: string; subscription_id: string }
  | { status: 'ungated'; feature: string; agent_id: string }
  | { status: 'checkout_required'; feature: string; agent_id: string; checkout_url: string }

export const subscribeAgentFeature = (agentId: string, feature: string) =>
  apiFetch<SubscribeResult>(
    `/agents/${encodeURIComponent(agentId)}/subscriptions/${encodeURIComponent(feature)}`,
    { method: 'POST' },
  )

export interface OrgAgentSubscription {
  agent_id: string
  feature: string
  status: string
  active: boolean
  price_monthly_cents: number
  current_period_end?: string
  cancel_at_period_end: boolean
  canceled_at?: string
  created_at: string
  stripe_subscription_id: string
}

export const listOrgAgentSubscriptions = () =>
  apiFetch<{ subscriptions: OrgAgentSubscription[] }>(
    '/billing/agent-subscriptions',
  )

export const cancelAgentFeature = (agentId: string, feature: string) =>
  apiFetch<{
    status: string
    feature: string
    agent_id: string
    cancel_at_period_end: boolean
    current_period_end?: string
  }>(
    `/agents/${encodeURIComponent(agentId)}/subscriptions/${encodeURIComponent(feature)}`,
    { method: 'DELETE' },
  )

/**
 * Streams a chat turn to an agent's instance and yields parsed SSE events
 * as they arrive. The upstream (sessions-api POST /v1/agents/:id/instances/:id/messages)
 * emits `data: {type:"text",content:"..."}` and `data: {type:"done"}`.
 *
 * Uses fetch + ReadableStream because EventSource is GET-only.
 */
export type ChatEvent =
  | { type: 'text'; content: string; conversation_id?: string }
  | { type: 'done' }
  | { type: 'raw'; data: string }

export interface ChatTurn { role: 'user' | 'assistant' | 'system'; content: string }

export async function* streamAgentChat(
  agentId: string,
  instanceId: string,
  content: string,
  conversationId?: string,
  history?: ChatTurn[],
): AsyncGenerator<ChatEvent, void, unknown> {
  // OpenAI-style chat-completions is stateless: the gateway needs the
  // full conversation each turn to have any memory of prior messages.
  // Caller passes `history` (already including the new user turn).
  // For backwards compat with older callers, we still accept `content`
  // as a single-turn shortcut.
  const body: Record<string, unknown> = {}
  if (history && history.length > 0) {
    body.messages = history
  } else {
    body.content = content
  }
  if (conversationId) body.conversation_id = conversationId

  const res = await fetch(
    `/api/dashboard/agents/${encodeURIComponent(agentId)}/instances/${encodeURIComponent(instanceId)}/messages`,
    {
      method: 'POST',
      credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    },
  )
  if (!res.ok || !res.body) {
    const text = await res.text().catch(() => '')
    throw new Error(`chat ${res.status}: ${text || 'no body'}`)
  }

  const reader = res.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  while (true) {
    const { done, value } = await reader.read()
    if (done) return
    buffer += decoder.decode(value, { stream: true })
    let idx: number
    while ((idx = buffer.indexOf('\n\n')) !== -1) {
      const block = buffer.slice(0, idx)
      buffer = buffer.slice(idx + 2)
      const data = block
        .split('\n')
        .filter((line) => line.startsWith('data:'))
        .map((line) => line.slice(5).trimStart())
        .join('\n')
      if (!data) continue
      try {
        yield JSON.parse(data) as ChatEvent
      } catch {
        yield { type: 'raw', data }
      }
    }
  }
}
