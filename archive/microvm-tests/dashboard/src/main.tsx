import { StrictMode } from 'react'
import ReactDOM from 'react-dom/client'
import { RouterProvider, createRouter } from '@tanstack/react-router'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { AuthKitProvider } from '@workos-inc/authkit-react'
import { routeTree } from './routeTree.gen'
import './index.css'

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 1000 * 60 * 5,
      retry: 1,
    },
  },
})

const router = createRouter({ 
  routeTree,
  context: {
    queryClient,
  },
  defaultPreload: 'intent',
})

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}

const rootElement = document.getElementById('root')!

if (!rootElement.innerHTML) {
  const root = ReactDOM.createRoot(rootElement)
  root.render(
    <StrictMode>
      <AuthKitProvider
        clientId={import.meta.env.VITE_WORKOS_CLIENT_ID || 'client_placeholder'}
        apiHostname={import.meta.env.VITE_WORKOS_API_HOSTNAME}
        redirectUri={import.meta.env.VITE_WORKOS_REDIRECT_URI || `${window.location.origin}/auth/callback`}
        devMode={import.meta.env.DEV}
      >
        <QueryClientProvider client={queryClient}>
          <RouterProvider router={router} />
        </QueryClientProvider>
      </AuthKitProvider>
    </StrictMode>,
  )
}
