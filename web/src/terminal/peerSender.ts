// Pure resolution and formatting helpers for cross-session peer messages
// (tested directly in web/scripts/test-peer-sender.mjs).

export type PeerPickHow = 'tab' | 'split'

// Mirrors internal/convo/convo.go PeerInfo.
export interface ConvoPeerInfo {
  key?: string
  session_id?: string
  host_id?: number
  host_name?: string
  persistence?: string
  mux_session?: string
  display_name?: string
  pane?: string
  workspace?: string
  title?: string
  program?: string
  alive?: boolean
}

export interface LivePeerTarget {
  key: string
  title: string
  where: string
  sessionGroup: string
  hostName: string
  sessionLabel: string
  sessionId: string
  pane?: string
  program?: string
  onPick: (how?: PeerPickHow) => void
}

export type ResolvedPeer =
  | {
      kind: 'live'
      name: string
      where: string
      detail?: string
      href: string
      program?: string
      rawFrom?: string
      onPick: (how?: PeerPickHow) => void
    }
  | {
      kind: 'exited'
      name: string
      where: string
      program?: string
      rawFrom?: string
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

function livePeerWhere(live: LivePeerTarget): string {
  const host = sanitizePeerText(live.hostName)
  const sess = sanitizePeerText(live.sessionLabel)
  const parts: string[] = []
  if (host) parts.push(host)
  if (sess && sess !== live.title) parts.push(sess)
  return parts.join(' · ') || sanitizePeerText(live.sessionGroup)
}

export function resolvePeerSender(input: {
  from?: string
  peerPid?: number
  peerStart?: number
  peers?: Record<string, ConvoPeerInfo>
  liveByKey?: ReadonlyMap<string, LivePeerTarget>
}): ResolvedPeer {
  const rawFrom = sanitizePeerText(input.from)
  const key = peerRefKey(input.peerPid, input.peerStart)
  const info = key && input.peers ? input.peers[key] : undefined
  if (!info) {
    return { kind: 'unknown', name: rawFrom || '?' }
  }
  if (info.alive && info.key) {
    const live = input.liveByKey?.get(info.key)
    if (live) {
      const href = peerHref(live.sessionId, live.pane)
      if (href) {
        return {
          kind: 'live',
          name: sanitizePeerText(live.title) || sanitizePeerText(info.title) || rawFrom || '?',
          where: livePeerWhere(live),
          detail: sanitizePeerText(live.where) || undefined,
          href,
          program: live.program ?? info.program,
          rawFrom: rawFrom || undefined,
          onPick: live.onPick,
        }
      }
    }
  }
  const name = sanitizePeerText(info.title) || rawFrom || '?'
  return {
    kind: 'exited',
    name,
    where: peerLocation(info, name),
    program: info.program,
    rawFrom: rawFrom || undefined,
  }
}

// Resolve a SendMessage tool call's target name for step headers, sidebar
// previews, and hover cards. Returns null when the tool is not a verified
// cross-session peer message.
export function resolvePeerToolTarget(
  tool: { name: string; title?: string; peer_pid?: number; peer_start?: number },
  peers?: Record<string, ConvoPeerInfo>,
  liveByKey?: ReadonlyMap<string, LivePeerTarget>,
): ResolvedPeer | null {
  if (tool.name !== 'SendMessage') return null
  if (!peerRefKey(tool.peer_pid, tool.peer_start)) return null
  return resolvePeerSender({
    from: tool.title,
    peerPid: tool.peer_pid,
    peerStart: tool.peer_start,
    peers,
    liveByKey,
  })
}

// Plain-text display label for Markdown exports and previews.
export function formatPeerPlain(
  r: ResolvedPeer,
  say: {
    from: (name: string) => string
    fromSession: (name: string) => string
    notInCorral: string
    exited: string
  },
): string {
  if (r.kind === 'live') {
    const head = say.from(r.name)
    return r.where ? `${head} (${r.where})` : head
  }
  if (r.kind === 'exited') {
    const head = say.from(r.name)
    const meta = [r.where, say.exited].filter(Boolean).join(' · ')
    return meta ? `${head} (${meta})` : head
  }
  return `${say.fromSession(r.name)} (${say.notInCorral})`
}

// Resolve click modifier to existing agent tab policy (plain click = current
// tab, Cmd/Ctrl = new tab, Alt/Option = split beside).
export function peerClickHow(e: { metaKey: boolean; ctrlKey: boolean; altKey: boolean }): PeerPickHow | undefined {
  return e.metaKey || e.ctrlKey ? 'tab' : e.altKey ? 'split' : undefined
}
