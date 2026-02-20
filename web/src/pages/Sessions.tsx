import { useState, useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'
import { getSessions, type Session } from '../api/client'

const statusFilters = ['', 'running', 'stopped', 'hibernated', 'error'] as const

export default function Sessions() {
  const [status, setStatus] = useState<string>('')
  const { data: sessions, isLoading } = useQuery({
    queryKey: ['sessions', status],
    queryFn: () => getSessions(status || undefined),
  })

  // Always fetch all sessions for the activity chart
  const { data: allSessions, isLoading: loadingAll } = useQuery({
    queryKey: ['sessions', ''],
    queryFn: () => getSessions(),
  })

  return (
    <div>
      <div style={{ marginBottom: 28 }}>
        <h1 className="page-title">Sessions</h1>
        <p className="page-subtitle">Session history</p>
      </div>

      {/* ── Activity Chart ── */}
      <ActivityChart sessions={allSessions ?? []} loading={loadingAll} />

      {/* ── Filters ── */}
      <div style={{ marginBottom: 16, display: 'flex', gap: 6 }}>
        {statusFilters.map(f => (
          <button key={f} onClick={() => setStatus(f)}
            className={`filter-btn${status === f ? ' active' : ''}`}>
            {f || 'All'}
          </button>
        ))}
      </div>

      {/* ── Table ── */}
      {isLoading ? (
        <div style={{ display: 'flex', justifyContent: 'center', padding: 48 }}>
          <div className="loading-spinner" />
        </div>
      ) : (
        <div className="glass-card animate-in stagger-1" style={{ overflow: 'hidden' }}>
          <table className="data-table">
            <thead>
              <tr>
                <th>Sandbox ID</th>
                <th>Template</th>
                <th>Status</th>
                <th>Started</th>
                <th>Stopped</th>
              </tr>
            </thead>
            <tbody>
              {(sessions ?? []).map((s: Session) => (
                <ClickableRow key={s.id} sandboxId={s.sandboxId}>
                  <td><code style={{ color: 'var(--text-accent)' }}>{s.sandboxId}</code></td>
                  <td>{s.template || 'base'}</td>
                  <td><StatusBadge status={s.status} /></td>
                  <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{new Date(s.startedAt).toLocaleString()}</td>
                  <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>{s.stoppedAt ? new Date(s.stoppedAt).toLocaleString() : '\u2014'}</td>
                </ClickableRow>
              ))}
              {(sessions ?? []).length === 0 && (
                <tr>
                  <td colSpan={5} style={{ textAlign: 'center', padding: 32, color: 'var(--text-tertiary)' }}>
                    No sessions found
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}

/* ── Activity Chart (last 14 days) ────────────────────────── */
function ActivityChart({ sessions, loading }: { sessions: Session[]; loading: boolean }) {
  const { days, maxCount } = useMemo(() => {
    const now = new Date()
    const dayBuckets: { label: string; date: string; count: number; hibernated: number; errored: number }[] = []

    for (let i = 13; i >= 0; i--) {
      const d = new Date(now)
      d.setDate(d.getDate() - i)
      const dateStr = d.toISOString().slice(0, 10)
      dayBuckets.push({
        label: d.toLocaleDateString('en-US', { month: 'short', day: 'numeric' }),
        date: dateStr,
        count: 0,
        hibernated: 0,
        errored: 0,
      })
    }

    for (const s of sessions) {
      const dateStr = new Date(s.startedAt).toISOString().slice(0, 10)
      const bucket = dayBuckets.find(b => b.date === dateStr)
      if (bucket) {
        bucket.count++
        if (s.status === 'hibernated') bucket.hibernated++
        else if (s.status === 'error') bucket.errored++
      }
    }

    const maxCount = Math.max(1, ...dayBuckets.map(b => b.count))
    return { days: dayBuckets, maxCount }
  }, [sessions])

  return (
    <div className="glass-card animate-in" style={{ padding: '22px 24px', marginBottom: 24 }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 16 }}>
        <span className="section-title" style={{ marginBottom: 0 }}>Activity</span>
        <span style={{ fontSize: 11, color: 'var(--text-tertiary)', fontFamily: 'var(--font-mono)' }}>
          Last 14 days
        </span>
      </div>

      {loading ? (
        <div style={{ display: 'flex', justifyContent: 'center', padding: 40 }}>
          <div className="loading-spinner" />
        </div>
      ) : (
        <div style={{ position: 'relative' }}>
          {/* Y-axis labels */}
          <div style={{
            position: 'absolute', left: 0, top: 0, bottom: 24,
            display: 'flex', flexDirection: 'column', justifyContent: 'space-between',
            width: 32,
          }}>
            <span style={{ fontSize: 10, color: 'var(--text-tertiary)', fontFamily: 'var(--font-mono)' }}>
              {maxCount}
            </span>
            <span style={{ fontSize: 10, color: 'var(--text-tertiary)', fontFamily: 'var(--font-mono)' }}>
              0
            </span>
          </div>

          {/* Chart bars */}
          <div style={{
            display: 'flex', alignItems: 'flex-end', gap: 4, height: 120,
            marginLeft: 36,
          }}>
            {days.map((day) => {
              const barHeight = maxCount > 0 ? Math.round((day.count / maxCount) * 110) : 0
              const hasHibernated = day.hibernated > 0
              const hasError = day.errored > 0

              return (
                <div
                  key={day.date}
                  title={`${day.label}: ${day.count} sessions${day.hibernated ? ` (${day.hibernated} hibernated)` : ''}${day.errored ? ` (${day.errored} errors)` : ''}`}
                  style={{
                    flex: 1,
                    height: day.count > 0 ? Math.max(barHeight, 4) : 0,
                    borderRadius: '4px 4px 2px 2px',
                    background: hasError
                      ? 'linear-gradient(to top, rgba(251,113,133,0.5), rgba(129,140,248,0.5))'
                      : hasHibernated
                      ? 'linear-gradient(to top, rgba(34,211,238,0.4), rgba(129,140,248,0.5))'
                      : day.count > 0
                      ? 'linear-gradient(to top, rgba(99,102,241,0.25), rgba(129,140,248,0.5))'
                      : 'transparent',
                    transition: 'height 0.3s ease',
                    cursor: 'default',
                    position: 'relative',
                  }}
                >
                  {day.count > 0 && (
                    <span style={{
                      position: 'absolute', top: -16, left: '50%', transform: 'translateX(-50%)',
                      fontSize: 9, fontFamily: 'var(--font-mono)', color: 'var(--text-tertiary)',
                      whiteSpace: 'nowrap',
                    }}>
                      {day.count}
                    </span>
                  )}
                </div>
              )
            })}
          </div>

          {/* X-axis labels */}
          <div style={{
            display: 'flex', gap: 4, marginLeft: 36, marginTop: 6,
          }}>
            {days.map((day, i) => (
              <div key={day.date} style={{
                flex: 1, textAlign: 'center',
                fontSize: 9, color: 'var(--text-tertiary)',
                fontFamily: 'var(--font-mono)',
              }}>
                {i % 2 === 0 ? day.label : ''}
              </div>
            ))}
          </div>

          {/* Legend */}
          <div style={{
            display: 'flex', gap: 16, justifyContent: 'center', marginTop: 12,
          }}>
            <LegendItem color="rgba(129,140,248,0.5)" label="Sessions" />
            <LegendItem color="rgba(34,211,238,0.4)" label="Hibernated" />
            <LegendItem color="rgba(251,113,133,0.5)" label="Errors" />
          </div>
        </div>
      )}
    </div>
  )
}

function LegendItem({ color, label }: { color: string; label: string }) {
  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 5 }}>
      <div style={{ width: 8, height: 8, borderRadius: 2, background: color }} />
      <span style={{ fontSize: 10, color: 'var(--text-tertiary)' }}>{label}</span>
    </div>
  )
}

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
