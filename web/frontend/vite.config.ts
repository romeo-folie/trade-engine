import { defineConfig, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'
import { resolve } from 'node:path'

const embedDir = resolve(process.cwd(), '../../internal/web/dist')

// Keep the Go embed target valid after package-level builds. Vite empties the
// output directory before writing bundles, so the placeholder must be emitted
// by Vite itself rather than restored only by the repository Makefile.
const embedPlaceholder = (): Plugin => ({
  name: 'embed-placeholder',
  generateBundle() {
    this.emitFile({ type: 'asset', fileName: '.gitkeep', source: '' })
  },
})

export default defineConfig({
  plugins: [react(), embedPlaceholder()],
  build: {
    outDir: embedDir,
    emptyOutDir: true,
  },
  server: {
    proxy: {
      '/api': 'http://localhost:8080',
      '/ws': {
        target: 'ws://localhost:8080',
        ws: true,
      },
    },
  },
})
