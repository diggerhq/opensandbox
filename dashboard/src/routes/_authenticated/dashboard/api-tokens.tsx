import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { 
  Plus, 
  Copy, 
  Check,
  Trash2,
  Key
} from 'lucide-react'

export const Route = createFileRoute('/_authenticated/dashboard/api-tokens')({
  component: ApiTokensPage,
})

interface ApiToken {
  id: string
  name: string
  prefix: string
  createdAt: string
  lastUsed: string | null
  expiresAt: string | null
}

function ApiTokensPage() {
  const [showNewModal, setShowNewModal] = useState(false)
  const [newTokenName, setNewTokenName] = useState('')
  const [generatedToken, setGeneratedToken] = useState<string | null>(null)
  const [copiedId, setCopiedId] = useState<string | null>(null)

  // Mock data - replace with TanStack Query
  const [tokens] = useState<ApiToken[]>([
    { 
      id: '1', 
      name: 'Production API Key', 
      prefix: 'sk_live_...a4b2', 
      createdAt: '2024-01-10', 
      lastUsed: '2 hours ago',
      expiresAt: null
    },
    { 
      id: '2', 
      name: 'Development Key', 
      prefix: 'sk_test_...f7c9', 
      createdAt: '2024-01-05', 
      lastUsed: '1 day ago',
      expiresAt: '2024-12-31'
    },
    { 
      id: '3', 
      name: 'CI/CD Pipeline', 
      prefix: 'sk_live_...x8m2', 
      createdAt: '2024-01-12', 
      lastUsed: '5 min ago',
      expiresAt: null
    },
  ])

  const handleCreateToken = () => {
    // Mock token generation - replace with actual API call
    const mockToken = `sk_live_${Math.random().toString(36).substring(2, 15)}${Math.random().toString(36).substring(2, 15)}`
    setGeneratedToken(mockToken)
  }

  const handleCopy = (text: string, id: string) => {
    navigator.clipboard.writeText(text)
    setCopiedId(id)
    setTimeout(() => setCopiedId(null), 2000)
  }

  const handleCloseModal = () => {
    setShowNewModal(false)
    setNewTokenName('')
    setGeneratedToken(null)
  }

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-mono text-white">API Tokens</h1>
          <p className="text-neutral-500 font-mono text-sm mt-1">
            Manage API tokens for programmatic access to OpenSandbox
          </p>
        </div>
        <button
          onClick={() => setShowNewModal(true)}
          className="btn-primary flex items-center gap-2 text-sm"
        >
          <Plus className="w-4 h-4" />
          Create Token
        </button>
      </div>

      {/* Info banner */}
      <div className="border border-neutral-800 rounded-lg p-4 bg-neutral-900/50">
        <p className="font-mono text-sm text-neutral-400">
          API tokens are used to authenticate requests to the OpenSandbox API. 
          Keep your tokens secure and never share them publicly.
        </p>
      </div>

      {/* Tokens list */}
      <div className="border border-neutral-800 rounded-lg overflow-hidden">
        <div className="px-6 py-4 border-b border-neutral-800 bg-neutral-900/50">
          <h2 className="font-mono text-white">Active Tokens</h2>
        </div>
        
        {tokens.length === 0 ? (
          <div className="p-12 text-center">
            <Key className="w-12 h-12 text-neutral-700 mx-auto mb-4" />
            <h3 className="font-mono text-neutral-400 mb-2">No API tokens yet</h3>
            <p className="text-neutral-600 font-mono text-sm mb-6">
              Create your first API token to start using the OpenSandbox API
            </p>
            <button
              onClick={() => setShowNewModal(true)}
              className="btn-primary text-sm"
            >
              Create Token
            </button>
          </div>
        ) : (
          <div className="divide-y divide-neutral-800">
            {tokens.map((token) => (
              <TokenRow 
                key={token.id} 
                token={token} 
                onCopy={handleCopy}
                copiedId={copiedId}
              />
            ))}
          </div>
        )}
      </div>

      {/* Create Token Modal */}
      {showNewModal && (
        <div className="fixed inset-0 bg-black/80 backdrop-blur-sm flex items-center justify-center z-50 p-4">
          <div className="bg-[#0f0f0f] border border-neutral-800 rounded-lg max-w-lg w-full">
            <div className="p-6 border-b border-neutral-800">
              <h2 className="font-mono text-white text-lg">
                {generatedToken ? 'Token Created' : 'Create New API Token'}
              </h2>
              <p className="font-mono text-neutral-500 text-sm mt-1">
                {generatedToken 
                  ? 'Make sure to copy your token now. You won\'t be able to see it again.'
                  : 'Give your token a name to help you identify it later.'
                }
              </p>
            </div>
            
            <div className="p-6">
              {generatedToken ? (
                <div className="space-y-4">
                  <div className="p-4 bg-neutral-900 border border-neutral-700 rounded-lg">
                    <div className="flex items-center justify-between gap-4">
                      <code className="font-mono text-sm text-green-400 break-all">
                        {generatedToken}
                      </code>
                      <button
                        onClick={() => handleCopy(generatedToken, 'new')}
                        className="shrink-0 p-2 text-neutral-500 hover:text-white transition-colors"
                      >
                        {copiedId === 'new' ? (
                          <Check className="w-4 h-4 text-green-500" />
                        ) : (
                          <Copy className="w-4 h-4" />
                        )}
                      </button>
                    </div>
                  </div>
                  <p className="font-mono text-xs text-yellow-500">
                    ⚠️ This token will only be shown once. Store it securely.
                  </p>
                </div>
              ) : (
                <div>
                  <label className="block font-mono text-sm text-neutral-400 mb-2">
                    Token Name
                  </label>
                  <input
                    type="text"
                    value={newTokenName}
                    onChange={(e) => setNewTokenName(e.target.value)}
                    placeholder="e.g., Production API Key"
                    className="input-field text-sm"
                    autoFocus
                  />
                </div>
              )}
            </div>
            
            <div className="p-6 border-t border-neutral-800 flex justify-end gap-3">
              {generatedToken ? (
                <button
                  onClick={handleCloseModal}
                  className="btn-primary text-sm"
                >
                  Done
                </button>
              ) : (
                <>
                  <button
                    onClick={handleCloseModal}
                    className="btn-secondary text-sm"
                  >
                    Cancel
                  </button>
                  <button 
                    onClick={handleCreateToken}
                    disabled={!newTokenName.trim()}
                    className="btn-primary text-sm disabled:opacity-50 disabled:cursor-not-allowed"
                  >
                    Create Token
                  </button>
                </>
              )}
            </div>
          </div>
        </div>
      )}
    </div>
  )
}

