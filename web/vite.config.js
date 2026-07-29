import { defineConfig } from 'vite'

// The demo talks to the Go server directly over its own WebSocket URL, so there
// is nothing to proxy. Point it elsewhere with VITE_YCOLLAB_URL.
export default defineConfig({
  server: {
    port: 5173,
    strictPort: true
  }
})
