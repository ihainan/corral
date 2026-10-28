import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { readdirSync, readFileSync, statSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { brotliCompressSync, constants, gzipSync } from 'node:zlib'

// The Go binary embeds web/dist; keep the placeholder so `go build` works on a clean checkout.
const keepPlaceholder = {
  name: 'keep-gitkeep',
  closeBundle() {
    writeFileSync('dist/.gitkeep', '')
  },
}

// Each text file of the build stored compressed beside it too (.br, .gz),
// which the gateway sends to a browser that takes it (internal/web/static.go):
// compressed once here, not on every request. Only where it is smaller.
const COMPRESSED = /\.(js|css|html|svg|json|webmanifest|txt)$/
const precompress = {
  name: 'precompress',
  closeBundle() {
    const walk = (dir: string): string[] => readdirSync(dir).flatMap((n) => (statSync(join(dir, n)).isDirectory() ? walk(join(dir, n)) : [join(dir, n)]))
    for (const f of walk('dist')) {
      if (!COMPRESSED.test(f)) continue
      const raw = readFileSync(f)
      if (raw.length < 1024) continue
      const br = brotliCompressSync(raw, { params: { [constants.BROTLI_PARAM_QUALITY]: 11, [constants.BROTLI_PARAM_SIZE_HINT]: raw.length } })
      const gz = gzipSync(raw, { level: 9 })
      if (br.length < raw.length) writeFileSync(f + '.br', br)
      if (gz.length < raw.length) writeFileSync(f + '.gz', gz)
    }
  },
}

export default defineConfig({
  plugins: [react(), keepPlaceholder, precompress],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    target: 'es2022',
    chunkSizeWarningLimit: 900,
  },
})
