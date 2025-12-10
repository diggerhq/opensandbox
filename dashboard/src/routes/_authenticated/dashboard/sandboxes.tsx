import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { 
  Plus, 
  Search, 
  Filter,
  Play,
  Square,
  Trash2,
  ExternalLink,
  MoreVertical,
  Box,
  Clock,
  Cpu
} from 'lucide-react'

export const Route = createFileRoute('/_authenticated/dashboard/sandboxes')({
  component: SandboxesPage,
})

interface Sandbox {
  id: string
  name: string
  status: 'running' | 'stopped' | 'creating'
  runtime: string
  cpu: string
  memory: string
  createdAt: string
  lastActive: string
}

function SandboxesPage() {
  const [searchQuery, setSearchQuery] = useState('')
  const [showNewModal, setShowNewModal] = useState(false)

  // Mock data - replace with TanStack Query
  const sandboxes: Sandbox[] = [
    { id: '1', name: 'my-react-app', status: 'running', runtime: 'Node.js 20', cpu: '0.5 vCPU', memory: '1GB', createdAt: '2024-01-15', lastActive: '2 min ago' },
    { id: '2', name: 'python-ml-project', status: 'stopped', runtime: 'Python 3.12', cpu: '1 vCPU', memory: '2GB', createdAt: '2024-01-10', lastActive: '1 hour ago' },
    { id: '3', name: 'go-api-server', status: 'running', runtime: 'Go 1.22', cpu: '0.5 vCPU', memory: '512MB', createdAt: '2024-01-12', lastActive: '5 min ago' },
    { id: '4', name: 'rust-wasm-demo', status: 'stopped', runtime: 'Rust 1.75', cpu: '1 vCPU', memory: '1GB', createdAt: '2024-01-08', lastActive: '3 days ago' },
    { id: '5', name: 'django-blog', status: 'creating', runtime: 'Python 3.12', cpu: '0.5 vCPU', memory: '1GB', createdAt: '2024-01-16', lastActive: 'Just now' },
  ]

  const filteredSandboxes = sandboxes.filter(s => 
    s.name.toLowerCase().includes(searchQuery.toLowerCase())
  )

  const templates = [
    { name: 'Node.js', icon: '🟢', description: 'Node.js 20 with npm' },
    { name: 'Python', icon: '🐍', description: 'Python 3.12 with pip' },
    { name: 'Go', icon: '🔵', description: 'Go 1.22' },
    { name: 'Rust', icon: '🦀', description: 'Rust 1.75 with cargo' },
    { name: 'Ruby', icon: '💎', description: 'Ruby 3.3 with bundler' },
    { name: 'Blank', icon: '📦', description: 'Start from scratch' },
  ]

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-mono text-white">Sandboxes</h1>
          <p className="text-neutral-500 font-mono text-sm mt-1">
            Manage your cloud development environments
          </p>
        </div>
        <button
          onClick={() => setShowNewModal(true)}
          className="btn-primary flex items-center gap-2 text-sm"
        >
          <Plus className="w-4 h-4" />
          New Sandbox
        </button>
      </div>

      {/* Filters */}
      <div className="flex items-center gap-4">
        <div className="relative flex-1 max-w-sm">
          <Search className="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-neutral-600" />
          <input
            type="text"
            placeholder="Search sandboxes..."
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
            className="input-field pl-10 text-sm"
          />
        </div>
        <button className="btn-secondary flex items-center gap-2 py-3 text-sm">
          <Filter className="w-4 h-4" />
          Filter
        </button>
      </div>

      {/* Sandboxes Grid */}
      <div className="grid gap-4">
        {filteredSandboxes.map((sandbox) => (
          <SandboxCard key={sandbox.id} sandbox={sandbox} />
        ))}
      </div>

      {filteredSandboxes.length === 0 && (
        <div className="border border-neutral-800 rounded-lg p-12 text-center">
          <Box className="w-12 h-12 text-neutral-700 mx-auto mb-4" />
          <h3 className="font-mono text-neutral-400 mb-2">No sandboxes found</h3>
          <p className="text-neutral-600 font-mono text-sm mb-6">
            {searchQuery ? 'Try a different search term' : 'Create your first sandbox to get started'}
          </p>
          <button
            onClick={() => setShowNewModal(true)}
            className="btn-primary text-sm"
          >
            Create Sandbox
          </button>
        </div>
      )}

      {/* New Sandbox Modal */}
      {showNewModal && (
        <div className="fixed inset-0 bg-black/80 backdrop-blur-sm flex items-center justify-center z-50 p-4">
          <div className="bg-[#0f0f0f] border border-neutral-800 rounded-lg max-w-2xl w-full max-h-[90vh] overflow-auto">
            <div className="p-6 border-b border-neutral-800">
              <h2 className="font-mono text-white text-lg">Create New Sandbox</h2>
              <p className="font-mono text-neutral-500 text-sm mt-1">
                Choose a template to get started quickly
              </p>
            </div>
            <div className="p-6">
              <div className="mb-6">
                <label className="block font-mono text-sm text-neutral-400 mb-2">
                  Sandbox Name
                </label>
                <input
                  type="text"
                  placeholder="my-awesome-project"
                  className="input-field text-sm"
                />
              </div>
              <div>
                <label className="block font-mono text-sm text-neutral-400 mb-3">
                  Select Template
                </label>
                <div className="grid grid-cols-2 md:grid-cols-3 gap-3">
                  {templates.map((template) => (
                    <button
                      key={template.name}
                      className="p-4 rounded-lg border border-neutral-800 hover:border-neutral-600 hover:bg-neutral-800/50 transition-all text-left group"
                    >
                      <span className="text-2xl mb-2 block">{template.icon}</span>
                      <p className="font-mono text-neutral-300 group-hover:text-white transition-colors">
                        {template.name}
                      </p>
                      <p className="font-mono text-xs text-neutral-600 mt-1">{template.description}</p>
                    </button>
                  ))}
                </div>
              </div>
            </div>
            <div className="p-6 border-t border-neutral-800 flex justify-end gap-3">
              <button
                onClick={() => setShowNewModal(false)}
                className="btn-secondary text-sm"
              >
                Cancel
              </button>
              <button className="btn-primary text-sm">
                Create Sandbox
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}

function SandboxCard({ sandbox }: { sandbox: Sandbox }) {
  const [menuOpen, setMenuOpen] = useState(false)

  const statusColors = {
    running: 'bg-green-500',
    stopped: 'bg-neutral-600',
    creating: 'bg-yellow-500 animate-pulse',
  }

  const statusText = {
    running: 'Running',
    stopped: 'Stopped',
    creating: 'Creating...',
  }

  return (
    <div className="border border-neutral-800 rounded-lg p-5 hover:border-neutral-700 transition-all group">
      <div className="flex items-start justify-between">
        <div className="flex items-start gap-4">
          <div className="w-12 h-12 rounded bg-neutral-800 flex items-center justify-center text-neutral-500 group-hover:text-neutral-300 transition-colors">
            <Box className="w-6 h-6" />
          </div>
          <div>
            <div className="flex items-center gap-3">
              <h3 className="font-mono text-white">{sandbox.name}</h3>
              <span className={`flex items-center gap-1.5 text-xs font-mono px-2 py-0.5 rounded ${
                sandbox.status === 'running' ? 'bg-green-500/10 text-green-400' :
                sandbox.status === 'creating' ? 'bg-yellow-500/10 text-yellow-400' :
                'bg-neutral-800 text-neutral-500'
              }`}>
                <span className={`w-1.5 h-1.5 rounded-full ${statusColors[sandbox.status]}`} />
                {statusText[sandbox.status]}
              </span>
            </div>
            <p className="font-mono text-sm text-neutral-600 mt-1">{sandbox.runtime}</p>
            <div className="flex items-center gap-4 mt-3 text-xs font-mono text-neutral-600">
              <span className="flex items-center gap-1">
                <Cpu className="w-3 h-3" />
                {sandbox.cpu}
              </span>
              <span className="flex items-center gap-1">
                <Clock className="w-3 h-3" />
                {sandbox.lastActive}
              </span>
            </div>
          </div>
        </div>

        <div className="flex items-center gap-2">
          {sandbox.status === 'running' && (
            <a
              href="#"
              className="p-2 rounded text-neutral-500 hover:text-white hover:bg-neutral-800 transition-colors"
              title="Open in browser"
            >
              <ExternalLink className="w-4 h-4" />
            </a>
          )}
          <button
            className={`p-2 rounded transition-colors ${
              sandbox.status === 'running'
                ? 'text-yellow-500 hover:bg-yellow-500/10'
                : sandbox.status === 'stopped'
                ? 'text-green-500 hover:bg-green-500/10'
                : 'text-neutral-700 cursor-not-allowed'
            }`}
            disabled={sandbox.status === 'creating'}
            title={sandbox.status === 'running' ? 'Stop' : 'Start'}
          >
            {sandbox.status === 'running' ? (
              <Square className="w-4 h-4" />
            ) : (
              <Play className="w-4 h-4" />
            )}
          </button>
          <div className="relative">
            <button
              onClick={() => setMenuOpen(!menuOpen)}
              className="p-2 rounded text-neutral-500 hover:text-white hover:bg-neutral-800 transition-colors"
            >
              <MoreVertical className="w-4 h-4" />
            </button>
            {menuOpen && (
              <>
                <div
                  className="fixed inset-0 z-10"
                  onClick={() => setMenuOpen(false)}
                />
                <div className="absolute right-0 top-full mt-1 bg-neutral-900 border border-neutral-800 rounded overflow-hidden z-20 min-w-[140px]">
                  <button className="w-full px-4 py-2 font-mono text-sm text-left text-neutral-400 hover:bg-neutral-800 hover:text-white transition-colors">
                    Duplicate
                  </button>
                  <button className="w-full px-4 py-2 font-mono text-sm text-left text-neutral-400 hover:bg-neutral-800 hover:text-white transition-colors">
                    Settings
                  </button>
                  <button className="w-full px-4 py-2 font-mono text-sm text-left text-red-400 hover:bg-red-500/10 transition-colors flex items-center gap-2">
                    <Trash2 className="w-3 h-3" />
                    Delete
                  </button>
                </div>
              </>
            )}
          </div>
        </div>
      </div>
    </div>
  )
}
