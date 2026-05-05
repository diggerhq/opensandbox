import { useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  getBilling, billingSetup, billingPortal, getBillingInvoices, redeemPromoCode,
  listOrgAgentSubscriptions, listAgents, cancelAgentFeature,
  type StripeInvoice, type OrgAgentSubscription, type Agent,
} from '../api/client'

type Tab = 'sandboxes' | 'agents' | 'invoices'

export default function Billing() {
  const [tab, setTab] = useState<Tab>('sandboxes')

  return (
    <div>
      <div style={{ marginBottom: 24 }}>
        <h1 className="page-title">Billing</h1>
        <p className="page-subtitle">Manage your plan, per-agent subscriptions, and invoices</p>
      </div>

      {/* Tabs */}
      <div style={{ display: 'flex', borderBottom: '1px solid var(--border-subtle)', marginBottom: 18, gap: 4 }}>
        {(['sandboxes', 'agents', 'invoices'] as Tab[]).map((t) => (
          <button
            key={t}
            onClick={() => setTab(t)}
            style={{
              background: 'none',
              border: 'none',
              borderBottom: tab === t ? '2px solid var(--accent-indigo)' : '2px solid transparent',
              padding: '10px 14px',
              color: tab === t ? 'var(--text-primary)' : 'var(--text-secondary)',
              fontSize: 13,
              fontFamily: 'var(--font-body)',
              fontWeight: tab === t ? 600 : 400,
              cursor: 'pointer',
              textTransform: 'capitalize',
            }}
          >
            {t}
          </button>
        ))}
      </div>

      {tab === 'sandboxes' && <PlanTab />}
      {tab === 'agents' && <AgentsTab />}
      {tab === 'invoices' && <InvoicesTab />}
    </div>
  )
}

// ───────────── Plan & Usage tab ─────────────

function PlanTab() {
  const queryClient = useQueryClient()
  const { data: billing, isLoading } = useQuery({
    queryKey: ['billing'], queryFn: getBilling, refetchInterval: 30_000,
  })

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

  if (isLoading) {
    return (
      <div style={{ display: 'flex', justifyContent: 'center', padding: 80 }}>
        <div className="loading-spinner" />
      </div>
    )
  }

  return (
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
            Usage & Payment Method
          </span>
          <p style={{ fontSize: 13, color: 'var(--text-secondary)', marginBottom: 14, lineHeight: 1.5 }}>
            View your current-cycle usage, manage your payment method, and download invoices on Stripe.
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
    </>
  )
}

// ───────────── Agents tab ─────────────

