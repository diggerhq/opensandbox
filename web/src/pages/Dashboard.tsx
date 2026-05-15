import { useState, useEffect, useRef } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { useNavigate, Link } from 'react-router-dom'
import { getSessions, getAPIKeys, createAPIKey, type Session } from '../api/client'

const SKILL_INSTALL_CMD = 'npx skills add diggerhq/opencomputer'

export default function Dashboard() {
  const { data: runningSessions, isLoading: loadingRunning } = useQuery({
    queryKey: ['sessions', 'running'],
    queryFn: () => getSessions('running'),
  })

  const { data: allSessions, isLoading: loadingAll } = useQuery({
    queryKey: ['sessions', ''],
    queryFn: () => getSessions(),
  })

  const active = runningSessions ?? []
  const all = allSessions ?? []
  const today = new Date().toISOString().slice(0, 10)
  const sessionsToday = all.filter(s => new Date(s.startedAt).toISOString().slice(0, 10) === today).length

  const isFirstRun = !loadingAll && all.length === 0

  return (
    <div>
      <div style={{ marginBottom: 32 }}>
        <h1 className="page-title">{isFirstRun ? 'Welcome to OpenComputer' : 'Dashboard'}</h1>
        <p className="page-subtitle">
          {isFirstRun
            ? 'Get your first sandbox running in two steps'
            : 'Overview of your sandbox infrastructure'}
        </p>
      </div>

      {isFirstRun ? (
        <GettingStarted />
      ) : (
        <>
      {/* ── Stat Cards ── */}
      <div style={{
        display: 'grid', gridTemplateColumns: 'repeat(2, 1fr)', gap: 14, marginBottom: 24,
      }}>
        <StatCard
          label="Active Sandboxes"
          value={loadingRunning ? '\u2014' : active.length}
          accent="var(--accent-emerald)"
        />
        <StatCard
          label="Sessions Today"
          value={loadingAll ? '\u2014' : sessionsToday}
          accent="#818cf8"
        />
      </div>

      {/* ── Live Sandboxes ── */}
      <div className="glass-card animate-in stagger-1" style={{ padding: '22px 24px', marginBottom: 24 }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 14 }}>
          <span className="section-title" style={{ marginBottom: 0 }}>Live Sandboxes</span>
          {active.length > 0 && (
            <span style={{
              fontSize: 11, fontFamily: 'var(--font-mono)', color: 'var(--accent-emerald)',
              display: 'flex', alignItems: 'center', gap: 6,
            }}>
              <span className="pulse-dot" style={{
                width: 6, height: 6, borderRadius: '50%',
                background: 'var(--accent-emerald)', display: 'inline-block',
              }} />
              {active.length} active
            </span>
          )}
        </div>

        {loadingRunning ? (
          <div style={{ display: 'flex', justifyContent: 'center', padding: 40 }}>
            <div className="loading-spinner" />
          </div>
        ) : active.length === 0 ? (
          <div style={{
            textAlign: 'center', padding: '40px 20px',
            color: 'var(--text-tertiary)', fontSize: 13,
          }}>
            No sandboxes running
          </div>
        ) : (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 6, maxHeight: 300, overflowY: 'auto' }}>
            {active.map(s => <SandboxRow key={s.id} session={s} />)}
          </div>
        )}
      </div>

      {/* ── Recent Sessions ── */}
      <div className="glass-card animate-in stagger-2" style={{ padding: '22px 24px' }}>
        <span className="section-title">Recent Sessions</span>
        {loadingAll ? (
          <div style={{ display: 'flex', justifyContent: 'center', padding: 40 }}>
            <div className="loading-spinner" />
          </div>
        ) : all.length === 0 ? (
          <div style={{
            textAlign: 'center', padding: '40px 20px',
            color: 'var(--text-tertiary)', fontSize: 13,
          }}>
            No sessions yet
          </div>
        ) : (
          <div style={{ overflow: 'hidden' }}>
            <table className="data-table">
              <thead>
                <tr>
                  <th>Sandbox ID</th>
                  <th>Template</th>
                  <th>Status</th>
                  <th>Started</th>
                  <th>Duration</th>
                </tr>
              </thead>
              <tbody>
                {all.slice(0, 20).map(s => (
                  <ClickableRow key={s.id} sandboxId={s.sandboxId}>
                    <td><code style={{ color: 'var(--text-accent)' }}>{s.sandboxId}</code></td>
                    <td>{s.template || 'base'}</td>
                    <td><StatusBadge status={s.status} /></td>
                    <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>
                      {new Date(s.startedAt).toLocaleString()}
                    </td>
                    <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>
                      {formatDuration(s)}
                    </td>
                  </ClickableRow>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
        </>
      )}
    </div>
  )
}

/* ── Getting Started (first-run onboarding) ───────────────── */
function GettingStarted() {
  const queryClient = useQueryClient()
  const { data: keys, isLoading: loadingKeys } = useQuery({ queryKey: ['api-keys'], queryFn: getAPIKeys })
  const [copied, setCopied] = useState<string | null>(null)
  const [createdKey, setCreatedKey] = useState<string | null>(null)
  const autoCreateRef = useRef(false)

  const createMutation = useMutation({
    mutationFn: () => createAPIKey('Default'),
    onSuccess: (data) => {
      setCreatedKey(data.key)
      queryClient.invalidateQueries({ queryKey: ['api-keys'] })
    },
  })

  const hasKeys = (keys?.length ?? 0) > 0

  // On first signup (no keys exist), auto-create a Default key so the user
  // sees their key immediately without having to click anything.
  useEffect(() => {
    if (loadingKeys || autoCreateRef.current) return
    if (!hasKeys && !createdKey && !createMutation.isPending) {
      autoCreateRef.current = true
      createMutation.mutate()
    }
  }, [loadingKeys, hasKeys, createdKey, createMutation])

  const copy = (text: string, id: string) => {
    navigator.clipboard.writeText(text)
    setCopied(id)
    setTimeout(() => setCopied(c => (c === id ? null : c)), 1500)
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
      <DeployAgentBanner />

      <StepCard
        index={1}
        title="Install the OpenComputer skill"
        description="Adds a skill to Claude Code (or any agent that supports the Agent Skills standard) so it can drive sandboxes for you."
      >
        <CommandRow command={SKILL_INSTALL_CMD} copied={copied === 'install'} onCopy={() => copy(SKILL_INSTALL_CMD, 'install')} />
      </StepCard>

      <StepCard
        index={2}
        title="Your API key"
        description="The skill uses this key to authenticate with OpenComputer. We've created a Default key for you — copy it now, you won't be able to see it again."
      >
        {(loadingKeys || createMutation.isPending) && (
          <div style={{ display: 'flex', alignItems: 'center', gap: 10, color: 'var(--text-tertiary)', fontSize: 13 }}>
            <div className="loading-spinner" style={{ width: 14, height: 14 }} />
            Preparing your API key…
          </div>
        )}

        {createdKey && (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
            <SecretRow
              secret={createdKey}
              copied={copied === 'key'}
              onCopy={() => copy(createdKey, 'key')}
            />
            <div style={{ fontSize: 12, color: 'var(--text-tertiary)', marginTop: 6 }}>
              Then run this in your terminal to configure the CLI:
            </div>
            <SecretRow
              secret={createdKey}
              wrap={(s) => `oc config set api-key ${s}`}
              copied={copied === 'cmd'}
              onCopy={() => copy(`oc config set api-key ${createdKey}`, 'cmd')}
            />
          </div>
        )}

        {!createdKey && !createMutation.isPending && hasKeys && (
          <div style={{ fontSize: 13, color: 'var(--text-secondary)' }}>
            You already have {keys!.length} API key{keys!.length === 1 ? '' : 's'} from a previous session.
            For security, existing key values can&apos;t be re-displayed.{' '}
            <Link to="/api-keys" style={{ color: 'var(--accent-indigo)' }}>Manage keys</Link> to rotate.
          </div>
        )}

        {createMutation.isError && (
          <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginTop: 8 }}>
            <span style={{ fontSize: 12, color: 'var(--accent-rose, #fb7185)' }}>
              Failed to create your API key.
            </span>
            <button
              className="btn-ghost"
              style={{ fontSize: 12 }}
              onClick={() => createMutation.mutate()}
            >
              Retry
            </button>
          </div>
        )}
      </StepCard>
    </div>
  )
}

