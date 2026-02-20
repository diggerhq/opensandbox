import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  base: '/',
  server: {
    port: 3000,
    proxy: {
      '/auth': 'http://localhost:8080',
      '/api': {
        target: 'http://localhost:8080',
        ws: true,
      },
    },
  },
})
