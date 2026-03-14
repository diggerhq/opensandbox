import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { getImages, deleteImage, type ImageCacheItem } from '../api/client'

function StatusBadge({ status }: { status: string }) {
  const isReady = status === 'ready'
  const isFailed = status === 'failed'
  return (
    <span style={{
      fontSize: 11,
      fontWeight: 600,
      padding: '2px 8px',
      borderRadius: 10,
      background: isReady
        ? 'rgba(34, 197, 94, 0.08)'
        : isFailed
          ? 'rgba(251, 113, 133, 0.08)'
          : 'rgba(234, 179, 8, 0.08)',
      color: isReady
        ? 'var(--accent-green)'
        : isFailed
          ? 'var(--accent-rose)'
          : '#eab308',
      border: `1px solid ${
        isReady
          ? 'rgba(34, 197, 94, 0.15)'
          : isFailed
            ? 'rgba(251, 113, 133, 0.15)'
            : 'rgba(234, 179, 8, 0.15)'
      }`,
    }}>
      {isReady ? 'Ready' : isFailed ? 'Failed' : 'Building'}
    </span>
  )
}

function formatSteps(manifest: Record<string, unknown>): string {
  const steps = manifest.steps as Array<Record<string, unknown>> | undefined
  if (!steps || steps.length === 0) return 'base'
  return steps.map(s => {
    const t = s.type as string
    const args = s.args as Record<string, unknown> | undefined
    switch (t) {
      case 'apt_install': return `apt: ${(args?.packages as string[])?.join(', ') || '...'}`
      case 'pip_install': return `pip: ${(args?.packages as string[])?.join(', ') || '...'}`
      case 'run': return `run: ${((args?.commands as string[]) || []).length} cmd(s)`
      case 'env': return `env: ${Object.keys((args?.vars as Record<string, string>) || {}).length} var(s)`
      case 'workdir': return `workdir: ${args?.path || '...'}`
      case 'add_file': return `file: ${args?.path || '...'}`
      default: return t
    }
  }).join(' + ')
}

export default function Templates() {
  const queryClient = useQueryClient()

  const { data: images, isLoading } = useQuery({
    queryKey: ['images'],
    queryFn: () => getImages(),
  })

  const deleteImageMutation = useMutation({
    mutationFn: (id: string) => deleteImage(id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['images'] })
    },
  })

  const imageCount = (images ?? []).length

  return (
    <div>
      {/* Header */}
      <div style={{ marginBottom: 28 }}>
        <h1 className="page-title">Templates</h1>
        <p className="page-subtitle">Declarative image snapshots for your organization</p>
      </div>

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
                <th>Steps</th>
                <th>Status</th>
                <th>Checkpoint</th>
                <th>Last Used</th>
                <th>Created</th>
                <th style={{ width: 80 }}></th>
              </tr>
            </thead>
            <tbody>
              {(images ?? []).map((img: ImageCacheItem) => (
                <tr key={img.id}>
                  <td style={{ fontWeight: 500, color: 'var(--text-primary)' }}>
                    {img.name || (
                      <span style={{ color: 'var(--text-tertiary)', fontStyle: 'italic' }}>auto-cached</span>
                    )}
                  </td>
                  <td style={{ fontSize: 12, color: 'var(--text-secondary)', maxWidth: 300, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                    {formatSteps(img.manifest)}
                  </td>
                  <td><StatusBadge status={img.status} /></td>
                  <td>
                    {img.checkpointId ? (
                      <code style={{ fontSize: 11 }}>{img.checkpointId.slice(0, 8)}</code>
                    ) : (
                      <span style={{ color: 'var(--text-tertiary)' }}>-</span>
                    )}
                  </td>
                  <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>
                    {new Date(img.lastUsedAt).toLocaleString()}
                  </td>
                  <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>
                    {new Date(img.createdAt).toLocaleString()}
                  </td>
                  <td>
                    <button
                      className="btn-danger"
                      onClick={() => {
                        const label = img.name ? `"${img.name}"` : 'this cached image'
                        if (confirm(`Delete ${label}? Existing sandboxes will not be affected.`)) {
                          deleteImageMutation.mutate(img.id)
                        }
                      }}
                    >
                      Delete
                    </button>
                  </td>
                </tr>
              ))}
              {imageCount === 0 && (
                <tr>
                  <td colSpan={7} style={{ textAlign: 'center', padding: 32, color: 'var(--text-tertiary)' }}>
                    No templates yet. Create them using the SDK with Image.base().aptInstall([...]) or snapshots.create().
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
