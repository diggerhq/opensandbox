import { useEffect, useRef, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import {
  getAgent,
  deleteAgent,
  installGbrain,
  uninstallGbrain,
  connectTelegram,
  disconnectTelegram,
  restartAgent,
  rebootSession,
  getAgentEvents,
  getAgentOperations,
  getAgentLogs,
  streamAgentChat,
  listAgentEntitlements,
  subscribeAgentFeature,
  cancelAgentFeature,
  type AgentEntitlement,
  type AgentDetail,
  type AgentOperation,
  type AgentEvent as AgentEventRow,
} from '../api/client'

type Tab = 'overview' | 'channels' | 'plugins' | 'events' | 'logs' | 'advanced'
type Banner = { kind: 'info' | 'error' | 'success'; text: string } | null

const statusColor: Record<string, string> = {
  ready: 'var(--accent-emerald, #34d399)',
  starting: 'var(--accent-amber, #fbbf24)',
  degraded: 'var(--accent-amber, #fbbf24)',
  error: 'var(--accent-rose, #f87171)',
  unknown: 'var(--text-tertiary)',
}

export default function AgentDetailPage() {
  const { agentId = '' } = useParams<{ agentId: string }>()
  const navigate = useNavigate()
  const [detail, setDetail] = useState<AgentDetail | null>(null)
  const [entitlementsByFeature, setEntitlementsByFeature] = useState<Record<string, AgentEntitlement>>({})
  const [loading, setLoading] = useState(true)
  const [tab, setTab] = useState<Tab>('overview')
  const [banner, setBanner] = useState<Banner>(null)
  const [acting, setActing] = useState<string | null>(null)

  async function refresh() {
    try {
      const d = await getAgent(agentId)
      setDetail(d)
      // Refresh entitlements alongside detail so the header upgrade
      // pill stays in sync with the latest subscription state.
      try {
        const e = await listAgentEntitlements(agentId)
        const map: Record<string, AgentEntitlement> = {}
        for (const ent of e.entitlements) map[ent.feature] = ent
        setEntitlementsByFeature(map)
      } catch {
        // best-effort — don't block the page on entitlement fetch
      }
    } catch (err) {
      setBanner({ kind: 'error', text: `Failed to load agent: ${(err as Error).message}` })
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void refresh()
    const interval = setInterval(() => void refresh(), 5000)
    return () => clearInterval(interval)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [agentId])

  async function act<T>(label: string, fn: () => Promise<T>, ok: string) {
    setActing(label)
    try {
      await fn()
      setBanner({ kind: 'success', text: ok })
      await refresh()
    } catch (err) {
      setBanner({ kind: 'error', text: `${label} failed: ${(err as Error).message}` })
    } finally {
      setActing(null)
    }
  }

  async function handleDelete() {
    if (!window.confirm(`Delete agent "${agentId}"? Sandbox will be killed.`)) return
    setActing('delete')
    try {
      await deleteAgent(agentId)
      navigate('/agents')
    } catch (err) {
      setBanner({ kind: 'error', text: `Delete failed: ${(err as Error).message}` })
      setActing(null)
    }
  }

  if (loading && !detail) {
    return (
      <div style={{ display: 'flex', alignItems: 'center', gap: 10, color: 'var(--text-tertiary)', fontSize: 13, padding: '32px 0' }}>
        <span className="loading-spinner" style={{ width: 14, height: 14, borderWidth: 1.5 }} />
        Loading agent…
      </div>
    )
  }

  if (!detail) {
    return (
      <div>
        <button onClick={() => navigate('/agents')} style={secondaryButton}>← Back</button>
        <p style={{ color: 'var(--accent-rose, #f87171)', marginTop: 16 }}>Agent not found.</p>
      </div>
    )
  }

  const sandboxId = detail.sandbox_id

  return (
    <div>
      {/* Top bar with back link */}
      <button
        onClick={() => navigate('/agents')}
        style={{ ...secondaryButton, marginBottom: 16 }}
      >
        ← Agents
      </button>

      {/* Header card */}
      <div
        style={{
          background: 'rgba(255,255,255,0.02)',
          border: '1px solid var(--border-subtle)',
          borderRadius: 10,
          padding: '18px 22px',
          display: 'flex',
          alignItems: 'flex-start',
          gap: 18,
          marginBottom: 18,
        }}
      >
        <div
          className={
            detail.status === 'starting' || detail.status === 'degraded' || detail.current_operation
              ? 'pulse-dot'
              : undefined
          }
          style={{
            width: 12,
            height: 12,
            borderRadius: '50%',
            background: statusColor[detail.status] ?? statusColor.unknown,
            boxShadow:
              detail.status === 'starting' || detail.current_operation
                ? `0 0 0 4px ${statusColor[detail.status]}22`
                : undefined,
            flexShrink: 0,
            marginTop: 6,
          }}
        />
        <div style={{ flex: 1, minWidth: 0 }}>
          <div style={{ display: 'flex', alignItems: 'baseline', gap: 10 }}>
            <h1 style={{ margin: 0, fontSize: 20, fontFamily: 'var(--font-display)', fontWeight: 700 }}>
              {detail.display_name}
            </h1>
            <span style={{ fontSize: 12, color: 'var(--text-tertiary)', fontFamily: 'var(--font-mono)' }}>
              {detail.id}
            </span>
            <span style={pill(detail.status === 'ready' ? 'emerald' : detail.status === 'error' ? 'rose' : 'amber')}>
              {detail.status}
            </span>
          </div>
          <div style={{ fontSize: 12, color: 'var(--text-secondary)', marginTop: 6, display: 'flex', gap: 14, flexWrap: 'wrap' }}>
            {detail.core && <span>core: <code style={codeInline}>{detail.core}</code></span>}
            {detail.model && <span>model: <code style={codeInline}>{detail.model}</code></span>}
            {detail.instance_id && <span>instance: <code style={codeInline}>{detail.instance_id.slice(0, 18)}…</code></span>}
            {sandboxId && <span>sandbox: <code style={codeInline}>{sandboxId}</code></span>}
          </div>
          {detail.current_operation && (
            <div style={{ fontSize: 12, color: 'var(--accent-amber, #fbbf24)', marginTop: 8, display: 'flex', alignItems: 'center', gap: 6 }}>
              <span
                className="loading-spinner"
                style={{ width: 11, height: 11, borderWidth: 1.5, borderColor: 'rgba(251,191,36,0.25)', borderTopColor: 'var(--accent-amber, #fbbf24)' }}
              />
              <span>
                {detail.current_operation.kind} · {detail.current_operation.phase}
                {detail.current_operation.message ? ` — ${detail.current_operation.message}` : ''}
              </span>
            </div>
          )}
          {detail.last_error && detail.status !== 'ready' && (
            <div style={{ fontSize: 12, color: 'var(--accent-rose, #f87171)', marginTop: 8 }}>
              {detail.last_error.phase}: {detail.last_error.message}
            </div>
          )}
        </div>
        <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', justifyContent: 'flex-end' }}>
          {(() => {
            const tg = entitlementsByFeature.telegram
            const needsUpgrade = tg && tg.entitled === false && tg.reason !== 'ungated'
            if (!needsUpgrade) return null
            const priceCents = tg.price_monthly_cents ?? 2000
            return (
              <button
                onClick={() => setTab('channels')}
                style={{
                  background: 'rgba(99,102,241,0.08)',
                  color: 'var(--accent-indigo, #818cf8)',
                  border: '1px solid rgba(99,102,241,0.35)',
                  padding: '6px 12px',
                  borderRadius: 999,
                  cursor: 'pointer',
                  fontSize: 12,
                  fontFamily: 'var(--font-body)',
                  display: 'inline-flex',
                  alignItems: 'center',
                  gap: 6,
                }}
                title={`Subscribe to Telegram channel for $${(priceCents / 100).toFixed(0)}/mo`}
              >
                <span style={{ fontSize: 11 }}>★</span>
                Upgrade — ${(priceCents / 100).toFixed(0)}/mo
              </button>
            )
          })()}
          <button
            onClick={() =>
              act('restart core', () => restartAgent(agentId), 'Core restarted.')
            }
            disabled={!!acting}
            style={secondaryButton}
            title="Kill and respawn the core process; sandbox VM stays up."
          >
            {acting === 'restart core' ? <BusyLabel text="Restarting…" /> : 'Restart core'}
          </button>
          <button
            onClick={() =>
              sandboxId
                ? act('reboot sandbox', () => rebootSession(sandboxId), 'Sandbox rebooted.')
                : null
            }
            disabled={!!acting || !sandboxId}
            style={secondaryButton}
            title="Reboot the sandbox VM (guest kernel restart)."
          >
            {acting === 'reboot sandbox' ? <BusyLabel text="Rebooting…" /> : 'Reboot sandbox'}
          </button>
          <button onClick={handleDelete} disabled={!!acting} style={dangerButton}>
            {acting === 'delete' ? <BusyLabel text="Deleting…" /> : 'Delete'}
          </button>
        </div>
      </div>

      {banner && (
        <div
          style={{
            padding: '10px 14px',
            borderRadius: 6,
            marginBottom: 14,
            fontSize: 13,
            background:
              banner.kind === 'error' ? 'rgba(248,113,113,0.08)'
                : banner.kind === 'success' ? 'rgba(52,211,153,0.08)'
                  : 'rgba(99,102,241,0.08)',
            border: `1px solid ${
              banner.kind === 'error' ? 'rgba(248,113,113,0.3)'
                : banner.kind === 'success' ? 'rgba(52,211,153,0.3)'
                  : 'rgba(99,102,241,0.3)'
            }`,
            display: 'flex', justifyContent: 'space-between', alignItems: 'center',
          }}
        >
          <span>{banner.text}</span>
          <button onClick={() => setBanner(null)} style={iconButton}>×</button>
        </div>
      )}

      {/* Tabs */}
      <div style={{ display: 'flex', borderBottom: '1px solid var(--border-subtle)', marginBottom: 18, gap: 4 }}>
        {(['overview', 'channels', 'plugins', 'events', 'logs', 'advanced'] as Tab[]).map((t) => (
          <button
            key={t}
            onClick={() => setTab(t)}
            style={{
              background: 'none',
              border: 'none',
              borderBottom: tab === t ? '2px solid var(--accent-indigo)' : '2px solid transparent',
              padding: '10px 14px',
              color: tab === t ? 'var(--text-primary)' : 'var(--text-secondary)',
              fontSize: 13,
              fontFamily: 'var(--font-body)',
              fontWeight: tab === t ? 600 : 400,
              cursor: 'pointer',
              textTransform: 'capitalize',
            }}
          >
            {t}
          </button>
        ))}
      </div>

      {tab === 'overview' && <OverviewTab agentId={agentId} detail={detail} />}
      {tab === 'channels' && (
        <ChannelsTab
          detail={detail}
          acting={acting}
          onAction={(label, fn, ok) => act(label, fn, ok)}
        />
      )}
      {tab === 'plugins' && (
        <PluginsTab
          detail={detail}
          acting={acting}
          onAction={(label, fn, ok) => act(label, fn, ok)}
        />
      )}
      {tab === 'events' && <EventsTab agentId={agentId} />}
      {tab === 'logs' && <LogsTab agentId={agentId} core={detail.core} />}
      {tab === 'advanced' && <AdvancedTab detail={detail} />}
    </div>
  )
}

function LogsTab({ agentId, core }: { agentId: string; core: string | null }) {
  const [content, setContent] = useState<string>('')
  const [error, setError] = useState<string | null>(null)
  const [paused, setPaused] = useState(false)
  const [loading, setLoading] = useState(true)
  const scrollRef = useRef<HTMLPreElement | null>(null)

  async function load() {
    try {
      const r = await getAgentLogs(agentId, 500)
      setContent(r.content)
      setError(null)
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    if (paused) return
    void load()
    const interval = setInterval(() => void load(), 3000)
    return () => clearInterval(interval)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [agentId, paused])

  // Stick to bottom when new content arrives, unless the user scrolled up.
  useEffect(() => {
    const el = scrollRef.current
    if (!el) return
    const distanceFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight
    if (distanceFromBottom < 80) {
      el.scrollTop = el.scrollHeight
    }
  }, [content])

  if (core !== 'openclaw') {
    return (
      <p style={{ fontSize: 13, color: 'var(--text-tertiary)' }}>
        Process logs are only available for OpenClaw agents right now.
      </p>
    )
  }

  return (
    <div style={{ display: 'grid', gap: 10 }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 8 }}>
        <div style={{ fontSize: 12, color: 'var(--text-tertiary)' }}>
          Tailing <code style={codeInline}>/tmp/openclaw-gateway.log</code> · refresh every 3s ·{' '}
          <span style={{ color: paused ? 'var(--accent-amber, #fbbf24)' : 'var(--accent-emerald, #34d399)' }}>
            {paused ? 'paused' : 'live'}
          </span>
        </div>
        <div style={{ display: 'flex', gap: 6 }}>
          <button
            onClick={() => setPaused((p) => !p)}
            style={secondaryButton}
          >
            {paused ? 'Resume' : 'Pause'}
          </button>
          <button onClick={() => void load()} style={secondaryButton}>Refresh</button>
        </div>
      </div>

      {error ? (
        <div style={{
          fontSize: 12, color: '#f87171',
          background: 'rgba(248,113,113,0.06)',
          border: '1px solid rgba(248,113,113,0.2)',
          padding: '8px 10px', borderRadius: 6,
        }}>
          {error}
        </div>
      ) : null}

      <pre
        ref={scrollRef}
        style={{
          fontSize: 11,
          fontFamily: 'var(--font-mono)',
          background: 'rgba(255,255,255,0.02)',
          border: '1px solid var(--border-subtle)',
          padding: 12,
          borderRadius: 6,
          margin: 0,
          height: '60vh',
          overflow: 'auto',
          whiteSpace: 'pre-wrap',
          wordBreak: 'break-word',
          color: 'var(--text-secondary)',
        }}
      >
        {loading && !content ? 'Loading…' : (content || '(no log lines yet)')}
      </pre>
    </div>
  )
}

function AdvancedTab({ detail }: { detail: AgentDetail }) {
  const navigate = useNavigate()
  const sandboxId = detail.sandbox_id
  return (
    <div style={{ display: 'grid', gap: 18, maxWidth: 720 }}>
      <Section title="Underlying sandbox">
        <p style={{ fontSize: 13, color: 'var(--text-secondary)', margin: 0 }}>
          Each managed agent runs inside an OC sandbox VM. Open the sandbox view to inspect
          processes, terminal access, preview URLs, reboot/power-cycle controls, and
          checkpoint history.
        </p>
        <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap', marginTop: 4 }}>
          {sandboxId ? (
            <>
              <code style={codeInline}>{sandboxId}</code>
              <button
                onClick={() => navigate(`/sessions/${encodeURIComponent(sandboxId)}`)}
                style={primaryButton}
              >
                Open sandbox details →
              </button>
            </>
          ) : (
            <span style={{ fontSize: 12, color: 'var(--text-tertiary)' }}>
              No sandbox provisioned yet.
            </span>
          )}
        </div>
      </Section>

      <Section title="Identifiers">
        <div style={{ display: 'grid', gap: 6, fontSize: 12 }}>
          <KVRow label="Agent ID" value={detail.id} />
          {detail.instance_id && <KVRow label="Instance ID" value={detail.instance_id} />}
          {sandboxId && <KVRow label="Sandbox ID" value={sandboxId} />}
          {detail.secret_store && <KVRow label="Secret store" value={detail.secret_store} />}
          {detail.core && <KVRow label="Core" value={detail.core} />}
        </div>
      </Section>

      <Section title="Raw config">
        <details>
          <summary style={{ cursor: 'pointer', fontSize: 12, color: 'var(--text-secondary)' }}>
            Show JSON
          </summary>
          <pre style={{
            fontSize: 11, fontFamily: 'var(--font-mono)',
            background: 'rgba(255,255,255,0.02)', border: '1px solid var(--border-subtle)',
            padding: 10, borderRadius: 6, overflow: 'auto', margin: '8px 0 0 0', maxHeight: 320,
          }}>
            {JSON.stringify(detail.config, null, 2)}
          </pre>
        </details>
      </Section>
    </div>
  )
}

function KVRow({ label, value }: { label: string; value: string }) {
  return (
    <div style={{ display: 'flex', gap: 10, alignItems: 'center' }}>
      <span style={{ color: 'var(--text-tertiary)', minWidth: 100 }}>{label}</span>
      <code style={{ ...codeInline, fontSize: 11, wordBreak: 'break-all' }}>{value}</code>
    </div>
  )
}

// ── Tabs ──────────────────────────────────────────────────────────

function OverviewTab({ agentId, detail }: { agentId: string; detail: AgentDetail }) {
  // Two-column layout: chat takes the bulk of the room; health summary
  // sits alongside as a compact side panel. Drops the JSON config dump
  // and other plumbing detail — the Events tab covers operational depth.
  return (
    <div
      style={{
        display: 'grid',
        gridTemplateColumns: 'minmax(0, 1fr) 280px',
        gap: 18,
        alignItems: 'start',
      }}
    >
      <ChatTab
        agentId={agentId}
        instanceId={detail.instance_id}
        disabled={detail.status !== 'ready'}
      />
      <HealthSidePanel detail={detail} />
    </div>
  )
}

function HealthSidePanel({ detail }: { detail: AgentDetail }) {
  const tone = detail.status === 'ready' ? 'emerald' : detail.status === 'error' ? 'rose' : 'amber'
  const channelTone = (status?: string) => status === 'connected' ? 'emerald' : status === 'error' ? 'rose' : 'amber'
  const packageTone = (status?: string) => status === 'installed' ? 'emerald' : status === 'error' ? 'rose' : 'amber'

  return (
    <aside
      style={{
        background: 'rgba(255,255,255,0.02)',
        border: '1px solid var(--border-subtle)',
        borderRadius: 10,
        padding: '16px 18px',
        display: 'grid',
        gap: 16,
        position: 'sticky',
        top: 16,
      }}
    >
      <div>
        <h3 style={sectionTitleStyle}>Health</h3>
        <div style={{ display: 'grid', gap: 8 }}>
          <HealthRow label="Agent" value={detail.status} tone={tone} />
          <HealthRow
            label="Core"
            value={detail.core_status?.status ?? 'unknown'}
            tone={
              detail.core_status?.status === 'ready' ? 'emerald'
                : detail.core_status?.status === 'error' ? 'rose' : 'amber'
            }
          />
          <HealthRow
            label="Instance"
            value={detail.instance_status ?? '—'}
            tone={detail.instance_status === 'running' ? 'emerald' : 'amber'}
          />
        </div>
        {detail.core_status?.message && detail.status !== 'ready' && (
          <div style={{ fontSize: 11, color: 'var(--text-tertiary)', marginTop: 8 }}>
            {detail.core_status.message}
          </div>
        )}
      </div>

      {Object.keys(detail.channel_status ?? {}).length > 0 && (
        <div>
          <h3 style={sectionTitleStyle}>Channels</h3>
          <div style={{ display: 'grid', gap: 6 }}>
            {Object.entries(detail.channel_status).map(([name, s]) => (
              <HealthRow
                key={name}
                label={name}
                value={s.status}
                tone={channelTone(s.status)}
                detail={s.message}
              />
            ))}
          </div>
        </div>
      )}

      {Object.keys(detail.package_status ?? {}).length > 0 && (
        <div>
          <h3 style={sectionTitleStyle}>Packages</h3>
          <div style={{ display: 'grid', gap: 6 }}>
            {Object.entries(detail.package_status).map(([name, s]) => (
              <HealthRow
                key={name}
                label={name}
                value={s.status}
                tone={packageTone(s.status)}
                detail={s.message}
              />
            ))}
          </div>
        </div>
      )}

      {detail.last_error && detail.status !== 'ready' && (
        <div>
          <h3 style={sectionTitleStyle}>Last error</h3>
          <div style={{
            fontSize: 11, fontFamily: 'var(--font-mono)',
            color: '#f87171',
            background: 'rgba(248,113,113,0.06)',
            border: '1px solid rgba(248,113,113,0.2)',
            padding: '8px 10px', borderRadius: 6,
            wordBreak: 'break-word',
          }}>
            <div style={{ color: '#fca5a5', marginBottom: 4 }}>{detail.last_error.phase}</div>
            {detail.last_error.message}
          </div>
        </div>
      )}
    </aside>
  )
}

const sectionTitleStyle: React.CSSProperties = {
  fontSize: 10,
  textTransform: 'uppercase',
  letterSpacing: '0.08em',
  color: 'var(--text-tertiary)',
  margin: '0 0 8px 0',
  fontFamily: 'var(--font-mono)',
  fontWeight: 600,
}

function HealthRow({
  label,
  value,
  tone,
  detail,
}: {
  label: string
  value: string
  tone: 'emerald' | 'amber' | 'rose'
  detail?: string
}) {
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
      <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 8, fontSize: 12 }}>
        <span style={{ color: 'var(--text-secondary)', textTransform: 'capitalize' }}>{label}</span>
        <span style={pill(tone)}>{value}</span>
      </div>
      {detail && (
        <div style={{ fontSize: 11, color: 'var(--text-tertiary)', wordBreak: 'break-word' }}>
          {detail}
        </div>
      )}
    </div>
  )
}

