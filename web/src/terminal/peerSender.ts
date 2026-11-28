// Pure resolution and formatting helpers for cross-session peer messages
// (tested directly in web/scripts/test-peer-sender.mjs).
//
// Display rule: the backend decides whether a peer's verified (pid, proc_start)
// matches a currently-selected Corral agent in the latest locate scan (and
// enforces the 10-minute slot age limit on every poll). The frontend does no
// identity matching — it only names the backend-matched (session_id, pane)
// using the sidebar's own naming function (peerTitles / sidebarHerdrTitle) so
// sidebar and peer sender names cannot drift. If refresh is failing, or if the
// backend did not supply a complete Corral target, show the message's own
// sender name + "Not in Corral".

export type PeerPickHow = 'tab' | 'split'

// Canonical target key for looking up a sidebar row title by (sessionId, pane).
export function peerTargetKey(sessionId: string, pane?: string): string {
  return `${sessionId}\x00${pane ?? ''}`
}

// Pure sidebar title rule for a herdr pane, shared by AgentsChat.tsx's labelOf
// and peer sender display so they cannot drift.
// Precedence:
//   1. Custom pane label always wins.
//   2. Same-workspace lineage child without a custom label shows its own name
//      rather than duplicating its parent's workspace name.
//   3. Plain workspace agent (not a same-workspace lineage child, ws !== '~')
//      keeps the workspace name.
//   4. Otherwise falls back to said.
export function sidebarHerdrTitle(p: {
  ws?: string
  label?: string
  agentName?: string
  agent?: string
  said: string
  named?: boolean
  sameWsParent?: boolean
}): string {
  const ws = p.ws && p.ws !== '~' ? p.ws : ''
  const own = p.label ?? ''
  const agentName = p.agentName ?? ''
  const named = p.named ?? true
  if (p.sameWsParent && !own) {
    return (named && p.said) || agentName || p.agent || p.said
  }
  if (!ws && !own) return p.said
  return own || ws
}

// Whether conversation peer data may render live links: withhold only while
// refreshes are failing. The server already enforces the 10-minute slot age
// limit on every poll, whereas healthy unchanged polls intentionally keep the
// prior state (and its okAt) without re-rendering.
export function peerFresh(opts: { failing?: boolean }): boolean {
  return !opts.failing
}

// Normalize a poll answer's peers map and reuse prev only when deeply equal,
// so healthy unchanged polls do not re-render while any peer addition, removal,
// or field change produces a new reference that updates conversation state.
export function nextConvoPeers(
  prev: Record<string, ConvoPeerInfo> | undefined,
  incoming: Record<string, ConvoPeerInfo> | undefined,
): Record<string, ConvoPeerInfo> | undefined {
  if (!incoming || !Object.keys(incoming).length) return undefined
  return JSON.stringify(incoming) === JSON.stringify(prev) ? prev : incoming
}

// Build the (sessionId, pane) -> sidebar row title map for peer sender naming.
// Includes both live herdr panes and tmux entries, and registers each sidebar
// row's title under every session connection merged into that deduped row.
export function buildPeerTitles<S extends { id: string; persistence?: string }>(
  entries: ReadonlyArray<{
    key: string
    sessionId: string
    pane?: string
    isHerdr: boolean
    lost?: boolean
    title: string
  }>,
  allSessions: ReadonlyArray<S>,
  keyFor: {
    herdr: (s: S, pane: string) => string
    tmux: (s: S) => string
  },
): Map<string, string> {
  const map = new Map<string, string>()
  for (const e of entries) {
    if (!e.title) continue
    if (e.isHerdr) {
      if (e.lost || !e.pane) continue
      map.set(peerTargetKey(e.sessionId, e.pane), e.title)
      for (const s of allSessions) {
        if (s.persistence === 'herdr' && keyFor.herdr(s, e.pane) === e.key) {
          map.set(peerTargetKey(s.id, e.pane), e.title)
        }
      }
    } else {
      map.set(peerTargetKey(e.sessionId), e.title)
      for (const s of allSessions) {
        if (s.persistence === 'tmux' && keyFor.tmux(s) === e.key) {
          map.set(peerTargetKey(s.id), e.title)
        }
      }
    }
  }
  return map
}

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
  const loc = parts.join(' · ')
  return loc === title ? '' : loc
}

export function resolvePeerSender(input: {
  from?: string
  peerPid?: number
  peerStart?: number
  peers?: Record<string, ConvoPeerInfo>
  peerTitles?: ReadonlyMap<string, string>
  failing?: boolean
}): ResolvedPeer {
  const rawFrom = sanitizePeerText(input.from)
  if (!peerFresh({ failing: input.failing })) {
    return { kind: 'unknown', name: rawFrom || '?' }
  }
  const key = peerRefKey(input.peerPid, input.peerStart)
  const info = key && input.peers ? input.peers[key] : undefined
  if (!info) {
    return { kind: 'unknown', name: rawFrom || '?' }
  }
  const sessionId = sanitizePeerText(info.session_id)
  const pane = sanitizePeerText(info.pane) || undefined
  const sidebarName = sessionId && input.peerTitles ? sanitizePeerText(input.peerTitles.get(peerTargetKey(sessionId, pane))) : ''
  const name = sidebarName || sanitizePeerText(info.title)
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
