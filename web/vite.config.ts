import { defineConfig } from 'vite'
import preact from '@preact/preset-vite'

export default defineConfig({
  plugins: [preact()],
  build: {
    target: 'es2022',
    outDir: 'dist',
    emptyOutDir: true,
    chunkSizeWarningLimit: 1500,
    // No inlining assets as data: URLs; the CSP allows data: only for images and fonts.
    assetsInlineLimit: 0,
  },
  server: {
    proxy: {
      '/api': { target: 'http://localhost:7700', ws: true, changeOrigin: false },
    },
  },
})