function ChatTab({
  agentId,
  instanceId,
  disabled,
}: {
  agentId: string
  instanceId: string | null
  disabled: boolean
}) {
  type Msg = { role: 'user' | 'assistant' | 'system'; text: string }
  const [history, setHistory] = useState<Msg[]>([])
  const [input, setInput] = useState('')
  const [streaming, setStreaming] = useState(false)
  const [conversationId] = useState(() =>
    typeof crypto !== 'undefined' && 'randomUUID' in crypto ? crypto.randomUUID() : `c-${Date.now()}`,
  )
  const scrollRef = useRef<HTMLDivElement | null>(null)
  const inputRef = useRef<HTMLInputElement | null>(null)

  useEffect(() => {
    if (scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight
    }
  }, [history, streaming])

  // Refocus the composer when streaming ends (or starts disabled→enabled)
  // so the user can keep typing without grabbing the mouse.
  useEffect(() => {
    if (!streaming && !disabled) {
      inputRef.current?.focus()
    }
  }, [streaming, disabled])

  async function send() {
    if (!input.trim() || !instanceId || streaming) return
    const userMsg = input.trim()
    setInput('')
    // Build the full history we'll send to the gateway BEFORE we mutate
    // local state. Includes every prior user/assistant turn (skipping
    // 'system' rows we generate locally for error display) plus the
    // new user turn. Empty-text turns are dropped — the placeholder
    // assistant bubble we add for UX must not be sent.
    const apiHistory = history
      .filter((m) => (m.role === 'user' || m.role === 'assistant') && m.text.length > 0)
      .map((m) => ({ role: m.role as 'user' | 'assistant', content: m.text }))
    apiHistory.push({ role: 'user', content: userMsg })

    setHistory((h) => [...h, { role: 'user', text: userMsg }, { role: 'assistant', text: '' }])
    setStreaming(true)
    let receivedText = false
    let saw_done = false
    try {
      for await (const ev of streamAgentChat(agentId, instanceId, userMsg, conversationId, apiHistory)) {
        if (ev.type === 'text' && ev.content) {
          receivedText = true
          setHistory((h) => {
            const next = [...h]
            const last = next[next.length - 1]
            if (last && last.role === 'assistant') {
              next[next.length - 1] = { ...last, text: last.text + ev.content }
            }
            return next
          })
        } else if (ev.type === 'done') {
          saw_done = true
          break
        }
      }
      // Stream closed without producing any assistant text — almost always
      // means the agent runtime failed to start inside the sandbox (missing
      // ANTHROPIC_API_KEY, claude CLI not found, etc). Surface it instead of
      // leaving the user staring at an empty bubble.
      if (!receivedText) {
        setHistory((h) => {
          const next = [...h]
          // Replace the empty assistant placeholder with a system error
          if (next[next.length - 1]?.role === 'assistant' && !next[next.length - 1].text) {
            next.pop()
          }
          next.push({
            role: 'system',
            text: saw_done
              ? 'Agent ended the turn without sending any text. Likely the OpenClaw chat-completions endpoint is disabled (snapshot needs gateway.http.endpoints.chatCompletions.enabled=true), or OPENROUTER_API_KEY is missing from the agent\'s secret store.'
              : 'Stream closed before any response. Likely sessions-api or the proxy timed out.',
          })
          return next
        })
      }
    } catch (err) {
      setHistory((h) => {
        const next = [...h]
        if (next[next.length - 1]?.role === 'assistant' && !next[next.length - 1].text) {
          next.pop()
        }
        next.push({ role: 'system', text: `Error: ${(err as Error).message}` })
        return next
      })
    } finally {
      setStreaming(false)
    }
  }

  if (!instanceId) {
    return (
      <p style={{ color: 'var(--text-tertiary)', fontSize: 13 }}>
        Agent has no instance yet. Once the sandbox is up you'll be able to chat here.
      </p>
    )
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '60vh', minHeight: 400 }}>
      <div
        ref={scrollRef}
        style={{
          flex: 1,
          overflowY: 'auto',
          background: 'rgba(255,255,255,0.02)',
          border: '1px solid var(--border-subtle)',
          borderRadius: 8,
          padding: 16,
          marginBottom: 12,
          display: 'flex',
          flexDirection: 'column',
          gap: 10,
        }}
      >
        {history.length === 0 && (
          <p style={{ color: 'var(--text-tertiary)', fontSize: 13, margin: 0 }}>
            Talk to the agent here. Messages don't go through Telegram — direct via the platform's instance API.
          </p>
        )}
        {history.map((m, i) => (
          <div key={i} style={{
            alignSelf: m.role === 'user' ? 'flex-end' : 'flex-start',
            maxWidth: '78%',
            background: m.role === 'user'
              ? 'var(--gradient-primary, #6366f1)'
              : m.role === 'system' ? 'rgba(248,113,113,0.1)'
                : 'rgba(255,255,255,0.04)',
            color: m.role === 'user' ? '#fff' : m.role === 'system' ? '#f87171' : 'var(--text-primary)',
            padding: '8px 12px',
            borderRadius: 10,
            fontSize: 13,
            whiteSpace: 'pre-wrap',
            border: m.role === 'system' ? '1px solid rgba(248,113,113,0.3)' : 'none',
          }}>
            {m.text || (streaming && m.role === 'assistant' && i === history.length - 1 ? '…' : '')}
          </div>
        ))}
      </div>

      <form
        onSubmit={(e) => { e.preventDefault(); void send() }}
        style={{ display: 'flex', gap: 8 }}
      >
        <input
          ref={inputRef}
          value={input}
          onChange={(e) => setInput(e.target.value)}
          placeholder={disabled ? 'Agent not ready' : 'Send a message…'}
          disabled={disabled || streaming}
          style={{ ...input1, flex: 1 }}
          autoFocus
        />
        <button type="submit" disabled={disabled || streaming || !input.trim()} style={primaryButton}>
          {streaming ? <BusyLabel text="Sending…" /> : 'Send'}
        </button>
      </form>
    </div>
  )
}

