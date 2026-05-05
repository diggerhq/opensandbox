// LogsPanel — sandbox session logs view.
//
// Streams from /api/dashboard/sessions/:sandboxId/logs (SSE) and renders
// a live, source-color-coded timeline of events: lines from /var/log
// inside the sandbox, stdout/stderr of every platform-exec'd command,
// and synthetic "exit_code" markers showing how each command finished.
//
// Design contract:
//
//   - Auto-scroll to bottom unless the user has scrolled up. Scrolling
//     up "pins" the view; scrolling back to within ~40px of the bottom
//     unpins it.
//   - Hard cap on retained events (visibleCap below). Once exceeded,
//     drop the oldest. Memory grows linearly with cap and is bounded.
//   - Search filters server-side (the SSE re-opens with `?q=...`); we
//     debounce so each keystroke isn't a new connection.
//   - Source filter is a comma-separated allowlist that maps directly
//     to the server's ?source= param.
//   - Pause toggle freezes display only — the EventSource keeps
//     receiving so unpausing catches up; if buffer overflows, oldest
//     pending events drop.
//
// PTY content is intentionally not part of this view (consent surface
// is different — see design doc).

import { useEffect, useMemo, useRef, useState } from 'react'
import { streamSessionLogs, type LogEvent } from '../api/client'

const visibleCap = 3000        // hard cap on retained events
const debounceMs = 200         // search-box debounce
const nearBottomPx = 40        // "auto-scroll if within this many px of bottom"

const sources: { value: LogEvent['source']; label: string; color: string }[] = [
  { value: 'exec_stdout', label: 'stdout', color: 'var(--text-secondary)' },
  { value: 'exec_stderr', label: 'stderr', color: 'var(--accent-rose)' },
  { value: 'var_log',     label: 'var/log', color: 'var(--text-tertiary)' },
  { value: 'agent',       label: 'agent', color: 'var(--accent-violet)' },
]

function useDebouncedValue<T>(value: T, delayMs: number): T {
  const [debounced, setDebounced] = useState(value)
  useEffect(() => {
    const t = setTimeout(() => setDebounced(value), delayMs)
    return () => clearTimeout(t)
  }, [value, delayMs])
  return debounced
}

interface LogsPanelProps {
  sandboxId: string
  onClose?: () => void
}

