import { createFileRoute, Outlet, Link, useLocation } from '@tanstack/react-router'
import { useAuth } from '@workos-inc/authkit-react'
import { 
  LayoutDashboard, 
  Boxes, 
  Settings, 
  LogOut, 
  ChevronDown,
  Plus,
  Search,
  Key
} from 'lucide-react'
import { useState } from 'react'

export const Route = createFileRoute('/_authenticated')({
  beforeLoad: async () => {
    // This will be handled by the component with useAuth
  },
  component: AuthenticatedLayout,
})

function AuthenticatedLayout() {
  const { user, isLoading, signIn, signOut } = useAuth()
  const location = useLocation()
  const [userMenuOpen, setUserMenuOpen] = useState(false)

  if (isLoading) {
    return (
      <div className="min-h-screen bg-[#0a0a0a] flex items-center justify-center">
        <div className="flex flex-col items-center gap-4">
          <div className="w-8 h-8 border-2 border-neutral-700 border-t-white rounded-full animate-spin" />
          <p className="text-neutral-500 font-mono text-sm">Loading...</p>
        </div>
      </div>
    )
  }

  if (!user) {
    // Show sign-in prompt instead of auto-redirecting to prevent loops
    return (
      <div className="min-h-screen bg-[#0a0a0a] flex items-center justify-center">
        <div className="flex flex-col items-center gap-6 text-center">
          <span className="font-mono text-2xl tracking-tight text-white">
            open<span className="text-neutral-500">sandbox</span>
          </span>
          <p className="text-neutral-500 font-mono text-sm">
            Sign in to access your dashboard
          </p>
          <button
            onClick={() => signIn()}
            className="btn-primary text-sm"
          >
            Sign In
          </button>
        </div>
      </div>
    )
  }

  const navItems = [
    { path: '/dashboard', label: 'Overview', icon: LayoutDashboard },
    { path: '/dashboard/sandboxes', label: 'Sandboxes', icon: Boxes },
    { path: '/dashboard/api-tokens', label: 'API Tokens', icon: Key },
    { path: '/dashboard/settings', label: 'Settings', icon: Settings },
  ]

  return (
    <div className="min-h-screen bg-[#0a0a0a] flex">
      {/* Sidebar */}
      <aside className="w-64 border-r border-neutral-800 flex flex-col">
        {/* Logo */}
        <div className="p-6 border-b border-neutral-800">
          <Link to="/" className="flex items-center gap-2">
            <span className="font-mono text-lg tracking-tight text-white">
              open<span className="text-neutral-500">sandbox</span>
            </span>
          </Link>
        </div>

        {/* Navigation */}
        <nav className="flex-1 p-4 space-y-1">
          {navItems.map((item) => {
            const isActive = location.pathname === item.path || 
              (item.path !== '/dashboard' && location.pathname.startsWith(item.path))
            return (
              <Link
                key={item.path}
                to={item.path}
                className={`flex items-center gap-3 px-4 py-3 rounded transition-all duration-200 font-mono text-sm ${
                  isActive
                    ? 'bg-neutral-800 text-white'
                    : 'text-neutral-500 hover:text-neutral-300 hover:bg-neutral-800/50'
                }`}
              >
                <item.icon className="w-4 h-4" />
                <span>{item.label}</span>
              </Link>
            )
          })}
        </nav>

        {/* User menu */}
        <div className="p-4 border-t border-neutral-800">
          <div className="relative">
            <button
              onClick={() => setUserMenuOpen(!userMenuOpen)}
              className="w-full flex items-center gap-3 px-4 py-3 rounded hover:bg-neutral-800/50 transition-colors"
            >
              <div className="w-8 h-8 rounded bg-neutral-800 flex items-center justify-center text-neutral-400 font-mono text-sm">
                {user.firstName?.[0] || user.email?.[0]?.toUpperCase() || 'U'}
              </div>
              <div className="flex-1 text-left">
                <p className="text-sm font-mono text-neutral-300 truncate">
                  {user.firstName ? `${user.firstName} ${user.lastName || ''}`.trim() : 'User'}
                </p>
                <p className="text-xs font-mono text-neutral-600 truncate">{user.email}</p>
              </div>
              <ChevronDown className={`w-4 h-4 text-neutral-600 transition-transform ${userMenuOpen ? 'rotate-180' : ''}`} />
            </button>

            {userMenuOpen && (
              <div className="absolute bottom-full left-0 right-0 mb-2 bg-neutral-900 border border-neutral-800 rounded overflow-hidden">
                <button
                  onClick={() => signOut()}
                  className="w-full flex items-center gap-3 px-4 py-3 text-red-400 hover:bg-red-500/10 transition-colors font-mono text-sm"
                >
                  <LogOut className="w-4 h-4" />
                  <span>Sign Out</span>
                </button>
              </div>
            )}
          </div>
        </div>
      </aside>

      {/* Main content */}
      <main className="flex-1 flex flex-col">
        {/* Top bar */}
        <header className="border-b border-neutral-800 px-6 py-4 flex items-center justify-between">
          <div className="flex items-center gap-4 flex-1">
            <div className="relative max-w-md flex-1">
              <Search className="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-neutral-600" />
              <input
                type="text"
                placeholder="Search sandboxes..."
                className="input-field pl-10 py-2 text-sm"
              />
            </div>
          </div>
          <div className="flex items-center gap-3">
            <Link to="/dashboard/sandboxes" search={{ new: true }} className="btn-primary py-2 text-sm flex items-center gap-2">
              <Plus className="w-4 h-4" />
              New Sandbox
            </Link>
          </div>
        </header>

        {/* Page content */}
        <div className="flex-1 p-6 overflow-auto">
          <Outlet />
        </div>
      </main>
    </div>
  )
}
