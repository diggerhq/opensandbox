import { useState } from 'react'
import { NavLink, Outlet } from 'react-router-dom'
import { useAuth } from '../hooks/useAuth'
import { logout } from '../api/client'

/* ── SVG Icons (inline, no extra deps) ────────────────────── */
const icons = {
  grid: (
    <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round">
      <rect x="3" y="3" width="7" height="7" rx="1.5" />
      <rect x="14" y="3" width="7" height="7" rx="1.5" />
      <rect x="3" y="14" width="7" height="7" rx="1.5" />
      <rect x="14" y="14" width="7" height="7" rx="1.5" />
    </svg>
  ),
  clock: (
    <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round">
      <circle cx="12" cy="12" r="9" />
      <polyline points="12 7 12 12 15.5 14.5" />
    </svg>
  ),
  key: (
    <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round">
      <path d="M21 2l-2 2m-7.61 7.61a5.5 5.5 0 1 1-7.778 7.778 5.5 5.5 0 0 1 7.777-7.777zm0 0L15.5 7.5m0 0l3 3L22 7l-3-3m-3.5 3.5L19 4" />
    </svg>
  ),
  layers: (
    <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round">
      <polygon points="12 2 2 7 12 12 22 7 12 2" />
      <polyline points="2 17 12 22 22 17" />
      <polyline points="2 12 12 17 22 12" />
    </svg>
  ),
  box: (
    <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round">
      <path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z" />
      <polyline points="3.27 6.96 12 12.01 20.73 6.96" />
      <line x1="12" y1="22.08" x2="12" y2="12" />
    </svg>
  ),
  creditCard: (
    <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round">
      <rect x="1" y="4" width="22" height="16" rx="2" ry="2" />
      <line x1="1" y1="10" x2="23" y2="10" />
    </svg>
  ),
  bot: (
    <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round">
      <rect x="4" y="7" width="16" height="12" rx="2" />
      <path d="M12 7V3" />
      <circle cx="12" cy="3" r="1" />
      <circle cx="9" cy="13" r="1" fill="currentColor" />
      <circle cx="15" cy="13" r="1" fill="currentColor" />
      <path d="M9 17h6" />
    </svg>
  ),
  gear: (
    <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round">
      <circle cx="12" cy="12" r="3" />
      <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06A1.65 1.65 0 0 0 4.68 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06A1.65 1.65 0 0 0 9 4.68a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06A1.65 1.65 0 0 0 19.4 9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z" />
    </svg>
  ),
  logout: (
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round">
      <path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4" />
      <polyline points="16 17 21 12 16 7" />
      <line x1="21" y1="12" x2="9" y2="12" />
    </svg>
  ),
}

const navItems = [
  { to: '/', label: 'Dashboard', end: true, icon: icons.grid },
  { to: '/sessions', label: 'Sessions', icon: icons.clock },
  { to: '/agents', label: 'Agents', icon: icons.bot },
  { to: '/checkpoints', label: 'Checkpoints', icon: icons.layers },
  { to: '/templates', label: 'Templates', icon: icons.box },
  { to: '/api-keys', label: 'API Keys', icon: icons.key },
  { to: '/billing', label: 'Billing', icon: icons.creditCard },
  { to: '/settings', label: 'Settings', icon: icons.gear },
]

