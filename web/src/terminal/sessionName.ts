import type { SessionInfo } from '../api'
import { t } from '../i18n'

// Which session of its host a session is, as tabs and lists name it:
// tmux's or herdr's own name; a plain shell is numbered among the host's
// plain shells (of `all`) only when there are several.
export function sessionWhich(s: SessionInfo, all: SessionInfo[]): string {
  if (s.persistence === 'tmux') return s.mux_session || 'tmux'
  if (s.persistence === 'herdr') return s.mux_session && s.mux_session !== 'default' ? 'herdr · ' + s.mux_session : 'herdr'
  const shells = all.filter((x) => x.host_id === s.host_id && x.persistence !== 'tmux' && x.persistence !== 'herdr')
  return shells.length > 1 ? t('tabs.shellN', { n: shells.indexOf(s) + 1 }) : t('tabs.shell')
}

// One agent of one herdr session on a host, whichever of the gateway's
// sessions (tabs) shows it.
export function agentKey(s: SessionInfo, pane: string): string {
  const name = !s.mux_session || s.mux_session === 'default' ? '' : s.mux_session
  return `${s.host_id}|${name}|${pane}`
}
