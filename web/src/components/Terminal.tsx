import { useEffect, useRef, useState } from 'react'
import { Terminal as XTerm } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { WebLinksAddon } from '@xterm/addon-web-links'
import '@xterm/xterm/css/xterm.css'

interface TerminalProps {
  sandboxId: string
  onClose?: () => void
}

export default function Terminal({ sandboxId, onClose }: TerminalProps) {
  const termRef = useRef<HTMLDivElement>(null)
  const xtermRef = useRef<XTerm | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const fitRef = useRef<FitAddon | null>(null)
  const ptySessionIdRef = useRef<string | null>(null)
  const [status, setStatus] = useState<'connecting' | 'connected' | 'disconnected' | 'error'>('connecting')
  const [errorMsg, setErrorMsg] = useState('')

  useEffect(() => {
    if (!termRef.current) return

    const term = new XTerm({
      cursorBlink: true,
      fontSize: 13,
      fontFamily: 'JetBrains Mono, Menlo, Monaco, Consolas, monospace',
      theme: {
        background: '#0a0a0f',
        foreground: '#e4e4e7',
        cursor: '#818cf8',
        selectionBackground: 'rgba(129, 140, 248, 0.3)',
        black: '#18181b',
        red: '#fb7185',
        green: '#34d399',
        yellow: '#fbbf24',
        blue: '#818cf8',
        magenta: '#c084fc',
        cyan: '#22d3ee',
        white: '#e4e4e7',
        brightBlack: '#52525b',
        brightRed: '#fda4af',
        brightGreen: '#6ee7b7',
        brightYellow: '#fde68a',
        brightBlue: '#a5b4fc',
        brightMagenta: '#d8b4fe',
        brightCyan: '#67e8f9',
        brightWhite: '#fafafa',
      },
      allowProposedApi: true,
    })

    const fit = new FitAddon()
    const webLinks = new WebLinksAddon()

    term.loadAddon(fit)
    term.loadAddon(webLinks)
    term.open(termRef.current)

    // Fit after a small delay to let the container render
    requestAnimationFrame(() => {
      fit.fit()
    })

    xtermRef.current = term
    fitRef.current = fit

    term.writeln('\x1b[2m  Connecting to sandbox...\x1b[0m')

    // Create PTY session, then connect WebSocket
    const initTerminal = async () => {
      try {
        const cols = term.cols
        const rows = term.rows

        // Create PTY session
        const res = await fetch(`/api/dashboard/sessions/${sandboxId}/pty`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          credentials: 'include',
          body: JSON.stringify({ cols, rows }),
        })

        if (!res.ok) {
          const data = await res.json()
          throw new Error(data.error || `HTTP ${res.status}`)
        }

        const { sessionId } = await res.json()
        ptySessionIdRef.current = sessionId

        // Connect WebSocket
        const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
        const wsUrl = `${proto}//${window.location.host}/api/dashboard/sessions/${sandboxId}/pty/${sessionId}`
        const ws = new WebSocket(wsUrl)
        ws.binaryType = 'arraybuffer'
        wsRef.current = ws

        ws.onopen = () => {
          setStatus('connected')
          term.clear()
          term.focus()
        }

        ws.onmessage = (event) => {
          const data = new Uint8Array(event.data)
          term.write(data)
        }

        ws.onclose = () => {
          setStatus('disconnected')
          term.writeln('')
          term.writeln('\x1b[2m  Session ended.\x1b[0m')
        }

        ws.onerror = () => {
          setStatus('error')
          setErrorMsg('WebSocket connection failed')
        }

        // Terminal input -> WebSocket
        term.onData((data) => {
          if (ws.readyState === WebSocket.OPEN) {
            ws.send(new TextEncoder().encode(data))
          }
        })

        // Handle resize — send to backend resize endpoint (not via PTY stream)
        term.onResize(({ cols, rows }) => {
          if (ptySessionIdRef.current) {
            fetch(`/api/dashboard/sessions/${sandboxId}/pty/${ptySessionIdRef.current}/resize`, {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              credentials: 'include',
              body: JSON.stringify({ cols, rows }),
            }).catch(() => {
              // Resize failed — not critical, ignore
            })
          }
        })
      } catch (err: unknown) {
        setStatus('error')
        const msg = err instanceof Error ? err.message : 'Failed to connect'
        setErrorMsg(msg)
        term.writeln(`\x1b[31m  Error: ${msg}\x1b[0m`)
      }
    }

    initTerminal()

    // Handle window resize
    const handleResize = () => {
      if (fitRef.current) {
        fitRef.current.fit()
      }
    }
    window.addEventListener('resize', handleResize)

    return () => {
      window.removeEventListener('resize', handleResize)
      if (wsRef.current) {
        wsRef.current.close()
      }
      term.dispose()
    }
  }, [sandboxId])

  return (
    <div>
      {/* Header */}
      <div style={{
        display: 'flex', justifyContent: 'space-between', alignItems: 'center',
        marginBottom: 12,
      }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" style={{ color: 'var(--text-tertiary)' }}>
            <polyline points="4 17 10 11 4 5" />
            <line x1="12" y1="19" x2="20" y2="19" />
          </svg>
          <span style={{ fontSize: 11, fontWeight: 600, color: 'var(--text-tertiary)', textTransform: 'uppercase', letterSpacing: '0.05em' }}>
            Terminal
          </span>
          <span style={{
            fontSize: 10, padding: '2px 6px', borderRadius: 4,
            background: status === 'connected' ? 'rgba(52,211,153,0.15)' :
                        status === 'connecting' ? 'rgba(129,140,248,0.15)' :
                        status === 'error' ? 'rgba(251,113,133,0.15)' :
                        'rgba(255,255,255,0.05)',
            color: status === 'connected' ? 'var(--accent-emerald)' :
                   status === 'connecting' ? '#818cf8' :
                   status === 'error' ? '#fb7185' :
                   'var(--text-tertiary)',
          }}>
            {status}
          </span>
        </div>
        {onClose && (
          <button
            onClick={onClose}
            style={{
              background: 'none', border: 'none', cursor: 'pointer',
              color: 'var(--text-tertiary)', fontSize: 11, padding: '4px 8px',
            }}
          >
            Close
          </button>
        )}
      </div>

      {/* Terminal container */}
      <div
        ref={termRef}
        style={{
          height: 350,
          borderRadius: 8,
          overflow: 'hidden',
          background: '#0a0a0f',
          padding: '8px 4px',
        }}
      />

      {status === 'error' && errorMsg && (
        <div style={{ fontSize: 11, color: '#fb7185', marginTop: 8 }}>
          {errorMsg}
        </div>
      )}
    </div>
  )
}