function ChannelsTab({
  detail,
  acting,
  onAction,
}: {
  detail: AgentDetail
  acting: string | null
  onAction: <T>(label: string, fn: () => Promise<T>, ok: string) => Promise<void>
}) {
  const [telegramModal, setTelegramModal] = useState(false)
  const [paywallModal, setPaywallModal] = useState(false)
  const [subscribing, setSubscribing] = useState(false)
  const [entitlements, setEntitlements] = useState<Record<string, AgentEntitlement>>({})
  const telegramConnected = detail.channels.some((c) => c.name === 'telegram')
  const canMutate = detail.core !== null

  async function refreshEntitlements() {
    try {
      const res = await listAgentEntitlements(detail.id)
      const map: Record<string, AgentEntitlement> = {}
      for (const e of res.entitlements) map[e.feature] = e
      setEntitlements(map)
    } catch {
      // best-effort: leave previous state
    }
  }

  useEffect(() => {
    void refreshEntitlements()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [detail.id])

  if (!canMutate) {
    return (
      <p style={{ fontSize: 13, color: 'var(--text-tertiary)' }}>
        This agent has no managed core, so channels aren't applicable.
      </p>
    )
  }

  const telegramEntitlement = entitlements.telegram
  const telegramEntitled = telegramEntitlement?.entitled !== false // undefined treated as entitled (loading) to avoid flash
  const telegramUngated = telegramEntitlement?.reason === 'ungated'
  const telegramPriceCents = telegramEntitlement?.price_monthly_cents ?? 2000

  async function handleTelegramAction() {
    if (telegramConnected) {
      // Already connected — manage flow
      setTelegramModal(true)
      return
    }
    if (!telegramEntitled) {
      // Show paywall first
      setPaywallModal(true)
      return
    }
    // Entitled but not connected — connect flow
    setTelegramModal(true)
  }

  async function handleSubscribe() {
    setSubscribing(true)
    try {
      const result = await subscribeAgentFeature(detail.id, 'telegram')
      if (result.status === 'checkout_required') {
        // Bounce to Stripe Checkout for first-time card capture.
        // Intentional: we lose the "open the connect modal next" flow,
        // because Stripe redirects out and back. The user comes back
        // to /agents/:id/?success=true and has to click Connect again
        // — entitlement check will pass at that point.
        window.location.href = result.checkout_url
        return
      }
      // active / already_subscribed / ungated → entitled now
      await refreshEntitlements()
      setPaywallModal(false)
      setTelegramModal(true)
    } catch (err) {
      alert(`Subscription failed: ${(err as Error).message}`)
    } finally {
      setSubscribing(false)
    }
  }

  return (
    <>
      <div style={integrationGrid}>
        <IntegrationCard
          tag="channel"
          accent="sky"
          icon="✈️"
          name="Telegram"
          description={
            telegramUngated
              ? 'Talk to the agent through a Telegram bot. (Paywall not configured on this deployment.)'
              : telegramEntitled
                ? 'Talk to the agent through a Telegram bot.'
                : `Talk to the agent through a Telegram bot. $${(telegramPriceCents / 100).toFixed(0)}/mo per agent.`
          }
          status={telegramConnected ? 'connected' : 'available'}
          actionLabel={
            telegramConnected
              ? 'Configure'
              : telegramEntitled
                ? 'Connect'
                : `Subscribe & connect — $${(telegramPriceCents / 100).toFixed(0)}/mo`
          }
          actionTone={telegramConnected ? 'secondary' : 'primary'}
          busy={acting === 'connect telegram' || acting === 'disconnect telegram'}
          disabled={!!acting}
          onAction={handleTelegramAction}
        />

        {COMING_SOON_CHANNELS.map((it) => (
          <IntegrationCard
            key={it.name}
            tag="channel"
            accent="muted"
            icon={it.icon}
            name={it.name}
            description={it.description}
            status="coming"
          />
        ))}
      </div>

      {paywallModal && (
        <TelegramPaywallModal
          priceCents={telegramPriceCents}
          subscribing={subscribing}
          onClose={() => setPaywallModal(false)}
          onSubscribe={handleSubscribe}
        />
      )}

      {telegramModal && (
        <TelegramConnectModal
          alreadyConnected={telegramConnected}
          ungated={telegramUngated}
          subscriptionActive={!!entitlements.telegram?.entitled && !telegramUngated}
          subscriptionWillCancel={!!entitlements.telegram?.cancel_at_period_end}
          subscriptionRenewsAt={entitlements.telegram?.current_period_end}
          onClose={() => setTelegramModal(false)}
          onSubmit={async (token) => {
            setTelegramModal(false)
            await onAction(
              'connect telegram',
              () => connectTelegram(detail.id, token),
              telegramConnected
                ? 'Telegram bot token updated — webhook re-registered.'
                : 'Telegram connect queued — webhook will register shortly.',
            )
          }}
          onDisconnect={async () => {
            setTelegramModal(false)
            await onAction(
              'disconnect telegram',
              () => disconnectTelegram(detail.id),
              'Telegram disconnected.',
            )
          }}
          onCancelSubscription={async () => {
            try {
              await cancelAgentFeature(detail.id, 'telegram')
              await refreshEntitlements()
              setTelegramModal(false)
              alert('Subscription scheduled to cancel at period end. Telegram remains active until then.')
            } catch (err) {
              alert(`Cancel failed: ${(err as Error).message}`)
            }
          }}
        />
      )}
    </>
  )
}

function TelegramPaywallModal({
  priceCents,
  subscribing,
  onClose,
  onSubscribe,
}: {
  priceCents: number
  subscribing: boolean
  onClose: () => void
  onSubscribe: () => Promise<void>
}) {
  return (
    <div
      onClick={onClose}
      style={{
        position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.5)', zIndex: 100,
        display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 20,
      }}
    >
      <div
        onClick={(e) => e.stopPropagation()}
        style={{
          background: 'var(--bg-deep)',
          border: '1px solid var(--border-subtle)',
          borderRadius: 12, width: '100%', maxWidth: 460, padding: 22,
        }}
      >
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 14 }}>
          <h2 style={{ fontSize: 16, margin: 0, fontFamily: 'var(--font-display)' }}>Subscribe to Telegram for this agent</h2>
          <button onClick={onClose} style={iconButton}>×</button>
        </div>
        <div style={{ fontSize: 13, color: 'var(--text-secondary)', lineHeight: 1.6, marginBottom: 16 }}>
          Connecting Telegram to a managed agent costs <b>${(priceCents / 100).toFixed(0)}/month</b>. Each agent
          you connect to Telegram is billed separately. Cancel any time — service runs to the end of the
          current billing period.
        </div>
        <div style={{
          background: 'rgba(99,102,241,0.08)',
          border: '1px solid rgba(99,102,241,0.2)',
          borderRadius: 6, padding: '10px 12px',
          fontSize: 12, color: 'var(--text-secondary)', marginBottom: 16,
        }}>
          Your card on file will be charged immediately and prorated to the end of the current billing
          period. If no card is on file, Stripe Checkout will collect one.
        </div>
        <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
          <button type="button" onClick={onClose} style={secondaryButton}>Cancel</button>
          <button onClick={() => void onSubscribe()} disabled={subscribing} style={primaryButton}>
            {subscribing ? <BusyLabel text="Subscribing…" /> : `Subscribe — $${(priceCents / 100).toFixed(0)}/mo`}
          </button>
        </div>
      </div>
    </div>
  )
}

