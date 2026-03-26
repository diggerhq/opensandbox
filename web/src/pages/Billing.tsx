import { useState, useEffect } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  getCredits, getBillingSettings, updateBillingSettings,
  createCheckoutSession, setupPaymentMethod,
  getBillingUsage, getBillingTransactions,
  type UsageTier, type CreditTransaction,
} from '../api/client'

const PRICING_TIERS = [
  { memory: '4 GB', vcpus: 1, monthly: 2.80, perSec: 0.000001065 },
  { memory: '8 GB', vcpus: 2, monthly: 3.62, perSec: 0.000000380 },
  { memory: '16 GB', vcpus: 4, monthly: 7.29, perSec: 0.000001521 },
  { memory: '32 GB', vcpus: 8, monthly: 14.58, perSec: 0.000002282 },
  { memory: '64 GB', vcpus: 16, monthly: 29.17, perSec: 0.000004563 },
]

export default function Billing() {
  const queryClient = useQueryClient()

  const { data: credits, isLoading: loadingCredits } = useQuery({
    queryKey: ['credits'],
    queryFn: getCredits,
  })

  const { data: settings, isLoading: loadingSettings } = useQuery({
    queryKey: ['billing-settings'],
    queryFn: getBillingSettings,
  })

  const { data: usage, isLoading: loadingUsage } = useQuery({
    queryKey: ['billing-usage'],
    queryFn: () => getBillingUsage(),
  })

  const { data: txnData, isLoading: loadingTxns } = useQuery({
    queryKey: ['billing-transactions'],
    queryFn: () => getBillingTransactions(20, 0),
  })

  // Add credits modal
  const [showAddCredits, setShowAddCredits] = useState(false)
  const [creditAmount, setCreditAmount] = useState('10')

  // Auto top-up form
  const [autoTopup, setAutoTopup] = useState(false)
  const [threshold, setThreshold] = useState('5')
  const [topupAmount, setTopupAmount] = useState('50')
  const [spendCap, setSpendCap] = useState('')
  const [settingsSaved, setSettingsSaved] = useState(false)

  useEffect(() => {
    if (settings) {
      setAutoTopup(settings.autoTopupEnabled)
      setThreshold((settings.autoTopupThresholdCents / 100).toString())
      setTopupAmount((settings.autoTopupAmountCents / 100).toString())
      setSpendCap(settings.monthlySpendCapCents != null ? (settings.monthlySpendCapCents / 100).toString() : '')
    }
  }, [settings])

  const checkoutMutation = useMutation({
    mutationFn: (amountCents: number) => createCheckoutSession(amountCents),
    onSuccess: (data) => {
      window.location.href = data.url
    },
  })

  const setupMutation = useMutation({
    mutationFn: () => setupPaymentMethod(),
    onSuccess: (data) => {
      window.location.href = data.url
    },
  })

  const settingsMutation = useMutation({
    mutationFn: () => updateBillingSettings({
      autoTopupEnabled: autoTopup,
      autoTopupThresholdCents: Math.round(parseFloat(threshold || '0') * 100),
      autoTopupAmountCents: Math.round(parseFloat(topupAmount || '0') * 100),
      monthlySpendCapCents: spendCap ? Math.round(parseFloat(spendCap) * 100) : null,
    }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['billing-settings'] })
      setSettingsSaved(true)
      setTimeout(() => setSettingsSaved(false), 2000)
    },
  })

  const balanceDollars = (credits?.balanceCents ?? 0) / 100
  const unbilledDollars = (credits?.unbilledUsageCents ?? 0) / 100

  return (
    <div>
      {/* Header */}
      <div style={{ marginBottom: 32 }}>
        <h1 className="page-title">Billing</h1>
        <p className="page-subtitle">Manage credits, usage, and payment settings</p>
      </div>

      {/* ── Credit Balance + Pricing ── */}
      <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 14, marginBottom: 14 }}>

        {/* Credit Balance */}
        <div className="glass-card animate-in stagger-1" style={{ padding: '28px' }}>
          <span className="section-title" style={{ marginBottom: 16, display: 'block' }}>Credit Balance</span>
          {loadingCredits ? (
            <div style={{ display: 'flex', justifyContent: 'center', padding: 40 }}>
              <div className="loading-spinner" />
            </div>
          ) : (
            <>
              <div className="metric-value" style={{
                fontSize: 42, fontWeight: 700, lineHeight: 1,
                color: (credits?.balanceCents ?? 0) > 0 ? 'var(--accent-emerald)' : 'var(--accent-rose)',
                marginBottom: 8,
              }}>
                ${balanceDollars.toFixed(2)}
              </div>
              {unbilledDollars > 0 && (
                <div style={{
                  fontSize: 12, color: 'var(--text-tertiary)',
                  fontFamily: 'var(--font-mono)', marginBottom: 20,
                }}>
                  {unbilledDollars.toFixed(4)} unbilled
                </div>
              )}
              {!unbilledDollars && <div style={{ marginBottom: 20 }} />}

              <div style={{ display: 'flex', gap: 10 }}>
                <button
                  className="btn-primary"
                  onClick={() => setShowAddCredits(true)}
                  style={{
                    padding: '8px 18px', fontSize: 13, fontWeight: 600,
                    fontFamily: 'var(--font-body)', cursor: 'pointer',
                    border: 'none', borderRadius: 'var(--radius-sm)',
                    background: 'var(--accent-indigo)', color: '#fff',
                  }}
                >
                  Add Credits
                </button>
                <button
                  onClick={() => setupMutation.mutate()}
                  disabled={setupMutation.isPending}
                  style={{
                    padding: '8px 18px', fontSize: 13, fontWeight: 500,
                    fontFamily: 'var(--font-body)', cursor: 'pointer',
                    border: '1px solid var(--border-subtle)', borderRadius: 'var(--radius-sm)',
                    background: 'rgba(255,255,255,0.03)', color: 'var(--text-secondary)',
                  }}
                >
                  {setupMutation.isPending ? 'Redirecting...' : credits?.hasPaymentMethod ? 'Update Card' : 'Add Card'}
                </button>
              </div>

              {credits?.hasPaymentMethod && (
                <div style={{
                  marginTop: 12, fontSize: 11, color: 'var(--accent-emerald)',
                  display: 'flex', alignItems: 'center', gap: 6,
                }}>
                  <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round">
                    <polyline points="20 6 9 17 4 12" />
                  </svg>
                  Payment method on file
                </div>
              )}
            </>
          )}
        </div>

        {/* Current Usage */}
        <div className="glass-card animate-in stagger-2" style={{ padding: '28px' }}>
          <span className="section-title" style={{ marginBottom: 16, display: 'block' }}>Current Month Usage</span>
          {loadingUsage ? (
            <div style={{ display: 'flex', justifyContent: 'center', padding: 40 }}>
              <div className="loading-spinner" />
            </div>
          ) : !usage?.tiers?.length ? (
            <div style={{
              textAlign: 'center', padding: '40px 20px',
              color: 'var(--text-tertiary)', fontSize: 13,
            }}>
              No usage this month
            </div>
          ) : (
            <>
              <div style={{ display: 'flex', flexDirection: 'column', gap: 8, marginBottom: 16 }}>
                {usage.tiers.map((tier: UsageTier) => (
                  <div key={tier.memoryMB} style={{
                    display: 'flex', justifyContent: 'space-between', alignItems: 'center',
                    padding: '8px 12px', borderRadius: 'var(--radius-sm)',
                    background: 'rgba(255,255,255,0.02)',
                    border: '1px solid rgba(255,255,255,0.03)',
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
                  ${(usage.totalCostCents / 100).toFixed(4)}
                </span>
              </div>
            </>
          )}
        </div>
      </div>

      {/* ── Pricing Table ── */}
      <div className="glass-card animate-in stagger-3" style={{ padding: '22px 24px', marginBottom: 14 }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 14 }}>
          <span className="section-title" style={{ marginBottom: 0 }}>Pricing</span>
          <span style={{
            fontSize: 11, color: 'var(--accent-emerald)',
            fontFamily: 'var(--font-mono)',
            display: 'flex', alignItems: 'center', gap: 6,
          }}>
            <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
              <path d="M18.36 6.64a9 9 0 1 1-12.73 0" />
              <line x1="12" y1="2" x2="12" y2="12" />
            </svg>
            Hibernated sandboxes are not charged
          </span>
        </div>
        <div style={{ overflow: 'hidden' }}>
          <table className="data-table">
            <thead>
              <tr>
                <th>Memory</th>
                <th>vCPUs</th>
                <th>Per Month</th>
                <th>Per Second</th>
              </tr>
            </thead>
            <tbody>
              {PRICING_TIERS.map(tier => (
                <tr key={tier.memory}>
                  <td style={{ fontWeight: 600, color: 'var(--text-primary)' }}>{tier.memory}</td>
                  <td>{tier.vcpus}</td>
                  <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>
                    ${tier.monthly.toFixed(2)}
                  </td>
                  <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--accent-cyan)' }}>
                    ${tier.perSec.toFixed(9)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>

      {/* ── Auto Top-up Settings ── */}
      <div className="glass-card animate-in stagger-4" style={{ padding: '22px 24px', marginBottom: 14 }}>
        <span className="section-title" style={{ marginBottom: 16, display: 'block' }}>Auto Top-up</span>
        {loadingSettings ? (
          <div style={{ display: 'flex', justifyContent: 'center', padding: 40 }}>
            <div className="loading-spinner" />
          </div>
        ) : (
          <>
            {/* Toggle */}
            <div style={{
              display: 'flex', alignItems: 'center', gap: 12, marginBottom: 20,
            }}>
              <button
                onClick={() => setAutoTopup(!autoTopup)}
                style={{
                  width: 44, height: 24, borderRadius: 12, border: 'none', cursor: 'pointer',
                  background: autoTopup ? 'var(--accent-indigo)' : 'rgba(255,255,255,0.1)',
                  position: 'relative', transition: 'background 0.2s ease',
                  flexShrink: 0,
                }}
              >
                <div style={{
                  width: 18, height: 18, borderRadius: '50%', background: '#fff',
                  position: 'absolute', top: 3,
                  left: autoTopup ? 23 : 3,
                  transition: 'left 0.2s ease',
                }} />
              </button>
              <div>
                <div style={{ fontSize: 13, fontWeight: 500, color: 'var(--text-primary)' }}>
                  Automatically top up when balance is low
                </div>
                <div style={{ fontSize: 11, color: 'var(--text-tertiary)', marginTop: 2 }}>
                  Charges your saved payment method
                </div>
              </div>
            </div>

            {autoTopup && (
              <div style={{
                display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 14, marginBottom: 20,
                padding: '16px', borderRadius: 'var(--radius-md)',
                background: 'rgba(255,255,255,0.02)',
                border: '1px solid rgba(255,255,255,0.04)',
              }}>
                <div>
                  <label style={{ fontSize: 11, color: 'var(--text-tertiary)', display: 'block', marginBottom: 6 }}>
                    When balance drops below
                  </label>
                  <div style={{ position: 'relative' }}>
                    <span style={{
                      position: 'absolute', left: 10, top: '50%', transform: 'translateY(-50%)',
                      color: 'var(--text-tertiary)', fontSize: 13,
                    }}>$</span>
                    <input
                      className="input"
                      type="number"
                      min="1"
                      value={threshold}
                      onChange={e => setThreshold(e.target.value)}
                      style={{
                        width: '100%', paddingLeft: 22, boxSizing: 'border-box',
                        fontFamily: 'var(--font-mono)', fontSize: 13,
                      }}
                    />
                  </div>
                </div>
                <div>
                  <label style={{ fontSize: 11, color: 'var(--text-tertiary)', display: 'block', marginBottom: 6 }}>
                    Top up amount
                  </label>
                  <div style={{ position: 'relative' }}>
                    <span style={{
                      position: 'absolute', left: 10, top: '50%', transform: 'translateY(-50%)',
                      color: 'var(--text-tertiary)', fontSize: 13,
                    }}>$</span>
                    <input
                      className="input"
                      type="number"
                      min="5"
                      value={topupAmount}
                      onChange={e => setTopupAmount(e.target.value)}
                      style={{
                        width: '100%', paddingLeft: 22, boxSizing: 'border-box',
                        fontFamily: 'var(--font-mono)', fontSize: 13,
                      }}
                    />
                  </div>
                </div>
                <div>
                  <label style={{ fontSize: 11, color: 'var(--text-tertiary)', display: 'block', marginBottom: 6 }}>
                    Monthly cap (optional)
                  </label>
                  <div style={{ position: 'relative' }}>
                    <span style={{
                      position: 'absolute', left: 10, top: '50%', transform: 'translateY(-50%)',
                      color: 'var(--text-tertiary)', fontSize: 13,
                    }}>$</span>
                    <input
                      className="input"
                      type="number"
                      min="0"
                      placeholder="No limit"
                      value={spendCap}
                      onChange={e => setSpendCap(e.target.value)}
                      style={{
                        width: '100%', paddingLeft: 22, boxSizing: 'border-box',
                        fontFamily: 'var(--font-mono)', fontSize: 13,
                      }}
                    />
                  </div>
                </div>
              </div>
            )}

            <div style={{ display: 'flex', gap: 10, alignItems: 'center' }}>
              <button
                onClick={() => settingsMutation.mutate()}
                disabled={settingsMutation.isPending}
                style={{
                  padding: '8px 20px', fontSize: 13, fontWeight: 600,
                  fontFamily: 'var(--font-body)', cursor: 'pointer',
                  border: 'none', borderRadius: 'var(--radius-sm)',
                  background: 'var(--accent-indigo)', color: '#fff',
                  opacity: settingsMutation.isPending ? 0.6 : 1,
                }}
              >
                {settingsMutation.isPending ? 'Saving...' : 'Save Settings'}
              </button>
              {settingsSaved && (
                <span style={{ fontSize: 12, color: 'var(--accent-emerald)' }}>Saved</span>
              )}
              {settingsMutation.isError && (
                <span style={{ fontSize: 12, color: 'var(--accent-rose)' }}>
                  {(settingsMutation.error as Error).message}
                </span>
              )}
            </div>
          </>
        )}
      </div>

      {/* ── Transaction History ── */}
      <div className="glass-card animate-in stagger-5" style={{ padding: '22px 24px' }}>
        <span className="section-title" style={{ marginBottom: 14, display: 'block' }}>Transaction History</span>
        {loadingTxns ? (
          <div style={{ display: 'flex', justifyContent: 'center', padding: 40 }}>
            <div className="loading-spinner" />
          </div>
        ) : !txnData?.transactions?.length ? (
          <div style={{
            textAlign: 'center', padding: '40px 20px',
            color: 'var(--text-tertiary)', fontSize: 13,
          }}>
            No transactions yet
          </div>
        ) : (
          <div style={{ overflow: 'hidden' }}>
            <table className="data-table">
              <thead>
                <tr>
                  <th>Date</th>
                  <th>Type</th>
                  <th>Amount</th>
                  <th>Balance</th>
                  <th>Description</th>
                </tr>
              </thead>
              <tbody>
                {txnData.transactions.map((txn: CreditTransaction) => (
                  <tr key={txn.id}>
                    <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12 }}>
                      {new Date(txn.createdAt).toLocaleDateString()}
                    </td>
                    <td>
                      <TxnTypeBadge type={txn.type} />
                    </td>
                    <td style={{
                      fontFamily: 'var(--font-mono)', fontSize: 13, fontWeight: 600,
                      color: txn.amountCents >= 0 ? 'var(--accent-emerald)' : 'var(--accent-rose)',
                    }}>
                      {txn.amountCents >= 0 ? '+' : ''}{(txn.amountCents / 100).toFixed(2)}
                    </td>
                    <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--text-secondary)' }}>
                      ${(txn.balanceAfterCents / 100).toFixed(2)}
                    </td>
                    <td style={{ color: 'var(--text-tertiary)', fontSize: 12 }}>
                      {txn.description || '\u2014'}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      {/* ── Add Credits Modal ── */}
      {showAddCredits && (
        <div
          onClick={() => setShowAddCredits(false)}
          style={{
            position: 'fixed', inset: 0, zIndex: 1000,
            background: 'rgba(0,0,0,0.6)', backdropFilter: 'blur(4px)',
            display: 'flex', alignItems: 'center', justifyContent: 'center',
          }}
        >
          <div
            onClick={e => e.stopPropagation()}
            style={{
              background: 'var(--bg-deep)',
              border: '1px solid var(--border-subtle)',
              borderRadius: 'var(--radius-lg)',
              padding: 28, width: 380,
              boxShadow: '0 16px 48px rgba(0,0,0,0.4)',
            }}
          >
            <h3 style={{
              fontSize: 18, fontWeight: 700, fontFamily: 'var(--font-display)',
              color: 'var(--text-primary)', marginBottom: 4, marginTop: 0,
            }}>
              Add Credits
            </h3>
            <p style={{ fontSize: 12, color: 'var(--text-tertiary)', marginBottom: 20 }}>
              You'll be redirected to Stripe to complete the purchase.
            </p>

            <div style={{ display: 'flex', gap: 8, marginBottom: 16 }}>
              {[10, 25, 50, 100].map(amt => (
                <button
                  key={amt}
                  onClick={() => setCreditAmount(amt.toString())}
                  style={{
                    flex: 1, padding: '8px 0', fontSize: 13, fontWeight: 600,
                    fontFamily: 'var(--font-mono)', cursor: 'pointer',
                    borderRadius: 'var(--radius-sm)',
                    border: creditAmount === amt.toString()
                      ? '1px solid var(--accent-indigo)'
                      : '1px solid var(--border-subtle)',
                    background: creditAmount === amt.toString()
                      ? 'rgba(99,102,241,0.1)'
                      : 'rgba(255,255,255,0.02)',
                    color: creditAmount === amt.toString()
                      ? 'var(--accent-indigo)'
                      : 'var(--text-secondary)',
                  }}
                >
                  ${amt}
                </button>
              ))}
            </div>

            <label style={{ fontSize: 11, color: 'var(--text-tertiary)', display: 'block', marginBottom: 6 }}>
              Custom amount
            </label>
            <div style={{ position: 'relative', marginBottom: 20 }}>
              <span style={{
                position: 'absolute', left: 10, top: '50%', transform: 'translateY(-50%)',
                color: 'var(--text-tertiary)', fontSize: 14,
              }}>$</span>
              <input
                className="input"
                type="number"
                min="5"
                step="1"
                value={creditAmount}
                onChange={e => setCreditAmount(e.target.value)}
                style={{
                  width: '100%', paddingLeft: 22, boxSizing: 'border-box',
                  fontFamily: 'var(--font-mono)', fontSize: 14,
                }}
              />
            </div>

            <div style={{ display: 'flex', gap: 10 }}>
              <button
                onClick={() => {
                  const cents = Math.round(parseFloat(creditAmount) * 100)
                  if (cents >= 500) checkoutMutation.mutate(cents)
                }}
                disabled={checkoutMutation.isPending || parseFloat(creditAmount) < 5}
                style={{
                  flex: 1, padding: '10px 0', fontSize: 14, fontWeight: 600,
                  fontFamily: 'var(--font-body)', cursor: 'pointer',
                  border: 'none', borderRadius: 'var(--radius-sm)',
                  background: 'var(--accent-indigo)', color: '#fff',
                  opacity: (checkoutMutation.isPending || parseFloat(creditAmount) < 5) ? 0.5 : 1,
                }}
              >
                {checkoutMutation.isPending ? 'Redirecting...' : `Pay $${parseFloat(creditAmount || '0').toFixed(2)}`}
              </button>
              <button
                onClick={() => setShowAddCredits(false)}
                style={{
                  padding: '10px 20px', fontSize: 13,
                  fontFamily: 'var(--font-body)', cursor: 'pointer',
                  border: '1px solid var(--border-subtle)', borderRadius: 'var(--radius-sm)',
                  background: 'transparent', color: 'var(--text-secondary)',
                }}
              >
                Cancel
              </button>
            </div>

            {checkoutMutation.isError && (
              <p style={{ fontSize: 12, color: 'var(--accent-rose)', marginTop: 10, marginBottom: 0 }}>
                {(checkoutMutation.error as Error).message}
              </p>
            )}
          </div>
        </div>
      )}
    </div>
  )
}

/* ── Transaction Type Badge ─────────────────────────────── */
function TxnTypeBadge({ type }: { type: string }) {
  let color = 'var(--text-tertiary)'
  let bg = 'rgba(255,255,255,0.04)'
  let label = type

  switch (type) {
    case 'purchase':
      color = 'var(--accent-emerald)'
      bg = 'rgba(52,211,153,0.1)'
      label = 'Purchase'
      break
    case 'auto_topup':
      color = 'var(--accent-cyan)'
      bg = 'rgba(34,211,238,0.1)'
      label = 'Auto Top-up'
      break
    case 'usage_deduction':
      color = 'var(--accent-rose)'
      bg = 'rgba(244,63,94,0.1)'
      label = 'Usage'
      break
    case 'initial_grant':
      color = '#a78bfa'
      bg = 'rgba(167,139,250,0.1)'
      label = 'Welcome Credit'
      break
  }

  return (
    <span style={{
      display: 'inline-block', padding: '2px 8px', borderRadius: 4,
      fontSize: 11, fontWeight: 600, color, background: bg,
      textTransform: 'uppercase', letterSpacing: '0.5px',
    }}>
      {label}
    </span>
  )
}

/* ── Helpers ─────────────────────────────────────────────── */
function formatSeconds(totalSeconds: number): string {
  if (totalSeconds < 60) return `${Math.round(totalSeconds)}s`
  if (totalSeconds < 3600) return `${Math.round(totalSeconds / 60)}m`
  return `${(totalSeconds / 3600).toFixed(1)}h`
}
