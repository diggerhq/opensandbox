import { defineConfig, loadEnv } from 'vite'
import react from '@vitejs/plugin-react'
import { TanStackRouterVite } from '@tanstack/router-plugin/vite'
import path from 'path'

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '')
  
  return {
    plugins: [
      TanStackRouterVite(),
      react(),
    ],
    server: {
      allowedHosts: env.VITE_ALLOWED_HOSTS?.split(',') || ['localhost'],
    },
    resolve: {
      alias: {
        '@': path.resolve(__dirname, './src'),
      },
    },
  }
})