function StepCard({ index, title, description, children }: {
  index: number
  title: string
  description: string
  children: React.ReactNode
}) {
  return (
    <div className="glass-card animate-in" style={{ padding: '22px 24px' }}>
      <div style={{ display: 'flex', alignItems: 'flex-start', gap: 14 }}>
        <div style={{
          width: 28, height: 28, borderRadius: '50%',
          background: 'rgba(99,102,241,0.12)',
          border: '1px solid var(--border-accent)',
          color: 'var(--accent-indigo)',
          display: 'flex', alignItems: 'center', justifyContent: 'center',
          fontSize: 13, fontWeight: 600, flexShrink: 0, fontFamily: 'var(--font-mono)',
        }}>
          {index}
        </div>
        <div style={{ flex: 1, display: 'flex', flexDirection: 'column', gap: 12 }}>
          <div>
            <div style={{ fontSize: 15, fontWeight: 600, color: 'var(--text-primary)', marginBottom: 4 }}>
              {title}
            </div>
            <div style={{ fontSize: 13, color: 'var(--text-tertiary)', lineHeight: 1.5 }}>
              {description}
            </div>
          </div>
          {children}
        </div>
      </div>
    </div>
  )
}

function CommandRow({ command, copied, onCopy }: {
  command: string
  copied: boolean
  onCopy: () => void
}) {
  return (
    <div style={{
      display: 'flex', alignItems: 'center', gap: 8,
      background: 'var(--bg-deep)',
      border: '1px solid var(--border-subtle)',
      borderRadius: 'var(--radius-sm)',
      padding: '10px 12px',
    }}>
      <code style={{
        flex: 1, fontFamily: 'var(--font-mono)', fontSize: 13,
        color: 'var(--text-accent)', wordBreak: 'break-all',
      }}>
        {command}
      </code>
      <button
        onClick={onCopy}
        className="btn-ghost"
        style={{ fontSize: 11, padding: '4px 10px', flexShrink: 0 }}
      >
        {copied ? 'Copied' : 'Copy'}
      </button>
    </div>
  )
}

