import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useAuth } from '@workos-inc/authkit-react'
import { useEffect, useState } from 'react'

export const Route = createFileRoute('/auth/callback')({
  component: AuthCallback,
})

function AuthCallback() {
  const { user, isLoading } = useAuth()
  const [error, setError] = useState<string | null>(null)
  const navigate = useNavigate()

  useEffect(() => {
    // Check for error in URL
    const params = new URLSearchParams(window.location.search)
    const errorParam = params.get('error')
    const errorDesc = params.get('error_description')
    
    if (errorParam) {
      setError(errorDesc || errorParam)
      return
    }

    // If we have a user, redirect to dashboard using client-side navigation
    // This preserves the session state without a full page reload
    if (!isLoading && user) {
      // Small delay to ensure session is fully saved
      setTimeout(() => {
        navigate({ to: '/dashboard' })
      }, 100)
      return
    }

    // If loading is done but no user and no code in URL, redirect to home
    if (!isLoading && !user) {
      const code = params.get('code')
      if (!code) {
        setTimeout(() => {
          navigate({ to: '/' })
        }, 2000)
      }
    }
  }, [user, isLoading, navigate])

  if (error) {
    return (
      <div className="min-h-screen bg-[#0a0a0a] flex items-center justify-center">
        <div className="flex flex-col items-center gap-4 text-center max-w-md">
          <div className="w-12 h-12 rounded-full bg-red-500/20 flex items-center justify-center">
            <span className="text-red-500 text-xl">✕</span>
          </div>
          <p className="text-red-400 font-mono">Authentication failed</p>
          <p className="text-neutral-500 font-mono text-sm">{error}</p>
          <a href="/" className="btn-secondary text-sm mt-4">
            Back to Home
          </a>
        </div>
      </div>
    )
  }

  return (
    <div className="min-h-screen bg-[#0a0a0a] flex items-center justify-center">
      <div className="flex flex-col items-center gap-4">
        <div className="w-8 h-8 border-2 border-neutral-700 border-t-white rounded-full animate-spin" />
        <p className="text-neutral-500 font-mono text-sm">Completing sign in...</p>
      </div>
    </div>
  )
}
