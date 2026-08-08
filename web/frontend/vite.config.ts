import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { writeFileSync } from 'node:fs'
import { resolve } from 'node:path'

const embedDir = resolve(process.cwd(), '../../internal/web/dist')

export default defineConfig({
  plugins: [
    react(),
    {
      name: 'preserve-go-embed-placeholder',
      closeBundle() {
        writeFileSync(resolve(embedDir, '.gitkeep'), '')
      },
    },
  ],
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