function AgentsTab() {
  const queryClient = useQueryClient()
  const { data: subsData, isLoading: subsLoading } = useQuery({
    queryKey: ['org-agent-subscriptions'],
    queryFn: listOrgAgentSubscriptions,
    refetchInterval: 30_000,
  })
  const { data: agentsData } = useQuery({
    queryKey: ['agents'], queryFn: listAgents,
  })

  const cancelMutation = useMutation({
    mutationFn: ({ agentId, feature }: { agentId: string; feature: string }) =>
      cancelAgentFeature(agentId, feature),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['org-agent-subscriptions'] })
    },
  })

  // Index agents by ID so we can show display names instead of raw IDs.
  const agentsById = useMemo(() => {
    const map: Record<string, Agent> = {}
    for (const a of agentsData?.agents ?? []) map[a.id] = a
    return map
  }, [agentsData])

  // De-duplicate to one row per (agent_id, feature). We sort by created_at
  // desc so the freshest row wins — covers cases where a user canceled then
  // re-subscribed.
  const rows = useMemo(() => {
    const seen = new Set<string>()
    const sorted = [...(subsData?.subscriptions ?? [])].sort((a, b) =>
      b.created_at.localeCompare(a.created_at),
    )
    const out: OrgAgentSubscription[] = []
    for (const sub of sorted) {
      const key = `${sub.agent_id}::${sub.feature}`
      if (seen.has(key)) continue
      seen.add(key)
      out.push(sub)
    }
    return out
  }, [subsData])

  const activeRows = rows.filter(r => r.active)
  const inactiveRows = rows.filter(r => !r.active)

  const totalMonthlyCents = activeRows.reduce((sum, r) => sum + r.price_monthly_cents, 0)

  if (subsLoading) {
    return (
      <div style={{ display: 'flex', justifyContent: 'center', padding: 80 }}>
        <div className="loading-spinner" />
      </div>
    )
  }

  return (
    <>
      {/* Summary card */}
      <div className="glass-card animate-in stagger-1" style={{ padding: 28, marginBottom: 14 }}>
        <span className="section-title" style={{ marginBottom: 12, display: 'block' }}>
          Per-agent features
        </span>
        <div style={{ display: 'flex', alignItems: 'baseline', gap: 12, marginBottom: 6 }}>
          <span className="metric-value" style={{ fontSize: 32, fontWeight: 700 }}>
            ${(totalMonthlyCents / 100).toFixed(2)}
          </span>
          <span style={{ fontSize: 12, color: 'var(--text-tertiary)' }}>
            /mo across {activeRows.length} active subscription{activeRows.length === 1 ? '' : 's'}
          </span>
        </div>
        <p style={{ fontSize: 13, color: 'var(--text-secondary)', lineHeight: 1.5, marginTop: 6 }}>
          Each connected channel (e.g. Telegram) is billed per-agent. Subscribe or cancel from each
          agent's page.
        </p>
      </div>

      {/* Active subscriptions */}
      <div className="glass-card animate-in stagger-2" style={{ padding: '22px 24px', marginBottom: 14 }}>
        <span className="section-title" style={{ marginBottom: 14, display: 'block' }}>
          Active
        </span>
        {activeRows.length === 0 ? (
          <div style={{ textAlign: 'center', padding: '32px 20px', color: 'var(--text-tertiary)', fontSize: 13 }}>
            No active per-agent subscriptions yet.
          </div>
        ) : (
          <table className="data-table">
            <thead>
              <tr>
                <th>Agent</th>
                <th>Feature</th>
                <th>Status</th>
                <th>Renews</th>
                <th style={{ textAlign: 'right' }}>Price</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {activeRows.map((sub) => {
                const agent = agentsById[sub.agent_id]
                const label = agent?.display_name || sub.agent_id
                const renewLabel = sub.cancel_at_period_end
                  ? `Ends ${formatDate(sub.current_period_end)}`
                  : sub.current_period_end
                    ? formatDate(sub.current_period_end)
                    : '—'
                return (
                  <tr key={sub.stripe_subscription_id}>
                    <td>
                      <Link to={`/agents/${encodeURIComponent(sub.agent_id)}`}
                        style={{ color: 'var(--accent-indigo)', textDecoration: 'none', fontWeight: 600 }}>
                        {label}
                      </Link>
                      {agent && (
                        <div style={{ fontSize: 11, color: 'var(--text-tertiary)', fontFamily: 'var(--font-mono)' }}>
                          {sub.agent_id}
                        </div>
                      )}
                    </td>
                    <td style={{ textTransform: 'capitalize', fontSize: 13 }}>{sub.feature}</td>
                    <td><SubscriptionStatus status={sub.status} cancelAtPeriodEnd={sub.cancel_at_period_end} /></td>
                    <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--text-secondary)' }}>
                      {renewLabel}
                    </td>
                    <td style={{ fontFamily: 'var(--font-mono)', fontSize: 13, fontWeight: 600, textAlign: 'right' }}>
                      ${(sub.price_monthly_cents / 100).toFixed(2)}/mo
                    </td>
                    <td style={{ textAlign: 'right' }}>
                      {!sub.cancel_at_period_end && (
                        <button
                          onClick={() => {
                            if (confirm(`Cancel ${sub.feature} for ${label} at end of billing period?`)) {
                              cancelMutation.mutate({ agentId: sub.agent_id, feature: sub.feature })
                            }
                          }}
                          disabled={cancelMutation.isPending}
                          style={{
                            background: 'none',
                            border: '1px solid var(--border-subtle)',
                            color: 'var(--text-secondary)',
                            fontSize: 12,
                            padding: '4px 10px',
                            borderRadius: 'var(--radius-sm)',
                            cursor: 'pointer',
                            fontFamily: 'var(--font-body)',
                          }}
                        >
                          Cancel
                        </button>
                      )}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
      </div>

      {/* Past subscriptions (collapsed-feeling) */}
      {inactiveRows.length > 0 && (
        <div className="glass-card animate-in stagger-3" style={{ padding: '22px 24px' }}>
          <span className="section-title" style={{ marginBottom: 14, display: 'block' }}>
            Past
          </span>
          <table className="data-table">
            <thead>
              <tr>
                <th>Agent</th>
                <th>Feature</th>
                <th>Status</th>
                <th>Ended</th>
              </tr>
            </thead>
            <tbody>
              {inactiveRows.map((sub) => {
                const agent = agentsById[sub.agent_id]
                const label = agent?.display_name || sub.agent_id
                return (
                  <tr key={sub.stripe_subscription_id}>
                    <td>
                      <Link to={`/agents/${encodeURIComponent(sub.agent_id)}`}
                        style={{ color: 'var(--text-secondary)', textDecoration: 'none' }}>
                        {label}
                      </Link>
                    </td>
                    <td style={{ textTransform: 'capitalize', fontSize: 13 }}>{sub.feature}</td>
                    <td><SubscriptionStatus status={sub.status} cancelAtPeriodEnd={false} /></td>
                    <td style={{ fontFamily: 'var(--font-mono)', fontSize: 12, color: 'var(--text-tertiary)' }}>
                      {formatDate(sub.canceled_at || sub.current_period_end)}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}
    </>
  )
}

// ───────────── Invoices tab ─────────────

function InvoicesTab() {
  const { data: billing } = useQuery({ queryKey: ['billing'], queryFn: getBilling })
  const { data: invoiceData, isLoading } = useQuery({
    queryKey: ['invoices'], queryFn: () => getBillingInvoices(),
  })

  const isPro = billing?.plan === 'pro'

  if (isLoading) {
    return (
      <div style={{ display: 'flex', justifyContent: 'center', padding: 80 }}>
        <div className="loading-spinner" />
      </div>
    )
  }

  if (!isPro) {
    return (
      <div className="glass-card" style={{ padding: 28, textAlign: 'center' }}>
        <p style={{ fontSize: 14, color: 'var(--text-secondary)' }}>
          Invoices appear here once you're on the Pro plan or have a per-agent subscription.
        </p>
      </div>
    )
  }

  return (
    <div className="glass-card animate-in stagger-1" style={{ padding: '22px 24px' }}>
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
  )
}

// ───────────── Helpers ─────────────

function formatDate(s?: string | null) {
  if (!s) return '—'
  const d = new Date(s)
  if (isNaN(d.getTime())) return '—'
  return d.toLocaleDateString()
}

function SubscriptionStatus({ status, cancelAtPeriodEnd }: { status: string; cancelAtPeriodEnd: boolean }) {
  let color = 'var(--text-tertiary)'
  let bg = 'rgba(255,255,255,0.04)'
  let label = status

  if (cancelAtPeriodEnd) {
    color = 'var(--accent-amber, #f59e0b)'
    bg = 'rgba(245,158,11,0.1)'
    label = 'cancels'
  } else if (status === 'active' || status === 'trialing') {
    color = 'var(--accent-emerald)'; bg = 'rgba(52,211,153,0.1)'
  } else if (status === 'past_due' || status === 'incomplete') {
    color = 'var(--accent-amber, #f59e0b)'; bg = 'rgba(245,158,11,0.1)'
  } else if (status === 'canceled' || status === 'unpaid' || status === 'incomplete_expired') {
    color = 'var(--accent-rose)'; bg = 'rgba(244,63,94,0.1)'
  }
  return (
    <span style={{
      display: 'inline-block', padding: '2px 8px', borderRadius: 4,
      fontSize: 11, fontWeight: 600, color, background: bg,
      textTransform: 'uppercase', letterSpacing: '0.5px',
    }}>{label}</span>
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