function PluginsTab({
  detail,
  acting,
  onAction,
}: {
  detail: AgentDetail
  acting: string | null
  onAction: <T>(label: string, fn: () => Promise<T>, ok: string) => Promise<void>
}) {
  const hasGbrain = detail.packages.some((p) => p.name === 'gbrain')
  const canMutate = detail.core !== null

  if (!canMutate) {
    return (
      <p style={{ fontSize: 13, color: 'var(--text-tertiary)' }}>
        This agent has no managed core, so plugins aren't applicable.
      </p>
    )
  }

  return (
    <div style={integrationGrid}>
      <IntegrationCard
        tag="plugin"
        accent="indigo"
        icon="🧠"
        name="gbrain"
        description="Long-term memory + vector recall."
        status={hasGbrain ? 'connected' : 'available'}
        statusLabel={hasGbrain ? 'installed' : undefined}
        actionLabel={hasGbrain ? 'Uninstall' : 'Install'}
        actionTone={hasGbrain ? 'danger' : 'primary'}
        busy={acting === 'install gbrain' || acting === 'uninstall gbrain'}
        disabled={!!acting}
        onAction={() =>
          hasGbrain
            ? onAction('uninstall gbrain', () => uninstallGbrain(detail.id), 'gbrain uninstalled.')
            : onAction('install gbrain', () => installGbrain(detail.id), 'gbrain install queued.')
        }
      />

      {COMING_SOON_PLUGINS.map((it) => (
        <IntegrationCard
          key={it.name}
          tag="plugin"
          accent="muted"
          icon={it.icon}
          name={it.name}
          description={it.description}
          status="coming"
        />
      ))}
    </div>
  )
}

