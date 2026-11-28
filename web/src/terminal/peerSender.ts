// Pure resolution and formatting helpers for cross-session peer messages
// (tested directly in web/scripts/test-peer-sender.mjs).
//
// Display rule: the backend decides whether a peer's verified (pid, proc_start)
// matches a currently-selected Corral agent in the latest locate scan. The
// frontend does no identity matching and never reads sidebar names — if the
// backend supplied a complete Corral target (title + session_id), show it with
// a link; otherwise show the message's own sender name + "Not in Corral".

export type PeerPickHow = 'tab' | 'split'

// Mirrors internal/convo/convo.go PeerInfo.
export interface ConvoPeerInfo {
  session_id?: string
  pane?: string
  title?: string
  host_name?: string
  persistence?: string
  mux_session?: string
  display_name?: string
  workspace?: string
}

export type ResolvedPeer =
  | {
      kind: 'live'
      name: string
      where: string
      sessionId: string
      pane?: string
      href: string
    }
  | {
      kind: 'unknown'
      name: string
    }

// Strip ASCII control chars and Unicode BiDi override/isolate controls from
// untrusted names/titles before display.
export function sanitizePeerText(s?: string): string {
  if (!s) return ''
  return s.replace(/[\x00-\x1f\x7f\u202a-\u202e\u2066-\u2069]/g, '').trim()
}

// Canonical lookup key matching Go peerMapKey(pid, start).
export function peerRefKey(pid?: number, start?: number): string {
  if (!pid || !start || !Number.isFinite(pid) || !Number.isFinite(start) || pid <= 0 || start <= 0) return ''
  return `${Math.trunc(pid)}:${Math.trunc(start)}`
}

// Build a safe internal Corral /agents?... URL from validated Corral session
// and pane ids (never from untrusted log strings).
export function peerHref(sessionId: string, pane?: string): string {
  const sid = sanitizePeerText(sessionId)
  if (!sid) return ''
  const q = new URLSearchParams({ s: sid })
  const p = sanitizePeerText(pane)
  if (p) q.set('pane', p)
  return '/agents?' + q.toString()
}

export function peerSessionLabel(info: Pick<ConvoPeerInfo, 'display_name' | 'persistence' | 'mux_session'>): string {
  const dn = sanitizePeerText(info.display_name)
  if (dn) return dn
  const mux = sanitizePeerText(info.mux_session)
  if (info.persistence === 'tmux') return mux || 'tmux'
  if (info.persistence === 'herdr') return mux && mux !== 'default' ? 'herdr ' + mux : 'herdr'
  return mux
}

// Concise host/session context shown beside the peer's Corral agent title.
export function peerLocation(
  info: Pick<ConvoPeerInfo, 'host_name' | 'persistence' | 'mux_session' | 'display_name' | 'workspace'>,
  title: string,
): string {
  const host = sanitizePeerText(info.host_name)
  const sess = peerSessionLabel(info)
  const ws = sanitizePeerText(info.workspace)
  const parts: string[] = []
  if (host) parts.push(host)
  if (sess && sess !== title) parts.push(sess)
  else if (ws && ws !== '~' && ws !== title) parts.push(ws)
  return parts.join(' · ')
}

export function resolvePeerSender(input: {
  from?: string
  peerPid?: number
  peerStart?: number
  peers?: Record<string, ConvoPeerInfo>
}): ResolvedPeer {
  const rawFrom = sanitizePeerText(input.from)
  const key = peerRefKey(input.peerPid, input.peerStart)
  const info = key && input.peers ? input.peers[key] : undefined
  if (!info) {
    return { kind: 'unknown', name: rawFrom || '?' }
  }
  const name = sanitizePeerText(info.title)
  const sessionId = sanitizePeerText(info.session_id)
  const pane = sanitizePeerText(info.pane) || undefined
  const href = sessionId ? peerHref(sessionId, pane) : ''
  if (!name || !sessionId || !href) {
    return { kind: 'unknown', name: rawFrom || '?' }
  }
  return {
    kind: 'live',
    name,
    where: peerLocation(info, name),
    sessionId,
    pane,
    href,
  }
}

// Plain-text display label for Markdown exports and previews.
export function formatPeerPlain(
  r: ResolvedPeer,
  say: {
    from: (name: string) => string
    fromSession: (name: string) => string
    notInCorral: string
  },
): string {
  if (r.kind === 'live') {
    const head = say.from(r.name)
    return r.where ? `${head} (${r.where})` : head
  }
  return `${say.fromSession(r.name)} (${say.notInCorral})`
}

// Resolve click modifier to existing agent tab policy (plain click = current
// tab, Cmd/Ctrl = new tab, Alt/Option = split beside).
export function peerClickHow(e: { metaKey: boolean; ctrlKey: boolean; altKey: boolean }): PeerPickHow | undefined {
  return e.metaKey || e.ctrlKey ? 'tab' : e.altKey ? 'split' : undefined
}
