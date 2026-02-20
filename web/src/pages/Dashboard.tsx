import { useQuery } from '@tanstack/react-query'
import { getSessions, type Session } from '../api/client'

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

  return (
    <div>
      <div style={{ marginBottom: 32 }}>
        <h1 className="page-title">Dashboard</h1>
        <p className="page-subtitle">Overview of your sandbox infrastructure</p>
      </div>

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
                  <tr key={s.id}>
                    <td><code>{s.sandboxId}</code></td>
                    <td>{s.template || 'base'}</td>
                    <td><StatusBadge status={s.status} /></td>
                    <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>
                      {new Date(s.startedAt).toLocaleString()}
                    </td>
                    <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>
                      {formatDuration(s)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
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
  const elapsed = Math.round((Date.now() - new Date(session.startedAt).getTime()) / 1000 / 60)
  return (
    <div style={{
      display: 'flex', alignItems: 'center', justifyContent: 'space-between',
      padding: '9px 12px', borderRadius: 8,
      background: 'rgba(255,255,255,0.015)',
      border: '1px solid rgba(255,255,255,0.035)',
      transition: 'all 0.15s ease', cursor: 'default',
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
