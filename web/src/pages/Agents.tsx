import { useEffect, useState } from 'react'
import {
  listAgents,
  getAgent,
  createAgent,
  deleteAgent,
  installGbrain,
  uninstallGbrain,
  connectTelegram,
  disconnectTelegram,
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
  const [drawer, setDrawer] = useState<string | null>(null)

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
            Managed agent runtimes (Hermes, OpenClaw). Add channels and packages to extend what they do.
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
        <div style={{ color: 'var(--text-tertiary)', fontSize: 13, padding: '32px 0' }}>Loading…</div>
      ) : agents.length === 0 ? (
        <EmptyState onCreate={() => setShowCreate(true)} />
      ) : (
        <div style={{ display: 'grid', gap: 10 }}>
          {agents.map((a) => {
            const d = details[a.id]
            return (
              <div
                key={a.id}
                style={{
                  background: 'rgba(255,255,255,0.02)',
                  border: '1px solid var(--border-subtle)',
                  borderRadius: 10,
                  padding: '14px 18px',
                  display: 'flex',
                  alignItems: 'center',
                  gap: 16,
                }}
              >
                <div
                  style={{
                    width: 8,
                    height: 8,
                    borderRadius: '50%',
                    background: statusColor[d?.status ?? 'unknown'] ?? statusColor.unknown,
                    flexShrink: 0,
                  }}
                  title={d?.status ?? 'unknown'}
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
                    <div style={{ fontSize: 11, color: 'var(--accent-amber, #fbbf24)', marginTop: 4 }}>
                      {d.current_operation.kind} · {d.current_operation.phase}
                      {d.current_operation.message ? ` — ${d.current_operation.message}` : ''}
                    </div>
                  )}
                  {d?.last_error && d.status !== 'ready' && (
                    <div style={{ fontSize: 11, color: 'var(--accent-rose, #f87171)', marginTop: 4 }}>
                      {d.last_error.phase}: {d.last_error.message}
                    </div>
                  )}
                </div>
                <div style={{ display: 'flex', gap: 6 }}>
                  <button onClick={() => setDrawer(a.id)} style={secondaryButton}>Manage</button>
                  <button onClick={() => handleDelete(a.id)} style={dangerButton}>Delete</button>
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

      {drawer && details[drawer] && (
        <ManageDrawer
          detail={details[drawer]!}
          onClose={() => setDrawer(null)}
          onAction={(b) => {
            setBanner(b)
            void refresh()
          }}
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
        padding: '60px 24px',
        border: '1px dashed var(--border-subtle)',
        borderRadius: 12,
      }}
    >
      <div style={{ fontSize: 16, fontWeight: 600, marginBottom: 8 }}>No agents yet</div>
      <p style={{ color: 'var(--text-secondary)', fontSize: 13, marginBottom: 20 }}>
        Spin up a managed Hermes agent and connect it to Telegram. Add gbrain to give it long-term memory.
      </p>
      <button onClick={onCreate} style={primaryButton}>Create your first agent</button>
    </div>
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
  const [core, setCore] = useState<'hermes' | 'openclaw' | ''>('hermes')
  const [model, setModel] = useState('')
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
        core: core || undefined,
        model: model || undefined,
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
        <Field label="Core" hint="Hermes is the standard chat agent. Leave blank for a bare sandbox.">
          <select value={core} onChange={(e) => setCore(e.target.value as 'hermes' | 'openclaw' | '')} style={input}>
            <option value="hermes">hermes</option>
            <option value="openclaw">openclaw</option>
            <option value="">(none — bare sandbox)</option>
          </select>
        </Field>
        <Field label="Model (optional)">
          <input value={model} onChange={(e) => setModel(e.target.value)} placeholder="anthropic/claude-3-5-sonnet" style={input} />
        </Field>
        <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end', marginTop: 4 }}>
          <button type="button" onClick={onClose} style={secondaryButton}>Cancel</button>
          <button type="submit" disabled={submitting || !id} style={primaryButton}>
            {submitting ? 'Creating…' : 'Create agent'}
          </button>
        </div>
      </form>
    </ModalShell>
  )
}

function ManageDrawer({
  detail,
  onClose,
  onAction,
}: {
  detail: AgentDetail
  onClose: () => void
  onAction: (b: { kind: 'info' | 'error' | 'success'; text: string }) => void
}) {
  const [busy, setBusy] = useState<string | null>(null)
  const [botToken, setBotToken] = useState('')

  const hasGbrain = detail.packages.some((p) => p.name === 'gbrain')
  const telegramConnected = detail.channels.some((c) => c.name === 'telegram')
  const canMutate = detail.core !== null

  async function run<T>(label: string, fn: () => Promise<T>, success: string) {
    setBusy(label)
    try {
      await fn()
      onAction({ kind: 'success', text: success })
      onClose()
    } catch (err) {
      onAction({ kind: 'error', text: `${label} failed: ${(err as Error).message}` })
    } finally {
      setBusy(null)
    }
  }

  return (
    <ModalShell onClose={onClose} title={`Manage: ${detail.display_name}`}>
      <div style={{ display: 'grid', gap: 18 }}>
        <Section title="Status">
          <KV k="status" v={detail.status} mono />
          <KV k="core" v={detail.core ?? '—'} mono />
          <KV k="instance" v={detail.instance_status ?? '—'} mono />
          {detail.core_status && <KV k="core status" v={`${detail.core_status.status} (${detail.core_status.reason ?? ''})`} mono />}
        </Section>

        {!canMutate && (
          <p style={{ fontSize: 12, color: 'var(--text-tertiary)' }}>
            This agent has no managed core. Channels and packages require a core (e.g. hermes).
          </p>
        )}

        {canMutate && (
          <>
            <Section title="gbrain (long-term memory)">
              {hasGbrain ? (
                <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
                  <span style={pill('emerald')}>installed</span>
                  <button
                    onClick={() => run('uninstall gbrain', () => uninstallGbrain(detail.id), 'gbrain uninstalled.')}
                    disabled={busy !== null}
                    style={dangerButton}
                  >
                    {busy === 'uninstall gbrain' ? 'Working…' : 'Uninstall'}
                  </button>
                </div>
              ) : (
                <button
                  onClick={() => run('install gbrain', () => installGbrain(detail.id), 'gbrain install queued.')}
                  disabled={busy !== null}
                  style={primaryButton}
                >
                  {busy === 'install gbrain' ? 'Queueing…' : 'Install gbrain'}
                </button>
              )}
            </Section>

            <Section title="Telegram">
              {telegramConnected ? (
                <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
                  <span style={pill('emerald')}>connected</span>
                  <button
                    onClick={() => run('disconnect telegram', () => disconnectTelegram(detail.id), 'Telegram disconnected.')}
                    disabled={busy !== null}
                    style={dangerButton}
                  >
                    {busy === 'disconnect telegram' ? 'Working…' : 'Disconnect'}
                  </button>
                </div>
              ) : (
                <div style={{ display: 'grid', gap: 8 }}>
                  <Field label="Bot token" hint="From @BotFather. Stored in this agent's secret store.">
                    <input
                      value={botToken}
                      onChange={(e) => setBotToken(e.target.value)}
                      placeholder="1234567890:AAA…"
                      style={input}
                    />
                  </Field>
                  <button
                    onClick={() =>
                      run(
                        'connect telegram',
                        () => connectTelegram(detail.id, botToken.trim()),
                        'Telegram connect queued — webhook will register shortly.',
                      )
                    }
                    disabled={busy !== null || !botToken.trim()}
                    style={primaryButton}
                  >
                    {busy === 'connect telegram' ? 'Queueing…' : 'Connect Telegram'}
                  </button>
                </div>
              )}
            </Section>
          </>
        )}
      </div>
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

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div>
      <h3 style={{ fontSize: 12, textTransform: 'uppercase', letterSpacing: '0.06em', color: 'var(--text-tertiary)', marginBottom: 10, fontFamily: 'var(--font-mono)' }}>
        {title}
      </h3>
      <div style={{ display: 'grid', gap: 8 }}>{children}</div>
    </div>
  )
}

function KV({ k, v, mono }: { k: string; v: string; mono?: boolean }) {
  return (
    <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 12 }}>
      <span style={{ color: 'var(--text-tertiary)' }}>{k}</span>
      <span style={{ color: 'var(--text-primary)', fontFamily: mono ? 'var(--font-mono)' : undefined }}>{v}</span>
    </div>
  )
}

function pill(tone: 'emerald' | 'amber' | 'rose'): React.CSSProperties {
  const palette = {
    emerald: { bg: 'rgba(52,211,153,0.12)', border: 'rgba(52,211,153,0.35)', fg: '#34d399' },
    amber: { bg: 'rgba(251,191,36,0.12)', border: 'rgba(251,191,36,0.35)', fg: '#fbbf24' },
    rose: { bg: 'rgba(248,113,113,0.12)', border: 'rgba(248,113,113,0.35)', fg: '#f87171' },
  }[tone]
  return {
    background: palette.bg,
    border: `1px solid ${palette.border}`,
    color: palette.fg,
    padding: '3px 9px',
    borderRadius: 999,
    fontSize: 11,
    fontWeight: 500,
    fontFamily: 'var(--font-mono)',
  }
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
