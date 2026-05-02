import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import {
  listAgents,
  getAgent,
  createAgent,
  deleteAgent,
  type Agent,
  type AgentDetail,
} from '../api/client'

type Banner = { kind: 'info' | 'error' | 'success'; text: string } | null

const statusColor: Record<string, string> = {
  ready: 'var(--accent-emerald, #34d399)',
  starting: 'var(--accent-amber, #fbbf24)',
  degraded: 'var(--accent-amber, #fbbf24)',
  error: 'var(--accent-rose, #f87171)',
  unknown: 'var(--text-tertiary)',
}

export default function Agents() {
  const [agents, setAgents] = useState<Agent[]>([])
  const [details, setDetails] = useState<Record<string, AgentDetail>>({})
  const [loading, setLoading] = useState(true)
  const [banner, setBanner] = useState<Banner>(null)
  const [showCreate, setShowCreate] = useState(false)
  const navigate = useNavigate()

  async function refresh() {
    try {
      const { agents } = await listAgents()
      setAgents(agents)
      const detailMap: Record<string, AgentDetail> = {}
      await Promise.all(
        agents.map(async (a) => {
          try {
            detailMap[a.id] = await getAgent(a.id)
          } catch {
            // best-effort: skip if detail fetch fails
          }
        }),
      )
      setDetails(detailMap)
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err)
      setBanner({ kind: 'error', text: `Failed to load agents: ${msg}` })
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void refresh()
    // Poll every 5s while the page is open — operation states change fast.
    const interval = setInterval(() => void refresh(), 5000)
    return () => clearInterval(interval)
  }, [])

  async function handleDelete(id: string) {
    if (!window.confirm(`Delete agent "${id}"? The sandbox will be killed.`)) return
    try {
      await deleteAgent(id)
      setBanner({ kind: 'success', text: `Agent ${id} deleted.` })
      await refresh()
    } catch (err) {
      setBanner({ kind: 'error', text: `Delete failed: ${(err as Error).message}` })
    }
  }

  return (
    <div>
      <header style={{ marginBottom: 28, display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
        <div>
          <h1 style={{ fontSize: 22, margin: 0, fontFamily: 'var(--font-display)', fontWeight: 700 }}>Agents</h1>
          <p style={{ color: 'var(--text-secondary)', fontSize: 13, marginTop: 6 }}>
            Managed agent runtimes powered by OpenClaw. Add channels and packages to extend what they do.
          </p>
        </div>
        <button onClick={() => setShowCreate(true)} style={primaryButton}>+ New agent</button>
      </header>

      {banner && (
        <div
          style={{
            padding: '10px 14px',
            borderRadius: 6,
            marginBottom: 16,
            fontSize: 13,
            background:
              banner.kind === 'error'
                ? 'rgba(248,113,113,0.08)'
                : banner.kind === 'success'
                  ? 'rgba(52,211,153,0.08)'
                  : 'rgba(99,102,241,0.08)',
            border: `1px solid ${
              banner.kind === 'error'
                ? 'rgba(248,113,113,0.3)'
                : banner.kind === 'success'
                  ? 'rgba(52,211,153,0.3)'
                  : 'rgba(99,102,241,0.3)'
            }`,
            color: 'var(--text-primary)',
            display: 'flex',
            justifyContent: 'space-between',
            alignItems: 'center',
          }}
        >
          <span>{banner.text}</span>
          <button onClick={() => setBanner(null)} style={iconButton}>×</button>
        </div>
      )}

      {loading ? (
        <div style={{
          display: 'flex', alignItems: 'center', gap: 10,
          color: 'var(--text-tertiary)', fontSize: 13, padding: '32px 0',
        }}>
          <span className="loading-spinner" style={{ width: 14, height: 14, borderWidth: 1.5 }} />
          Loading agents…
        </div>
      ) : agents.length === 0 ? (
        <EmptyState onCreate={() => setShowCreate(true)} />
      ) : (
        <div style={{ display: 'grid', gap: 10 }}>
          {agents.map((a) => {
            const d = details[a.id]
            return (
              <div
                key={a.id}
                role="button"
                tabIndex={0}
                onClick={() => navigate(`/agents/${encodeURIComponent(a.id)}`)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' || e.key === ' ') {
                    e.preventDefault()
                    navigate(`/agents/${encodeURIComponent(a.id)}`)
                  }
                }}
                onMouseEnter={(e) => { e.currentTarget.style.background = 'rgba(255,255,255,0.04)' }}
                onMouseLeave={(e) => { e.currentTarget.style.background = 'rgba(255,255,255,0.02)' }}
                style={{
                  background: 'rgba(255,255,255,0.02)',
                  border: '1px solid var(--border-subtle)',
                  borderRadius: 10,
                  padding: '14px 18px',
                  display: 'flex',
                  alignItems: 'center',
                  gap: 16,
                  cursor: 'pointer',
                  transition: 'background 0.12s ease',
                }}
              >
                <div
                  className={
                    d?.status === 'starting' || d?.status === 'degraded' || d?.current_operation
                      ? 'pulse-dot'
                      : undefined
                  }
                  style={{
                    width: 8,
                    height: 8,
                    borderRadius: '50%',
                    background: statusColor[d?.status ?? 'unknown'] ?? statusColor.unknown,
                    flexShrink: 0,
                    boxShadow:
                      d?.status === 'starting' || d?.current_operation
                        ? `0 0 0 3px ${statusColor[d?.status ?? 'starting']}22`
                        : undefined,
                  }}
                  title={d?.current_operation ? `${d.current_operation.kind} · ${d.current_operation.phase}` : (d?.status ?? 'unknown')}
                />
                <div style={{ flex: 1, minWidth: 0 }}>
                  <div style={{ display: 'flex', alignItems: 'baseline', gap: 8 }}>
                    <span style={{ fontWeight: 600, fontSize: 14 }}>{a.display_name}</span>
                    <span style={{ fontSize: 11, color: 'var(--text-tertiary)', fontFamily: 'var(--font-mono)' }}>
                      {a.id}
                    </span>
                  </div>
                  <div style={{ fontSize: 11, color: 'var(--text-secondary)', marginTop: 4, display: 'flex', gap: 12 }}>
                    {a.core && <span>core: <code style={codeInline}>{a.core}</code></span>}
                    <span>
                      packages: {a.packages.length === 0 ? '—' : a.packages.map((p) => p.name).join(', ')}
                    </span>
                    <span>
                      channels: {a.channels.length === 0 ? '—' : a.channels.map((c) => c.name).join(', ')}
                    </span>
                  </div>
                  {d?.current_operation && (
                    <div style={{
                      fontSize: 11, color: 'var(--accent-amber, #fbbf24)', marginTop: 4,
                      display: 'flex', alignItems: 'center', gap: 6,
                    }}>
                      <span
                        className="loading-spinner"
                        style={{
                          width: 10, height: 10, borderWidth: 1.5,
                          borderColor: 'rgba(251,191,36,0.25)',
                          borderTopColor: 'var(--accent-amber, #fbbf24)',
                        }}
                      />
                      <span>
                        {d.current_operation.kind} · {d.current_operation.phase}
                        {d.current_operation.message ? ` — ${d.current_operation.message}` : ''}
                      </span>
                    </div>
                  )}
                  {d?.last_error && d.status !== 'ready' && (
                    <div style={{ fontSize: 11, color: 'var(--accent-rose, #f87171)', marginTop: 4 }}>
                      {d.last_error.phase}: {d.last_error.message}
                    </div>
                  )}
                </div>
                <div style={{ display: 'flex', gap: 6 }} onClick={(e) => e.stopPropagation()}>
                  <button
                    onClick={(e) => {
                      e.stopPropagation()
                      handleDelete(a.id)
                    }}
                    style={dangerButton}
                  >
                    Delete
                  </button>
                </div>
              </div>
            )
          })}
        </div>
      )}

      {showCreate && (
        <CreateAgentModal
          onClose={() => setShowCreate(false)}
          onCreated={(msg) => {
            setShowCreate(false)
            setBanner({ kind: 'success', text: msg })
            void refresh()
          }}
          onError={(msg) => setBanner({ kind: 'error', text: msg })}
        />
      )}

    </div>
  )
}