function SecretRow({ secret, wrap, copied, onCopy }: {
  secret: string
  wrap?: (s: string) => string
  copied: boolean
  onCopy: () => void
}) {
  const [revealed, setRevealed] = useState(false)
  const masked = '•'.repeat(Math.min(secret.length, 32))
  const display = revealed ? secret : masked
  const text = wrap ? wrap(display) : display

  return (
    <div style={{
      display: 'flex', alignItems: 'center', gap: 8,
      background: 'var(--bg-deep)',
      border: '1px solid var(--border-subtle)',
      borderRadius: 'var(--radius-sm)',
      padding: '10px 12px',
    }}>
      <code style={{
        flex: 1, fontFamily: 'var(--font-mono)', fontSize: 13,
        color: 'var(--text-accent)', wordBreak: 'break-all',
        letterSpacing: revealed ? 'normal' : '0.05em',
      }}>
        {text}
      </code>
      <button
        onClick={() => setRevealed(r => !r)}
        className="btn-ghost"
        style={{ fontSize: 11, padding: '4px 10px', flexShrink: 0 }}
        aria-label={revealed ? 'Hide secret' : 'Reveal secret'}
      >
        {revealed ? 'Hide' : 'Reveal'}
      </button>
      <button
        onClick={onCopy}
        className="btn-ghost"
        style={{ fontSize: 11, padding: '4px 10px', flexShrink: 0 }}
      >
        {copied ? 'Copied' : 'Copy'}
      </button>
    </div>
  )
}

