import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  getBilling, billingSetup, billingPortal, getBillingInvoices, redeemPromoCode,
  type StripeInvoice,
} from '../api/client'

export default function Billing() {
  const queryClient = useQueryClient()
  const { data: billing, isLoading } = useQuery({ queryKey: ['billing'], queryFn: getBilling, refetchInterval: 30_000 })
  const { data: invoiceData } = useQuery({ queryKey: ['invoices'], queryFn: () => getBillingInvoices() })

  const [promoCode, setPromoCode] = useState('')
  const [redeemSuccess, setRedeemSuccess] = useState('')

  const setupMutation = useMutation({
    mutationFn: billingSetup,
    onSuccess: (data) => { window.location.href = data.url },
  })

  const portalMutation = useMutation({
    mutationFn: billingPortal,
    onSuccess: (data) => { window.location.href = data.url },
  })

  const redeemMutation = useMutation({
    mutationFn: () => redeemPromoCode(promoCode),
    onSuccess: (data) => {
      queryClient.invalidateQueries({ queryKey: ['billing'] })
      setPromoCode('')
      setRedeemSuccess(`$${(data.creditAppliedCents / 100).toFixed(2)} credit applied!`)
      setTimeout(() => setRedeemSuccess(''), 4000)
    },
  })

  const isPro = billing?.plan === 'pro'

  return (
    <div>
      <div style={{ marginBottom: 32 }}>
        <h1 className="page-title">Billing</h1>
        <p className="page-subtitle">Manage your plan and payment settings</p>
      </div>

      {isLoading ? (
        <div style={{ display: 'flex', justifyContent: 'center', padding: 80 }}>
          <div className="loading-spinner" />
        </div>
      ) : (
        <>
          {/* Plan Card */}
          <div className="glass-card animate-in stagger-1" style={{ padding: 28, marginBottom: 14 }}>
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
              <span style={{ fontSize: 12, color: 'var(--text-tertiary)' }}>
                {isPro
                  ? `${billing?.maxConcurrentSandboxes ?? 5} concurrent sandboxes, all tiers`
                  : `${billing?.maxConcurrentSandboxes ?? 5} concurrent sandboxes, up to 4GB / 1 vCPU`}
              </span>
            </div>

            {isPro && billing?.stripeCreditCents != null && billing.stripeCreditCents > 0 && (
              <div style={{ fontSize: 13, color: 'var(--accent-emerald)', marginBottom: 12 }}>
                ${(billing.stripeCreditCents / 100).toFixed(2)} promotional credit remaining
              </div>
            )}

            {!isPro && billing != null && (
              <div style={{ marginTop: 12 }}>
                {billing.freeCreditsRemainingCents > 0 ? (
                  <div style={{
                    fontSize: 14, fontWeight: 600, fontFamily: 'var(--font-mono)',
                    color: 'var(--accent-emerald)', marginBottom: 12,
                  }}>
                    ${(billing.freeCreditsRemainingCents / 100).toFixed(2)} free trial credit remaining
                  </div>
                ) : (
                  <div style={{
                    fontSize: 13, color: 'var(--accent-rose)', marginBottom: 12,
                    display: 'flex', alignItems: 'center', gap: 6,
                  }}>
                    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
                      <circle cx="12" cy="12" r="10" /><line x1="12" y1="8" x2="12" y2="12" /><line x1="12" y1="16" x2="12.01" y2="16" />
                    </svg>
                    Free trial credits exhausted — upgrade to continue using sandboxes
                  </div>
                )}

                <div style={{ fontSize: 13, color: 'var(--text-secondary)', marginBottom: 10 }}>
                  Upgrade to Pro for an additional $30 free credit and larger machine sizes
                </div>
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
                  {setupMutation.isPending ? 'Redirecting...' : 'Upgrade to Pro'}
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

            <div style={{ fontSize: 12, color: 'var(--text-tertiary)', marginTop: 12 }}>
              Need more concurrency?{' '}
              <a href="https://cal.com/team/digger/opencomputer-founder-chat" target="_blank" rel="noreferrer"
                style={{ color: 'var(--accent-indigo)', textDecoration: 'none' }}>
                Talk to us
              </a>
            </div>
          </div>

          {/* Stripe Billing Portal CTA (pro only) */}
          {isPro && (
            <div className="glass-card animate-in stagger-2" style={{ padding: '22px 24px', marginBottom: 14 }}>
              <span className="section-title" style={{ marginBottom: 10, display: 'block' }}>
                Usage & Invoices
              </span>
              <p style={{ fontSize: 13, color: 'var(--text-secondary)', marginBottom: 14, lineHeight: 1.5 }}>
                View your current-cycle usage, invoices, and manage your payment method on Stripe.
              </p>
              <button
                onClick={() => portalMutation.mutate()}
                disabled={portalMutation.isPending}
                style={{
                  padding: '10px 20px', fontSize: 13, fontWeight: 600,
                  fontFamily: 'var(--font-body)', cursor: 'pointer',
                  border: '1px solid var(--border-subtle)', borderRadius: 'var(--radius-sm)',
                  background: 'rgba(255,255,255,0.02)', color: 'var(--text-primary)',
                  opacity: portalMutation.isPending ? 0.6 : 1,
                }}
              >
                {portalMutation.isPending ? 'Opening Stripe…' : 'Open Stripe billing portal ↗'}
              </button>
              {portalMutation.isError && (
                <p style={{ fontSize: 12, color: 'var(--accent-rose)', marginTop: 10 }}>
                  {(portalMutation.error as Error).message}
                </p>
              )}
            </div>
          )}

          {/* Redeem Promotion Code (pro only) */}
          {isPro && (
            <div className="glass-card animate-in stagger-3" style={{ padding: '22px 24px', marginBottom: 14 }}>
              <span className="section-title" style={{ marginBottom: 12, display: 'block' }}>Promotion Code</span>
              <div style={{ display: 'flex', alignItems: 'end', gap: 14 }}>
                <div>
                  <label style={{ fontSize: 11, color: 'var(--text-tertiary)', display: 'block', marginBottom: 6 }}>
                    Enter a promotion code to apply credit
                  </label>
                  <input
                    className="input"
                    type="text"
                    placeholder="e.g. WELCOME100"
                    value={promoCode}
                    onChange={e => setPromoCode(e.target.value.toUpperCase())}
                    style={{ width: 240, fontFamily: 'var(--font-mono)', fontSize: 13 }}
                  />
                </div>
                <button
                  onClick={() => redeemMutation.mutate()}
                  disabled={redeemMutation.isPending || !promoCode.trim()}
                  style={{
                    padding: '8px 20px', fontSize: 13, fontWeight: 600,
                    fontFamily: 'var(--font-body)', cursor: 'pointer',
                    border: 'none', borderRadius: 'var(--radius-sm)',
                    background: 'var(--accent-indigo)', color: '#fff',
                    opacity: redeemMutation.isPending || !promoCode.trim() ? 0.5 : 1,
                  }}
                >
                  {redeemMutation.isPending ? 'Applying...' : 'Redeem'}
                </button>
              </div>
              {redeemSuccess && (
                <p style={{ fontSize: 13, color: 'var(--accent-emerald)', marginTop: 10 }}>{redeemSuccess}</p>
              )}
              {redeemMutation.isError && (
                <p style={{ fontSize: 12, color: 'var(--accent-rose)', marginTop: 10 }}>
                  {(redeemMutation.error as Error).message}
                </p>
              )}
            </div>
          )}

          {/* Invoice History (pro only) */}
          {isPro && (
            <div className="glass-card animate-in stagger-4" style={{ padding: '22px 24px' }}>
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
