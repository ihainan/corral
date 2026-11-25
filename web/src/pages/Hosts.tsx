import { useCallback, useEffect, useRef, useState } from 'react'
import { Check, ChevronDown, CircleStop, Code2, MoreHorizontal, Pencil, Plus, Search, Server, SquareTerminal, Trash2, X, Eye } from 'lucide-react'
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
import { useDeskPages } from '../workbench'

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
  const hues = hostHues(hosts)
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
  const desk = useDeskPages()
  const [sel, setSel] = useState<number | null>(null)
  const hostMenu = (h: Host) => [
    { label: t('common.edit'), icon: <Pencil size={16} />, href: `/hosts/${h.id}/edit`, onSelect: () => navigate(`/hosts/${h.id}/edit`) },
    boot.code && h.auth !== 'ask' ? { label: t('code.open'), icon: <Code2 size={16} />, onSelect: () => code.open(h.id, h.name) } : null,
    boot.code && code.running[h.id] ? { label: t('code.stop'), icon: <CircleStop size={16} />, onSelect: () => code.stop(h.id) } : null,
    { label: t('host.delete'), icon: <Trash2 size={16} />, danger: true, separator: true, onSelect: () => deleteHost(h) },
  ]
  const hostAddr = (h: Host) => `${h.username}@${h.address}${h.port !== 22 ? ':' + h.port : ''}${h.jump_host_id ? ' · ' + t('host.via', { name: hosts?.find((x) => x.id === h.jump_host_id)?.name ?? '?' }) : ''}`
  // Others' sessions this user may watch: under the running ones, not past
  // all the hosts (pages-1105 2.6) — and shown with no host of one's own too.
  const sharedSection = (
        <section className="section section-group shared-sessions" data-flip="shared">
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
  // A desktop's (UI 12): a toolbar — the page's name and count, the search, adding one; the running sessions a strip
  // under it; the hosts a table, a row chosen by a click or ↑↓, opened by a double click or Return, its menu at its
  // ⋯ or by a right click.
  if (desk) {
    const at = shownHosts.findIndex((h) => h.id === sel)
    const rowEl = (id: number) => pageEl.current?.querySelector<HTMLElement>(`tr[data-host="${id}"]`)
    const go = (i: number) => {
      const h = shownHosts[Math.max(0, Math.min(shownHosts.length - 1, i))]
      if (!h) return
      setSel(h.id)
      rowEl(h.id)?.focus()
    }
    return (
      <main className="page page-desk" ref={pageEl}>
        <div className="desk-bar">
          <h1>{t('hosts.title')}</h1>
          {hosts && hosts.length > 0 && <span className="desk-n">{hosts.length}</span>}
          <div className="spacer" />
          {sessFailed && (
            <button className="link-btn quiet-note" onClick={refreshSessions}>{t('hosts.sessionsStale')}</button>
          )}
          {hosts && hosts.length > 0 && (
            <label className="page-search desk-search">
              <Search size={15} aria-hidden="true" />
              <input type="search" value={q} onChange={(e) => setQ(e.target.value)} placeholder={t('hosts.search')} aria-label={t('hosts.searchLong')} />
            </label>
          )}
          {!empty && (
            <a className="btn desk-btn" href="/hosts/new" onClick={linkHandler('/hosts/new')}>
              <Plus size={15} />
              {t('hosts.add')}
            </a>
          )}
        </div>
        {running.length > 0 && (
          <section className="desk-strip" aria-labelledby="live-title">
            <h2 id="live-title" className="desk-strip-label">{t('hosts.running')}</h2>
            <ul className="desk-strip-list">
              {running.map((s) => (
                <LiveCard key={s.id} s={s} all={running} stale={sessFailed} hue={hues.get(s.host_id)} onClose={(label) => closeSession(s, label)} pill />
              ))}
            </ul>
          </section>
        )}
        <div className="desk-body">
          {failed && <LoadError onRetry={load} text={t('hosts.loadFailed')} />}
          {hosts === null && !failed && <p className="loading">{t('common.loading')}</p>}
          {(shared.length > 0 || sharedFailed) && sharedSection}
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
          {hosts && hosts.length > 0 && shownHosts.length === 0 && (
            <p className="page-none">
              {t('hosts.noMatch')}{' '}
              <button type="button" className="link-btn" onClick={() => setQ('')}>{t('common.clearSearch')}</button>
            </p>
          )}
        </div>
        {shownHosts.length > 0 && (
          <table className="desk-table" aria-label={t('hosts.all')}>
            <thead>
              <tr>
                {(['name', 'addr', 'state', 'recent'] as const).map((c) => {
                  const sortable = c === 'name' || c === 'recent'
                  const label = t(c === 'name' ? 'hosts.col.name' : c === 'addr' ? 'hosts.col.addr' : c === 'state' ? 'hosts.col.state' : 'hosts.col.recent')
                  return (
                    <th key={c} className={'desk-col-' + c} aria-sort={sortable && order === c ? (c === 'recent' ? 'descending' : 'ascending') : undefined}>
                      {sortable ? (
                        <button type="button" className="desk-sort" aria-pressed={order === c} title={t(c === 'name' ? 'hosts.orderName' : 'hosts.orderRecent')} onClick={() => (setOrder(c), local.set(HOST_ORDER, c))}>
                          {label}
                          {order === c && <ChevronDown size={13} aria-hidden="true" />}
                        </button>
                      ) : (
                        label
                      )}
                    </th>
                  )
                })}
                <th className="desk-col-menu"><span className="sr-only">{t('common.more')}</span></th>
              </tr>
            </thead>
            <tbody
              onKeyDown={(e) => {
                if (e.target !== e.currentTarget && (e.target as HTMLElement).tagName !== 'TR') return
                if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
                  e.preventDefault()
                  go(at < 0 ? 0 : at + (e.key === 'ArrowDown' ? 1 : -1))
                } else if (e.key === 'Home' || e.key === 'End') {
                  e.preventDefault()
                  go(e.key === 'Home' ? 0 : shownHosts.length - 1)
                } else if (e.key === 'Enter' && at >= 0) {
                  e.preventDefault()
                  void connect(shownHosts[at])
                }
              }}
            >
              {shownHosts.map((h, i) => {
                const st = hostState(h.id)
                const on = h.id === sel
                return (
                  <tr
                    key={h.id}
                    data-host={h.id}
                    className={on ? 'is-sel' : undefined}
                    aria-selected={on}
                    tabIndex={on || (at < 0 && i === 0) ? 0 : -1}
                    title={t('hosts.rowHint')}
                    onClick={() => setSel(h.id)}
                    onFocus={() => setSel(h.id)}
                    onDoubleClick={() => busyHost !== h.id && void connect(h)}
                    onContextMenu={(e) => {
                      e.preventDefault()
                      setSel(h.id)
                      e.currentTarget.querySelector<HTMLElement>('.desk-col-menu .icon-btn')?.click()
                    }}
                  >
                    <td className="desk-col-name">
                      <span className="desk-name">
                        <HostMark name={h.name} hue={hues.get(h.id)} />
                        <span className="desk-ellipsis">{h.name}</span>
                      </span>
                    </td>
                    <td className="desk-col-addr desk-mono"><span className="desk-ellipsis">{hostAddr(h)}</span></td>
                    <td className="desk-col-state">
                      {st && (
                        <span className={'host-card-state ' + (st.wait ? 'is-wait' : 'is-run')}>
                          <span className={'dot ' + (st.wait ? 'warn' : 'ok')} aria-hidden="true" />
                          {st.text}
                        </span>
                      )}
                    </td>
                    <td className="desk-col-recent desk-dim">{busyHost === h.id ? t('hosts.connecting') : h.last_connected_at ? relTime(h.last_connected_at) : ''}</td>
                    <td className="desk-col-menu" onDoubleClick={(e) => e.stopPropagation()}>
                      <Menu
                        label={h.name}
                        button={(open, toggle) => (
                          <button className="icon-btn" tabIndex={-1} aria-label={`${t('common.more')}：${h.name}`} aria-expanded={open} onClick={toggle}>
                            <MoreHorizontal size={16} />
                          </button>
                        )}
                        items={() => hostMenu(h)}
                      />
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
        {confirm}
        {dialogs}
        {code.dialog}
      </main>
    )
  }
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
        <section className="section section-group live" aria-labelledby="live-title">
          <h2 id="live-title" data-flip="live" data-flip-in="">{t('hosts.running')}<span className="section-n">{running.length}</span></h2>
          <ul className="live-rows">
            {running.map((s) => (
              <LiveCard key={s.id} s={s} all={running} stale={sessFailed} hue={hues.get(s.host_id)} onClose={(label) => closeSession(s, label)} />
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
        <section className="section section-group" data-flip="hosts">
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
          <ul className="host-grid">
            {shownHosts.map((h) => {
              const st = hostState(h.id)
              return (
                <li key={h.id} className="host-card">
                  <button className="host-card-main" onClick={() => connect(h)} disabled={busyHost === h.id} aria-label={`${t('hosts.connect')} ${h.name}`} aria-describedby={'hs-' + h.id}>
                    <span className="host-card-top">
                      <HostMark name={h.name} hue={hues.get(h.id)} />
                      <span className="host-card-name">{h.name}</span>
                    </span>
                    <span className="host-card-addr" id={'hs-' + h.id}>
                      {h.username}@{h.address}
                      {h.port !== 22 ? ':' + h.port : ''}
                      {h.jump_host_id ? ' · ' + t('host.via', { name: hosts?.find((x) => x.id === h.jump_host_id)?.name ?? '?' }) : ''}
                    </span>
                    <span className="host-card-foot">
                      {st && (
                        <span className={'host-card-state ' + (st.wait ? 'is-wait' : 'is-run')}>
                          <span className={'dot ' + (st.wait ? 'warn' : 'ok')} aria-hidden="true" />
                          {st.text}
                        </span>
                      )}
                      {/* ("8 天前连接": which time it is — ux1101 PG-11.) */}
                      <span className="host-card-when">{busyHost === h.id ? t('hosts.connecting') : h.last_connected_at ? t('hosts.lastConnected', { when: relTime(h.last_connected_at) }) : ''}</span>
                    </span>
                  </button>
                  <div className="host-card-menu">
                    <Menu
                      label={h.name}
                      button={(open, toggle) => (
                        <button className="icon-btn" aria-label={`${t('common.more')}：${h.name}`} aria-expanded={open} onClick={toggle}>
                          <MoreHorizontal size={18} />
                        </button>
                      )}
                      items={() => hostMenu(h)}
                    />
                  </div>
                </li>
              )
            })}
          </ul>
        </section>
      )}
      {confirm}
      {dialogs}
      {code.dialog}
    </main>
  )
}

// A host's mark (11-15): its initials on a tint of its own, the same hue
// wherever the host shows (its card, the dot of its running sessions), so a
// host is found by colour as well as by its name — "Home Services" HS,
// "dev-230" D2, "101" 10.
// The hue is given by the host's place among the user's hosts (oldest first),
// not by its name (11-16): from a name, three hosts came out two greens and a
// purple, and two alike read as two of a kind. No green, amber or red here:
// those are what the page says states in (running, waiting, failed).
const HOST_HUES = [214, 278, 188, 338, 26]
export function hostHues(hosts: Host[] | null): Map<number, number> {
  const ids = (hosts ?? []).map((h) => h.id).sort((a, b) => a - b)
  return new Map(ids.map((id, i) => [id, HOST_HUES[i % HOST_HUES.length]]))
}
export function hostInitials(name: string): string {
  const w = name.trim().split(/[\s._-]+/).filter(Boolean)
  const two = w.length > 1 ? [...w[0]][0] + [...w[1]][0] : [...name.trim()].slice(0, 2).join('')
  return two.toUpperCase() || '?'
}
function HostMark({ name, hue }: { name: string; hue: number | undefined }) {
  return (
    <span className="host-mark" style={{ '--h': hue ?? HOST_HUES[0] } as React.CSSProperties} aria-hidden="true">
      {hostInitials(name)}
    </span>
  )
}

// A running session, a row (11-05: no screen in small — at a card's size it
// told little, and without one a black block took the page): the host and
// which session of it this is (two shells of one host told apart, pages-1105
// 1.4); herdr agents waiting for an answer said first, in their colour. Their
// state when herdr was lost since, or the list could not be read just now
// (stale), is not told as now: it is said to be unknown (1.2).
function LiveCard({ s, all, stale, hue, onClose, pill }: { s: SessionInfo; all: SessionInfo[]; stale: boolean; hue: number | undefined; onClose: (label: string) => void; pill?: boolean }) {
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
    <li className={'live-row' + (pill ? ' is-pill' : '')} data-flip={'l:' + s.id} data-flip-in="">
      <a className="live-link" href={'/t/' + s.id} onClick={linkHandler('/t/' + s.id)} aria-label={`${s.host_name}：${label}`} aria-describedby={'ld-' + s.id} title={s.title || undefined}>
        <span className="live-info">
          {/* (A session, not a host: a terminal's mark, the session named first, its
              host after it in its hue — 11-16, the two read alike at a glance.) */}
          <span className="live-title">
            <span className="live-mark" aria-hidden="true">
              <SquareTerminal size={18} strokeWidth={1.75} />
            </span>
            <span className="live-which">{which}</span>
            <span className="live-host">
              {hue !== undefined && <span className="live-host-dot" style={{ '--h': hue } as React.CSSProperties} aria-hidden="true" />}
              {s.host_name}
            </span>
          </span>
          <span className="live-sub" id={'ld-' + s.id}>
            <span className={'dot ' + dot} aria-hidden="true" />
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