/* ── Stat Card ───────────────────────────────────────────── */
function StatCard({ label, value, accent }: {
  label: string
  value: number | string
  accent: string
}) {
  return (
    <div className="stat-card animate-in">
      <div style={{ fontSize: 11, color: 'var(--text-tertiary)', marginBottom: 8, letterSpacing: '0.03em' }}>
        {label}
      </div>
      <div className="metric-value" style={{
        fontSize: 30, fontWeight: 700, lineHeight: 1, color: accent,
      }}>
        {typeof value === 'number' ? value.toLocaleString() : value}
      </div>
    </div>
  )
}

/* ── Sandbox Row ──────────────────────────────────────────── */
function SandboxRow({ session }: { session: Session }) {
  const navigate = useNavigate()
  const elapsed = Math.round((Date.now() - new Date(session.startedAt).getTime()) / 1000 / 60)
  return (
    <div
      onClick={() => navigate(`/sessions/${session.sandboxId}`)}
      style={{
        display: 'flex', alignItems: 'center', justifyContent: 'space-between',
        padding: '9px 12px', borderRadius: 8,
        background: 'rgba(255,255,255,0.015)',
        border: '1px solid rgba(255,255,255,0.035)',
        transition: 'all 0.15s ease', cursor: 'pointer',
      }}
      onMouseOver={e => {
        e.currentTarget.style.background = 'rgba(99,102,241,0.05)'
        e.currentTarget.style.borderColor = 'rgba(99,102,241,0.12)'
      }}
      onMouseOut={e => {
        e.currentTarget.style.background = 'rgba(255,255,255,0.015)'
        e.currentTarget.style.borderColor = 'rgba(255,255,255,0.035)'
      }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
        <span className="pulse-dot" style={{
          width: 6, height: 6, borderRadius: '50%',
          background: 'var(--accent-emerald)', flexShrink: 0,
        }} />
        <div>
          <code style={{ fontSize: 12, fontFamily: 'var(--font-mono)', color: 'var(--text-accent)' }}>
            {session.sandboxId}
          </code>
          <div style={{ fontSize: 10, color: 'var(--text-tertiary)', marginTop: 1 }}>
            {session.template || 'base'}
          </div>
        </div>
      </div>
      <span className="metric-value" style={{ fontSize: 11, color: 'var(--text-tertiary)' }}>
        {elapsed}m
      </span>
    </div>
  )
}

/* ── Clickable Table Row ──────────────────────────────────── */
function ClickableRow({ sandboxId, children }: { sandboxId: string; children: React.ReactNode }) {
  const navigate = useNavigate()
  return (
    <tr
      onClick={() => navigate(`/sessions/${sandboxId}`)}
      style={{ cursor: 'pointer' }}
      onMouseOver={e => { e.currentTarget.style.background = 'rgba(99,102,241,0.04)' }}
      onMouseOut={e => { e.currentTarget.style.background = '' }}
    >
      {children}
    </tr>
  )
}

/* ── Status Badge ─────────────────────────────────────────── */
function StatusBadge({ status }: { status: string }) {
  const cls =
    status === 'running' ? 'badge-running'
    : status === 'hibernated' ? 'badge-hibernated'
    : status === 'error' ? 'badge-error'
    : 'badge-stopped'
  return (
    <span className={`badge ${cls}`}>
      {status === 'running' && (
        <span className="pulse-dot" style={{
          width: 5, height: 5, borderRadius: '50%',
          background: 'currentColor', display: 'inline-block',
        }} />
      )}
      {status}
    </span>
  )
}

/* ── Helpers ──────────────────────────────────────────────── */
function formatDuration(session: Session): string {
  const start = new Date(session.startedAt).getTime()
  const end = session.stoppedAt ? new Date(session.stoppedAt).getTime() : Date.now()
  const secs = Math.round((end - start) / 1000)
  if (secs < 60) return `${secs}s`
  if (secs < 3600) return `${Math.round(secs / 60)}m`
  return `${Math.round(secs / 3600 * 10) / 10}h`
}

// Deploy-managed-agent banner shown above the getting-started checklist on
// fresh signups. The Agents page has the same lobster mark in its empty
// state; keeping the two consistent makes the OpenClaw / managed-agent
// product visible from minute one without asking the user to navigate.
function DeployAgentBanner() {
  const navigate = useNavigate()
  return (
    <div
      onClick={() => navigate('/agents')}
      onKeyDown={(e) => {
        if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); navigate('/agents') }
      }}
      role="button"
      tabIndex={0}
      style={{
        display: 'flex', alignItems: 'center', gap: 18,
        padding: '20px 24px',
        border: '1px solid rgba(255,77,77,0.30)',
        borderRadius: 14,
        background:
          'radial-gradient(ellipse at left, rgba(255,77,77,0.14), transparent 62%), rgba(255,77,77,0.04)',
        cursor: 'pointer',
        transition: 'background 0.12s ease, border-color 0.12s ease',
      }}
      onMouseEnter={(e) => { (e.currentTarget as HTMLDivElement).style.borderColor = 'rgba(255,77,77,0.55)' }}
      onMouseLeave={(e) => { (e.currentTarget as HTMLDivElement).style.borderColor = 'rgba(255,77,77,0.30)' }}
    >
      <DashClawLogo />
      <div style={{ flex: 1 }}>
        <div style={{ fontSize: 16, fontWeight: 700, fontFamily: 'var(--font-display)', marginBottom: 4 }}>
          Deploy an OpenClaw managed agent
        </div>
        <div style={{ fontSize: 13, color: 'var(--text-secondary)', lineHeight: 1.5 }}>
          Spin up a managed agent runtime with built-in chat, Telegram, and gbrain memory.
          We host the gateway and the model routing — you bring the prompt.
        </div>
      </div>
      <div
        style={{
          padding: '9px 18px',
          fontSize: 13, fontWeight: 600,
          background: 'var(--accent-indigo)', color: '#fff',
          borderRadius: 'var(--radius-sm)',
          whiteSpace: 'nowrap',
        }}
      >
        Deploy →
      </div>
    </div>
  )
}

