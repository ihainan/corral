import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { writeFileSync } from 'node:fs'

// The Go binary embeds web/dist; keep the placeholder so `go build` works on a clean checkout.
const keepPlaceholder = {
  name: 'keep-gitkeep',
  closeBundle() {
    writeFileSync('dist/.gitkeep', '')
  },
}

export default defineConfig({
  plugins: [react(), keepPlaceholder],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    target: 'es2022',
    chunkSizeWarningLimit: 900,
  },
})