// Inspired by Pipedream Connect's most-popular integrations — surfaces
// what the agent will eventually be able to act on without misleading
// users about current capability.
const COMING_SOON_PLUGINS: Array<{ icon: string; name: string; description: string }> = [
  { icon: '📝', name: 'Notion', description: 'Read/write pages and databases.' },
  { icon: '🐙', name: 'GitHub', description: 'Issues, PRs, comments, files.' },
  { icon: '#',  name: 'Slack', description: 'Post messages, react, search.' },
  { icon: '📧', name: 'Gmail', description: 'Read/send mail, labels, drafts.' },
  { icon: '📅', name: 'Google Calendar', description: 'Events, scheduling, busy/free.' },
  { icon: '📄', name: 'Google Drive', description: 'Search, read, share files.' },
  { icon: '🎫', name: 'Linear', description: 'Issues, cycles, projects.' },
  { icon: '🎨', name: 'Figma', description: 'Files, comments, frames.' },
  { icon: '💳', name: 'Stripe', description: 'Customers, payments, subscriptions.' },
  { icon: '🛠️', name: 'MCP servers', description: 'Bring your own tool integrations.' },
]

const integrationGrid: React.CSSProperties = {
  display: 'grid',
  gridTemplateColumns: 'repeat(auto-fill, minmax(220px, 1fr))',
  gap: 14,
}

