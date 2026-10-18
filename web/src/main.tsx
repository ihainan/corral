import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import '@fontsource/jetbrains-mono/400.css'
import '@fontsource/jetbrains-mono/700.css'
import '@xterm/xterm/css/xterm.css'
import './styles/app.css'
import './styles/terminal.css'
import { getLang } from './i18n'
import { App } from './app'

document.documentElement.lang = getLang()

// Older versions kept composer drafts in sessionStorage; drafts can hold secrets.
try {
  for (let i = sessionStorage.length - 1; i >= 0; i--) {
    const k = sessionStorage.key(i)
    if (k?.startsWith('corral.draft.')) sessionStorage.removeItem(k)
  }
} catch {
  /* storage unavailable */
}

if ('serviceWorker' in navigator && location.protocol === 'https:') {
  navigator.serviceWorker.register('/sw.js').catch(() => {})
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