export default function LogsPanel({ sandboxId, onClose }: LogsPanelProps) {
  const [query, setQuery] = useState('')
  const [paused, setPaused] = useState(false)
  const [activeSources, setActiveSources] = useState<Set<LogEvent['source']>>(new Set())
  const [events, setEvents] = useState<LogEvent[]>([])
  const [connState, setConnState] = useState<'connecting' | 'open' | 'error' | 'closed'>('connecting')

  const debouncedQuery = useDebouncedValue(query, debounceMs)

  // Buffered events that arrived while paused; flushed on resume.
  const pausedBufRef = useRef<LogEvent[]>([])
  const containerRef = useRef<HTMLDivElement | null>(null)
  const stickToBottomRef = useRef(true)

  // (Re)open the EventSource whenever filter inputs change. Each new
  // open clears the local buffer because the historical batch will
  // re-stream from the start.
  useEffect(() => {
    setEvents([])
    pausedBufRef.current = []
    stickToBottomRef.current = true
    setConnState('connecting')

    const sourceParam = activeSources.size > 0 ? Array.from(activeSources).join(',') : undefined
    const es = streamSessionLogs(sandboxId, {
      tail: true,
      q: debouncedQuery || undefined,
      source: sourceParam,
    })

    es.onopen = () => setConnState('open')
    es.onerror = () => setConnState('error')
    es.onmessage = (e) => {
      let ev: LogEvent
      try {
        ev = JSON.parse(e.data) as LogEvent
      } catch {
        return
      }
      if (paused) {
        pausedBufRef.current.push(ev)
        // Defensive cap on the paused buffer.
        if (pausedBufRef.current.length > visibleCap) {
          pausedBufRef.current = pausedBufRef.current.slice(-visibleCap)
        }
        return
      }
      setEvents((prev) => {
        const next = prev.length >= visibleCap ? prev.slice(prev.length - visibleCap + 1) : prev.slice()
        next.push(ev)
        return next
      })
    }

    return () => {
      es.close()
      setConnState('closed')
    }
    // We intentionally re-subscribe whenever query/source change. We
    // do NOT include `paused` here — pausing is purely client-side.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sandboxId, debouncedQuery, activeSources])

  // When unpausing, drain the paused buffer into events.
  useEffect(() => {
    if (!paused && pausedBufRef.current.length > 0) {
      const buf = pausedBufRef.current
      pausedBufRef.current = []
      setEvents((prev) => {
        const merged = prev.concat(buf)
        return merged.length > visibleCap ? merged.slice(merged.length - visibleCap) : merged
      })
    }
  }, [paused])

  // Auto-scroll to bottom when new events arrive — but only if the
  // user is already near the bottom (i.e. they haven't scrolled up to
  // read older lines).
  useEffect(() => {
    if (!stickToBottomRef.current) return
    const el = containerRef.current
    if (!el) return
    el.scrollTop = el.scrollHeight
  }, [events])

  const handleScroll = () => {
    const el = containerRef.current
    if (!el) return
    const distFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight
    stickToBottomRef.current = distFromBottom <= nearBottomPx
  }

  const toggleSource = (src: LogEvent['source']) => {
    setActiveSources((prev) => {
      const next = new Set(prev)
      if (next.has(src)) next.delete(src)
      else next.add(src)
      return next
    })
  }

  const visible = useMemo(() => events, [events])

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: 480 }}>
      {/* Header */}
      <div style={{
        display: 'flex',
        alignItems: 'center',
        gap: 12,
        marginBottom: 12,
        flexWrap: 'wrap',
      }}>
        <div style={{ fontSize: 11, fontWeight: 600, color: 'var(--text-tertiary)', textTransform: 'uppercase', letterSpacing: '0.05em' }}>
          Logs
        </div>

        <ConnectionDot state={connState} />

        <input
          type="search"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="Search…"
          style={{
            flex: 1,
            minWidth: 160,
            background: 'var(--bg-deep)',
            border: '1px solid var(--border-subtle)',
            borderRadius: 'var(--radius-sm)',
            color: 'var(--text-primary)',
            fontSize: 13,
            padding: '6px 10px',
            fontFamily: 'inherit',
          }}
        />

        <div style={{ display: 'flex', gap: 4 }}>
          {sources.map((src) => {
            const isActive = activeSources.has(src.value)
            const noFilters = activeSources.size === 0
            return (
              <button
                key={src.value}
                onClick={() => toggleSource(src.value)}
                title={`Filter to ${src.label}`}
                style={{
                  background: isActive ? src.color : 'transparent',
                  color: isActive ? 'var(--bg-deep)' : (noFilters ? src.color : 'var(--text-tertiary)'),
                  border: `1px solid ${isActive ? src.color : 'var(--border-subtle)'}`,
                  borderRadius: 'var(--radius-sm)',
                  fontSize: 11,
                  padding: '3px 8px',
                  cursor: 'pointer',
                  fontFamily: 'var(--font-mono)',
                }}
              >
                {src.label}
              </button>
            )
          })}
        </div>

        <button
          onClick={() => setPaused((p) => !p)}
          className="btn-ghost"
          title={paused ? 'Resume live tail' : 'Pause display (stream keeps running)'}
        >
          {paused ? 'Resume' : 'Pause'}
          {paused && pausedBufRef.current.length > 0 && (
            <span style={{ marginLeft: 6, color: 'var(--text-tertiary)' }}>
              ({pausedBufRef.current.length} buffered)
            </span>
          )}
        </button>

        {onClose && (
          <button onClick={onClose} className="btn-ghost" title="Close logs">
            Close
          </button>
        )}
      </div>

      {/* Stream view */}
      <div
        ref={containerRef}
        onScroll={handleScroll}
        style={{
          flex: 1,
          background: 'var(--bg-deep)',
          border: '1px solid var(--border-subtle)',
          borderRadius: 'var(--radius-sm)',
          padding: '8px 0',
          overflowY: 'auto',
          fontFamily: 'var(--font-mono)',
          fontSize: 12,
          lineHeight: 1.45,
        }}
      >
        {visible.length === 0 && connState === 'open' && (
          <div style={{ padding: '20px', textAlign: 'center', color: 'var(--text-tertiary)' }}>
            No logs yet. Run a command, write to <code>/var/log/...</code>, or wait for system activity.
          </div>
        )}
        {visible.length === 0 && connState === 'error' && (
          <div style={{ padding: '20px', textAlign: 'center', color: 'var(--accent-rose)' }}>
            Couldn't connect to log stream. Logs may not be configured for this deployment.
          </div>
        )}
        {visible.map((ev, i) => (
          <Row key={i} ev={ev} />
        ))}
      </div>

      {/* Footer: cap notice */}
      {visible.length >= visibleCap && (
        <div style={{ marginTop: 6, fontSize: 11, color: 'var(--text-tertiary)' }}>
          Showing latest {visibleCap} events; older events scrolled off.
        </div>
      )}
    </div>
  )
}

