import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { CircleStop, Code2, MoreHorizontal, Pencil, Plus, Server, Trash2, X, Eye } from 'lucide-react'
import { api, type Host, type SessionInfo, type SessionPreview, type SharedSession } from '../api'
import { relTime, t } from '../i18n'
import { linkHandler, navigate } from '../router'
import { Confirm, EmptyState, LoadError, Menu } from '../components/ui'
import { useConnect } from '../connect'
import { useSession } from '../app'
import { useCode } from '../terminal/CodeDialog'
import { termScheme, usePrefs } from '../prefs'

export function HostsPage() {
  const [hosts, setHosts] = useState<Host[] | null>(null)
  const [failed, setFailed] = useState(false)
  const [sessions, setSessions] = useState<SessionInfo[]>([])
  const [shared, setShared] = useState<SharedSession[]>([]) // others' sessions this user may watch
  const [sharedFailed, setSharedFailed] = useState(false)
  const [sessFailed, setSessFailed] = useState(false)
  const [confirm, setConfirm] = useState<React.ReactNode>(null)

  const refreshSessions = useCallback(() => {
    api<SharedSession[]>('GET', '/api/shared')
      .then((l) => {
        setShared(l)
        setSharedFailed(false)
      })
      .catch(() => setSharedFailed(true))
    return api<SessionInfo[]>('GET', '/api/sessions')
      .then((x) => {
        setSessions(x)
        setSessFailed(false)
      })
      .catch(() => setSessFailed(true))
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
  const running = sessions.filter((s) => s.state === 'running').sort((a, b) => a.created_at - b.created_at)

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
  }, [sessions])

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
          <ul className="live-grid">
            {running.map((s) => (
              <LiveCard key={s.id} s={s} preview={previews[s.id]} onClose={(label) => closeSession(s, label)} />
            ))}
          </ul>
        </section>
      )}
      {failed && <LoadError onRetry={load} text={t('hosts.loadFailed')} />}
      {hosts === null && !failed && <p className="loading">{t('common.loading')}</p>}
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
      {hosts && hosts.length > 0 && (
        <section className="section">
          {running.length > 0 && <h2>{t('hosts.all')}</h2>}
          <ul className="rows">
            {hosts.map((h) => (
              <li key={h.id} className="row-group">
                <div className="row">
                  <button className="row-main" onClick={() => connect(h)} disabled={busyHost === h.id} aria-label={`${t('hosts.connect')} ${h.name}`} aria-describedby={'hs-' + h.id}>
                    <Server className="row-icon" size={18} strokeWidth={1.75} />
                    <span className="row-title">{h.name}</span>
                    <span className="row-meta">
                      {busyHost === h.id ? t('hosts.connecting') : h.last_connected_at ? relTime(h.last_connected_at) : ''}
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
      {(shared.length > 0 || sharedFailed) && (
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
      )}
      {confirm}
      {dialogs}
      {code.dialog}
    </main>
  )
}

// A running session: its screen in small (as a window of the owner last
// showed it), the host, and what it runs in; herdr agents waiting for an
// answer are named first.
function LiveCard({ s, preview, onClose }: { s: SessionInfo; preview?: SessionPreview; onClose: (label: string) => void }) {
  usePrefs() // (the screen follows the terminal colours)
  const theme = termScheme().theme
  const kind = s.persistence === 'tmux' ? 'tmux · ' + (s.mux_session ?? '') : s.persistence === 'herdr' ? 'herdr' + (s.mux_session ? ' · ' + s.mux_session : '') : t('host.persistNone')
  const waiting = s.meta?.t === 'herdr' ? s.meta.waiting : 0
  const working = s.meta?.t === 'herdr' ? s.meta.workspaces.reduce((n, w) => n + w.agents.filter((a) => a.status === 'working').length, 0) : 0
  const old = preview && Date.now() / 1000 - preview.at > 120
  const herdrState = waiting > 0 ? t('hosts.herdrWaiting', { n: waiting }) : working > 0 ? t('hosts.herdrWorking', { n: working }) : ''
  const detail = [herdrState, kind, old ? t('hosts.previewAge', { when: relTime(preview.at) }) : '']
    .filter(Boolean)
    .join(' · ')
  const label = s.title || kind
  return (
    <li className="live-card">
      <a className="live-link" href={'/t/' + s.id} onClick={linkHandler('/t/' + s.id)} aria-label={`${s.host_name}：${label}`} aria-describedby={'ld-' + s.id} title={s.title || undefined}>
        {preview ? (
          <LiveScreen lines={preview.lines} cols={preview.cols} bg={theme.background ?? '#000'} fg={theme.foreground ?? '#fff'} />
        ) : (
          <div className="live-screen" aria-hidden="true" style={{ background: theme.background, color: theme.foreground }}>
            <span className="live-empty">{t('hosts.noPreview')}</span>
          </div>
        )}
        <span className="live-info">
          <span className="live-title">
            <span className={'dot ' + (waiting > 0 ? 'warn' : 'ok')} aria-hidden="true" />
            <span className="live-name">{s.host_name}</span>
          </span>
          <span className="live-sub" id={'ld-' + s.id}>{detail}</span>
        </span>
      </a>
      <button className="icon-btn live-close" aria-label={t('hosts.closeNamed', { name: `${s.host_name}：${label}` })} title={t('hosts.closeSession')} onClick={() => onClose(label)}>
        <X size={16} />
      </button>
    </li>
  )
}

// The screen in small: its last lines, without blank ones at either end, in
// whole lines; the text as large as the card's width allows for the
// terminal's width, but not smaller than legible. What does not fit fades out
// at the top (older lines) and the right (a wider terminal).
const PREVIEW_BASE = 10 // px, the size the probe is measured at
const PREVIEW_MIN = 6 // px
const PREVIEW_MAX = 11
const PREVIEW_LINE = 1.25
function LiveScreen({ lines, cols, bg, fg }: { lines: string[]; cols: number; bg: string; fg: string }) {
  const box = useRef<HTMLDivElement>(null)
  const pre = useRef<HTMLPreElement>(null)
  const probe = useRef<HTMLSpanElement>(null)
  const [fit, setFit] = useState<{ font: number; rows: number } | null>(null)
  // Cut on the right: told by the width drawn (wide and combining characters).
  const [cutRight, setCutRight] = useState(false)
  let first = 0
  let last = lines.length
  while (first < last && !lines[first].trim()) first++
  while (last > first && !lines[last - 1].trim()) last--
  const text = lines.slice(first, last)
  useLayoutEffect(() => {
    const b = box.current
    const p = pre.current
    const m = probe.current
    if (!b || !p || !m) return
    const measure = () => {
      const w = p.clientWidth
      const h = b.clientHeight - parseFloat(getComputedStyle(b).paddingTop) - parseFloat(getComputedStyle(b).paddingBottom)
      const cell = m.offsetWidth / Math.max(1, cols) // at the base size
      if (!w || !cell) return
      // (A little room: glyphs scaled down round wider than measured.)
      const font = Math.min(PREVIEW_MAX, Math.max(PREVIEW_MIN, (0.96 * PREVIEW_BASE * w) / (cell * Math.max(cols, 20))))
      const next = { font, rows: Math.max(1, Math.floor(h / (font * PREVIEW_LINE))) }
      setFit((f) => (f && f.font === next.font && f.rows === next.rows ? f : next))
      setCutRight(p.scrollWidth > p.clientWidth + 1) // (again once a new size applies, below)
    }
    measure()
    const ro = new ResizeObserver(measure)
    ro.observe(b)
    return () => ro.disconnect()
  }, [cols])
  const shown = fit ? text.slice(Math.max(0, text.length - fit.rows)) : text
  const cutTop = !!fit && text.length > fit.rows
  const body = shown.join('\n')
  useLayoutEffect(() => {
    const p = pre.current
    if (p && fit) setCutRight(p.scrollWidth > p.clientWidth + 1)
  }, [fit, body])
  return (
    <div
      ref={box}
      className={'live-screen' + (cutTop ? ' cut-top' : '') + (cutRight ? ' cut-right' : '')}
      aria-hidden="true"
      style={{ background: bg, color: fg, ['--live-bg' as string]: bg }}
    >
      <span ref={probe} className="live-probe">{'0'.repeat(Math.max(1, cols))}</span>
      <pre ref={pre} style={{ fontSize: fit ? fit.font + 'px' : undefined, lineHeight: PREVIEW_LINE, visibility: fit ? undefined : 'hidden' }}>
        {body}
      </pre>
    </div>
  )
}
