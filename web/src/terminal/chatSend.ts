import type { SessionInfo } from '../api'

// A tmux session's agent, whichever of the gateway's sessions shows it.
export const tmuxKey = (s: SessionInfo) => `${s.host_id}|tmux|${s.mux_session ?? ''}`

// The terminal's look on the whole page (no page scroll, toasts under the
// bar) while any page that holds it is mounted.
let held = 0
export function holdTermMode(): () => void {
  held++
  document.documentElement.classList.add('term-mode')
  return () => {
    if (--held > 0) return
    document.documentElement.classList.remove('term-dark', 'term-mode')
  }
}
