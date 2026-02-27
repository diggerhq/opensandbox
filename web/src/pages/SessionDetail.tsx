import { useState } from 'react'
import { useParams, useNavigate } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { getSessionDetail, getSessionStats } from '../api/client'
import Terminal from '../components/Terminal'

function StatusBadge({ status }: { status: string }) {
  const cls =
    status === 'running' ? 'badge-running' :
    status === 'stopped' ? 'badge-stopped' :
    status === 'hibernated' ? 'badge-hibernated' :
    status === 'error' ? 'badge-error' : ''

  return <span className={`status-badge ${cls}`}>{status}</span>
}

function formatBytes(bytes: number): string {
  if (bytes === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB']
  const i = Math.floor(Math.log(bytes) / Math.log(1024))
  return `${(bytes / Math.pow(1024, i)).toFixed(i > 0 ? 1 : 0)} ${units[i]}`
}

function timeAgo(dateStr: string): string {
  const diff = Date.now() - new Date(dateStr).getTime()
  const mins = Math.floor(diff / 60000)
  if (mins < 1) return 'just now'
  if (mins < 60) return `${mins}m ago`
  const hrs = Math.floor(mins / 60)
  if (hrs < 24) return `${hrs}h ${mins % 60}m ago`
  const days = Math.floor(hrs / 24)
  return `${days}d ago`
}

function StatCard({ label, value, sub }: { label: string; value: string; sub?: string }) {
  return (
    <div style={{
      background: 'rgba(255,255,255,0.02)',
      border: '1px solid var(--border-subtle)',
      borderRadius: 'var(--radius-md)',
      padding: '16px 20px',
      flex: 1,
    }}>
      <div style={{ fontSize: 11, fontWeight: 600, color: 'var(--text-tertiary)', textTransform: 'uppercase', letterSpacing: '0.05em', marginBottom: 6 }}>
        {label}
      </div>
      <div style={{ fontSize: 22, fontWeight: 700, color: 'var(--text-primary)' }}>
        {value}
      </div>
      {sub && (
        <div style={{ fontSize: 11, color: 'var(--text-tertiary)', marginTop: 2 }}>
          {sub}
        </div>
      )}
    </div>
  )
}

export default function SessionDetail() {
  const { sandboxId } = useParams<{ sandboxId: string }>()
  const navigate = useNavigate()
  const [copiedUrl, setCopiedUrl] = useState<string | null>(null)
  const [showTerminal, setShowTerminal] = useState(false)
  const [showInternal, setShowInternal] = useState(false)

  const { data: session, isLoading } = useQuery({
    queryKey: ['session-detail', sandboxId],
    queryFn: () => getSessionDetail(sandboxId!),
    enabled: !!sandboxId,
  })

  const { data: stats } = useQuery({
    queryKey: ['session-stats', sandboxId],
    queryFn: () => getSessionStats(sandboxId!),
    enabled: !!sandboxId && session?.status === 'running',
    refetchInterval: 5000,
    retry: false,
  })

  const copyUrl = (hostname: string, key: string) => {
    navigator.clipboard.writeText(`https://${hostname}`)
    setCopiedUrl(key)
    setTimeout(() => setCopiedUrl(null), 2000)
  }

  if (isLoading) {
    return (
      <div style={{ display: 'flex', justifyContent: 'center', padding: 80 }}>
        <div className="loading-spinner" />
      </div>
    )
  }

  if (!session) {
    return (
      <div style={{ padding: 40, textAlign: 'center', color: 'var(--text-tertiary)' }}>
        Session not found
      </div>
    )
  }

  return (
    <div>
      {/* Back link */}
      <button
        onClick={() => navigate('/sessions')}
        style={{
          background: 'none', border: 'none', cursor: 'pointer',
          color: 'var(--text-tertiary)', fontSize: 13, padding: 0,
          marginBottom: 20, display: 'flex', alignItems: 'center', gap: 6,
        }}
      >
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <path d="M19 12H5" /><polyline points="12 19 5 12 12 5" />
        </svg>
        Back to Sessions
      </button>

      {/* Header */}
      <div className="glass-card animate-in" style={{ padding: 24, marginBottom: 16 }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start' }}>
          <div>
            <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 8 }}>
              <code style={{ fontSize: 20, fontWeight: 700, fontFamily: 'var(--font-mono)', color: 'var(--text-primary)' }}>
                {session.sandboxId}
              </code>
              <StatusBadge status={session.status} />
            </div>
            <div style={{ fontSize: 13, color: 'var(--text-tertiary)' }}>
              {session.template || 'base'} &middot; Started {timeAgo(session.startedAt)}
            </div>
          </div>

          {/* Actions */}
          <div style={{ display: 'flex', gap: 8 }}>
            {session.status === 'running' && (
              <button
                className={showTerminal ? 'btn-primary' : 'btn-ghost'}
                onClick={() => setShowTerminal(!showTerminal)}
              >
                <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round">
                  <polyline points="4 17 10 11 4 5" />
                  <line x1="12" y1="19" x2="20" y2="19" />
                </svg>
                Terminal
              </button>
            )}
          </div>
        </div>
      </div>

      {/* Terminal */}
      {showTerminal && session.status === 'running' && (
        <div className="glass-card animate-in" style={{ padding: 20, marginBottom: 16 }}>
          <Terminal sandboxId={sandboxId!} onClose={() => setShowTerminal(false)} />
        </div>
      )}

      {/* Stats cards — only for running sandboxes */}
      {session.status === 'running' && (
        <div className="glass-card animate-in stagger-1" style={{ padding: 20, marginBottom: 16 }}>
          <div style={{ fontSize: 11, fontWeight: 600, color: 'var(--text-tertiary)', textTransform: 'uppercase', letterSpacing: '0.05em', marginBottom: 14 }}>
            Resource Usage
          </div>
          <div style={{ display: 'flex', gap: 12 }}>
            <StatCard
              label="CPU"
              value={stats ? `${stats.cpuPercent.toFixed(1)}%` : '—'}
            />
            <StatCard
              label="Memory"
              value={stats ? formatBytes(stats.memUsage) : '—'}
              sub={stats ? `of ${formatBytes(stats.memLimit)}` : undefined}
            />
            <StatCard
              label="Processes"
              value={stats ? String(stats.pids) : '—'}
            />
            <StatCard
              label="Network"
              value={stats ? `↑${formatBytes(stats.netOutput)}` : '—'}
              sub={stats ? `↓${formatBytes(stats.netInput)}` : undefined}
            />
          </div>
        </div>
      )}

      {/* Preview URLs */}
      {session.previewUrls && session.previewUrls.length > 0 && (() => {
        const hasCustom = session.previewUrls.some(u => u.customHostname)
        return (
          <div className="glass-card animate-in stagger-2" style={{ padding: 20, marginBottom: 16 }}>
            <div style={{ fontSize: 11, fontWeight: 600, color: 'var(--text-tertiary)', textTransform: 'uppercase', letterSpacing: '0.05em', marginBottom: 10 }}>
              Preview URLs
            </div>
            <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
              {session.previewUrls.map((url) => {
                const displayHost = url.customHostname || url.hostname
                return (
                  <div key={url.id} style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
                    <div style={{
                      minWidth: 60,
                      fontSize: 12,
                      fontWeight: 600,
                      color: 'var(--text-tertiary)',
                      fontFamily: 'var(--font-mono)',
                    }}>
                      :{url.port}
                    </div>
                    <div style={{
                      flex: 1,
                      background: 'var(--bg-deep)',
                      border: '1px solid var(--border-subtle)',
                      borderRadius: 'var(--radius-sm)',
                      padding: '10px 14px',
                      fontFamily: 'var(--font-mono)',
                      fontSize: 13,
                      color: 'var(--text-accent)',
                    }}>
                      <a
                        href={`https://${displayHost}`}
                        target="_blank"
                        rel="noopener noreferrer"
                        style={{ color: 'var(--text-accent)', textDecoration: 'none' }}
                      >
                        https://{displayHost}
                      </a>
                    </div>
                    <button className="btn-ghost" onClick={() => copyUrl(displayHost, `${url.port}`)} style={{ whiteSpace: 'nowrap' }}>
                      {copiedUrl === `${url.port}` ? 'Copied' : 'Copy'}
                    </button>
                  </div>
                )
              })}
            </div>

            {/* Internal URLs toggle — only shown when custom domain URLs are displayed */}
            {hasCustom && (
              <div style={{ marginTop: 12 }}>
                <button
                  onClick={() => setShowInternal(!showInternal)}
                  style={{
                    background: 'none', border: 'none', cursor: 'pointer', padding: 0,
                    fontSize: 11, color: 'var(--text-tertiary)', display: 'flex', alignItems: 'center', gap: 4,
                  }}
                >
                  <svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round"
                    style={{ transform: showInternal ? 'rotate(90deg)' : 'none', transition: 'transform 0.15s' }}>
                    <polyline points="9 18 15 12 9 6" />
                  </svg>
                  Internal URLs
                </button>
                {showInternal && (
                  <div style={{ display: 'flex', flexDirection: 'column', gap: 6, marginTop: 8 }}>
                    {session.previewUrls.map((url) => (
                      <div key={`int-${url.id}`} style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
                        <div style={{ minWidth: 60, fontSize: 11, color: 'var(--text-tertiary)', fontFamily: 'var(--font-mono)' }}>
                          :{url.port}
                        </div>
                        <div style={{
                          flex: 1,
                          background: 'var(--bg-deep)',
                          border: '1px solid var(--border-subtle)',
                          borderRadius: 'var(--radius-sm)',
                          padding: '8px 12px',
                          fontFamily: 'var(--font-mono)',
                          fontSize: 12,
                          color: 'var(--text-tertiary)',
                        }}>
                          <a
                            href={`https://${url.hostname}`}
                            target="_blank"
                            rel="noopener noreferrer"
                            style={{ color: 'var(--text-tertiary)', textDecoration: 'none' }}
                          >
                            https://{url.hostname}
                          </a>
                        </div>
                        <button className="btn-ghost" onClick={() => copyUrl(url.hostname, `int-${url.port}`)} style={{ whiteSpace: 'nowrap', fontSize: 11 }}>
                          {copiedUrl === `int-${url.port}` ? 'Copied' : 'Copy'}
                        </button>
                      </div>
                    ))}
                  </div>
                )}
              </div>
            )}
          </div>
        )
      })()}

      {/* Details */}
      <div className="glass-card animate-in stagger-3" style={{ padding: 20, marginBottom: 16 }}>
        <div style={{ fontSize: 11, fontWeight: 600, color: 'var(--text-tertiary)', textTransform: 'uppercase', letterSpacing: '0.05em', marginBottom: 14 }}>
          Details
        </div>
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '12px 32px' }}>
          <DetailRow label="Template" value={session.template || 'base'} />
          <DetailRow label="Timeout" value={session.config?.timeout ? `${session.config.timeout}s` : '300s'} />
          <DetailRow label="CPUs" value={String(session.config?.cpuCount ?? 1)} />
          <DetailRow label="Memory" value={`${session.config?.memoryMB ?? 512} MB`} />
          <DetailRow label="Network" value={session.config?.networkEnabled ? 'Enabled' : 'Disabled'} />
          <DetailRow label="Started" value={new Date(session.startedAt).toLocaleString()} />
          {session.stoppedAt && (
            <DetailRow label="Stopped" value={new Date(session.stoppedAt).toLocaleString()} />
          )}
          {session.errorMsg && (
            <DetailRow label="Error" value={session.errorMsg} isError />
          )}
        </div>
      </div>

      {/* Checkpoint info for hibernated */}
      {session.checkpoint && (
        <div className="glass-card animate-in stagger-4" style={{ padding: 20 }}>
          <div style={{ fontSize: 11, fontWeight: 600, color: 'var(--text-tertiary)', textTransform: 'uppercase', letterSpacing: '0.05em', marginBottom: 14 }}>
            Checkpoint
          </div>
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '12px 32px' }}>
            <DetailRow label="Size" value={formatBytes(session.checkpoint.sizeBytes)} />
            <DetailRow label="Hibernated" value={new Date(session.checkpoint.hibernatedAt).toLocaleString()} />
          </div>
        </div>
      )}
    </div>
  )
}

function DetailRow({ label, value, isError }: { label: string; value: string; isError?: boolean }) {
  return (
    <div>
      <div style={{ fontSize: 11, color: 'var(--text-tertiary)', marginBottom: 2 }}>{label}</div>
      <div style={{
        fontSize: 13,
        fontFamily: 'var(--font-mono)',
        color: isError ? 'var(--accent-rose)' : 'var(--text-primary)',
        wordBreak: 'break-all',
      }}>
        {value}
      </div>
    </div>
  )
}
