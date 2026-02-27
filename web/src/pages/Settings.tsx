import { useState, useEffect } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { getOrg, updateOrg, setCustomDomain, deleteCustomDomain, refreshCustomDomain } from '../api/client'

function StatusBadge({ status }: { status: string }) {
  let color = 'var(--text-tertiary)'
  let bg = 'rgba(255,255,255,0.04)'
  if (status === 'active') {
    color = 'var(--accent-emerald)'
    bg = 'rgba(52,211,153,0.1)'
  } else if (status === 'pending' || status === 'pending_validation' || status === 'initializing') {
    color = 'var(--accent-amber, #f59e0b)'
    bg = 'rgba(245,158,11,0.1)'
  } else if (status === 'none') {
    color = 'var(--text-tertiary)'
    bg = 'rgba(255,255,255,0.04)'
  }
  return (
    <span style={{
      display: 'inline-block',
      padding: '2px 8px',
      borderRadius: 4,
      fontSize: 11,
      fontWeight: 600,
      color,
      background: bg,
      textTransform: 'uppercase',
      letterSpacing: '0.5px',
    }}>
      {status}
    </span>
  )
}

export default function Settings() {
  const queryClient = useQueryClient()
  const { data: org, isLoading } = useQuery({
    queryKey: ['org'],
    queryFn: getOrg,
  })

  const [name, setName] = useState('')
  const [saved, setSaved] = useState(false)
  const [domainInput, setDomainInput] = useState('')

  useEffect(() => {
    if (org) setName(org.name)
  }, [org])

  const mutation = useMutation({
    mutationFn: (n: string) => updateOrg(n),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['org'] })
      setSaved(true)
      setTimeout(() => setSaved(false), 2000)
    },
  })

  const setDomainMutation = useMutation({
    mutationFn: (domain: string) => setCustomDomain(domain),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['org'] })
      setDomainInput('')
    },
  })

  const deleteDomainMutation = useMutation({
    mutationFn: () => deleteCustomDomain(),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['org'] })
    },
  })

  const refreshDomainMutation = useMutation({
    mutationFn: () => refreshCustomDomain(),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['org'] })
    },
  })

  if (isLoading) {
    return (
      <div style={{ display: 'flex', justifyContent: 'center', padding: 64 }}>
        <div className="loading-spinner" />
      </div>
    )
  }

  const unchanged = name === org?.name
  const hasDomain = org?.customDomain && org.customDomain !== ''
  const labelStyle = { display: 'block' as const, fontSize: 12, fontWeight: 600, color: 'var(--text-secondary)', marginBottom: 6 }
  const readOnlyStyle = {
    padding: '10px 14px',
    background: 'rgba(255,255,255,0.02)',
    border: '1px solid var(--border-subtle)',
    borderRadius: 'var(--radius-sm)',
    fontSize: 13,
    fontFamily: 'var(--font-mono)',
    color: 'var(--text-tertiary)',
  }

  return (
    <div>
      <div style={{ marginBottom: 28 }}>
        <h1 className="page-title">Settings</h1>
        <p className="page-subtitle">Organization configuration</p>
      </div>

      <div className="glass-card animate-in stagger-1" style={{ padding: 28, maxWidth: 520 }}>
        {/* Org Name */}
        <div style={{ marginBottom: 22 }}>
          <label style={labelStyle}>
            Organization Name
          </label>
          <input
            type="text"
            value={name}
            onChange={e => setName(e.target.value)}
            className="input"
          />
        </div>

        {/* Plan (read-only) */}
        <div style={{ marginBottom: 22 }}>
          <label style={labelStyle}>
            Plan
          </label>
          <div style={{
            padding: '10px 14px',
            background: 'rgba(255,255,255,0.02)',
            border: '1px solid var(--border-subtle)',
            borderRadius: 'var(--radius-sm)',
            fontSize: 14,
            color: 'var(--text-tertiary)',
            textTransform: 'capitalize',
          }}>
            {org?.plan ?? 'free'}
          </div>
        </div>

        {/* Limits */}
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16, marginBottom: 24 }}>
          <div>
            <label style={labelStyle}>
              Max Concurrent Sandboxes
            </label>
            <div style={readOnlyStyle}>
              {org?.maxConcurrentSandboxes}
            </div>
          </div>
          <div>
            <label style={labelStyle}>
              Max Timeout (sec)
            </label>
            <div style={readOnlyStyle}>
              {org?.maxSandboxTimeoutSec}
            </div>
          </div>
        </div>

        {/* Save */}
        <div style={{ display: 'flex', alignItems: 'center', gap: 14 }}>
          <button
            className="btn-primary"
            onClick={() => mutation.mutate(name)}
            disabled={mutation.isPending || unchanged}
          >
            {mutation.isPending ? 'Saving\u2026' : 'Save Changes'}
          </button>
          {saved && (
            <span style={{
              fontSize: 12, fontWeight: 500,
              color: 'var(--accent-emerald)',
              animation: 'fadeInUp 0.3s ease',
            }}>
              Saved
            </span>
          )}
        </div>
      </div>

      {/* Custom Domain */}
      <div className="glass-card animate-in stagger-2" style={{ padding: 28, maxWidth: 520, marginTop: 20 }}>
        <div style={{ marginBottom: 18 }}>
          <h2 style={{ fontSize: 16, fontWeight: 600, color: 'var(--text-primary)', margin: 0 }}>Custom Domain</h2>
          <p style={{ fontSize: 13, color: 'var(--text-tertiary)', margin: '4px 0 0' }}>
            Configure a custom domain for sandbox preview URLs (e.g. sandboxes become accessible at &lt;id&gt;.yourdomain.com)
          </p>
        </div>

        {hasDomain ? (
          <>
            {/* Current domain */}
            <div style={{ marginBottom: 16 }}>
              <label style={labelStyle}>Domain</label>
              <div style={readOnlyStyle}>
                *.{org!.customDomain}
              </div>
            </div>

            {/* Status */}
            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16, marginBottom: 16 }}>
              <div>
                <label style={labelStyle}>Verification</label>
                <StatusBadge status={org!.domainVerificationStatus} />
              </div>
              <div>
                <label style={labelStyle}>SSL</label>
                <StatusBadge status={org!.domainSslStatus} />
              </div>
            </div>

            {/* DNS Records */}
            {(org!.verificationTxtName || org!.sslTxtName) && (
              <div style={{ marginBottom: 16 }}>
                <label style={labelStyle}>Required DNS TXT Records</label>
                <div style={{
                  background: 'rgba(255,255,255,0.02)',
                  border: '1px solid var(--border-subtle)',
                  borderRadius: 'var(--radius-sm)',
                  padding: 14,
                  fontSize: 12,
                  fontFamily: 'var(--font-mono)',
                  lineHeight: 1.6,
                  color: 'var(--text-tertiary)',
                  wordBreak: 'break-all',
                }}>
                  {org!.verificationTxtName && (
                    <div style={{ marginBottom: org!.sslTxtName ? 12 : 0 }}>
                      <div style={{ color: 'var(--text-secondary)', fontWeight: 600, marginBottom: 2 }}>Domain Verification:</div>
                      <div>Name: {org!.verificationTxtName}</div>
                      <div>Value: {org!.verificationTxtValue}</div>
                    </div>
                  )}
                  {org!.sslTxtName && (
                    <div>
                      <div style={{ color: 'var(--text-secondary)', fontWeight: 600, marginBottom: 2 }}>SSL Validation:</div>
                      <div>Name: {org!.sslTxtName}</div>
                      <div>Value: {org!.sslTxtValue}</div>
                    </div>
                  )}
                </div>
              </div>
            )}

            {/* Wildcard CNAME instruction for preview URLs */}
            {org!.domainVerificationStatus === 'active' && (
              <div style={{ marginBottom: 16 }}>
                <label style={labelStyle}>Preview URL Setup</label>
                <div style={{
                  background: 'rgba(255,255,255,0.02)',
                  border: '1px solid var(--border-subtle)',
                  borderRadius: 'var(--radius-sm)',
                  padding: 14,
                  fontSize: 12,
                  fontFamily: 'var(--font-mono)',
                  lineHeight: 1.6,
                  color: 'var(--text-tertiary)',
                  wordBreak: 'break-all',
                }}>
                  <div style={{ color: 'var(--text-secondary)', fontWeight: 600, marginBottom: 6 }}>
                    Add a wildcard CNAME record for preview URLs:
                  </div>
                  <div>Type: <span style={{ color: 'var(--text-primary)' }}>CNAME</span></div>
                  <div>Name: <span style={{ color: 'var(--text-primary)' }}>*.{org!.customDomain}</span></div>
                  <div>Target: <span style={{ color: 'var(--text-primary)' }}>fallback-origin.opencomputer.dev</span></div>
                  <div style={{ marginTop: 8, fontSize: 11, color: 'var(--text-tertiary)' }}>
                    This enables per-sandbox preview URLs (e.g. sb-abc123.{org!.customDomain}) via the SDK's createPreviewURL() method.
                  </div>
                </div>
              </div>
            )}

            {/* Actions */}
            <div style={{ display: 'flex', gap: 10 }}>
              <button
                className="btn-primary"
                onClick={() => refreshDomainMutation.mutate()}
                disabled={refreshDomainMutation.isPending}
              >
                {refreshDomainMutation.isPending ? 'Refreshing\u2026' : 'Refresh Status'}
              </button>
              <button
                className="btn-secondary"
                onClick={() => {
                  if (confirm('Remove custom domain? Sandbox URLs will revert to the default domain.')) {
                    deleteDomainMutation.mutate()
                  }
                }}
                disabled={deleteDomainMutation.isPending}
                style={{ color: 'var(--accent-red, #ef4444)' }}
              >
                {deleteDomainMutation.isPending ? 'Removing\u2026' : 'Remove Domain'}
              </button>
            </div>

            {(setDomainMutation.isError || deleteDomainMutation.isError || refreshDomainMutation.isError) && (
              <div style={{ marginTop: 10, fontSize: 12, color: 'var(--accent-red, #ef4444)' }}>
                {(setDomainMutation.error || deleteDomainMutation.error || refreshDomainMutation.error)?.message}
              </div>
            )}
          </>
        ) : (
          <>
            {/* Set new domain */}
            <div style={{ marginBottom: 14 }}>
              <label style={labelStyle}>Domain</label>
              <input
                type="text"
                value={domainInput}
                onChange={e => setDomainInput(e.target.value)}
                placeholder="acme.dev"
                className="input"
              />
            </div>
            <button
              className="btn-primary"
              onClick={() => setDomainMutation.mutate(domainInput)}
              disabled={setDomainMutation.isPending || !domainInput.trim()}
            >
              {setDomainMutation.isPending ? 'Setting up\u2026' : 'Set Domain'}
            </button>
            {setDomainMutation.isError && (
              <div style={{ marginTop: 10, fontSize: 12, color: 'var(--accent-red, #ef4444)' }}>
                {setDomainMutation.error?.message}
              </div>
            )}
          </>
        )}
      </div>
    </div>
  )
}
