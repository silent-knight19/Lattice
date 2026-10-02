import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// The dev server proxies /api to a locally running daemon so the SPA can be developed
// against real data. It binds to loopback only and MUST NOT be exposed on a shared host:
// the dev server has none of the daemon's Host/Origin/CSRF defences, since those live in
// the Go server that a production build talks to directly.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    // web/dist is embedded by web/embed.go via //go:embed all:dist.
    outDir: 'dist',
    emptyOutDir: true,
    // Deterministic, human-readable names so a bundle diff is reviewable.
    sourcemap: false,
  },
  server: {
    host: '127.0.0.1',
    port: 5173,
    strictPort: true,
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:7070',
        changeOrigin: false,
      },
    },
  },
})
