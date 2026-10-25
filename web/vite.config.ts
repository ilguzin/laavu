import { defineConfig } from 'vite'
import preact from '@preact/preset-vite'

export default defineConfig({
  plugins: [preact()],
  build: {
    target: 'es2022',
    outDir: 'dist',
    emptyOutDir: true,
    chunkSizeWarningLimit: 1500,
    // Без инлайна ассетов в data: — CSP разрешает data: только картинкам и шрифтам.
    assetsInlineLimit: 0,
  },
  server: {
    proxy: {
      '/api': { target: 'http://localhost:7700', ws: true, changeOrigin: false },
    },
  },
})
