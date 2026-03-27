import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  getBilling, billingSetup, updateBillingSettings, getBillingInvoices,
  type BillingTierUsage, type StripeInvoice,
} from '../api/client'

const PRICING_TIERS = [
  { memory: '4 GB', vcpus: 1, perSec: 0.00003240740741 },
  { memory: '8 GB', vcpus: 2, perSec: 0.000150462963 },
  { memory: '16 GB', vcpus: 4, perSec: 0.0008101851852 },
  { memory: '32 GB', vcpus: 8, perSec: 0.005787037037 },
  { memory: '64 GB', vcpus: 16, perSec: 0.0162037037 },
]

export default function Billing() {
  const queryClient = useQueryClient()
  const { data: billing, isLoading } = useQuery({ queryKey: ['billing'], queryFn: getBilling })
  const { data: invoiceData } = useQuery({ queryKey: ['invoices'], queryFn: () => getBillingInvoices() })

  const [spendCap, setSpendCap] = useState('')
  const [settingsSaved, setSettingsSaved] = useState(false)

  const setupMutation = useMutation({
    mutationFn: billingSetup,
    onSuccess: (data) => { window.location.href = data.url },
  })

  const settingsMutation = useMutation({
    mutationFn: () => updateBillingSettings({
      monthlySpendCapCents: spendCap ? Math.round(parseFloat(spendCap) * 100) : null,
    }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['billing'] })
      setSettingsSaved(true)
      setTimeout(() => setSettingsSaved(false), 2000)
    },
  })

  const isPro = billing?.plan === 'pro'

  return (
    <div>
      <div style={{ marginBottom: 32 }}>
        <h1 className="page-title">Billing</h1>
        <p className="page-subtitle">Manage your plan, usage, and payment settings</p>
      </div>

      {isLoading ? (
        <div style={{ display: 'flex', justifyContent: 'center', padding: 80 }}>
          <div className="loading-spinner" />
        </div>
      ) : (
        <>
          {/* Plan + Usage row */}
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 14, marginBottom: 14 }}>

            {/* Plan Card */}
            <div className="glass-card animate-in stagger-1" style={{ padding: 28 }}>
              <span className="section-title" style={{ marginBottom: 16, display: 'block' }}>
                Current Plan
              </span>
              <div style={{ display: 'flex', alignItems: 'baseline', gap: 12, marginBottom: 8 }}>
                <span className="metric-value" style={{
                  fontSize: 36, fontWeight: 700,
                  color: isPro ? 'var(--accent-indigo)' : 'var(--text-primary)',
                }}>
                  {isPro ? 'Pro' : 'Free'}
                </span>
                {!isPro && (
                  <span style={{ fontSize: 12, color: 'var(--text-tertiary)' }}>
                    1 sandbox, 4GB / 1 vCPU
                  </span>
                )}
              </div>

              {isPro && billing?.stripeCreditCents != null && billing.stripeCreditCents > 0 && (
                <div style={{ fontSize: 13, color: 'var(--accent-emerald)', marginBottom: 12 }}>
                  ${(billing.stripeCreditCents / 100).toFixed(2)} promotional credit remaining
                </div>
              )}

              {!isPro && (
                <div style={{ marginTop: 16 }}>
                  <button
                    onClick={() => setupMutation.mutate()}
                    disabled={setupMutation.isPending}
                    style={{
                      padding: '10px 24px', fontSize: 14, fontWeight: 600,
                      fontFamily: 'var(--font-body)', cursor: 'pointer',
                      border: 'none', borderRadius: 'var(--radius-sm)',
                      background: 'var(--accent-indigo)', color: '#fff',
                      opacity: setupMutation.isPending ? 0.6 : 1,
                    }}
                  >
                    {setupMutation.isPending ? 'Redirecting...' : 'Upgrade to Pro — $30 free credit'}
                  </button>
                  {setupMutation.isError && (
                    <p style={{ fontSize: 12, color: 'var(--accent-rose)', marginTop: 8 }}>
                      {(setupMutation.error as Error).message}
                    </p>
                  )}
                </div>
              )}

              {isPro && (
                <div style={{
                  fontSize: 11, color: 'var(--accent-emerald)', marginTop: 4,
                  display: 'flex', alignItems: 'center', gap: 6,
                }}>
                  <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5"><polyline points="20 6 9 17 4 12" /></svg>
                  Payment method on file — billed monthly via Stripe
                </div>
              )}
            </div>

            {/* Current Usage */}
            <div className="glass-card animate-in stagger-2" style={{ padding: 28 }}>
              <span className="section-title" style={{ marginBottom: 16, display: 'block' }}>
                Current Month Usage
              </span>
              {!billing?.currentUsage?.tiers?.length ? (
                <div style={{ textAlign: 'center', padding: '40px 20px', color: 'var(--text-tertiary)', fontSize: 13 }}>
                  No usage this month
                </div>
              ) : (
                <>
                  <div style={{ display: 'flex', flexDirection: 'column', gap: 8, marginBottom: 16 }}>
                    {billing.currentUsage.tiers.map((tier: BillingTierUsage) => (
                      <div key={tier.memoryMB} style={{
                        display: 'flex', justifyContent: 'space-between', alignItems: 'center',
                        padding: '8px 12px', borderRadius: 'var(--radius-sm)',
                        background: 'rgba(255,255,255,0.02)', border: '1px solid rgba(255,255,255,0.03)',
                      }}>
                        <div>
                          <span style={{ fontSize: 13, color: 'var(--text-primary)', fontWeight: 500 }}>
                            {tier.memoryMB / 1024} GB
                          </span>
                          <span style={{ fontSize: 11, color: 'var(--text-tertiary)', marginLeft: 8 }}>
                            {tier.vcpus} vCPU
                          </span>
                        </div>
                        <div style={{ textAlign: 'right' }}>
                          <span style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--text-primary)' }}>
                            ${(tier.costCents / 100).toFixed(4)}
                          </span>
                          <span style={{ fontSize: 10, color: 'var(--text-tertiary)', marginLeft: 8 }}>
                            {formatSeconds(tier.totalSeconds)}
                          </span>
                        </div>
                      </div>
                    ))}
                  </div>
                  <div style={{
                    display: 'flex', justifyContent: 'space-between', alignItems: 'center',
                    paddingTop: 12, borderTop: '1px solid var(--border-subtle)',
                  }}>
                    <span style={{ fontSize: 13, fontWeight: 600, color: 'var(--text-secondary)' }}>Total</span>
                    <span className="metric-value" style={{ fontSize: 18, color: 'var(--accent-cyan)' }}>
                      ${(billing.currentUsage.totalCostCents / 100).toFixed(4)}
                    </span>
                  </div>
                </>
              )}
            </div>
          </div>

          {/* Pricing Table */}
          <div className="glass-card animate-in stagger-3" style={{ padding: '22px 24px', marginBottom: 14 }}>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 14 }}>
              <span className="section-title" style={{ marginBottom: 0 }}>Pricing</span>
              <span style={{ fontSize: 11, color: 'var(--accent-emerald)', fontFamily: 'var(--font-mono)' }}>
                Hibernated sandboxes are not charged
              </span>
            </div>
            <table className="data-table">
              <thead>
                <tr><th>Memory</th><th>vCPUs</th><th>Per Second</th></tr>
              </thead>
              <tbody>
                {PRICING_TIERS.map(t => (
                  <tr key={t.memory}>
                    <td style={{ fontWeight: 600, color: 'var(--text-primary)' }}>{t.memory}</td>
                    <td>{t.vcpus}</td>
                    <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--accent-cyan)' }}>
                      ${t.perSec.toFixed(11)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          {/* Monthly Spend Cap (pro only) */}
          {isPro && (
            <div className="glass-card animate-in stagger-4" style={{ padding: '22px 24px', marginBottom: 14 }}>
              <span className="section-title" style={{ marginBottom: 16, display: 'block' }}>Spending Cap</span>
              <div style={{ display: 'flex', alignItems: 'end', gap: 14 }}>
                <div>
                  <label style={{ fontSize: 11, color: 'var(--text-tertiary)', display: 'block', marginBottom: 6 }}>
                    Max monthly spend (optional)
                  </label>
                  <div style={{ position: 'relative' }}>
                    <span style={{
                      position: 'absolute', left: 10, top: '50%', transform: 'translateY(-50%)',
                      color: 'var(--text-tertiary)', fontSize: 13,
                    }}>$</span>
                    <input
                      className="input"
                      type="number" min="0" placeholder="No limit"
                      value={spendCap}
                      onChange={e => setSpendCap(e.target.value)}
                      style={{ width: 200, paddingLeft: 22, boxSizing: 'border-box', fontFamily: 'var(--font-mono)', fontSize: 13 }}
                    />
                  </div>
                </div>
                <button
                  onClick={() => settingsMutation.mutate()}
                  disabled={settingsMutation.isPending}
                  style={{
                    padding: '8px 20px', fontSize: 13, fontWeight: 600,
                    fontFamily: 'var(--font-body)', cursor: 'pointer',
                    border: 'none', borderRadius: 'var(--radius-sm)',
                    background: 'var(--accent-indigo)', color: '#fff',
                  }}
                >
                  Save
                </button>
                {settingsSaved && <span style={{ fontSize: 12, color: 'var(--accent-emerald)' }}>Saved</span>}
              </div>
            </div>
          )}

          {/* Invoice History (pro only) */}
          {isPro && (
            <div className="glass-card animate-in stagger-5" style={{ padding: '22px 24px' }}>
              <span className="section-title" style={{ marginBottom: 14, display: 'block' }}>Invoices</span>
              {!invoiceData?.invoices?.length ? (
                <div style={{ textAlign: 'center', padding: '40px 20px', color: 'var(--text-tertiary)', fontSize: 13 }}>
                  No invoices yet — your first invoice will appear at the end of the billing period
                </div>
              ) : (
                <table className="data-table">
                  <thead>
                    <tr><th>Date</th><th>Number</th><th>Status</th><th>Amount</th><th></th></tr>
                  </thead>
                  <tbody>
                    {invoiceData.invoices.map((inv: StripeInvoice) => (
                      <tr key={inv.id}>
                        <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>
                          {new Date(inv.created * 1000).toLocaleDateString()}
                        </td>
                        <td style={{ fontSize: 12 }}>{inv.number}</td>
                        <td><InvoiceStatus status={inv.status} /></td>
                        <td style={{ fontFamily: 'var(--font-mono)', fontSize: 13, fontWeight: 600 }}>
                          ${(inv.amountDue / 100).toFixed(2)}
                        </td>
                        <td>
                          {inv.hostedUrl && (
                            <a href={inv.hostedUrl} target="_blank" rel="noreferrer"
                              style={{ fontSize: 12, color: 'var(--accent-indigo)', textDecoration: 'none' }}>
                              View
                            </a>
                          )}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </div>
          )}
        </>
      )}
    </div>
  )
}

function InvoiceStatus({ status }: { status: string }) {
  let color = 'var(--text-tertiary)'
  let bg = 'rgba(255,255,255,0.04)'
  if (status === 'paid') { color = 'var(--accent-emerald)'; bg = 'rgba(52,211,153,0.1)' }
  else if (status === 'open') { color = 'var(--accent-cyan)'; bg = 'rgba(34,211,238,0.1)' }
  else if (status === 'uncollectible') { color = 'var(--accent-rose)'; bg = 'rgba(244,63,94,0.1)' }
  return (
    <span style={{
      display: 'inline-block', padding: '2px 8px', borderRadius: 4,
      fontSize: 11, fontWeight: 600, color, background: bg,
      textTransform: 'uppercase', letterSpacing: '0.5px',
    }}>{status}</span>
  )
}

function formatSeconds(s: number): string {
  if (s < 60) return `${Math.round(s)}s`
  if (s < 3600) return `${Math.round(s / 60)}m`
  return `${(s / 3600).toFixed(1)}h`
}
