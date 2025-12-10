import { createFileRoute, Link } from '@tanstack/react-router'
import { useAuth } from '@workos-inc/authkit-react'
import { Copy, Check, Github } from 'lucide-react'
import { useState } from 'react'

export const Route = createFileRoute('/')({
  component: LandingPage,
})

type InstallTab = 'npm' | 'yarn' | 'pnpm' | 'curl'

const installCommands: Record<InstallTab, string> = {
  npm: 'npm install @opensandbox/sdk',
  yarn: 'yarn add @opensandbox/sdk',
  pnpm: 'pnpm add @opensandbox/sdk',
  curl: 'curl -fsSL https://opensandbox.dev/install | bash',
}

function LandingPage() {
  const { user, isLoading, signIn } = useAuth()
  const [activeTab, setActiveTab] = useState<InstallTab>('npm')
  const [copied, setCopied] = useState(false)

  const handleCopy = () => {
    navigator.clipboard.writeText(installCommands[activeTab])
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  return (
    <div className="min-h-screen bg-[#0a0a0a]">
      {/* Navigation */}
      <nav className="border-b border-neutral-800">
        <div className="max-w-6xl mx-auto px-6 py-4 flex items-center justify-between">
          <span className="font-mono text-xl tracking-tight text-white">
            open<span className="text-neutral-500">sandbox</span>
          </span>
          
          <div className="flex items-center gap-8">
            <a 
              href="https://github.com/diggerhq/opensandbox" 
              target="_blank"
              rel="noopener noreferrer"
              className="flex items-center gap-2 text-neutral-400 hover:text-white transition-colors font-mono text-sm"
            >
              <Github className="w-4 h-4" />
              GitHub
            </a>
            <a href="https://docs.opensandbox.dev" className="text-neutral-400 hover:text-white transition-colors font-mono text-sm">
              Docs
            </a>
            <a href="/pricing" className="text-neutral-400 hover:text-white transition-colors font-mono text-sm">
              Pricing
            </a>
            {isLoading ? (
              <div className="w-20 h-8 bg-neutral-800 rounded animate-pulse" />
            ) : user ? (
              <Link 
                to="/dashboard" 
                className="text-neutral-400 hover:text-white transition-colors font-mono text-sm"
              >
                Dashboard
              </Link>
            ) : (
              <button 
                onClick={() => signIn()} 
                className="text-neutral-400 hover:text-white transition-colors font-mono text-sm"
              >
                Sign In
              </button>
            )}
          </div>
        </div>
      </nav>

      {/* Hero Section */}
      <main className="max-w-6xl mx-auto px-6 pt-24 pb-32">
        <div className="max-w-3xl">
          {/* Version badge */}
          <p className="text-neutral-500 font-mono text-sm mb-8">
            What's new in v0.1.0
          </p>
          
          {/* Main headline */}
          <h1 className="text-5xl md:text-6xl lg:text-7xl font-mono font-normal text-white leading-[1.1] tracking-tight mb-8">
            The cloud sandbox<br />
            infrastructure
          </h1>
          
          {/* Description */}
          <p className="text-neutral-400 font-mono text-lg leading-relaxed mb-4 max-w-2xl">
            OpenSandbox provides instant, isolated development environments.
            Spin up sandboxes programmatically with our SDK or CLI.
          </p>

          {/* Tagline */}
          <p className="text-white font-mono text-lg mb-12">
            Install and use. No complex setup required.
          </p>

          {/* Install tabs */}
          <div className="border border-neutral-800 rounded-lg overflow-hidden max-w-xl">
            {/* Tab headers */}
            <div className="flex border-b border-neutral-800">
              {(['npm', 'yarn', 'pnpm', 'curl'] as InstallTab[]).map((tab) => (
                <button
                  key={tab}
                  onClick={() => setActiveTab(tab)}
                  className={`px-6 py-3 font-mono text-sm transition-colors ${
                    activeTab === tab
                      ? 'text-white border-b-2 border-white -mb-[1px]'
                      : 'text-neutral-500 hover:text-neutral-300'
                  }`}
                >
                  {tab}
                </button>
              ))}
            </div>
            
            {/* Command display */}
            <div className="p-6 flex items-center justify-between gap-4 bg-[#0f0f0f]">
              <code className="font-mono text-sm text-neutral-300">
                {installCommands[activeTab]}
              </code>
              <button
                onClick={handleCopy}
                className="text-neutral-500 hover:text-white transition-colors p-1"
                title="Copy to clipboard"
              >
                {copied ? (
                  <Check className="w-4 h-4 text-green-500" />
                ) : (
                  <Copy className="w-4 h-4" />
                )}
              </button>
            </div>
          </div>

          {/* Bottom text */}
          <p className="text-neutral-600 font-mono text-sm mt-8 leading-relaxed">
            Available via SDK for JavaScript, Python, and Go.<br />
            CLI available for all platforms.
          </p>
        </div>
      </main>

      {/* Footer */}
      <footer className="border-t border-neutral-800 py-8">
        <div className="max-w-6xl mx-auto px-6 flex items-center justify-between">
          <span className="font-mono text-sm text-neutral-600">
            © 2024 OpenSandbox
          </span>
          <div className="flex items-center gap-6">
            <a href="#" className="text-neutral-600 hover:text-neutral-400 transition-colors font-mono text-sm">
              Privacy
            </a>
            <a href="#" className="text-neutral-600 hover:text-neutral-400 transition-colors font-mono text-sm">
              Terms
            </a>
          </div>
        </div>
      </footer>
    </div>
  )
}

export default LandingPage