function EmptyState({ onCreate }: { onCreate: () => void }) {
  return (
    <div
      style={{
        textAlign: 'center',
        padding: '56px 24px 64px',
        border: '1px solid rgba(255,77,77,0.22)',
        borderRadius: 14,
        background:
          'radial-gradient(ellipse at top, rgba(255,77,77,0.10), transparent 60%), rgba(255,255,255,0.015)',
        position: 'relative',
        overflow: 'hidden',
      }}
    >
      <ClawLogo />
      <div style={{ fontSize: 22, fontWeight: 700, marginTop: 18, fontFamily: 'var(--font-display)' }}>
        Deploy an OpenClaw managed agent
      </div>
      <p style={{
        color: 'var(--text-secondary)', fontSize: 14, marginTop: 10, marginBottom: 26,
        maxWidth: 520, marginLeft: 'auto', marginRight: 'auto', lineHeight: 1.55,
      }}>
        OpenClaw runs the gateway, model routing, and tool execution inside a sandbox we
        manage for you. Connect Telegram, install gbrain for long-term memory, and you're
        live in under a minute.
      </p>
      <button onClick={onCreate} style={primaryButton}>Create your first agent</button>
    </div>
  )
}

// OpenClaw lobster mark — pulled from openclaw.ai/favicon.svg so the dashboard
// and agents-empty-state both use the same brand asset as the marketing site.
// Inlined so it ships with the bundle. Gradient id is scoped (`agents-…`) to
// avoid colliding with the same logo on other pages.
function ClawLogo() {
  return (
    <svg
      width="80"
      height="80"
      viewBox="0 0 120 120"
      fill="none"
      xmlns="http://www.w3.org/2000/svg"
      aria-hidden="true"
      style={{ filter: 'drop-shadow(0 0 22px rgba(255,77,77,0.40))' }}
    >
      <defs>
        <linearGradient id="agents-lobster-gradient" x1="0%" y1="0%" x2="100%" y2="100%">
          <stop offset="0%" stopColor="#ff4d4d" />
          <stop offset="100%" stopColor="#991b1b" />
        </linearGradient>
      </defs>
      <path d="M60 10 C30 10 15 35 15 55 C15 75 30 95 45 100 L45 110 L55 110 L55 100 C55 100 60 102 65 100 L65 110 L75 110 L75 100 C90 95 105 75 105 55 C105 35 90 10 60 10Z" fill="url(#agents-lobster-gradient)" />
      <path d="M20 45 C5 40 0 50 5 60 C10 70 20 65 25 55 C28 48 25 45 20 45Z" fill="url(#agents-lobster-gradient)" />
      <path d="M100 45 C115 40 120 50 115 60 C110 70 100 65 95 55 C92 48 95 45 100 45Z" fill="url(#agents-lobster-gradient)" />
      <path d="M45 15 Q35 5 30 8" stroke="#ff4d4d" strokeWidth="3" strokeLinecap="round" />
      <path d="M75 15 Q85 5 90 8" stroke="#ff4d4d" strokeWidth="3" strokeLinecap="round" />
      <circle cx="45" cy="35" r="6" fill="#050810" />
      <circle cx="75" cy="35" r="6" fill="#050810" />
      <circle cx="46" cy="34" r="2.5" fill="#00e5cc" />
      <circle cx="76" cy="34" r="2.5" fill="#00e5cc" />
    </svg>
  )
}