function ConnectionDot({ state }: { state: 'connecting' | 'open' | 'error' | 'closed' }) {
  const color =
    state === 'open' ? 'var(--accent-emerald, #10b981)' :
    state === 'error' ? 'var(--accent-rose)' :
    'var(--text-tertiary)'
  return (
    <span
      title={`Connection: ${state}`}
      style={{
        display: 'inline-block',
        width: 8,
        height: 8,
        borderRadius: '50%',
        background: color,
      }}
    />
  )
}

function Row({ ev }: { ev: LogEvent }) {
  const sourceMeta = sources.find((s) => s.value === ev.source)
  const time = formatTime(ev._time)

  // Exec EOF marker: line is empty + exit_code is present. Render as a
  // synthetic system row so users can see "command X exited 0/1" inline.
  if (ev.line === '' && ev.exit_code !== undefined) {
    const ok = ev.exit_code === 0
    return (
      <div style={{
        display: 'flex',
        gap: 10,
        padding: '2px 14px',
        color: ok ? 'var(--accent-emerald, #10b981)' : 'var(--accent-rose)',
        fontStyle: 'italic',
        opacity: 0.85,
      }}>
        <span style={{ color: 'var(--text-tertiary)', fontStyle: 'normal' }}>{time}</span>
        <span>
          {ok ? '✓' : '✗'} {ev.command || 'command'} exited {ev.exit_code}
        </span>
      </div>
    )
  }

  return (
    <div style={{
      display: 'flex',
      gap: 10,
      padding: '1px 14px',
      whiteSpace: 'pre-wrap',
      wordBreak: 'break-word',
    }}>
      <span style={{ color: 'var(--text-tertiary)', flexShrink: 0 }}>{time}</span>
      <span
        style={{
          color: sourceMeta?.color || 'var(--text-tertiary)',
          flexShrink: 0,
          minWidth: 56,
          fontSize: 11,
          paddingTop: 1,
        }}
      >
        {sourceMeta?.label || ev.source}
      </span>
      <span style={{ color: 'var(--text-primary)', flex: 1 }}>
        {ev.line}
      </span>
    </div>
  )
}

function formatTime(rfc3339: string): string {
  // Display HH:MM:SS.mmm — matches the granularity shippers use.
  const d = new Date(rfc3339)
  if (isNaN(d.getTime())) return ''
  const hh = String(d.getHours()).padStart(2, '0')
  const mm = String(d.getMinutes()).padStart(2, '0')
  const ss = String(d.getSeconds()).padStart(2, '0')
  const ms = String(d.getMilliseconds()).padStart(3, '0')
  return `${hh}:${mm}:${ss}.${ms}`
}