const COMING_SOON_CHANNELS: Array<{ icon: string; name: string; description: string }> = [
  { icon: '💬', name: 'Discord', description: 'Server chat + DMs.' },
  { icon: '📱', name: 'WhatsApp', description: 'Cloud API webhook.' },
  { icon: '#',  name: 'Slack', description: 'Workspace bot.' },
  { icon: '✉️', name: 'Email', description: 'IMAP / inbound webhook.' },
]

type IntegrationStatus = 'connected' | 'available' | 'coming'
type IntegrationAccent = 'indigo' | 'sky' | 'muted'
type ActionTone = 'primary' | 'secondary' | 'danger'

function IntegrationCard({
  tag,
  accent,
  icon,
  name,
  description,
  status,
  statusLabel,
  actionLabel,
  actionTone,
  busy,
  disabled,
  onAction,
}: {
  tag: 'channel' | 'plugin'
  accent: IntegrationAccent
  icon: string
  name: string
  description: string
  status: IntegrationStatus
  statusLabel?: string
  actionLabel?: string
  actionTone?: ActionTone
  busy?: boolean
  disabled?: boolean
  onAction?: () => void
}) {
  const accentColor =
    accent === 'indigo' ? 'rgba(99,102,241,0.7)'
      : accent === 'sky' ? 'rgba(56,189,248,0.7)'
        : 'rgba(255,255,255,0.12)'

  return (
    <div
      style={{
        background: 'rgba(255,255,255,0.02)',
        border: '1px solid var(--border-subtle)',
        borderRadius: 10,
        padding: 14,
        display: 'flex',
        flexDirection: 'column',
        gap: 12,
        opacity: status === 'coming' ? 0.55 : 1,
        minHeight: 168,
      }}
    >
      <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', gap: 8 }}>
        <div
          style={{
            width: 34, height: 34, borderRadius: 8,
            background: `linear-gradient(135deg, ${accentColor}, rgba(255,255,255,0.04))`,
            display: 'flex', alignItems: 'center', justifyContent: 'center',
            fontSize: 18,
          }}
          aria-hidden
        >
          {icon}
        </div>
        <span style={{
          fontSize: 9,
          textTransform: 'uppercase',
          letterSpacing: '0.08em',
          color: 'var(--text-tertiary)',
          fontFamily: 'var(--font-mono)',
        }}>
          {tag}
        </span>
      </div>

      <div style={{ flex: 1 }}>
        <div style={{ fontSize: 14, fontWeight: 600, color: 'var(--text-primary)' }}>{name}</div>
        <div style={{ fontSize: 12, color: 'var(--text-secondary)', marginTop: 4, lineHeight: 1.4 }}>
          {description}
        </div>
      </div>

      {status === 'connected' && (
        <span style={pill('emerald')}>{statusLabel ?? 'connected'}</span>
      )}
      {status === 'coming' && (
        <span style={{
          alignSelf: 'flex-start',
          fontSize: 10, fontFamily: 'var(--font-mono)',
          color: 'var(--text-tertiary)',
          background: 'rgba(255,255,255,0.04)',
          border: '1px solid var(--border-subtle)',
          padding: '3px 8px', borderRadius: 999,
          textTransform: 'uppercase', letterSpacing: '0.08em',
        }}>
          coming soon
        </span>
      )}

      {actionLabel && status !== 'coming' && (
        <button
          onClick={onAction}
          disabled={disabled || busy}
          style={
            actionTone === 'danger' ? dangerButton
              : actionTone === 'secondary' ? secondaryButton
                : primaryButton
          }
        >
          {busy ? <BusyLabel text="Working…" /> : actionLabel}
        </button>
      )}
    </div>
  )
}