function CreateAgentModal({
  onClose,
  onCreated,
  onError,
}: {
  onClose: () => void
  onCreated: (msg: string) => void
  onError: (msg: string) => void
}) {
  const [id, setId] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [core, setCore] = useState<'openclaw'>('openclaw')
  const [submitting, setSubmitting] = useState(false)

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    if (!/^[a-z0-9][a-z0-9-]*$/.test(id)) {
      onError('id must be lowercase alphanumeric + hyphens, starting with a letter or digit')
      return
    }
    setSubmitting(true)
    try {
      await createAgent({
        id,
        display_name: displayName || id,
        core,
      })
      onCreated(`Agent "${id}" created. Sandbox is provisioning.`)
    } catch (err) {
      onError(`Create failed: ${(err as Error).message}`)
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <ModalShell onClose={onClose} title="Create agent">
      <form onSubmit={submit} style={{ display: 'grid', gap: 14 }}>
        <Field label="Agent ID" hint="lowercase, hyphens, must be unique">
          <input
            autoFocus
            value={id}
            onChange={(e) => setId(e.target.value)}
            placeholder="my-bot"
            style={input}
            required
          />
        </Field>
        <Field label="Display name (optional)">
          <input value={displayName} onChange={(e) => setDisplayName(e.target.value)} placeholder={id || 'My Bot'} style={input} />
        </Field>
        <Field label="Core" hint="OpenClaw is the only runtime available right now. Hermes support is coming soon.">
          <select value={core} onChange={(e) => setCore(e.target.value as 'openclaw')} style={input}>
            <option value="openclaw">openclaw</option>
            <option value="hermes" disabled>hermes (coming soon)</option>
          </select>
        </Field>
        <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end', marginTop: 4 }}>
          <button type="button" onClick={onClose} style={secondaryButton}>Cancel</button>
          <button type="submit" disabled={submitting || !id} style={primaryButton}>
            {submitting ? <BusyLabel text="Creating…" /> : 'Create agent'}
          </button>
        </div>
      </form>
    </ModalShell>
  )
}

