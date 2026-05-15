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

// One colour per source. Distinct hues across the four; no overlap
// with the green used for success-EOF rows (✓ exited 0) and the rose
// used for failure-EOF rows.
const sources: { value: LogEvent['source']; label: string; color: string }[] = [
  { value: 'exec_stdout', label: 'stdout',  color: 'var(--text-secondary)' },
  { value: 'exec_stderr', label: 'stderr',  color: 'var(--accent-rose)' },
  { value: 'var_log',     label: 'var/log', color: '#f59e0b' /* amber — distinct from emerald used for ✓ exited */ },
  { value: 'agent',       label: 'agent',   color: 'var(--accent-violet)' },
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
  // Subtractive filter: every source is shown by default. Clicking a
  // chip *hides* that source; clicking again brings it back.
  const [hiddenSources, setHiddenSources] = useState<Set<LogEvent['source']>>(new Set())
  const [events, setEvents] = useState<LogEvent[]>([])
  const [connState, setConnState] = useState<'connecting' | 'open' | 'error' | 'closed'>('connecting')

  const debouncedQuery = useDebouncedValue(query, debounceMs)

  // Buffered events that arrived while paused; flushed on resume.
  const pausedBufRef = useRef<LogEvent[]>([])
  const containerRef = useRef<HTMLDivElement | null>(null)
  const stickToBottomRef = useRef(true)

  // Open the EventSource ONCE per sandbox. Search and source filters
  // are applied client-side against the in-memory buffer — no SSE
  // re-open on filter change, no flicker. The historical batch is
  // bounded by visibleCap so memory stays cheap; if a user really
  // wants to search beyond the current buffer they can adjust the
  // since= URL param later.
  useEffect(() => {
    setEvents([])
    pausedBufRef.current = []
    stickToBottomRef.current = true
    setConnState('connecting')

    const es = streamSessionLogs(sandboxId, { tail: true })

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
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sandboxId])

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
    setHiddenSources((prev) => {
      const next = new Set(prev)
      if (next.has(src)) next.delete(src)
      else next.add(src)
      return next
    })
  }

  // Client-side filter pipeline: source filter → text search → EOF
  // dedup. Each command emits one EOF event per stream (stdout AND
  // stderr) so without dedup the timeline shows "exited 0" twice for
  // the same exec. We collapse them to one per exec_id.
  const visible = useMemo(() => {
    const lowerQuery = debouncedQuery.trim().toLowerCase()

    // Two-pass:
    //  1. Dedupe EOFs by exec_id — agent emits one EOF on stdout AND
    //     one on stderr per exec; we keep the first (stdout, by emit
    //     order), drop the rest. Must happen BEFORE the source filter
    //     — otherwise hiding stdout lets the suppressed stderr-EOF
    //     through and the row "moves" from stdout-coloured to stderr.
    //  2. Apply source + text filters to the deduped list.
    const seenEofExecIDs = new Set<string>()
    const deduped: LogEvent[] = []
    for (const ev of events) {
      const isEof = ev.line === '' && ev.exit_code !== undefined
      if (isEof && ev.exec_id) {
        if (seenEofExecIDs.has(ev.exec_id)) continue
        seenEofExecIDs.add(ev.exec_id)
      }
      deduped.push(ev)
    }

    return deduped.filter((ev) => {
      if (hiddenSources.has(ev.source)) return false
      if (lowerQuery && !searchCorpus(ev).includes(lowerQuery)) return false
      return true
    })
  }, [events, hiddenSources, debouncedQuery])

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

        <div style={{ flex: 1, minWidth: 160, position: 'relative' }}>
          <input
            type="text"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="Search…"
            style={{
              width: '100%',
              boxSizing: 'border-box',
              background: 'var(--bg-deep)',
              border: '1px solid var(--border-subtle)',
              borderRadius: 'var(--radius-sm)',
              color: 'var(--text-primary)',
              fontSize: 13,
              padding: '6px 28px 6px 10px',
              fontFamily: 'inherit',
            }}
          />
          {query && (
            <button
              type="button"
              onClick={() => setQuery('')}
              aria-label="Clear search"
              title="Clear"
              style={{
                position: 'absolute',
                right: 6,
                top: '50%',
                transform: 'translateY(-50%)',
                background: 'none',
                border: 'none',
                padding: 4,
                cursor: 'pointer',
                color: 'var(--text-tertiary)',
                display: 'inline-flex',
                alignItems: 'center',
                justifyContent: 'center',
                lineHeight: 0,
                opacity: 0.7,
                transition: 'opacity 0.1s',
              }}
              onMouseEnter={(e) => (e.currentTarget.style.opacity = '1')}
              onMouseLeave={(e) => (e.currentTarget.style.opacity = '0.7')}
            >
              <svg width="11" height="11" viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round">
                <line x1="2" y1="2" x2="10" y2="10" />
                <line x1="10" y1="2" x2="2" y2="10" />
              </svg>
            </button>
          )}
        </div>

        <div style={{ display: 'flex', gap: 4 }}>
          {sources.map((src) => {
            // Subtractive filter: chip ON = source visible (the
            // default); chip OFF = source hidden. Click toggles.
            // ON state uses a subtle ~18% tint of the source color
            // rather than a full fill — keeps the four chips legible
            // when all four are active by default.
            const isVisible = !hiddenSources.has(src.value)
            const tinted = `color-mix(in srgb, ${src.color} 18%, transparent)`
            return (
              <button
                key={src.value}
                onClick={() => toggleSource(src.value)}
                title={isVisible ? `Hide ${src.label}` : `Show ${src.label}`}
                style={{
                  background: isVisible ? tinted : 'transparent',
                  color: src.color,
                  border: `1px solid ${src.color}`,
                  borderRadius: 'var(--radius-sm)',
                  fontSize: 11,
                  fontWeight: isVisible ? 600 : 400,
                  opacity: isVisible ? 1 : 0.45,
                  padding: '3px 8px',
                  cursor: 'pointer',
                  fontFamily: 'var(--font-mono)',
                  transition: 'background 0.1s, opacity 0.1s',
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
  const sourceColor = sourceMeta?.color || 'var(--text-tertiary)'
  const time = formatTime(ev._time)

  // 3px left border in the source color so the chip palette and the
  // row palette obviously share a key. Padding matches the
  // non-bordered baseline so rows of different sources align.
  const baseStyle: React.CSSProperties = {
    display: 'flex',
    gap: 10,
    padding: '1px 14px',
    borderLeft: `3px solid ${sourceColor}`,
    paddingLeft: 11, // 14 - 3 to keep content alignment
  }

  // Exec EOF marker: line is empty + exit_code is present. Render as a
  // synthetic system row so users can see "command X exited 0/1" inline.
  if (ev.line === '' && ev.exit_code !== undefined) {
    const ok = ev.exit_code === 0
    return (
      <div style={{
        ...baseStyle,
        padding: '2px 14px 2px 11px',
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
      ...baseStyle,
      whiteSpace: 'pre-wrap',
      wordBreak: 'break-word',
    }}>
      <span style={{ color: 'var(--text-tertiary)', flexShrink: 0 }}>{time}</span>
      <span
        style={{
          color: sourceColor,
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

// searchCorpus returns the text the search box matches against for a
// given event, lower-cased. For real lines it's just `line`; for EOF
// markers (which have line=='' and a synthesized "X exited N"
// rendering) we include the synthesized text plus the command name +
// argv so users can search for "exit", a command, or its args.
function searchCorpus(ev: LogEvent): string {
  if (ev.line === '' && ev.exit_code !== undefined) {
    const cmd = ev.command || 'command'
    const argv = ev.argv ? ev.argv.join(' ') : ''
    return `${cmd} ${argv} exited ${ev.exit_code}`.toLowerCase()
  }
  return ev.line.toLowerCase()
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
