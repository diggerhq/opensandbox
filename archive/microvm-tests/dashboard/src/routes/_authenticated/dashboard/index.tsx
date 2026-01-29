import { createFileRoute, Link } from '@tanstack/react-router'
import { useAuth } from '@workos-inc/authkit-react'
import { 
  Boxes, 
  Clock, 
  Cpu, 
  HardDrive,
  ArrowUpRight,
  Play,
  Square
} from 'lucide-react'

export const Route = createFileRoute('/_authenticated/dashboard/')({
  component: DashboardOverview,
})

function DashboardOverview() {
  const { user } = useAuth()

  // Mock data - replace with real API calls using TanStack Query
  const stats = [
    { label: 'Active Sandboxes', value: '3', icon: Boxes, change: '+2 this week' },
    { label: 'Total Runtime', value: '24.5h', icon: Clock, change: 'This month' },
    { label: 'CPU Usage', value: '45%', icon: Cpu, change: 'Average' },
    { label: 'Storage Used', value: '2.3GB', icon: HardDrive, change: 'of 10GB' },
  ]

  const recentSandboxes = [
    { id: '1', name: 'my-react-app', status: 'running', runtime: 'Node.js 20', lastActive: '2 min ago' },
    { id: '2', name: 'python-ml-project', status: 'stopped', runtime: 'Python 3.12', lastActive: '1 hour ago' },
    { id: '3', name: 'go-api-server', status: 'running', runtime: 'Go 1.22', lastActive: '5 min ago' },
  ]

  return (
    <div className="space-y-8">
      {/* Welcome */}
      <div>
        <h1 className="text-2xl font-mono text-white mb-2">
          Welcome back, {user?.firstName || 'Developer'}
        </h1>
        <p className="text-neutral-500 font-mono text-sm">
          Here's what's happening with your sandboxes today.
        </p>
      </div>

      {/* Stats Grid */}
      <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-4 gap-4">
        {stats.map((stat) => (
          <div
            key={stat.label}
            className="border border-neutral-800 rounded-lg p-6 bg-neutral-900/50"
          >
            <div className="flex items-start justify-between mb-4">
              <div className="w-10 h-10 rounded bg-neutral-800 flex items-center justify-center text-neutral-400">
                <stat.icon className="w-5 h-5" />
              </div>
            </div>
            <p className="text-3xl font-mono text-white mb-1">{stat.value}</p>
            <p className="text-sm font-mono text-neutral-500">{stat.label}</p>
            <p className="text-xs font-mono text-neutral-600 mt-1">{stat.change}</p>
          </div>
        ))}
      </div>

      {/* Recent Sandboxes */}
      <div className="border border-neutral-800 rounded-lg overflow-hidden">
        <div className="px-6 py-4 border-b border-neutral-800 flex items-center justify-between bg-neutral-900/50">
          <h2 className="font-mono text-white">Recent Sandboxes</h2>
          <Link
            to="/dashboard/sandboxes"
            className="text-sm font-mono text-neutral-500 hover:text-white flex items-center gap-1 transition-colors"
          >
            View all <ArrowUpRight className="w-4 h-4" />
          </Link>
        </div>
        <div className="divide-y divide-neutral-800">
          {recentSandboxes.map((sandbox) => (
            <div
              key={sandbox.id}
              className="px-6 py-4 flex items-center justify-between hover:bg-neutral-800/30 transition-colors"
            >
              <div className="flex items-center gap-4">
                <div className={`w-2 h-2 rounded-full ${
                  sandbox.status === 'running' ? 'bg-green-500' : 'bg-neutral-600'
                }`} />
                <div>
                  <p className="font-mono text-white">{sandbox.name}</p>
                  <p className="text-sm font-mono text-neutral-600">{sandbox.runtime}</p>
                </div>
              </div>
              <div className="flex items-center gap-4">
                <span className="text-sm font-mono text-neutral-600">{sandbox.lastActive}</span>
                <button
                  className={`p-2 rounded transition-colors ${
                    sandbox.status === 'running'
                      ? 'text-yellow-500 hover:bg-yellow-500/10'
                      : 'text-green-500 hover:bg-green-500/10'
                  }`}
                >
                  {sandbox.status === 'running' ? (
                    <Square className="w-4 h-4" />
                  ) : (
                    <Play className="w-4 h-4" />
                  )}
                </button>
              </div>
            </div>
          ))}
        </div>
      </div>

      {/* Quick Actions */}
      <div className="grid md:grid-cols-2 gap-4">
        <div className="border border-neutral-800 rounded-lg p-6 hover:border-neutral-700 transition-colors">
          <h3 className="font-mono text-white mb-2">Quick Start Templates</h3>
          <p className="text-neutral-500 font-mono text-sm mb-4">
            Launch a pre-configured environment in seconds.
          </p>
          <div className="flex flex-wrap gap-2">
            {['Node.js', 'Python', 'Go', 'Rust'].map((template) => (
              <button
                key={template}
                className="px-3 py-1.5 text-sm font-mono rounded bg-neutral-800 text-neutral-400 hover:bg-neutral-700 hover:text-white transition-colors"
              >
                {template}
              </button>
            ))}
          </div>
        </div>
        <div className="border border-neutral-800 rounded-lg p-6 hover:border-neutral-700 transition-colors">
          <h3 className="font-mono text-white mb-2">Documentation</h3>
          <p className="text-neutral-500 font-mono text-sm mb-4">
            Learn how to get the most out of OpenSandbox.
          </p>
          <a
            href="#"
            className="inline-flex items-center gap-2 text-neutral-400 hover:text-white font-mono text-sm transition-colors"
          >
            Read the docs <ArrowUpRight className="w-4 h-4" />
          </a>
        </div>
      </div>
    </div>
  )
}
