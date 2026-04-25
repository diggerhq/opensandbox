import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { getAPIKeys, createAPIKey, deleteAPIKey, type APIKey } from '../api/client'

function defaultKeyName(existing: APIKey[]): string {
  const today = new Date().toISOString().slice(0, 10)
  const base = `Key ${today}`
  const taken = new Set(existing.map(k => k.name))
  if (!taken.has(base)) return base
  for (let i = 2; i < 100; i++) {
    const candidate = `${base} (${i})`
    if (!taken.has(candidate)) return candidate
  }
  return `${base} (${Date.now()})`
}

export default function APIKeys() {
  const queryClient = useQueryClient()
  const { data: keys, isLoading } = useQuery({
    queryKey: ['api-keys'],
    queryFn: getAPIKeys,
  })

  const [createdKey, setCreatedKey] = useState<string | null>(null)
  const [copied, setCopied] = useState(false)

  const createMutation = useMutation({
    mutationFn: (name: string) => createAPIKey(name),
    onSuccess: (data) => {
      setCreatedKey(data.key)
      setCopied(false)
      queryClient.invalidateQueries({ queryKey: ['api-keys'] })
    },
  })

  const copyKey = () => {
    if (!createdKey) return
    navigator.clipboard.writeText(createdKey)
    setCopied(true)
    setTimeout(() => setCopied(false), 1500)
  }

  const deleteMutation = useMutation({
    mutationFn: (id: string) => deleteAPIKey(id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['api-keys'] })
    },
  })

  return (
    <div>
      {/* Header */}
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: 28 }}>
        <div>
          <h1 className="page-title">API Keys</h1>
          <p className="page-subtitle">Manage authentication tokens for your integrations</p>
        </div>
        <button
          className="btn-primary"
          onClick={() => createMutation.mutate(defaultKeyName(keys ?? []))}
          disabled={createMutation.isPending}
        >
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round">
            <line x1="12" y1="5" x2="12" y2="19" /><line x1="5" y1="12" x2="19" y2="12" />
          </svg>
          {createMutation.isPending ? 'Creating\u2026' : 'Create Key'}
        </button>
      </div>

      {/* Created Key Banner */}
      {createdKey && (
        <div className="animate-in" style={{
          background: 'rgba(99, 102, 241, 0.06)',
          border: '1px solid var(--border-accent)',
          borderRadius: 'var(--radius-lg)',
          padding: 20,
          marginBottom: 20,
        }}>
          <div style={{ fontWeight: 600, marginBottom: 4, fontSize: 14 }}>New API key created</div>
          <div style={{ fontSize: 12, color: 'var(--text-tertiary)', marginBottom: 10 }}>
            Copy this key now. You won&apos;t be able to see it again.
          </div>
          <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
            <code style={{
              flex: 1,
              background: 'var(--bg-deep)',
              color: 'var(--text-accent)',
              padding: 14,
              borderRadius: 'var(--radius-sm)',
              fontSize: 13,
              fontFamily: 'var(--font-mono)',
              wordBreak: 'break-all',
              border: '1px solid var(--border-subtle)',
            }}>{createdKey}</code>
            <button
              onClick={copyKey}
              className="btn-ghost"
              style={{ flexShrink: 0 }}
            >
              {copied ? 'Copied' : 'Copy'}
            </button>
          </div>
          <button
            onClick={() => setCreatedKey(null)}
            style={{
              marginTop: 10, fontSize: 12, background: 'none', border: 'none',
              color: 'var(--accent-indigo)', cursor: 'pointer',
              fontFamily: 'var(--font-body)',
            }}
          >
            Dismiss
          </button>
        </div>
      )}

      {createMutation.isError && (
        <div className="animate-in" style={{
          background: 'rgba(251,113,133,0.06)',
          border: '1px solid rgba(251,113,133,0.2)',
          borderRadius: 'var(--radius-lg)',
          padding: 14, marginBottom: 20,
          fontSize: 13, color: 'var(--accent-rose, #fb7185)',
        }}>
          Failed to create key. Please try again.
        </div>
      )}

      {/* Table */}
      {isLoading ? (
        <div style={{ display: 'flex', justifyContent: 'center', padding: 48 }}>
          <div className="loading-spinner" />
        </div>
      ) : (
        <div className="glass-card animate-in stagger-1" style={{ overflow: 'hidden' }}>
          <table className="data-table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Prefix</th>
                <th>Last Used</th>
                <th>Created</th>
                <th style={{ width: 80 }}></th>
              </tr>
            </thead>
            <tbody>
              {(keys ?? []).map((k: APIKey) => (
                <tr key={k.id}>
                  <td style={{ fontWeight: 500, color: 'var(--text-primary)' }}>{k.name}</td>
                  <td><code>{k.keyPrefix}…</code></td>
                  <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>
                    {k.lastUsed ? new Date(k.lastUsed).toLocaleString() : 'Never'}
                  </td>
                  <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>
                    {new Date(k.createdAt).toLocaleString()}
                  </td>
                  <td>
                    <button
                      className="btn-danger"
                      onClick={() => {
                        if (confirm('Revoke this API key?')) {
                          deleteMutation.mutate(k.id)
                        }
                      }}
                    >
                      Revoke
                    </button>
                  </td>
                </tr>
              ))}
              {(keys ?? []).length === 0 && (
                <tr>
                  <td colSpan={5} style={{ textAlign: 'center', padding: 32, color: 'var(--text-tertiary)' }}>
                    No API keys yet
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
