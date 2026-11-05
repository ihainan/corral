import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { CircleStop, Code2, MoreHorizontal, Pencil, Plus, Search, Server, Trash2, X, Eye } from 'lucide-react'
import { api, authGen, type Host, type SessionInfo, type SessionPreview, type SharedSession } from '../api'
import { relTime, t } from '../i18n'
import { linkHandler, navigate } from '../router'
import { Confirm, EmptyState, LoadError, Menu } from '../components/ui'
import { useConnect } from '../connect'
import { useSession } from '../app'
import { useCode } from '../terminal/CodeDialog'
import { termScheme, usePrefs } from '../prefs'
import { noteSessions, sessionAgents } from '../terminal/agents'
import { sessionWhich } from '../terminal/sessionName'
import { local } from '../storage'
import { useMedia } from '../components/ui'

const HOST_ORDER = 'corral.hostOrder'
const HOST_SEARCH_FROM = 6 // (hosts: more than this, and a search shows)

export function HostsPage() {
  const [hosts, setHosts] = useState<Host[] | null>(null)
  const [failed, setFailed] = useState(false)
  const [sessions, setSessions] = useState<SessionInfo[]>([])
  const [shared, setShared] = useState<SharedSession[]>([]) // others' sessions this user may watch
  const [sharedFailed, setSharedFailed] = useState(false)
  const [sessFailed, setSessFailed] = useState(false)
  const [confirm, setConfirm] = useState<React.ReactNode>(null)

  // (An answer that comes after the page was left, or after another account
  // signed in, is not this list's — nor the navigation's: review b006ffd.)
  const alive = useRef(true)
  useEffect(() => {
    alive.current = true
    return () => void (alive.current = false)
  }, [])
  const refreshSessions = useCallback(() => {
    const gen = authGen()
    const ours = () => alive.current && gen === authGen()
    api<SharedSession[]>('GET', '/api/shared')
      .then((l) => {
        setShared(l)
        setSharedFailed(false)
      })
      .catch(() => setSharedFailed(true))
    // (Told to the navigation too: its count of agents waiting is this list's.)
    return api<SessionInfo[]>('GET', '/api/sessions')
      .then((x) => {
        if (!ours()) return
        setSessions(x)
        setSessFailed(false)
        noteSessions(x)
      })
      .catch(() => {
        if (!ours()) return
        setSessFailed(true)
        noteSessions(null)
      })
  }, [])
  const load = useCallback(() => {
    api<Host[]>('GET', '/api/hosts')
      .then((h) => {
        setHosts(h)
        setFailed(false)
      })
      .catch(() => setFailed(true))
    void refreshSessions()
  }, [refreshSessions])

  useEffect(() => {
    load()
    const onVisible = () => document.visibilityState === 'visible' && load()
    document.addEventListener('visibilitychange', onVisible)
    const timer = setInterval(() => document.visibilityState === 'visible' && refreshSessions(), 10000)
    return () => {
      document.removeEventListener('visibilitychange', onVisible)
      clearInterval(timer)
    }
  }, [load, refreshSessions])

  const { connect, busyHost, dialogs } = useConnect((s) => navigate('/t/' + s.id))
  const { boot } = useSession()
  const code = useCode()
  const refreshCode = code.refresh
  useEffect(() => {
    if (boot.code && hosts) refreshCode(hosts.filter((h) => h.auth !== 'ask').map((h) => h.id))
  }, [boot.code, hosts, refreshCode])
  // (Those with agents waiting for the user first, then as they were opened.)
  const waitingIn = (s: SessionInfo) => {
    const a = sessionAgents(s)
    return a && !a.lost && !sessFailed ? a.waiting : 0
  }
  const running = sessions
    .filter((s) => s.state === 'running')
    .sort((a, b) => Number(waitingIn(b) > 0) - Number(waitingIn(a) > 0) || a.created_at - b.created_at)
  // A phone shows the running sessions as rows: a screen in small there is
  // too small to read (pages-1105 1.4), and is not fetched.
  const narrow = useMedia('(max-width: 599px)')

  // Each running session's latest screen, fetched again when the list says
  // a newer one was sent. Responses may arrive out of order: the newest one
  // stays. Sessions that are gone are forgotten.
  const [previews, setPreviews] = useState<Record<string, SessionPreview>>({})
  const asked = useRef<Record<string, number>>({}) // the newest revision asked for, per session
  useEffect(() => {
    const live = new Set(sessions.filter((s) => s.state === 'running').map((s) => s.id))
    for (const id of Object.keys(asked.current)) if (!live.has(id)) delete asked.current[id]
    setPreviews((cur) => {
      const gone = Object.keys(cur).filter((id) => !live.has(id))
      if (gone.length === 0) return cur
      const next = { ...cur }
      for (const id of gone) delete next[id]
      return next
    })
    if (narrow) return
    for (const s of sessions) {
      const rev = s.preview_rev ?? 0
      if (!live.has(s.id) || rev === 0 || (asked.current[s.id] ?? 0) >= rev) continue
      asked.current[s.id] = rev
      api<SessionPreview>('GET', `/api/sessions/${s.id}/preview`)
        .then((p) => setPreviews((cur) => (!(s.id in asked.current) || (cur[s.id]?.rev ?? 0) >= p.rev ? cur : { ...cur, [s.id]: p })))
        .catch(() => {
          if (asked.current[s.id] === rev) delete asked.current[s.id] // (asked again next time)
        })
    }
  }, [sessions, narrow])

  // The confirmation names exactly what the row showed, plus when it started,
  // so sessions without a title (or with the same one) can be told apart.
  const closeSession = (s: SessionInfo, label: string) =>
    setConfirm(
      <Confirm
        title={t('hosts.closeSessionOf', { name: `${s.host_name}：${label}（${t('hosts.startedAgo', { when: relTime(s.created_at) })} · #${s.id.slice(0, 6)}）` })}
        body={t('hosts.closeSessionConfirm')}
        confirm={t('hosts.closeSession')}
        danger
        onClose={() => setConfirm(null)}
        onConfirm={async () => {
          await api('DELETE', '/api/sessions/' + s.id)
          load()
        }}
      />,
    )
  const deleteHost = (h: Host) =>
    setConfirm(
      <Confirm
        title={t('host.delete')}
        body={t('hosts.deleteConfirm', { name: h.name })}
        confirm={t('common.delete')}
        danger
        onClose={() => setConfirm(null)}
        onConfirm={async () => {
          await api('DELETE', '/api/hosts/' + h.id)
          load()
        }}
      />,
    )

  const empty = hosts !== null && hosts.length === 0
  // Finding a host among many (pages-1105 1.5): a search once there are more
  // than a few (by all there are, not by what it shows), and the order — by
  // when last connected, or by name — kept on this device.
  const [q, setQ] = useState('')
  const [order, setOrder] = useState<'recent' | 'name'>(() => (local.get(HOST_ORDER) === 'recent' ? 'recent' : 'name'))
  const findable = (hosts?.length ?? 0) > HOST_SEARCH_FROM || q !== ''
  const needle = q.trim().toLowerCase()
  const shownHosts = (hosts ?? [])
    .filter((h) => !needle || [h.name, h.address, h.username].some((x) => x.toLowerCase().includes(needle)))
    .sort((a, b) => (order === 'recent' ? b.last_connected_at - a.last_connected_at : 0) || a.name.localeCompare(b.name) || a.id - b.id)
  // Others' sessions this user may watch: under the running ones, not past
  // all the hosts (pages-1105 2.6) — and shown with no host of one's own too.
  const sharedSection = (
        <section className="section shared-sessions">
          <h2>{t('watch.sharedTitle')}</h2>
          {sharedFailed && <LoadError text={t('watch.sharedFailed')} onRetry={() => void refreshSessions()} />}
          <ul className="rows">
            {shared.map((s) => (
              <li className="row row-session" key={s.id}>
                <a className="row-main" href={'/w/' + s.id} onClick={linkHandler('/w/' + s.id)}>
                  <Eye className="row-icon" size={18} strokeWidth={1.75} />
                  <span className="row-title">{s.host_name}{s.title ? ' · ' + s.title : ''}</span>
                  <span className="row-meta" title={new Date(s.created_at * 1000).toLocaleString()}>{t('hosts.startedAgo', { when: relTime(s.created_at) })}</span>
                  <span className="row-sub">{t('watch.sharedBy', { owner: s.owner_name })}</span>
                </a>
              </li>
            ))}
          </ul>
        </section>
  )
  return (
    <main className="page">
      <div className="page-head">
        <h1>{t('hosts.title')}</h1>
        <div className="spacer" />
        {sessFailed && (
          <button className="link-btn quiet-note" onClick={refreshSessions}>{t('hosts.sessionsStale')}</button>
        )}
        {!empty && (
          <a className="btn btn-primary" href="/hosts/new" onClick={linkHandler('/hosts/new')}>
            <Plus size={16} />
            {t('hosts.add')}
          </a>
        )}
      </div>

      {running.length > 0 && (
        <section className="section live" aria-labelledby="live-title">
          <h2 id="live-title">{t('hosts.running')}</h2>
          <ul className={narrow ? 'live-rows' : 'live-grid'}>
            {running.map((s) => (
              <LiveCard key={s.id} s={s} all={running} row={narrow} stale={sessFailed} preview={previews[s.id]} onClose={(label) => closeSession(s, label)} />
            ))}
          </ul>
        </section>
      )}
      {failed && <LoadError onRetry={load} text={t('hosts.loadFailed')} />}
      {hosts === null && !failed && <p className="loading">{t('common.loading')}</p>}
      {empty && (shared.length > 0 || sharedFailed) && sharedSection}
      {empty && (
        <EmptyState
          icon={<Server size={20} strokeWidth={1.75} />}
          title={t('hosts.empty.title')}
          body={t('hosts.empty.body')}
          action={
            <a className="btn btn-primary" href="/hosts/new" onClick={linkHandler('/hosts/new')}>
              <Plus size={16} />
              {t('hosts.add')}
            </a>
          }
        />
      )}
      {!empty && (shared.length > 0 || sharedFailed) && sharedSection}
      {hosts && hosts.length > 0 && (
        <section className="section">
          {(running.length > 0 || shared.length > 0) && <h2>{t('hosts.all')}</h2>}
          {findable && (
            <div className="page-tools">
              <label className="page-search">
                <Search size={16} aria-hidden="true" />
                <input type="search" value={q} onChange={(e) => setQ(e.target.value)} placeholder={t('hosts.search')} aria-label={t('hosts.search')} />
              </label>
              <div className="seg page-order" role="group" aria-label={t('hosts.order')}>
                {(['name', 'recent'] as const).map((o) => (
                  <button
                    key={o}
                    type="button"
                    aria-pressed={order === o}
                    onClick={() => {
                      setOrder(o)
                      local.set(HOST_ORDER, o)
                    }}
                  >
                    {t(o === 'name' ? 'hosts.orderName' : 'hosts.orderRecent')}
                  </button>
                ))}
              </div>
            </div>
          )}
          {shownHosts.length === 0 && (
            <p className="page-none">
              {t('hosts.noMatch')}{' '}
              <button type="button" className="link-btn" onClick={() => setQ('')}>{t('common.clearSearch')}</button>
            </p>
          )}
          <ul className="rows">
            {shownHosts.map((h) => (
              <li key={h.id} className="row-group">
                <div className="row">
                  <button className="row-main" onClick={() => connect(h)} disabled={busyHost === h.id} aria-label={`${t('hosts.connect')} ${h.name}`} aria-describedby={'hs-' + h.id}>
                    <Server className="row-icon" size={18} strokeWidth={1.75} />
                    <span className="row-title">{h.name}</span>
                    <span className="row-meta">
                      {/* ("8 天前连接": which time it is — ux1101 PG-11.) */}
                      {busyHost === h.id ? t('hosts.connecting') : h.last_connected_at ? t('hosts.lastConnected', { when: relTime(h.last_connected_at) }) : ''}
                    </span>
                    <span className="row-sub" id={'hs-' + h.id}>
                      {h.username}@{h.address}
                      {h.port !== 22 ? ':' + h.port : ''}
                      {h.jump_host_id ? ' · ' + t('host.via', { name: hosts?.find((x) => x.id === h.jump_host_id)?.name ?? '?' }) : ''}
                    </span>
                  </button>
                  <div className="row-actions">
                    <Menu
                      label={h.name}
                      button={(open, toggle) => (
                        <button className="icon-btn" aria-label={`${t('common.more')}：${h.name}`} aria-expanded={open} onClick={toggle}>
                          <MoreHorizontal size={18} />
                        </button>
                      )}
                      items={() => [
                        { label: t('common.edit'), icon: <Pencil size={16} />, href: `/hosts/${h.id}/edit`, onSelect: () => navigate(`/hosts/${h.id}/edit`) },
                        boot.code && h.auth !== 'ask' ? { label: t('code.open'), icon: <Code2 size={16} />, onSelect: () => code.open(h.id, h.name) } : null,
                        boot.code && code.running[h.id] ? { label: t('code.stop'), icon: <CircleStop size={16} />, onSelect: () => code.stop(h.id) } : null,
                        { label: t('host.delete'), icon: <Trash2 size={16} />, danger: true, separator: true, onSelect: () => deleteHost(h) },
                      ]}
                    />
                  </div>
                </div>
              </li>
            ))}
          </ul>
        </section>
      )}
      {confirm}
      {dialogs}
      {code.dialog}
    </main>
  )
}