function TelegramConnectModal({
  alreadyConnected,
  ungated,
  subscriptionActive,
  subscriptionWillCancel,
  subscriptionRenewsAt,
  onClose,
  onSubmit,
  onDisconnect,
  onCancelSubscription,
}: {
  alreadyConnected: boolean
  ungated: boolean
  subscriptionActive: boolean
  subscriptionWillCancel: boolean
  subscriptionRenewsAt?: string
  onClose: () => void
  onSubmit: (token: string) => void | Promise<void>
  onDisconnect: () => void | Promise<void>
  onCancelSubscription: () => void | Promise<void>
}) {
  const [token, setToken] = useState('')
  const title = alreadyConnected ? 'Configure Telegram' : 'Connect Telegram'
  return (
    <div
      onClick={onClose}
      style={{
        position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.5)', zIndex: 100,
        display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 20,
      }}
    >
      <div
        onClick={(e) => e.stopPropagation()}
        style={{
          background: 'var(--bg-deep)',
          border: '1px solid var(--border-subtle)',
          borderRadius: 12, width: '100%', maxWidth: 460, padding: 22,
        }}
      >
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 16 }}>
          <h2 style={{ fontSize: 16, margin: 0, fontFamily: 'var(--font-display)' }}>{title}</h2>
          <button onClick={onClose} style={iconButton}>×</button>
        </div>

        {alreadyConnected && (
          <p style={{ fontSize: 12, color: 'var(--text-secondary)', margin: '0 0 14px 0', lineHeight: 1.5 }}>
            Telegram is currently connected. Paste a new bot token to swap it,
            or disconnect to detach this agent from Telegram entirely.
          </p>
        )}

        {ungated && (
          <div style={{
            background: 'rgba(251,191,36,0.08)',
            border: '1px solid rgba(251,191,36,0.25)',
            borderRadius: 6, padding: '8px 12px', marginBottom: 14,
            fontSize: 12, color: 'var(--text-secondary)',
          }}>
            <b style={{ color: '#fbbf24' }}>Not gated yet</b> on this deployment — connect proceeds without
            a subscription. Set <code style={codeInline}>STRIPE_TELEGRAM_AGENT_PRICE_ID</code> on the server
            to enable the paywall.
          </div>
        )}

        {!ungated && subscriptionActive && (
          <div style={{
            background: 'rgba(52,211,153,0.08)',
            border: '1px solid rgba(52,211,153,0.2)',
            borderRadius: 6, padding: '8px 12px', marginBottom: 14,
            fontSize: 12, color: 'var(--text-secondary)',
          }}>
            {subscriptionWillCancel ? (
              <>
                Subscription <b>scheduled to cancel</b>
                {subscriptionRenewsAt && ` on ${new Date(subscriptionRenewsAt).toLocaleDateString()}`}
                . Telegram remains active until then.
              </>
            ) : (
              <>
                Subscription <b style={{ color: '#34d399' }}>active</b>
                {subscriptionRenewsAt && ` · renews ${new Date(subscriptionRenewsAt).toLocaleDateString()}`}
                .{' '}
                <button
                  type="button"
                  onClick={() => {
                    if (window.confirm('Cancel Telegram subscription? Service continues until period end.')) {
                      void onCancelSubscription()
                    }
                  }}
                  style={{ background: 'none', border: 'none', color: 'var(--accent-rose, #f87171)', cursor: 'pointer', padding: 0, fontSize: 12, textDecoration: 'underline' }}
                >
                  Cancel subscription
                </button>
              </>
            )}
          </div>
        )}

        <form
          onSubmit={(e) => { e.preventDefault(); if (token.trim()) void onSubmit(token.trim()) }}
          style={{ display: 'grid', gap: 14 }}
        >
          <Field
            label={alreadyConnected ? 'New bot token' : 'Bot token'}
            hint="Paste the token @BotFather gave you. Stored in this agent's secret store."
          >
            <input
              autoFocus
              value={token}
              onChange={(e) => setToken(e.target.value)}
              placeholder="1234567890:AAA…"
              style={input1}
            />
          </Field>
          <div style={{ display: 'flex', gap: 8, justifyContent: 'space-between', alignItems: 'center' }}>
            {alreadyConnected ? (
              <button
                type="button"
                onClick={() => {
                  if (window.confirm('Disconnect Telegram from this agent? Subscription remains active until cancelled separately.')) {
                    void onDisconnect()
                  }
                }}
                style={dangerButton}
              >
                Disconnect
              </button>
            ) : <span />}
            <div style={{ display: 'flex', gap: 8 }}>
              <button type="button" onClick={onClose} style={secondaryButton}>Cancel</button>
              <button type="submit" disabled={!token.trim()} style={primaryButton}>
                {alreadyConnected ? 'Update' : 'Connect'}
              </button>
            </div>
          </div>
        </form>
      </div>
    </div>
  )
}

