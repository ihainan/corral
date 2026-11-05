import { useCallback, useEffect, useRef, useState } from 'react'
import { Check, ChevronDown, CircleStop, Code2, MoreHorizontal, Pencil, Plus, Search, Server, Trash2, X, Eye } from 'lucide-react'
import { api, authGen, type Host, type SessionInfo, type SharedSession } from '../api'
import { relTime, t } from '../i18n'
import { linkHandler, navigate } from '../router'
import { Confirm, EmptyState, LoadError, Menu } from '../components/ui'
import { useConnect } from '../connect'
import { useFlip } from '../flip'
import { useSession } from '../app'
import { useCode } from '../terminal/CodeDialog'
import { noteSessions, sessionAgents, waitingAgents } from '../terminal/agents'
import { sessionWhich } from '../terminal/sessionName'
import { local } from '../storage'

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
  // Each host's sessions, as its row says them (11-05): agents waiting, in
  // their colour and weight, else that it runs — words, no badge.
  const hostState = (id: number): { text: string; wait: boolean } | null => {
    const mine = running.filter((x) => x.host_id === id)
    if (!mine.length) return null
    // (Each agent once, however many of its sessions show it — as the navigation counts: review.)
    const w = sessFailed ? 0 : waitingAgents(mine)
    if (w) return { text: t('hosts.herdrWaiting', { n: w }), wait: true }
    return { text: mine.length > 1 ? t('hosts.sessionsRunning', { n: mine.length }) : t('hosts.runningOne'), wait: false }
  }
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
  // A session opened or closed elsewhere, come in by the poll: what is under
  // it slides to its place, the new row fading in (motion-1105). Not for a
  // search or a sort, the person's own.
  const pageEl = useRef<HTMLElement>(null)
  useFlip(pageEl, needle + '|' + order)
  // Others' sessions this user may watch: under the running ones, not past
  // all the hosts (pages-1105 2.6) — and shown with no host of one's own too.
  const sharedSection = (
        <section className="section shared-sessions" data-flip="shared">
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
    <main className="page" ref={pageEl}>
      <div className="page-head">
        <h1>{t('hosts.title')}</h1>
        <div className="spacer" />
        {sessFailed && (
          <button className="link-btn quiet-note" onClick={refreshSessions}>{t('hosts.sessionsStale')}</button>
        )}
        {!empty && (
          // (Outlined: what is going on, below, is what is to be seen first — 11-05.)
          <a className="btn" href="/hosts/new" onClick={linkHandler('/hosts/new')}>
            <Plus size={16} />
            {t('hosts.add')}
          </a>
        )}
      </div>

      {running.length > 0 && (
        <section className="section live" aria-labelledby="live-title">
          <h2 id="live-title" data-flip="live" data-flip-in="">{t('hosts.running')}<span className="section-n">{running.length}</span></h2>
          <ul className="live-rows">
            {running.map((s) => (
              <LiveCard key={s.id} s={s} all={running} stale={sessFailed} onClose={(label) => closeSession(s, label)} />
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
        <section className="section" data-flip="hosts">
          {(running.length > 0 || shared.length > 0) && <h2>{t('hosts.all')}<span className="section-n">{hosts.length}</span></h2>}
          {findable && (
            <div className="page-tools">
              <label className="page-search">
                <Search size={16} aria-hidden="true" />
                <input type="search" value={q} onChange={(e) => setQ(e.target.value)} placeholder={t('hosts.search')} aria-label={t('hosts.searchLong')} />
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
              {/* (A phone's: one button beside the search, the choice in its menu — 11-05.) */}
              <Menu
                label={t('hosts.order')}
                items={() =>
                  (['name', 'recent'] as const).map((o) => ({
                    label: t(o === 'name' ? 'hosts.orderName' : 'hosts.orderRecent'),
                    checked: order === o,
                    icon: order === o ? <Check size={16} /> : <span className="menu-icon-space" />,
                    onSelect: () => {
                      setOrder(o)
                      local.set(HOST_ORDER, o)
                    },
                  }))
                }
                button={(open, toggle) => (
                  <button type="button" className="btn page-order-menu" aria-expanded={open} aria-haspopup="menu" onClick={toggle}>
                    {t('hosts.order')}
                    <ChevronDown size={15} />
                  </button>
                )}
              />
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
                  <button className="row-main no-icon has-state" onClick={() => connect(h)} disabled={busyHost === h.id} aria-label={`${t('hosts.connect')} ${h.name}`} aria-describedby={'hs-' + h.id}>
                    <span className="row-title">{h.name}</span>
                    {(() => {
                      const st = hostState(h.id)
                      return st ? <span className={'row-state ' + (st.wait ? 'is-wait' : 'is-run')}>{st.text}</span> : <span className="row-state" />
                    })()}
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

// A running session, a row (11-05: no screen in small — at a card's size it
// told little, and without one a black block took the page): the host and
// which session of it this is (two shells of one host told apart, pages-1105
// 1.4); herdr agents waiting for an answer said first, in their colour. Their
// state when herdr was lost since, or the list could not be read just now
// (stale), is not told as now: it is said to be unknown (1.2).
function LiveCard({ s, all, stale, onClose }: { s: SessionInfo; all: SessionInfo[]; stale: boolean; onClose: (label: string) => void }) {
  const which = sessionWhich(s, all)
  const ag = sessionAgents(s)
  const unknown = !!ag && (ag.lost || stale)
  const waiting = ag && !unknown ? ag.waiting : 0
  const done = ag && !unknown ? ag.done : 0 // finished, not looked at since
  const working = ag && !unknown ? ag.working : 0
  const herdrState = unknown
    ? t('hosts.agentsUnknown')
    : waiting > 0 ? t('hosts.herdrWaiting', { n: waiting }) : done > 0 ? t('hosts.herdrDone', { n: done }) : working > 0 ? t('hosts.herdrWorking', { n: working }) : ''
  const marked = waiting > 0 || done > 0
  const detail = [marked ? '' : herdrState, s.title, !s.title && !herdrState ? t('hosts.startedAgo', { when: relTime(s.created_at) }) : ''].filter(Boolean).join(' · ')
  const label = `${which}${s.title ? ' · ' + s.title : ''}`
  const dot = unknown ? 'unknown' : waiting > 0 ? 'warn' : 'ok'
  return (
    <li className="live-row" data-flip={'l:' + s.id} data-flip-in="">
      <a className="live-link" href={'/t/' + s.id} onClick={linkHandler('/t/' + s.id)} aria-label={`${s.host_name}：${label}`} aria-describedby={'ld-' + s.id} title={s.title || undefined}>
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