// A running session: its screen in small (as a window of the owner last
// showed it) — on a phone a row, without it —, the host and which session of
// it this is (two shells of one host told apart, pages-1105 1.4); herdr
// agents waiting for an answer are named first. Their state when herdr was
// lost since, or the list could not be read just now (stale), is not told
// as now: it is said to be unknown (1.2).
function LiveCard({ s, all, row, stale, preview, onClose }: { s: SessionInfo; all: SessionInfo[]; row: boolean; stale: boolean; preview?: SessionPreview; onClose: (label: string) => void }) {
  usePrefs() // (the screen follows the terminal colours)
  const theme = termScheme().theme
  const which = sessionWhich(s, all)
  const ag = sessionAgents(s)
  const unknown = !!ag && (ag.lost || stale)
  const waiting = ag && !unknown ? ag.waiting : 0
  const done = ag && !unknown ? ag.done : 0 // finished, not looked at since
  const working = ag && !unknown ? ag.working : 0
  const old = preview && Date.now() / 1000 - preview.at > 120
  const herdrState = unknown
    ? t('hosts.agentsUnknown')
    : waiting > 0 ? t('hosts.herdrWaiting', { n: waiting }) : done > 0 ? t('hosts.herdrDone', { n: done }) : working > 0 ? t('hosts.herdrWorking', { n: working }) : ''
  const marked = waiting > 0 || done > 0
  // (Agents waiting, or done and not seen, come first, in their colour.)
  const detail = [marked ? '' : herdrState, s.title, !s.title && !herdrState ? t('hosts.startedAgo', { when: relTime(s.created_at) }) : '', old && !row ? t('hosts.previewAge', { when: relTime(preview.at) }) : '']
    .filter(Boolean)
    .join(' · ')
  const label = `${which}${s.title ? ' · ' + s.title : ''}`
  const dot = unknown ? 'unknown' : waiting > 0 ? 'warn' : 'ok'
  return (
    <li className={row ? 'live-row' : 'live-card'}>
      <a className="live-link" href={'/t/' + s.id} onClick={linkHandler('/t/' + s.id)} aria-label={`${s.host_name}：${label}`} aria-describedby={'ld-' + s.id} title={s.title || undefined}>
        {row ? null : preview ? (
          <LiveScreen lines={preview.lines} bg={theme.background ?? '#000'} fg={theme.foreground ?? '#fff'} />
        ) : (
          <div className="live-screen" aria-hidden="true" style={{ background: theme.background, color: theme.foreground }}>
            <span className="live-empty">{t('hosts.noPreview')}</span>
          </div>
        )}
        <span className="live-info">
          <span className="live-title">
            <span className={'dot ' + dot} aria-hidden="true" />
            <span className="live-name">{s.host_name}</span>
            <span className="live-which">{which}</span>
          </span>
          <span className="live-sub" id={'ld-' + s.id}>
            {marked && <span className={waiting > 0 ? 'live-wait' : 'live-done'}>{herdrState}</span>}
            {marked && detail ? ' · ' : ''}
            {detail}
          </span>
        </span>
      </a>
      <button className="icon-btn live-close" aria-label={t('hosts.closeNamed', { name: `${s.host_name}：${label}` })} title={t('hosts.closeSession')} onClick={() => onClose(label)}>
        <X size={16} />
      </button>
    </li>
  )
}