// OpenClaw lobster mark — pulled directly from openclaw.ai/favicon.svg so the
// dashboard banner uses the same brand asset as the marketing site. Inline so
// it ships with the bundle (no extra HTTP fetch); gradient ID is scoped per
// page (`dash-…`) to avoid collisions when multiple copies render at once.
function DashClawLogo() {
  return (
    <svg
      width="56"
      height="56"
      viewBox="0 0 120 120"
      fill="none"
      xmlns="http://www.w3.org/2000/svg"
      aria-hidden="true"
      style={{ filter: 'drop-shadow(0 0 16px rgba(255,77,77,0.45))', flexShrink: 0 }}
    >
      <defs>
        <linearGradient id="dash-lobster-gradient" x1="0%" y1="0%" x2="100%" y2="100%">
          <stop offset="0%" stopColor="#ff4d4d" />
          <stop offset="100%" stopColor="#991b1b" />
        </linearGradient>
      </defs>
      <path d="M60 10 C30 10 15 35 15 55 C15 75 30 95 45 100 L45 110 L55 110 L55 100 C55 100 60 102 65 100 L65 110 L75 110 L75 100 C90 95 105 75 105 55 C105 35 90 10 60 10Z" fill="url(#dash-lobster-gradient)" />
      <path d="M20 45 C5 40 0 50 5 60 C10 70 20 65 25 55 C28 48 25 45 20 45Z" fill="url(#dash-lobster-gradient)" />
      <path d="M100 45 C115 40 120 50 115 60 C110 70 100 65 95 55 C92 48 95 45 100 45Z" fill="url(#dash-lobster-gradient)" />
      <path d="M45 15 Q35 5 30 8" stroke="#ff4d4d" strokeWidth="3" strokeLinecap="round" />
      <path d="M75 15 Q85 5 90 8" stroke="#ff4d4d" strokeWidth="3" strokeLinecap="round" />
      <circle cx="45" cy="35" r="6" fill="#050810" />
      <circle cx="75" cy="35" r="6" fill="#050810" />
      <circle cx="46" cy="34" r="2.5" fill="#00e5cc" />
      <circle cx="76" cy="34" r="2.5" fill="#00e5cc" />
    </svg>
  )
}