function EventsTab({ agentId }: { agentId: string }) {
  const [events, setEvents] = useState<AgentEventRow[]>([])
  const [operations, setOperations] = useState<AgentOperation[]>([])
  const [loading, setLoading] = useState(true)

  async function load() {
    try {
      const [e, o] = await Promise.all([
        getAgentEvents(agentId, 100),
        getAgentOperations(agentId, 30),
      ])
      setEvents(e.events)
      setOperations(o.operations)
    } catch {
      // silent — UI will just show nothing
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
    const interval = setInterval(() => void load(), 5000)
    return () => clearInterval(interval)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [agentId])

  if (loading) {
    return (
      <div style={{ display: 'flex', alignItems: 'center', gap: 10, color: 'var(--text-tertiary)', fontSize: 13 }}>
        <span className="loading-spinner" style={{ width: 14, height: 14, borderWidth: 1.5 }} />
        Loading events…
      </div>
    )
  }

  return (
    <div style={{ display: 'grid', gap: 22 }}>
      <Section title="Events">
        {events.length === 0 ? (
          <p style={{ fontSize: 12, color: 'var(--text-tertiary)' }}>No events yet.</p>
        ) : (
          <div style={{ display: 'grid', gap: 6 }}>
            {events.map((e) => (
              <div key={e.id} style={{
                fontSize: 12,
                fontFamily: 'var(--font-mono)',
                display: 'flex', gap: 10,
                padding: '6px 10px',
                background: 'rgba(255,255,255,0.02)',
                borderLeft: `2px solid ${e.type === 'error' ? '#f87171' : e.type === 'warning' ? '#fbbf24' : '#6366f1'}`,
                borderRadius: 4,
              }}>
                <span style={{ color: 'var(--text-tertiary)', flexShrink: 0 }}>{new Date(e.at).toLocaleTimeString()}</span>
                <span style={{ color: e.type === 'error' ? '#f87171' : e.type === 'warning' ? '#fbbf24' : 'var(--text-secondary)', flexShrink: 0 }}>
                  {e.phase}
                </span>
                <span style={{ color: 'var(--text-primary)', wordBreak: 'break-word' }}>{e.message}</span>
              </div>
            ))}
          </div>
        )}
      </Section>

      <Section title="Operations">
        {operations.length === 0 ? (
          <p style={{ fontSize: 12, color: 'var(--text-tertiary)' }}>No operations recorded.</p>
        ) : (
          <div style={{ display: 'grid', gap: 6 }}>
            {operations.map((op) => (
              <div key={op.id} style={{
                fontSize: 12,
                display: 'flex', gap: 10,
                padding: '6px 10px',
                background: 'rgba(255,255,255,0.02)',
                borderRadius: 4,
              }}>
                <span style={{ color: 'var(--text-tertiary)', fontFamily: 'var(--font-mono)', flexShrink: 0 }}>
                  {new Date(op.created_at).toLocaleTimeString()}
                </span>
                <span style={{ color: 'var(--text-primary)', fontFamily: 'var(--font-mono)', flexShrink: 0 }}>
                  {op.kind}
                </span>
                {op.target_key && <span style={{ color: 'var(--text-tertiary)' }}>{op.target_key}</span>}
                <span style={pill(op.state === 'success' ? 'emerald' : op.state === 'error' ? 'rose' : 'amber')}>
                  {op.state}
                </span>
                {op.message && <span style={{ color: 'var(--text-secondary)', wordBreak: 'break-word' }}>{op.message}</span>}
              </div>
            ))}
          </div>
        )}
      </Section>
    </div>
  )
}

// ── helpers ─────────────────────────────────────────────────────

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div>
      <h3 style={{
        fontSize: 11, textTransform: 'uppercase', letterSpacing: '0.06em',
        color: 'var(--text-tertiary)', marginBottom: 10, fontFamily: 'var(--font-mono)',
      }}>
        {title}
      </h3>
      <div style={{ display: 'grid', gap: 8 }}>{children}</div>
    </div>
  )
}

function Field({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <label style={{ display: 'grid', gap: 4 }}>
      <span style={{ fontSize: 12, color: 'var(--text-secondary)', fontWeight: 500 }}>{label}</span>
      {children}
      {hint && <span style={{ fontSize: 11, color: 'var(--text-tertiary)' }}>{hint}</span>}
    </label>
  )
}

function BusyLabel({ text }: { text: string }) {
  return (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
      <span
        className="loading-spinner"
        style={{ width: 11, height: 11, borderWidth: 1.5, borderColor: 'rgba(255,255,255,0.25)', borderTopColor: 'currentColor' }}
      />
      {text}
    </span>
  )
}

function pill(tone: 'emerald' | 'amber' | 'rose'): React.CSSProperties {
  const palette = {
    emerald: { bg: 'rgba(52,211,153,0.12)', border: 'rgba(52,211,153,0.35)', fg: '#34d399' },
    amber: { bg: 'rgba(251,191,36,0.12)', border: 'rgba(251,191,36,0.35)', fg: '#fbbf24' },
    rose: { bg: 'rgba(248,113,113,0.12)', border: 'rgba(248,113,113,0.35)', fg: '#f87171' },
  }[tone]
  return {
    background: palette.bg, border: `1px solid ${palette.border}`, color: palette.fg,
    padding: '3px 9px', borderRadius: 999, fontSize: 11, fontWeight: 500, fontFamily: 'var(--font-mono)',
  }
}

const primaryButton: React.CSSProperties = {
  background: 'var(--gradient-primary, #6366f1)', color: '#fff',
  padding: '8px 14px', border: 'none', borderRadius: 6, cursor: 'pointer',
  fontSize: 13, fontWeight: 600, fontFamily: 'var(--font-body)',
}
const secondaryButton: React.CSSProperties = {
  background: 'rgba(255,255,255,0.04)', color: 'var(--text-primary)',
  padding: '7px 12px', border: '1px solid var(--border-subtle)', borderRadius: 6,
  cursor: 'pointer', fontSize: 12, fontFamily: 'var(--font-body)',
}
const dangerButton: React.CSSProperties = {
  background: 'rgba(248,113,113,0.08)', color: '#f87171',
  padding: '7px 12px', border: '1px solid rgba(248,113,113,0.35)', borderRadius: 6,
  cursor: 'pointer', fontSize: 12, fontFamily: 'var(--font-body)',
}
const iconButton: React.CSSProperties = {
  background: 'none', border: 'none', color: 'var(--text-tertiary)',
  cursor: 'pointer', fontSize: 22, padding: 0, width: 22, height: 22, lineHeight: 1,
}
const input1: React.CSSProperties = {
  background: 'rgba(255,255,255,0.02)', border: '1px solid var(--border-subtle)',
  borderRadius: 6, padding: '8px 10px',
  color: 'var(--text-primary)', fontSize: 13, fontFamily: 'var(--font-body)', outline: 'none',
}
const codeInline: React.CSSProperties = {
  fontFamily: 'var(--font-mono)', background: 'rgba(255,255,255,0.04)',
  padding: '1px 5px', borderRadius: 3, fontSize: 10.5,
}