function TokenRow({ 
  token, 
  onCopy, 
  copiedId 
}: { 
  token: ApiToken
  onCopy: (text: string, id: string) => void
  copiedId: string | null
}) {
  const [showDeleteConfirm, setShowDeleteConfirm] = useState(false)

  return (
    <div className="px-6 py-4 flex items-center justify-between hover:bg-neutral-800/30 transition-colors">
      <div className="flex items-center gap-4">
        <div className="w-10 h-10 rounded bg-neutral-800 flex items-center justify-center text-neutral-500">
          <Key className="w-5 h-5" />
        </div>
        <div>
          <p className="font-mono text-white">{token.name}</p>
          <div className="flex items-center gap-3 mt-1">
            <code className="font-mono text-sm text-neutral-600">{token.prefix}</code>
            <span className="text-neutral-700">·</span>
            <span className="font-mono text-xs text-neutral-600">
              Created {token.createdAt}
            </span>
            {token.lastUsed && (
              <>
                <span className="text-neutral-700">·</span>
                <span className="font-mono text-xs text-neutral-600">
                  Last used {token.lastUsed}
                </span>
              </>
            )}
          </div>
          {token.expiresAt && (
            <p className="font-mono text-xs text-yellow-600 mt-1">
              Expires {token.expiresAt}
            </p>
          )}
        </div>
      </div>
      
      <div className="flex items-center gap-2">
        <button
          onClick={() => onCopy(token.prefix, token.id)}
          className="p-2 rounded text-neutral-500 hover:text-white hover:bg-neutral-800 transition-colors"
          title="Copy token prefix"
        >
          {copiedId === token.id ? (
            <Check className="w-4 h-4 text-green-500" />
          ) : (
            <Copy className="w-4 h-4" />
          )}
        </button>
        
        <div className="relative">
          <button
            onClick={() => setShowDeleteConfirm(true)}
            className="p-2 rounded text-neutral-500 hover:text-red-400 hover:bg-red-500/10 transition-colors"
            title="Revoke token"
          >
            <Trash2 className="w-4 h-4" />
          </button>
          
          {showDeleteConfirm && (
            <>
              <div 
                className="fixed inset-0 z-10" 
                onClick={() => setShowDeleteConfirm(false)} 
              />
              <div className="absolute right-0 top-full mt-2 bg-neutral-900 border border-neutral-800 rounded-lg p-4 z-20 w-64">
                <p className="font-mono text-sm text-neutral-300 mb-3">
                  Revoke this token? This action cannot be undone.
                </p>
                <div className="flex justify-end gap-2">
                  <button
                    onClick={() => setShowDeleteConfirm(false)}
                    className="px-3 py-1.5 font-mono text-xs text-neutral-400 hover:text-white transition-colors"
                  >
                    Cancel
                  </button>
                  <button
                    onClick={() => {
                      // TODO: Call API to revoke token
                      setShowDeleteConfirm(false)
                    }}
                    className="px-3 py-1.5 font-mono text-xs bg-red-500/20 text-red-400 rounded hover:bg-red-500/30 transition-colors"
                  >
                    Revoke
                  </button>
                </div>
              </div>
            </>
          )}
        </div>
      </div>
    </div>
  )
}