// The screen in small: the whole of it, without blank lines at either end,
// scaled down to fit the card, however small (laid out at a fixed size, then shrunk, so the
// browser's smallest font size does not apply and wide characters measure as
// drawn); a few lines stay at a readable size rather than grow. Only a screen
// too big even at the smallest scale is cut: what does not fit fades out at
// the top (older lines) and the right.
const PREVIEW_BASE = 10 // px, the size it is laid out at
const PREVIEW_MAX = 11 / PREVIEW_BASE // the largest scale
const PREVIEW_MIN = 1.5 / PREVIEW_BASE // the smallest (on a phone a card is an outline of the screen)
const PREVIEW_LINE = 1.25
function LiveScreen({ lines, bg, fg }: { lines: string[]; bg: string; fg: string }) {
  const box = useRef<HTMLDivElement>(null)
  const pre = useRef<HTMLPreElement>(null)
  const [fit, setFit] = useState<{ scale: number; cutTop: boolean; cutRight: boolean } | null>(null)
  let first = 0
  let last = lines.length
  while (first < last && !lines[first].trim()) first++
  while (last > first && !lines[last - 1].trim()) last--
  const body = lines.slice(first, last).join('\n')
  useLayoutEffect(() => {
    const b = box.current
    const p = pre.current
    if (!b || !p) return
    const measure = () => {
      const cs = getComputedStyle(b)
      const w = b.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight)
      const h = b.clientHeight - parseFloat(cs.paddingTop) - parseFloat(cs.paddingBottom)
      // (The size as laid out: a transform does not change it.)
      const tw = p.scrollWidth
      const th = p.scrollHeight
      if (!w || !h || !tw || !th) return
      // (A little room: glyphs scaled down round wider than measured.)
      const scale = Math.min(PREVIEW_MAX, Math.max(PREVIEW_MIN, Math.min((0.98 * w) / tw, h / th)))
      const next = { scale, cutTop: th * scale > h + 1, cutRight: tw * scale > w + 1 }
      setFit((f) => (f && f.scale === next.scale && f.cutTop === next.cutTop && f.cutRight === next.cutRight ? f : next))
    }
    measure()
    const ro = new ResizeObserver(measure)
    ro.observe(b)
    return () => ro.disconnect()
  }, [body])
  return (
    <div
      ref={box}
      className={'live-screen' + (fit?.cutTop ? ' cut-top' : '') + (fit?.cutRight ? ' cut-right' : '')}
      aria-hidden="true"
      style={{ background: bg, color: fg, ['--live-bg' as string]: bg }}
    >
      <pre
        ref={pre}
        style={{ fontSize: PREVIEW_BASE + 'px', lineHeight: PREVIEW_LINE, transform: fit ? `scale(${fit.scale})` : undefined, visibility: fit ? undefined : 'hidden' }}
      >
        {body}
      </pre>
    </div>
  )
}