function ModalShell({ children, title, onClose }: { children: React.ReactNode; title: string; onClose: () => void }) {
  return (
    <div
      onClick={onClose}
      style={{
        position: 'fixed',
        inset: 0,
        background: 'rgba(0,0,0,0.5)',
        zIndex: 100,
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        padding: 20,
      }}
    >
      <div
        onClick={(e) => e.stopPropagation()}
        style={{
          background: 'var(--bg-deep)',
          border: '1px solid var(--border-subtle)',
          borderRadius: 12,
          width: '100%',
          maxWidth: 480,
          maxHeight: '90vh',
          overflowY: 'auto',
          padding: 22,
        }}
      >
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 18 }}>
          <h2 style={{ fontSize: 16, margin: 0, fontFamily: 'var(--font-display)' }}>{title}</h2>
          <button onClick={onClose} style={iconButton}>×</button>
        </div>
        {children}
      </div>
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
        style={{
          width: 11, height: 11, borderWidth: 1.5,
          borderColor: 'rgba(255,255,255,0.25)',
          borderTopColor: 'currentColor',
        }}
      />
      {text}
    </span>
  )
}

const primaryButton: React.CSSProperties = {
  background: 'var(--gradient-primary, #6366f1)',
  color: '#fff',
  padding: '8px 14px',
  border: 'none',
  borderRadius: 6,
  cursor: 'pointer',
  fontSize: 13,
  fontWeight: 600,
  fontFamily: 'var(--font-body)',
}

const secondaryButton: React.CSSProperties = {
  background: 'rgba(255,255,255,0.04)',
  color: 'var(--text-primary)',
  padding: '7px 12px',
  border: '1px solid var(--border-subtle)',
  borderRadius: 6,
  cursor: 'pointer',
  fontSize: 12,
  fontFamily: 'var(--font-body)',
}

const dangerButton: React.CSSProperties = {
  background: 'rgba(248,113,113,0.08)',
  color: '#f87171',
  padding: '7px 12px',
  border: '1px solid rgba(248,113,113,0.35)',
  borderRadius: 6,
  cursor: 'pointer',
  fontSize: 12,
  fontFamily: 'var(--font-body)',
}

const iconButton: React.CSSProperties = {
  background: 'none',
  border: 'none',
  color: 'var(--text-tertiary)',
  cursor: 'pointer',
  fontSize: 22,
  padding: 0,
  width: 22,
  height: 22,
  lineHeight: 1,
}

const input: React.CSSProperties = {
  background: 'rgba(255,255,255,0.02)',
  border: '1px solid var(--border-subtle)',
  borderRadius: 6,
  padding: '8px 10px',
  color: 'var(--text-primary)',
  fontSize: 13,
  fontFamily: 'var(--font-body)',
  outline: 'none',
}

const codeInline: React.CSSProperties = {
  fontFamily: 'var(--font-mono)',
  background: 'rgba(255,255,255,0.04)',
  padding: '1px 5px',
  borderRadius: 3,
  fontSize: 10.5,
}
