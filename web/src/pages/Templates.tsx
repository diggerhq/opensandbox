import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { getTemplates, buildTemplate, deleteTemplate, type Template } from '../api/client'

export default function Templates() {
  const queryClient = useQueryClient()
  const { data: templates, isLoading } = useQuery({
    queryKey: ['templates'],
    queryFn: getTemplates,
  })

  const [showCreate, setShowCreate] = useState(false)
  const [newName, setNewName] = useState('')
  const [newDockerfile, setNewDockerfile] = useState('')

  const buildMutation = useMutation({
    mutationFn: ({ name, dockerfile }: { name: string; dockerfile: string }) =>
      buildTemplate(name, dockerfile),
    onSuccess: () => {
      setShowCreate(false)
      setNewName('')
      setNewDockerfile('')
      queryClient.invalidateQueries({ queryKey: ['templates'] })
    },
  })

  const deleteMutation = useMutation({
    mutationFn: (id: string) => deleteTemplate(id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['templates'] })
    },
  })

  return (
    <div>
      {/* Header */}
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: 28 }}>
        <div>
          <h1 className="page-title">Templates</h1>
          <p className="page-subtitle">Manage sandbox base images for your organization</p>
        </div>
        <button className="btn-primary" onClick={() => setShowCreate(true)}>
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round">
            <line x1="12" y1="5" x2="12" y2="19" /><line x1="5" y1="12" x2="19" y2="12" />
          </svg>
          Create Template
        </button>
      </div>

      {/* Create Dialog */}
      {showCreate && (
        <div className="glass-card animate-in" style={{ padding: 22, marginBottom: 20 }}>
          <div style={{ marginBottom: 14 }}>
            <label style={{ display: 'block', fontSize: 12, fontWeight: 600, color: 'var(--text-secondary)', marginBottom: 6 }}>
              Template Name
            </label>
            <input
              type="text"
              value={newName}
              onChange={e => setNewName(e.target.value)}
              placeholder="e.g. my-app"
              className="input"
              autoFocus
            />
          </div>
          <div style={{ marginBottom: 14 }}>
            <label style={{ display: 'block', fontSize: 12, fontWeight: 600, color: 'var(--text-secondary)', marginBottom: 6 }}>
              Dockerfile
            </label>
            <textarea
              value={newDockerfile}
              onChange={e => setNewDockerfile(e.target.value)}
              placeholder={'FROM ubuntu:22.04\nRUN apt-get update && apt-get install -y curl'}
              className="input"
              rows={8}
              style={{
                fontFamily: 'var(--font-mono)',
                fontSize: 13,
                resize: 'vertical',
                minHeight: 120,
              }}
            />
          </div>
          {buildMutation.isError && (
            <div style={{
              color: 'var(--accent-rose)',
              fontSize: 12,
              marginBottom: 12,
              padding: '8px 12px',
              background: 'rgba(244, 63, 94, 0.06)',
              borderRadius: 'var(--radius-sm)',
              border: '1px solid rgba(244, 63, 94, 0.15)',
            }}>
              {buildMutation.error?.message || 'Build failed'}
            </div>
          )}
          <div style={{ display: 'flex', gap: 8 }}>
            <button
              className="btn-primary"
              onClick={() => buildMutation.mutate({ name: newName, dockerfile: newDockerfile })}
              disabled={buildMutation.isPending || !newName.trim() || !newDockerfile.trim()}
            >
              {buildMutation.isPending ? 'Building\u2026' : 'Build Template'}
            </button>
            <button className="btn-ghost" onClick={() => { setShowCreate(false); setNewName(''); setNewDockerfile('') }}>
              Cancel
            </button>
          </div>
          {buildMutation.isPending && (
            <div style={{ marginTop: 12, fontSize: 12, color: 'var(--text-tertiary)' }}>
              Building image and pushing to registry. This may take a few minutes...
            </div>
          )}
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
                <th>Tag</th>
                <th>Type</th>
                <th>Image</th>
                <th>Created</th>
                <th style={{ width: 80 }}></th>
              </tr>
            </thead>
            <tbody>
              {(templates ?? []).map((t: Template) => (
                <tr key={t.id}>
                  <td style={{ fontWeight: 500, color: 'var(--text-primary)' }}>{t.name}</td>
                  <td><code>{t.tag}</code></td>
                  <td>
                    <span style={{
                      fontSize: 11,
                      fontWeight: 600,
                      padding: '2px 8px',
                      borderRadius: 10,
                      background: t.isPublic
                        ? 'rgba(34, 197, 94, 0.08)'
                        : 'rgba(99, 102, 241, 0.08)',
                      color: t.isPublic
                        ? 'var(--accent-green)'
                        : 'var(--accent-indigo)',
                      border: `1px solid ${t.isPublic ? 'rgba(34, 197, 94, 0.15)' : 'rgba(99, 102, 241, 0.15)'}`,
                    }}>
                      {t.isPublic ? 'Built-in' : 'Custom'}
                    </span>
                  </td>
                  <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12, maxWidth: 300, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                    {t.imageRef}
                  </td>
                  <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>
                    {new Date(t.createdAt).toLocaleString()}
                  </td>
                  <td>
                    {!t.isPublic && (
                      <button
                        className="btn-danger"
                        onClick={() => {
                          if (confirm('Delete this template? Existing sandboxes using it will not be affected.')) {
                            deleteMutation.mutate(t.id)
                          }
                        }}
                      >
                        Delete
                      </button>
                    )}
                  </td>
                </tr>
              ))}
              {(templates ?? []).length === 0 && (
                <tr>
                  <td colSpan={6} style={{ textAlign: 'center', padding: 32, color: 'var(--text-tertiary)' }}>
                    No templates yet
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