export default function Layout() {
  const { user, switchOrg } = useAuth()
  const [orgSwitcherOpen, setOrgSwitcherOpen] = useState(false)
  const hasMultipleOrgs = (user?.orgs?.length ?? 0) > 1
  const activeOrg = user?.orgs?.find(o => o.isActive)

  return (
    <div style={{ display: 'flex', minHeight: '100vh' }}>
      {/* ── Sidebar ── */}
      <nav style={{
        width: 232,
        background: 'var(--bg-deep)',
        borderRight: '1px solid var(--border-subtle)',
        display: 'flex',
        flexDirection: 'column',
        position: 'fixed',
        top: 0,
        left: 0,
        bottom: 0,
        zIndex: 50,
      }}>
        {/* Brand */}
        <div style={{ padding: '22px 20px 18px', borderBottom: '1px solid var(--border-subtle)' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
            <div style={{
              width: 30, height: 30, borderRadius: 8,
              background: 'var(--gradient-primary)',
              display: 'flex', alignItems: 'center', justifyContent: 'center',
              fontSize: 12, fontWeight: 800, color: '#fff',
              fontFamily: 'var(--font-display)',
              boxShadow: '0 0 16px rgba(99,102,241,0.25)',
            }}>OS</div>
            <div>
              <div style={{
                fontSize: 14, fontWeight: 700,
                fontFamily: 'var(--font-display)',
                color: 'var(--text-primary)',
                letterSpacing: '-0.02em',
              }}>OpenComputer</div>
              <div style={{
                fontSize: 9, color: 'var(--text-tertiary)',
                fontFamily: 'var(--font-mono)',
                letterSpacing: '0.06em',
                textTransform: 'uppercase',
              }}>Console</div>
            </div>
          </div>
        </div>

        {/* Org Switcher (only visible with multiple orgs) */}
        {hasMultipleOrgs && (
          <div style={{ padding: '10px 12px', borderBottom: '1px solid var(--border-subtle)', position: 'relative' }}>
            <button
              onClick={() => setOrgSwitcherOpen(!orgSwitcherOpen)}
              style={{
                width: '100%', display: 'flex', alignItems: 'center', justifyContent: 'space-between',
                padding: '8px 10px', background: 'rgba(255,255,255,0.03)',
                border: '1px solid var(--border-subtle)', borderRadius: 6,
                cursor: 'pointer', color: 'var(--text-primary)', fontSize: 12,
                fontFamily: 'var(--font-body)',
              }}
            >
              <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                {activeOrg?.name || 'Select org'}
              </span>
              <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
                <polyline points="6 9 12 15 18 9" />
              </svg>
            </button>
            {orgSwitcherOpen && (
              <div style={{
                position: 'absolute', top: '100%', left: 12, right: 12,
                background: 'var(--bg-deep)', border: '1px solid var(--border-subtle)',
                borderRadius: 6, marginTop: 4, zIndex: 100,
                boxShadow: '0 4px 16px rgba(0,0,0,0.3)',
              }}>
                {user?.orgs?.map(org => (
                  <button
                    key={org.id}
                    onClick={() => {
                      switchOrg(org.id)
                      setOrgSwitcherOpen(false)
                    }}
                    style={{
                      display: 'block', width: '100%', textAlign: 'left',
                      padding: '8px 12px', background: org.isActive ? 'rgba(99,102,241,0.1)' : 'none',
                      border: 'none', cursor: 'pointer',
                      color: org.isActive ? 'var(--text-primary)' : 'var(--text-secondary)',
                      fontSize: 12, fontFamily: 'var(--font-body)',
                      borderBottom: '1px solid var(--border-subtle)',
                    }}
                    onMouseOver={e => { if (!org.isActive) e.currentTarget.style.background = 'rgba(255,255,255,0.03)' }}
                    onMouseOut={e => { if (!org.isActive) e.currentTarget.style.background = 'none' }}
                  >
                    {org.name}
                    {org.isPersonal && <span style={{ fontSize: 10, color: 'var(--text-tertiary)', marginLeft: 6 }}>(personal)</span>}
                  </button>
                ))}
              </div>
            )}
          </div>
        )}

        {/* Nav Links */}
        <div style={{ flex: 1, padding: '10px 8px' }}>
          {navItems.map(item => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.end}
              style={({ isActive }) => ({
                display: 'flex',
                alignItems: 'center',
                gap: 10,
                padding: '9px 14px',
                color: isActive ? 'var(--text-primary)' : 'var(--text-secondary)',
                background: isActive ? 'rgba(99, 102, 241, 0.07)' : 'transparent',
                textDecoration: 'none',
                fontSize: 13,
                fontWeight: isActive ? 600 : 400,
                fontFamily: 'var(--font-body)',
                borderRadius: 8,
                marginBottom: 2,
                transition: 'all 0.15s ease',
                borderLeft: isActive
                  ? '2px solid var(--accent-indigo)'
                  : '2px solid transparent',
              })}
            >
              <span style={{ display: 'flex', alignItems: 'center', opacity: 0.6 }}>{item.icon}</span>
              {item.label}
            </NavLink>
          ))}
        </div>

        {/* User */}
        <div style={{
          padding: '14px 16px',
          borderTop: '1px solid var(--border-subtle)',
        }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 10, marginBottom: 10 }}>
            <div style={{
              width: 28, height: 28, borderRadius: '50%',
              background: 'var(--gradient-primary)',
              display: 'flex', alignItems: 'center', justifyContent: 'center',
              fontSize: 11, fontWeight: 700, color: '#fff',
              flexShrink: 0,
            }}>
              {user?.email?.charAt(0).toUpperCase() || '?'}
            </div>
            <div style={{
              fontSize: 12, color: 'var(--text-secondary)',
              overflow: 'hidden', textOverflow: 'ellipsis',
              whiteSpace: 'nowrap', flex: 1,
            }}>{user?.email}</div>
          </div>
          <button
            onClick={() => logout()}
            style={{
              display: 'flex', alignItems: 'center', gap: 6,
              background: 'none', border: 'none',
              color: 'var(--text-tertiary)',
              cursor: 'pointer', fontSize: 12,
              fontFamily: 'var(--font-body)',
              padding: 0,
              transition: 'color 0.15s ease',
            }}
            onMouseOver={e => (e.currentTarget.style.color = 'var(--accent-rose)')}
            onMouseOut={e => (e.currentTarget.style.color = 'var(--text-tertiary)')}
          >
            {icons.logout}
            Sign out
          </button>
        </div>
      </nav>

      {/* ── Main Content ── */}
      <main style={{
        flex: 1,
        marginLeft: 232,
        padding: '32px 36px',
        minHeight: '100vh',
        background: 'radial-gradient(ellipse 80% 50% at 50% -10%, rgba(99,102,241,0.04) 0%, transparent 70%)',
      }}>
        <Outlet />
      </main>
    </div>
  )
}
