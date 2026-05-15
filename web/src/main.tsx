import React from 'react'
import ReactDOM from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { BrowserRouter } from 'react-router-dom'
import posthog from 'posthog-js'
import { PostHogProvider } from '@posthog/react'
import App from './App'
import './styles/theme.css'

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: 1,
      staleTime: 30_000,
    },
  },
})

const PH_TOKEN = import.meta.env.VITE_PUBLIC_POSTHOG_PROJECT_TOKEN
const PH_HOST = import.meta.env.VITE_PUBLIC_POSTHOG_HOST
if (PH_TOKEN) {
  posthog.init(PH_TOKEN, {
    api_host: PH_HOST || 'https://us.i.posthog.com',
    defaults: '2025-05-24',
    person_profiles: 'identified_only',
  })
}

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <PostHogProvider client={posthog}>
      <QueryClientProvider client={queryClient}>
        <BrowserRouter>
          <App />
        </BrowserRouter>
      </QueryClientProvider>
    </PostHogProvider>
  </React.StrictMode>,
)
